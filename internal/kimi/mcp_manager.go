package kimi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// MCP servers are a kernel feature: the installed application owns the list and
// exposes it through its own client. This module is the manager the panel calls.
//
// Two properties matter more than the CRUD surface:
//
//   - a server description is validated field by field against an allowlist, so a
//     field the kernel does not know is refused rather than forwarded;
//   - every value that reaches a caller is redacted: a command line, an argument,
//     a header or an environment value may carry a credential, and a UI that
//     echoes it would put the secret on screen and in a log.
//
// The transport is a narrow interface rather than a concrete client, because the
// kernel client is a JS module. A caller supplies it; a test supplies a fake.

const (
	mcpMaxServers    = 500
	mcpMaxName       = 64
	mcpMaxCommand    = 1024
	mcpMaxArg        = 1024
	mcpMaxArgs       = 64
	mcpMaxEnv        = 64
	mcpMaxEnvName    = 128
	mcpMaxEnvValue   = 8192
	mcpMaxURL        = 2048
	mcpMaxTools      = 256
	mcpMaxToolName   = 128
	mcpMaxOutput     = 1200
	mcpMaxOutputLine = 40
	mcpMaxMessage    = 400
)

var (
	mcpNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	mcpEnvPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	mcpHeaderPattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)
	mcpSecretWord    = regexp.MustCompile(`(?i)key|token|secret|passwd|pwd|auth|credential|bearer`)
)

// blockedKeys are refused before validation: they would reach a JS object's
// prototype chain.
var mcpBlockedKeys = map[string]bool{"__proto__": true, "prototype": true, "constructor": true}

var mcpServerKeys = map[string]bool{"name": true, "transport": true, "enabled": true, "deferred": true,
	"enabledTools": true, "disabledTools": true, "startupTimeoutMs": true, "toolTimeoutMs": true}

var mcpStdioKeys = unionKeys(mcpServerKeys, []string{"command", "args", "env", "cwd", "executor", "environment_id"})
var mcpURLKeys = unionKeys(mcpServerKeys, []string{"url", "headers", "auth", "bearerTokenEnvVar"})
var mcpAllKeys = unionKeys(mcpStdioKeys, keysOf(mcpURLKeys))

func unionKeys(base map[string]bool, extra []string) map[string]bool {
	out := make(map[string]bool, len(base)+len(extra))
	for key := range base {
		out[key] = true
	}
	for _, key := range extra {
		out[key] = true
	}
	return out
}

func keysOf(source map[string]bool) []string {
	out := make([]string, 0, len(source))
	for key := range source {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// MCPServer is one server description. A field that is absent is preserved on
// update, which is what a UI that edits one row needs.
type MCPServer struct {
	Name              string            `json:"name"`
	Transport         string            `json:"transport"`
	Enabled           *bool             `json:"enabled,omitempty"`
	Deferred          *bool             `json:"deferred,omitempty"`
	EnabledTools      []string          `json:"enabledTools,omitempty"`
	DisabledTools     []string          `json:"disabledTools,omitempty"`
	StartupTimeoutMS  *int64            `json:"startupTimeoutMs,omitempty"`
	ToolTimeoutMS     *int64            `json:"toolTimeoutMs,omitempty"`
	Command           string            `json:"command,omitempty"`
	Args              []string          `json:"args,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	Cwd               string            `json:"cwd,omitempty"`
	Executor          string            `json:"executor,omitempty"`
	EnvironmentID     string            `json:"environment_id,omitempty"`
	URL               string            `json:"url,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	Auth              string            `json:"auth,omitempty"`
	BearerTokenEnvVar string            `json:"bearerTokenEnvVar,omitempty"`
}

// MCPServerView is the redacted projection a caller may see.
type MCPServerView struct {
	Name        string   `json:"name"`
	Transport   string   `json:"transport"`
	Enabled     *bool    `json:"enabled,omitempty"`
	Deferred    *bool    `json:"deferred,omitempty"`
	Tools       int      `json:"tools,omitempty"`
	Command     string   `json:"command,omitempty"`
	ArgsCount   int      `json:"argsCount,omitempty"`
	URL         string   `json:"url,omitempty"`
	HeaderNames []string `json:"headerNames,omitempty"`
	EnvNames    []string `json:"envNames,omitempty"`
}

// MCPStatus is the answer to every operation.
type MCPStatus struct {
	OK      bool            `json:"ok"`
	State   string          `json:"state"`
	Reason  string          `json:"reason,omitempty"`
	Field   string          `json:"field,omitempty"`
	Servers []MCPServerView `json:"servers,omitempty"`
	Server  *MCPServerView  `json:"server,omitempty"`
	Tools   []string        `json:"tools,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Detail  string          `json:"detail,omitempty"`
}

// MCPTransport is the kernel client this manager drives. The installed
// application's client implements it; a test supplies a fake.
type MCPTransport interface {
	ListServers(ctx context.Context) ([]MCPServer, error)
	AddServer(ctx context.Context, server MCPServer) error
	UpdateServer(ctx context.Context, server MCPServer) error
	RemoveServer(ctx context.Context, name string) error
	TestServer(ctx context.Context, name, cwd string) (MCPTestResult, error)
	Close(ctx context.Context) error
}

// MCPTestResult is what the kernel reports after starting a server.
type MCPTestResult struct {
	Success bool
	Output  string
}

// MCPManager runs the CRUD surface against one transport.
type MCPManager struct {
	mu          sync.Mutex
	transport   MCPTransport
	timeout     time.Duration
	testTimeout time.Duration
	closed      bool
	pendingTest bool
	now         func() time.Time
}

// NewMCPManager builds a manager. The transport is required.
func NewMCPManager(transport MCPTransport) (*MCPManager, error) {
	if transport == nil {
		return nil, errors.New("native_mcp_transport_required")
	}
	return &MCPManager{transport: transport, timeout: 15 * time.Second,
		testTimeout: 30 * time.Second, now: time.Now}, nil
}

// List returns every server, redacted.
func (m *MCPManager) List(ctx context.Context) MCPStatus {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return mcpFailure("closed", "native_mcp_manager_closed")
	}
	m.mu.Unlock()
	servers, err := m.call(ctx, m.timeout)
	if err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	return MCPStatus{OK: true, State: "ready", Servers: redactServers(servers)}
}

func (m *MCPManager) call(ctx context.Context, timeout time.Duration) ([]MCPServer, error) {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return m.transport.ListServers(bounded)
}

// Add creates one server.
func (m *MCPManager) Add(ctx context.Context, server MCPServer) MCPStatus {
	checked, field, ok := validateMCPServer(server, false)
	if !ok {
		return mcpFailure("invalid", "native_mcp_invalid_input", field)
	}
	servers, err := m.call(ctx, m.timeout)
	if err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	for _, existing := range servers {
		if existing.Name == checked.Name {
			return MCPStatus{State: "exists", Reason: "native_mcp_server_exists"}
		}
	}
	bounded, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	if err := m.transport.AddServer(bounded, checked); err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	after, err := m.call(ctx, m.timeout)
	if err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	view := projectServer(after, checked.Name)
	return MCPStatus{OK: true, State: "added", Server: view}
}

// Update merges a patch into the stored server.
func (m *MCPManager) Update(ctx context.Context, name string, patch MCPServer) MCPStatus {
	if !validMCPName(name) {
		return mcpFailure("invalid", "native_mcp_invalid_input", "name")
	}
	if patch.Name != "" && patch.Name != name {
		return mcpFailure("invalid", "native_mcp_invalid_input", "name")
	}
	servers, err := m.call(ctx, m.timeout)
	if err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	current, found := findServer(servers, name)
	if !found {
		return MCPStatus{State: "not_found", Reason: "native_mcp_server_not_found"}
	}
	merged := mergeServer(current, patch)
	checked, field, ok := validateMCPServer(merged, false)
	if !ok {
		return mcpFailure("invalid", "native_mcp_invalid_input", field)
	}
	bounded, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	if err := m.transport.UpdateServer(bounded, checked); err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	after, err := m.call(ctx, m.timeout)
	if err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	return MCPStatus{OK: true, State: "updated", Server: projectServer(after, name)}
}

// Remove deletes one server. Removing an absent server succeeds: the caller
// asked for it to be gone, and it is.
func (m *MCPManager) Remove(ctx context.Context, name string) MCPStatus {
	if !validMCPName(name) {
		return mcpFailure("invalid", "native_mcp_invalid_input", "name")
	}
	servers, err := m.call(ctx, m.timeout)
	if err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	if _, found := findServer(servers, name); !found {
		return MCPStatus{OK: true, State: "absent"}
	}
	bounded, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	if err := m.transport.RemoveServer(bounded, name); err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	after, err := m.call(ctx, m.timeout)
	if err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	if _, still := findServer(after, name); still {
		return mcpFailure("unavailable", "native_mcp_operation_failed")
	}
	return MCPStatus{OK: true, State: "removed"}
}

// Test starts one server and reports what it exposed. This is the one operation
// that can start an external process, so it only runs when the caller names the
// server, and a second test while one is unsettled is refused rather than
// overlapping two probes of the same server.
func (m *MCPManager) Test(ctx context.Context, name, cwd string) MCPStatus {
	if !validMCPName(name) {
		return mcpFailure("invalid", "native_mcp_invalid_input", "name")
	}
	if cwd != "" && (len(cwd) > mcpMaxCommand || !isAbsPath(cwd)) {
		return mcpFailure("invalid", "native_mcp_invalid_input", "cwd")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return mcpFailure("closed", "native_mcp_manager_closed")
	}
	if m.pendingTest {
		m.mu.Unlock()
		return mcpFailure("busy", "native_mcp_test_in_progress")
	}
	m.pendingTest = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.pendingTest = false
		m.mu.Unlock()
	}()

	servers, err := m.call(ctx, m.timeout)
	if err != nil {
		return mcpFailure("unavailable", mcpReason(err))
	}
	entry, found := findServer(servers, name)
	if !found {
		return MCPStatus{State: "not_found", Reason: "native_mcp_server_not_found"}
	}
	secrets := secretValues(entry)
	bounded, cancel := context.WithTimeout(ctx, m.testTimeout)
	defer cancel()
	result, err := m.transport.TestServer(bounded, name, cwd)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return mcpFailure("timeout", "native_mcp_test_timeout")
		}
		// A store that cannot run a probe says so with its own code: the panel
		// must show "needs the kernel runtime" rather than a generic failure.
		if IsMCPTestUnavailable(err) {
			return mcpFailure("unavailable", MCPTestUnavailableReason)
		}
		return mcpFailure("unavailable", mcpReason(err))
	}
	detail := boundedOutput(result.Output, secrets)
	tools := toolNames(result.Output, secrets)
	summary := summarizeOutput(result.Output, tools)
	if result.Success {
		return MCPStatus{OK: true, State: "connected", Tools: tools, Summary: summary, Detail: detail}
	}
	return MCPStatus{State: "failed", Reason: "native_mcp_test_failed", Tools: tools,
		Summary: summary, Detail: detail}
}

// Close releases the transport.
func (m *MCPManager) Close(ctx context.Context) MCPStatus {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return MCPStatus{OK: true, State: "closed"}
	}
	m.closed = true
	m.mu.Unlock()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = m.transport.Close(bounded)
	return MCPStatus{OK: true, State: "closed"}
}

// ---------------------------------------------------------------------------
// validation

func validMCPName(name string) bool {
	return len(name) <= mcpMaxName && mcpNamePattern.MatchString(name)
}

// validateMCPServer checks one description field by field. A field the kernel
// does not know is refused rather than forwarded, so a typo cannot become a
// silently ignored setting.
func validateMCPServer(server MCPServer, partial bool) (MCPServer, string, bool) {
	transport := server.Transport
	if transport != "stdio" && transport != "http" && transport != "sse" {
		return MCPServer{}, "transport", false
	}
	if !validMCPName(server.Name) {
		return MCPServer{}, "name", false
	}
	allowed := mcpStdioKeys
	if transport != "stdio" {
		allowed = mcpURLKeys
	}
	// Every populated field must be allowed for this transport family.
	for _, name := range populatedFields(server) {
		if mcpBlockedKeys[name] {
			return MCPServer{}, name, false
		}
		if !allowed[name] {
			return MCPServer{}, name, false
		}
	}
	out := server
	if transport == "stdio" {
		if server.Command == "" || len(server.Command) > mcpMaxCommand {
			return MCPServer{}, "command", false
		}
		if len(server.Args) > mcpMaxArgs {
			return MCPServer{}, "args", false
		}
		for _, argument := range server.Args {
			if len(argument) > mcpMaxArg {
				return MCPServer{}, "args", false
			}
		}
		if len(server.Env) > mcpMaxEnv {
			return MCPServer{}, "env", false
		}
		for name, value := range server.Env {
			if len(name) > mcpMaxEnvName || !mcpEnvPattern.MatchString(name) || len(value) > mcpMaxEnvValue {
				return MCPServer{}, "env", false
			}
		}
		if server.Cwd != "" && len(server.Cwd) > mcpMaxCommand {
			return MCPServer{}, "cwd", false
		}
		if server.Executor != "" && server.Executor != "local" && server.Executor != "kaos" {
			return MCPServer{}, "executor", false
		}
		if server.EnvironmentID != "" && len(server.EnvironmentID) > mcpMaxEnvName {
			return MCPServer{}, "environment_id", false
		}
		// A stdio server must not carry a URL family field.
		out.URL, out.Headers, out.Auth, out.BearerTokenEnvVar = "", nil, "", ""
		return out, "", true
	}
	if server.URL == "" || len(server.URL) > mcpMaxURL || !validMCPURL(server.URL) {
		return MCPServer{}, "url", false
	}
	for name, value := range server.Headers {
		if !mcpHeaderPattern.MatchString(name) || len(value) > mcpMaxEnvValue {
			return MCPServer{}, "headers", false
		}
	}
	if server.Auth != "" && server.Auth != "none" && server.Auth != "bearer" {
		return MCPServer{}, "auth", false
	}
	// A URL server must not carry a stdio family field.
	out.Command, out.Args, out.Env, out.Cwd, out.Executor, out.EnvironmentID = "", nil, nil, "", "", ""
	return out, "", true
}

func validMCPURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// populatedFields lists the field names this value actually sets.
func populatedFields(server MCPServer) []string {
	var out []string
	add := func(name string, present bool) {
		if present {
			out = append(out, name)
		}
	}
	add("name", server.Name != "")
	add("transport", server.Transport != "")
	add("enabled", server.Enabled != nil)
	add("deferred", server.Deferred != nil)
	add("enabledTools", server.EnabledTools != nil)
	add("disabledTools", server.DisabledTools != nil)
	add("startupTimeoutMs", server.StartupTimeoutMS != nil)
	add("toolTimeoutMs", server.ToolTimeoutMS != nil)
	add("command", server.Command != "")
	add("args", server.Args != nil)
	add("env", server.Env != nil)
	add("cwd", server.Cwd != "")
	add("executor", server.Executor != "")
	add("environment_id", server.EnvironmentID != "")
	add("url", server.URL != "")
	add("headers", server.Headers != nil)
	add("auth", server.Auth != "")
	add("bearerTokenEnvVar", server.BearerTokenEnvVar != "")
	return out
}

// mergeServer applies a patch over the stored entry. An omitted field keeps its
// stored value, which is what "edit one row" means.
func mergeServer(current, patch MCPServer) MCPServer {
	merged := current
	if patch.Transport != "" {
		merged.Transport = patch.Transport
	}
	if patch.Enabled != nil {
		merged.Enabled = patch.Enabled
	}
	if patch.Deferred != nil {
		merged.Deferred = patch.Deferred
	}
	if patch.EnabledTools != nil {
		merged.EnabledTools = patch.EnabledTools
	}
	if patch.DisabledTools != nil {
		merged.DisabledTools = patch.DisabledTools
	}
	if patch.StartupTimeoutMS != nil {
		merged.StartupTimeoutMS = patch.StartupTimeoutMS
	}
	if patch.ToolTimeoutMS != nil {
		merged.ToolTimeoutMS = patch.ToolTimeoutMS
	}
	if patch.Command != "" {
		merged.Command = patch.Command
	}
	if patch.Args != nil {
		merged.Args = patch.Args
	}
	if patch.Env != nil {
		merged.Env = patch.Env
	}
	if patch.Cwd != "" {
		merged.Cwd = patch.Cwd
	}
	if patch.Executor != "" {
		merged.Executor = patch.Executor
	}
	if patch.EnvironmentID != "" {
		merged.EnvironmentID = patch.EnvironmentID
	}
	if patch.URL != "" {
		merged.URL = patch.URL
	}
	if patch.Headers != nil {
		merged.Headers = patch.Headers
	}
	if patch.Auth != "" {
		merged.Auth = patch.Auth
	}
	if patch.BearerTokenEnvVar != "" {
		merged.BearerTokenEnvVar = patch.BearerTokenEnvVar
	}
	return merged
}

func findServer(servers []MCPServer, name string) (MCPServer, bool) {
	for _, server := range servers {
		if server.Name == name {
			return server, true
		}
	}
	return MCPServer{}, false
}

// ---------------------------------------------------------------------------
// redaction

// RedactMCPServer is the projection a caller may see. It keeps the shape of the
// entry and drops every value that could carry a credential.
func RedactMCPServer(server MCPServer) MCPServerView {
	view := MCPServerView{Name: server.Name, Transport: server.Transport,
		Enabled: server.Enabled, Deferred: server.Deferred,
		ArgsCount: len(server.Args)}
	if server.Command != "" {
		view.Command = clip(redactSecrets(server.Command, secretValues(server)), mcpMaxCommand)
	}
	if server.URL != "" {
		// A URL may carry userinfo or a query; only the origin and path survive.
		if parsed, err := url.Parse(server.URL); err == nil {
			parsed.User = nil
			parsed.RawQuery = ""
			parsed.Fragment = ""
			view.URL = parsed.String()
		}
	}
	if len(server.Headers) > 0 {
		view.HeaderNames = sortedKeys(server.Headers)
	}
	if len(server.Env) > 0 {
		view.EnvNames = sortedKeys(server.Env)
	}
	return view
}

func redactServers(servers []MCPServer) []MCPServerView {
	out := make([]MCPServerView, 0, len(servers))
	for i, server := range servers {
		if i >= mcpMaxServers {
			break
		}
		out = append(out, RedactMCPServer(server))
	}
	return out
}

func projectServer(servers []MCPServer, name string) *MCPServerView {
	server, found := findServer(servers, name)
	if !found {
		return nil
	}
	view := RedactMCPServer(server)
	return &view
}

func sortedKeys(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// secretValues collects the literal strings this entry could leak: its arguments,
// an env value whose name looks secret, a header value, and URL userinfo.
func secretValues(server MCPServer) []string {
	var out []string
	for _, argument := range server.Args {
		if len(argument) >= 6 {
			out = append(out, argument)
		}
	}
	for name, value := range server.Env {
		if mcpSecretWord.MatchString(name) && len(value) >= 6 {
			out = append(out, value)
		}
	}
	for name, value := range server.Headers {
		if mcpSecretWord.MatchString(name) && len(value) >= 6 {
			out = append(out, value)
		}
	}
	if server.BearerTokenEnvVar != "" {
		out = append(out, server.BearerTokenEnvVar)
	}
	if parsed, err := url.Parse(server.URL); err == nil && parsed.User != nil {
		if pass, present := parsed.User.Password(); present && len(pass) >= 6 {
			out = append(out, pass)
		}
	}
	return out
}

// genericSecretPatterns are the shapes a credential takes in a tool's output.
var genericSecretPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`\bBearer\s+[A-Za-z0-9._~+/=-]{6,}`), "Bearer [redacted]"},
	{regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{8,}`), "[redacted]"},
	{regexp.MustCompile(`\b(?:ghp|gho|ghs|ghu|github_pat)_[A-Za-z0-9_]{10,}`), "[redacted]"},
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{8,}`), "[redacted]"},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|passwd|authorization)["']?\s*[=:]\s*)("?[^\s"',}]{6,}"?)`), "$1[redacted]"},
	{regexp.MustCompile(`\{\{[A-Za-z0-9_]{6,}\}\}`), "[redacted]"},
}

// redactSecrets removes the entry's own literals and the generic shapes.
func redactSecrets(value string, secrets []string) string {
	out := value
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, "[redacted]")
	}
	for _, rule := range genericSecretPatterns {
		out = rule.pattern.ReplaceAllString(out, rule.replacement)
	}
	return out
}

func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// boundedOutput redacts and bounds a test's output. Only the tail survives: a
// failing server's useful line is its last one.
func boundedOutput(output string, secrets []string) string {
	redacted := redactSecrets(output, secrets)
	lines := strings.Split(redacted, "\n")
	if len(lines) > mcpMaxOutputLine {
		lines = lines[len(lines)-mcpMaxOutputLine:]
	}
	joined := strings.Join(lines, "\n")
	return clip(joined, mcpMaxOutput)
}

// toolNames extracts the tool names a probe reported, redacted and bounded.
func toolNames(output string, secrets []string) []string {
	redacted := redactSecrets(output, secrets)
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(redacted, "\n") {
		for _, field := range strings.Fields(line) {
			candidate := strings.Trim(field, `",:[]{}`)
			if candidate == "" || len(candidate) > mcpMaxToolName || seen[candidate] {
				continue
			}
			if !mcpNamePattern.MatchString(candidate) {
				continue
			}
			seen[candidate] = true
			out = append(out, candidate)
			if len(out) >= mcpMaxTools {
				return out
			}
		}
	}
	return out
}

// summarizeOutput is the one line the panel shows.
func summarizeOutput(output string, tools []string) string {
	if len(tools) > 0 {
		return clip("connected; tools: "+strings.Join(tools, ", "), mcpMaxMessage)
	}
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return "connected; no tools reported"
	}
	lines := strings.Split(trimmed, "\n")
	return clip(strings.TrimSpace(lines[len(lines)-1]), mcpMaxMessage)
}

func mcpFailure(state, reason string, field ...string) MCPStatus {
	status := MCPStatus{State: state, Reason: reason}
	if len(field) > 0 {
		status.Field = field[0]
	}
	return status
}

func mcpReason(err error) string {
	if err == nil {
		return "native_mcp_failed"
	}
	message := err.Error()
	if strings.HasPrefix(message, "native_mcp_") && len(message) <= 80 {
		return message
	}
	return "native_mcp_failed"
}

// MCPFingerprint is a stable digest of a server list, for change detection
// without exposing the entries.
func MCPFingerprint(servers []MCPServer) string {
	encoded, err := json.Marshal(servers)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
