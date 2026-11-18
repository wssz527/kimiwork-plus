package bridge

import (
	"context"
	"encoding/json"
	"kimiworkplus.local/migration/internal/core"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newIsolatedTestPolicies(dir string) (*nativePolicies, error) {
	home := filepath.Join(dir, "kimi-profile", "daimon-share", "daimon")
	return newNativePolicies(dir, home)
}

func TestNativeSettingsStayIsolated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, e := core.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	p, e := newIsolatedTestPolicies(dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(p.subagents.HomeDir(), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p.subagents.ConfigPath(), []byte("# fixture\n"), 0600); e != nil {
		t.Fatal(e)
	}
	s := &Service{Store: store, StateDir: dir, Keys: CacheKeys(testKeys{})}
	n := &Native{service: s, policies: p}
	if e = n.initEnhancements(); e != nil {
		t.Fatal(e)
	}
	defer n.web.Close()
	d, _, _ := store.Read()
	if e = p.Configure(d); e != nil {
		t.Fatal(e)
	}
	result, e := s.saveNativeContext(p, core.Document{"policy": core.Document{"mode": "custom", "triggerRatio": 0.85, "reservedTokens": float64(0)}})
	if e != nil {
		t.Fatal(e)
	}
	if core.Object(core.Object(result)["context"])["status"] != "file-applied" {
		t.Fatal(result)
	}
	_, e = s.saveNativeContext(p, core.Document{"services": core.Document{"search": core.Document{"enabled": true, "baseURL": "https://example.test/search", "apiKey": "synthetic-web"}}})
	if e != nil {
		t.Fatal(e)
	}
	d, _, _ = store.Read()
	raw, _ := json.Marshal(p.State(d))
	if strings.Contains(string(raw), "synthetic-web") {
		t.Fatal("key leaked")
	}
	result, e = n.toolsAction(context.Background(), core.Document{"action": "add", "server": core.Document{"name": "fixture", "transport": "stdio", "command": "/usr/bin/false"}})
	if e != nil {
		t.Fatal(e)
	}
	if core.Object(core.Object(result)["tools"])["available"] != true {
		t.Fatal(result)
	}
	if _, e = n.toolsAction(context.Background(), core.Document{"action": "test", "name": "fixture"}); e == nil {
		t.Fatal("claimed runtime probe")
	}
	result, e = n.webAction(core.Document{"action": "save", "provider": "official"})
	if e != nil {
		t.Fatal(e)
	}
	if result == nil {
		t.Fatal("missing web state")
	}
}

func TestConnectionWithoutKeyAndSubagentPolicy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, e := core.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	keys := CacheKeys(testKeys{})
	runtime, e := NewRuntime(dir, store, keys)
	if e != nil {
		t.Fatal(e)
	}
	s := &Service{Store: store, Keys: keys, OnConfig: runtime.Apply, Gateway: runtime}
	_, rev, _ := store.Read()
	if _, e = s.configure("/api/connections", core.Document{"revision": rev, "connections": []any{core.Document{"id": "empty", "name": "Empty", "baseURL": "http://127.0.0.1:9999/v1"}}}); e != nil {
		t.Fatal(e)
	}
	if _, e = NewRuntime(dir, store, keys); e != nil {
		t.Fatal(e)
	}
	_, rev, _ = store.Read()
	_, e = s.configure("/api/models", core.Document{"revision": rev, "models": []any{core.Document{"id": "kimi-alpha", "connectionId": "empty", "protocol": "chat", "contextWindow": float64(32000), "tools": true}}})
	if e != nil {
		t.Fatal(e)
	}
	_, rev, _ = store.Read()
	if _, e = s.configure("/api/subagents", core.Document{"revision": rev, "mode": "force", "model": "kimi-alpha"}); e != nil {
		t.Fatal(e)
	}
	d, _, _ := store.Read()
	if core.Object(d["subagents"])["modelAlias"] != "byok-kth-kimi/kimi-alpha" {
		t.Fatal(d)
	}
}

// A policy whose models are not declared in config.toml cannot be written, and
// the file it was not written to says nothing. Reporting that as "inherit" while
// the saved mode is "force" claims the opposite of what the user asked for, so the
// state must carry the wait.
func TestSubagentStateReportsAModelThatIsNotYetDeclared(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	p, err := newIsolatedTestPolicies(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(p.subagents.HomeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.subagents.ConfigPath(), []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := core.Document{
		"subagents": core.Document{"mode": "force", "model": "group/glm-5-3-flash"},
		"models":    []any{core.Document{"id": "group/glm-5-3-flash", "protocol": "chat", "tools": true}},
	}
	if err = p.Configure(document); err != nil {
		t.Fatal(err)
	}
	state := subagentStateDocument(t, p, document)
	policy := state["policy"].(map[string]any)
	runtime := state["runtime"].(map[string]any)
	if policy["state"] != "waiting-config" {
		t.Fatalf("policy after an undeclared model: %#v", policy)
	}
	if policy["reason"] != "models_missing" {
		t.Fatalf("policy reason: %#v", policy)
	}
	if policy["mode"] != "force" {
		t.Fatalf("policy mode: %#v", policy)
	}
	if runtime["state"] != "waiting-config" || runtime["mode"] != "force" {
		t.Fatalf("runtime after an undeclared model: %#v", runtime)
	}
	if core.Object(state["configured"])["modelAlias"] != "byok-kth-resp/group/glm-5-3-flash" {
		t.Fatalf("configured alias: %#v", state["configured"])
	}

	// Declaring the model the way the official import does is what lets the block
	// be written: the alias the saved policy names is the one the import creates.
	declared := "[models.\"byok-kth-resp/group/glm-5-3-flash\"]\nprovider = \"fixture\"\nmodel = \"group/glm-5-3-flash\"\n"
	if err = os.WriteFile(p.subagents.ConfigPath(), []byte(declared), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = p.Configure(document); err != nil {
		t.Fatal(err)
	}
	state = subagentStateDocument(t, p, document)
	if state["policy"].(map[string]any)["state"] != "file-applied" {
		t.Fatalf("policy after the declaration: %#v", state["policy"])
	}
	if state["runtime"].(map[string]any)["state"] != "file-applied" {
		t.Fatalf("runtime after the declaration: %#v", state["runtime"])
	}
	content, err := os.ReadFile(p.subagents.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "[secondary_model]") ||
		!strings.Contains(string(content), "\"byok-kth-resp/group/glm-5-3-flash\"") {
		t.Fatalf("the managed block is missing:\n%s", content)
	}
}

// An inherit policy has nothing to wait for: the file and the saved mode already
// agree, so the update result must not be substituted.
func TestSubagentStateDoesNotInventAWaitForInherit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	p, err := newIsolatedTestPolicies(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(p.subagents.HomeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.subagents.ConfigPath(), []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := core.Document{"subagents": core.Document{"mode": "inherit"}}
	if err = p.Configure(document); err != nil {
		t.Fatal(err)
	}
	state := subagentStateDocument(t, p, document)
	if state["policy"].(map[string]any)["state"] != "inherit" {
		t.Fatalf("inherit policy: %#v", state["policy"])
	}
	if state["runtime"].(map[string]any)["state"] != "inherit" {
		t.Fatalf("inherit runtime: %#v", state["runtime"])
	}
}

// subagentStateDocument renders the panel document and reads it back through
// JSON, which is how the front end sees the nested status records.
func subagentStateDocument(t *testing.T, p *nativePolicies, document core.Document) map[string]any {
	t.Helper()
	raw, err := json.Marshal(core.Object(p.State(document))["subagents"])
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
