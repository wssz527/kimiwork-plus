package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The MCP server list is not in config.toml. The installed kernel keeps it in
// `<homeDir>/mcp.json` as ordinary JSON:
//
//	{ "mcpServers": { "<name>": { "transport": "stdio", "command": … } }, … }
//
// That shape was read from the shipped kernel bundle, not guessed:
//
//	McpConfigStore   path = join(homeDir, "mcp.json")
//	                 read() = JSON.parse(bytes)["mcpServers"]
//	                 write() = {...raw, mcpServers} with 2-space JSON + "\n",
//	                           atomic
//	list/add/update/remove all go through that one file
//	persistedEntry   stores the entry without its `name`
//	McpServerConfigSchema  transport is "stdio" | "http" | "sse"; a missing
//	                 transport is inferred from `command` (stdio) or `url` (http)
//
// So CRUD is a plain file operation, and this type implements it directly. What
// is NOT a file operation is `testGlobalMcpServer`: the kernel routes it through
// `IMcpManagementService`, which owns the connection manager and the process it
// starts. That needs the kernel's own runtime, and `MCPTestUnavailable` says so
// instead of pretending.

// MCPFileStore is the real transport: it reads and writes `mcp.json`.
//
// Every write is a whole-file atomic replace of the document the kernel itself
// would write, with unknown top-level keys and unknown server fields preserved,
// because the kernel tolerates both and dropping them would silently discard a
// setting this build does not know about.
type MCPFileStore struct {
	homeDir string
}

// NewMCPFileStore opens the store for one explicit Kimi home. `kimiHome` is the
// Work data directory; the kernel's own home is derived from it, so no default
// can reach the user's real installation.
func NewMCPFileStore(kimiHome string) (*MCPFileStore, error) {
	paths, err := newConfigPaths(kimiHome)
	if err != nil {
		return nil, err
	}
	return &MCPFileStore{homeDir: paths.homeDir}, nil
}

// Path is the file this store edits, for diagnostics.
func (s *MCPFileStore) Path() string { return filepath.Join(s.homeDir, "mcp.json") }

// mcpDocument is the file's structure. `raw` keeps every top-level key the
// kernel wrote, including ones this build does not model.
type mcpDocument struct {
	raw     map[string]json.RawMessage
	servers map[string]json.RawMessage
}

func (s *MCPFileStore) read() (mcpDocument, error) {
	document := mcpDocument{raw: map[string]json.RawMessage{}, servers: map[string]json.RawMessage{}}
	path := s.Path()
	info, err := lstatPath(path)
	if err != nil {
		if isNotExist(err) {
			return document, nil
		}
		return document, errors.New("native_mcp_config_unavailable")
	}
	if info.isSymlink || !info.isFile {
		return document, errors.New("native_mcp_config_unavailable")
	}
	if info.size > 8<<20 {
		return document, errors.New("native_mcp_config_too_large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return document, errors.New("native_mcp_config_unavailable")
	}
	if strings.TrimSpace(string(data)) == "" {
		return document, nil
	}
	if json.Unmarshal(data, &document.raw) != nil {
		return document, errors.New("native_mcp_config_invalid")
	}
	if raw, present := document.raw["mcpServers"]; present {
		if json.Unmarshal(raw, &document.servers) != nil {
			return document, errors.New("native_mcp_config_invalid")
		}
	}
	return document, nil
}

// write replaces the file the way the kernel does: the whole document, two-space
// indent, trailing newline, atomic.
func (s *MCPFileStore) write(document mcpDocument) error {
	if err := os.MkdirAll(s.homeDir, 0o700); err != nil {
		return errors.New("native_mcp_config_unavailable")
	}
	servers := document.servers
	if servers == nil {
		servers = map[string]json.RawMessage{}
	}
	encoded, err := json.Marshal(servers)
	if err != nil {
		return errors.New("native_mcp_config_invalid")
	}
	document.raw["mcpServers"] = encoded
	content, err := json.MarshalIndent(document.raw, "", "  ")
	if err != nil {
		return errors.New("native_mcp_config_invalid")
	}
	mode := os.FileMode(0o600)
	if info, err := lstatPath(s.Path()); err == nil {
		mode = info.mode
	}
	if err := writePrivateFile(s.Path(), append(content, '\n'), mode); err != nil {
		return errors.New("native_mcp_config_unavailable")
	}
	return nil
}

// ListServers reads every stored entry, in file order.
//
// The order is preserved because the kernel lists them in file order too, and a
// UI that reorders the list on every read would look unstable.
func (s *MCPFileStore) ListServers(context.Context) ([]MCPServer, error) {
	document, err := s.read()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(document.servers))
	for name := range document.servers {
		names = append(names, name)
	}
	// Go map iteration is randomised; the file's own order is recovered by
	// re-reading the key sequence from the raw bytes.
	if ordered, ok := orderedServerNames(document.raw["mcpServers"]); ok {
		names = ordered
	}
	servers := make([]MCPServer, 0, len(names))
	for _, name := range names {
		raw, present := document.servers[name]
		if !present {
			continue
		}
		server, ok := decodeMCPServer(name, raw)
		if !ok {
			// An entry this build cannot read is still listed, with its name and
			// an unknown transport: hiding it would make the panel disagree with
			// the file.
			servers = append(servers, MCPServer{Name: name, Transport: "unknown"})
			continue
		}
		servers = append(servers, server)
	}
	return servers, nil
}

// orderedServerNames recovers the key order from the raw JSON object.
func orderedServerNames(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, false
	}
	var names []string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		name, ok := key.(string)
		if !ok {
			return nil, false
		}
		names = append(names, name)
		var skip json.RawMessage
		if decoder.Decode(&skip) != nil {
			return nil, false
		}
	}
	return names, true
}

// decodeMCPServer maps one stored entry. The transport is inferred the same way
// the kernel infers it: an explicit `transport`, else `command` means stdio and
// `url` means http.
func decodeMCPServer(name string, raw json.RawMessage) (MCPServer, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return MCPServer{}, false
	}
	server := MCPServer{Name: name}
	server.Transport = stringValue(fields, "transport")
	if server.Transport == "" {
		if stringValue(fields, "command") != "" {
			server.Transport = "stdio"
		} else if stringValue(fields, "url") != "" {
			server.Transport = "http"
		}
	}
	if server.Transport != "stdio" && server.Transport != "http" && server.Transport != "sse" {
		return MCPServer{}, false
	}
	server.Command = stringValue(fields, "command")
	server.Cwd = stringValue(fields, "cwd")
	server.Executor = stringValue(fields, "executor")
	server.EnvironmentID = stringValue(fields, "environment_id")
	server.URL = stringValue(fields, "url")
	server.Auth = stringValue(fields, "auth")
	server.BearerTokenEnvVar = stringValue(fields, "bearerTokenEnvVar")
	server.Enabled = boolPointer(fields, "enabled")
	server.Deferred = boolPointer(fields, "deferred")
	server.StartupTimeoutMS = intPointer(fields, "startupTimeoutMs")
	server.ToolTimeoutMS = intPointer(fields, "toolTimeoutMs")
	server.EnabledTools = stringSliceValue(fields, "enabledTools")
	server.DisabledTools = stringSliceValue(fields, "disabledTools")
	server.Args = stringSliceValue(fields, "args")
	server.Env = stringMapValue(fields, "env")
	server.Headers = stringMapValue(fields, "headers")
	return server, true
}

// encodeMCPServer renders one entry the way the kernel persists it: the entry
// without its `name`, in a stable field order.
func encodeMCPServer(server MCPServer) map[string]any {
	entry := map[string]any{"transport": server.Transport}
	if server.Transport == "stdio" {
		entry["command"] = server.Command
		if server.Args != nil {
			entry["args"] = server.Args
		}
		if server.Env != nil {
			entry["env"] = server.Env
		}
		if server.Cwd != "" {
			entry["cwd"] = server.Cwd
		}
		if server.Executor != "" {
			entry["executor"] = server.Executor
		}
		if server.EnvironmentID != "" {
			entry["environment_id"] = server.EnvironmentID
		}
	} else {
		entry["url"] = server.URL
		if server.Headers != nil {
			entry["headers"] = server.Headers
		}
		if server.Auth != "" {
			entry["auth"] = server.Auth
		}
		if server.BearerTokenEnvVar != "" {
			entry["bearerTokenEnvVar"] = server.BearerTokenEnvVar
		}
	}
	if server.Enabled != nil {
		entry["enabled"] = *server.Enabled
	}
	if server.Deferred != nil {
		entry["deferred"] = *server.Deferred
	}
	if server.StartupTimeoutMS != nil {
		entry["startupTimeoutMs"] = *server.StartupTimeoutMS
	}
	if server.ToolTimeoutMS != nil {
		entry["toolTimeoutMs"] = *server.ToolTimeoutMS
	}
	if server.EnabledTools != nil {
		entry["enabledTools"] = server.EnabledTools
	}
	if server.DisabledTools != nil {
		entry["disabledTools"] = server.DisabledTools
	}
	return entry
}

// AddServer stores one entry, refusing a duplicate name the way the kernel does.
func (s *MCPFileStore) AddServer(_ context.Context, server MCPServer) error {
	document, err := s.read()
	if err != nil {
		return err
	}
	if _, present := document.servers[server.Name]; present {
		return errors.New("native_mcp_server_exists")
	}
	encoded, err := json.Marshal(encodeMCPServer(server))
	if err != nil {
		return errors.New("native_mcp_config_invalid")
	}
	document.servers[server.Name] = encoded
	return s.write(document)
}

// UpdateServer replaces one entry, refusing an unknown name.
func (s *MCPFileStore) UpdateServer(_ context.Context, server MCPServer) error {
	document, err := s.read()
	if err != nil {
		return err
	}
	if _, present := document.servers[server.Name]; !present {
		return errors.New("native_mcp_server_not_found")
	}
	encoded, err := json.Marshal(encodeMCPServer(server))
	if err != nil {
		return errors.New("native_mcp_config_invalid")
	}
	document.servers[server.Name] = encoded
	return s.write(document)
}

// RemoveServer deletes one entry. Removing an absent name is a no-op, exactly as
// the kernel's own store treats it.
func (s *MCPFileStore) RemoveServer(_ context.Context, name string) error {
	document, err := s.read()
	if err != nil {
		return err
	}
	if _, present := document.servers[name]; !present {
		return nil
	}
	delete(document.servers, name)
	return s.write(document)
}

// TestServer is not available from a file store.
//
// The kernel's `testGlobalMcpServer` resolves the entry, then hands it to
// `IMcpManagementService.testServer`, which owns the connection manager: it
// spawns the process or opens the connection, performs the MCP handshake, lists
// the tools and shuts the probe down. None of that is reachable from the JSON
// file, and reimplementing it would mean writing a second MCP client with its own
// handshake, which would then disagree with the kernel's.
func (s *MCPFileStore) TestServer(context.Context, string, string) (MCPTestResult, error) {
	return MCPTestResult{}, ErrMCPTestUnavailable
}

// Close is a no-op: the store holds no handle between calls.
func (s *MCPFileStore) Close(context.Context) error { return nil }

// ErrMCPTestUnavailable is returned by a transport that cannot run a probe.
var ErrMCPTestUnavailable = errors.New("native_mcp_test_unavailable")

// MCPTestUnavailableReason is the code the panel shows.
const MCPTestUnavailableReason = "native_mcp_test_requires_kernel_runtime"

// IsMCPTestUnavailable reports whether an error is the "no probe here" answer.
func IsMCPTestUnavailable(err error) bool {
	return errors.Is(err, ErrMCPTestUnavailable)
}

// ---------------------------------------------------------------------------
// field helpers

func stringValue(fields map[string]json.RawMessage, name string) string {
	raw, present := fields[name]
	if !present {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func boolPointer(fields map[string]json.RawMessage, name string) *bool {
	raw, present := fields[name]
	if !present {
		return nil
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

func intPointer(fields map[string]json.RawMessage, name string) *int64 {
	raw, present := fields[name]
	if !present {
		return nil
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

func stringSliceValue(fields map[string]json.RawMessage, name string) []string {
	raw, present := fields[name]
	if !present {
		return nil
	}
	var value []string
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

func stringMapValue(fields map[string]json.RawMessage, name string) map[string]string {
	raw, present := fields[name]
	if !present {
		return nil
	}
	var value map[string]string
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}
