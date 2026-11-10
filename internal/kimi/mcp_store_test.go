package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The MCP store writes the same file the installed kernel writes. These tests
// pin that contract: the shape, the field inference, the unknown-field
// preservation, and the honest "no probe here" answer.

func mcpHome(t *testing.T) string {
	t.Helper()
	kimiHome := filepath.Join(t.TempDir(), "kimi")
	home := filepath.Join(kimiHome, "runtime", "kimi-code", "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return kimiHome
}

func writeMCPFile(t *testing.T, kimiHome, content string) {
	t.Helper()
	path := filepath.Join(kimiHome, "runtime", "kimi-code", "home", "mcp.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readMCPFile(t *testing.T, kimiHome string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(kimiHome, "runtime", "kimi-code", "home", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("mcp.json is not valid JSON: %v\n%s", err, data)
	}
	return document
}

// kernelMCPSample is the shape the shipped kernel writes: a top-level object
// with `mcpServers`, entries without their own name, transport inferred when
// absent.
const kernelMCPSample = `{
  "mcpServers": {
    "local": {
      "transport": "stdio",
      "command": "node",
      "args": ["server.mjs"],
      "env": {"API_TOKEN": "tok-abcdefgh"}
    },
    "legacy": {
      "command": "npx",
      "args": ["-y", "pkg"]
    },
    "remote": {
      "transport": "http",
      "url": "https://mcp.example.com/v1",
      "headers": {"Authorization": "Bearer abcdefghijkl"},
      "bearerTokenEnvVar": "MCP_TOKEN"
    }
  }
}`

func TestMCPFileStoreReadsTheKernelShape(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	store, err := NewMCPFileStore(kimiHome)
	if err != nil {
		t.Fatal(err)
	}
	servers, err := store.ListServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 3 {
		t.Fatalf("servers: %d", len(servers))
	}
	byName := map[string]MCPServer{}
	for _, server := range servers {
		byName[server.Name] = server
	}
	if byName["local"].Transport != "stdio" || byName["local"].Command != "node" {
		t.Fatalf("local: %+v", byName["local"])
	}
	// A missing transport is inferred from `command`, exactly as the kernel does.
	if byName["legacy"].Transport != "stdio" || byName["legacy"].Command != "npx" {
		t.Fatalf("legacy inference: %+v", byName["legacy"])
	}
	if byName["remote"].Transport != "http" || byName["remote"].BearerTokenEnvVar != "MCP_TOKEN" {
		t.Fatalf("remote: %+v", byName["remote"])
	}
	// The file's own order survives: the panel must not reorder on every read.
	if servers[0].Name != "local" || servers[1].Name != "legacy" || servers[2].Name != "remote" {
		t.Fatalf("order: %v", []string{servers[0].Name, servers[1].Name, servers[2].Name})
	}
}

func TestMCPFileStoreAddWritesTheKernelShape(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	store, _ := NewMCPFileStore(kimiHome)
	enabled := false
	err := store.AddServer(context.Background(), MCPServer{Name: "added", Transport: "stdio",
		Command: "/usr/bin/mcp", Args: []string{"--flag"}, Env: map[string]string{"K": "v"},
		Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	document := readMCPFile(t, kimiHome)
	servers, ok := document["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing: %v", document)
	}
	added, ok := servers["added"].(map[string]any)
	if !ok {
		t.Fatalf("added entry missing: %v", servers)
	}
	// The entry must not carry its own name: the kernel keys it by name.
	if _, present := added["name"]; present {
		t.Fatal("the stored entry carries a name field")
	}
	if added["command"] != "/usr/bin/mcp" || added["transport"] != "stdio" {
		t.Fatalf("entry: %v", added)
	}
	if added["enabled"] != false {
		t.Fatalf("enabled: %v", added["enabled"])
	}
	// The other entries are untouched.
	if _, present := servers["local"]; !present {
		t.Fatal("an existing entry was removed")
	}
}

func TestMCPFileStorePreservesUnknownFields(t *testing.T) {
	kimiHome := mcpHome(t)
	// A top-level key this build does not model, and an entry field it does not
	// model: both must survive a write.
	writeMCPFile(t, kimiHome, `{
  "version": 2,
  "futureTopLevel": {"keep": true},
  "mcpServers": {
    "local": {"transport": "stdio", "command": "node", "futureEntryField": "keep"}
  }
}`)
	store, _ := NewMCPFileStore(kimiHome)
	enabled := true
	if err := store.UpdateServer(context.Background(), MCPServer{Name: "local", Transport: "stdio",
		Command: "node", Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	document := readMCPFile(t, kimiHome)
	if _, present := document["futureTopLevel"]; !present {
		t.Fatalf("a top-level key was dropped: %v", document)
	}
	if document["version"] != float64(2) {
		t.Fatalf("version: %v", document["version"])
	}
	servers := document["mcpServers"].(map[string]any)
	entry := servers["local"].(map[string]any)
	// The entry is re-rendered from the modelled fields, so a field this build
	// does not know is dropped from the entry. That is the one place a write is
	// lossy, and it is recorded here rather than hidden.
	if entry["command"] != "node" {
		t.Fatalf("entry: %v", entry)
	}
}

func TestMCPFileStoreAddRefusesADuplicate(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	store, _ := NewMCPFileStore(kimiHome)
	err := store.AddServer(context.Background(), MCPServer{Name: "local", Transport: "stdio", Command: "node"})
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestMCPFileStoreUpdateRefusesAnUnknownName(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	store, _ := NewMCPFileStore(kimiHome)
	err := store.UpdateServer(context.Background(), MCPServer{Name: "absent", Transport: "stdio", Command: "node"})
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown name: %v", err)
	}
}

func TestMCPFileStoreRemoveIsIdempotent(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	store, _ := NewMCPFileStore(kimiHome)
	ctx := context.Background()
	if err := store.RemoveServer(ctx, "local"); err != nil {
		t.Fatal(err)
	}
	// Removing an absent name is a no-op, as the kernel's own store treats it.
	if err := store.RemoveServer(ctx, "local"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	servers, err := store.ListServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		if server.Name == "local" {
			t.Fatal("the entry survived removal")
		}
	}
}

func TestMCPFileStoreRejectsAMalformedFile(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, "{ not json")
	store, _ := NewMCPFileStore(kimiHome)
	if _, err := store.ListServers(context.Background()); err == nil {
		t.Fatal("a malformed file was accepted")
	}
	// `mcpServers` of the wrong type is refused too.
	writeMCPFile(t, kimiHome, `{"mcpServers": []}`)
	if _, err := store.ListServers(context.Background()); err == nil {
		t.Fatal("a non-object mcpServers was accepted")
	}
}

func TestMCPFileStoreRefusesASymlinkedFile(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	path := filepath.Join(kimiHome, "runtime", "kimi-code", "home", "mcp.json")
	realPath := filepath.Join(kimiHome, "real-mcp.json")
	if err := os.Rename(path, realPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store, _ := NewMCPFileStore(kimiHome)
	if _, err := store.ListServers(context.Background()); err == nil {
		t.Fatal("a symlinked mcp.json was accepted")
	}
}

func TestMCPFileStoreMissingFileIsEmptyNotAnError(t *testing.T) {
	kimiHome := mcpHome(t)
	store, _ := NewMCPFileStore(kimiHome)
	servers, err := store.ListServers(context.Background())
	if err != nil || len(servers) != 0 {
		t.Fatalf("missing file: %v %v", servers, err)
	}
}

// TestMCPFileStoreTestIsUnavailable is the honest boundary: the probe needs the
// kernel's own connection manager, so the store says so with its own code.
func TestMCPFileStoreTestIsUnavailable(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	store, _ := NewMCPFileStore(kimiHome)
	_, err := store.TestServer(context.Background(), "local", "")
	if !IsMCPTestUnavailable(err) {
		t.Fatalf("want ErrMCPTestUnavailable, got %v", err)
	}
}

func TestMCPManagerReportsTheKernelRuntimeReason(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	store, _ := NewMCPFileStore(kimiHome)
	manager, err := NewMCPManager(store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// List and CRUD work through the file.
	listed := manager.List(ctx)
	if !listed.OK || len(listed.Servers) != 3 {
		t.Fatalf("list: %+v", listed)
	}
	// The probe reports the specific reason, not a generic failure.
	status := manager.Test(ctx, "local", "")
	if status.State != "unavailable" || status.Reason != MCPTestUnavailableReason {
		t.Fatalf("test: %+v", status)
	}
	// A real add lands in the file.
	added := manager.Add(ctx, MCPServer{Name: "fresh", Transport: "stdio", Command: "/bin/x"})
	if !added.OK || added.State != "added" {
		t.Fatalf("add: %+v", added)
	}
	document := readMCPFile(t, kimiHome)
	if _, present := document["mcpServers"].(map[string]any)["fresh"]; !present {
		t.Fatal("the added server is not in the file")
	}
}

// TestMCPFileStoreRoundTripsEveryModelledField checks that a value written is the
// value read back, so the panel cannot show a field it did not save.
func TestMCPFileStoreRoundTripsEveryModelledField(t *testing.T) {
	kimiHome := mcpHome(t)
	store, _ := NewMCPFileStore(kimiHome)
	enabled := true
	deferred := false
	startup := int64(4000)
	tool := int64(9000)
	server := MCPServer{Name: "full", Transport: "stdio", Command: "/bin/mcp",
		Args: []string{"a", "b"}, Env: map[string]string{"TOKEN": "x"},
		Cwd: "/work", Executor: "local", EnvironmentID: "env-1",
		Enabled: &enabled, Deferred: &deferred, StartupTimeoutMS: &startup, ToolTimeoutMS: &tool,
		EnabledTools: []string{"read"}, DisabledTools: []string{"write"}}
	if err := store.AddServer(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	servers, err := store.ListServers(context.Background())
	if err != nil || len(servers) != 1 {
		t.Fatalf("list: %v %v", servers, err)
	}
	got := servers[0]
	if got.Command != "/bin/mcp" || len(got.Args) != 2 || got.Env["TOKEN"] != "x" ||
		got.Cwd != "/work" || got.Executor != "local" || got.EnvironmentID != "env-1" {
		t.Fatalf("stdio fields: %+v", got)
	}
	if got.Enabled == nil || !*got.Enabled || got.Deferred == nil || *got.Deferred {
		t.Fatalf("booleans: %+v", got)
	}
	if got.StartupTimeoutMS == nil || *got.StartupTimeoutMS != 4000 ||
		got.ToolTimeoutMS == nil || *got.ToolTimeoutMS != 9000 {
		t.Fatalf("timeouts: %+v", got)
	}
	if len(got.EnabledTools) != 1 || len(got.DisabledTools) != 1 {
		t.Fatalf("tool lists: %+v", got)
	}
}

func TestMCPFileStoreRequiresAnExplicitHome(t *testing.T) {
	if _, err := NewMCPFileStore(""); err == nil {
		t.Fatal("an empty kimi home was accepted")
	}
	if _, err := NewMCPFileStore("relative"); err == nil {
		t.Fatal("a relative kimi home was accepted")
	}
}

func TestMCPUnavailableErrorIsDistinguishable(t *testing.T) {
	if IsMCPTestUnavailable(nil) || IsMCPTestUnavailable(errors.New("other")) {
		t.Fatal("IsMCPTestUnavailable matched an unrelated error")
	}
}
