package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The native TOML writers are tested against synthetic Kimi homes. Every fixture
// lives in a temporary directory; no real installation is read or written.

func nativeHome(t *testing.T) (kimiHome string, stateDir string) {
	t.Helper()
	base := t.TempDir()
	kimiHome = filepath.Join(base, "kimi")
	stateDir = filepath.Join(base, "state")
	home := filepath.Join(kimiHome, "runtime", "kimi-code", "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return kimiHome, stateDir
}

func writeConfig(t *testing.T, kimiHome, content string) {
	t.Helper()
	path := filepath.Join(kimiHome, "runtime", "kimi-code", "home", "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, kimiHome string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(kimiHome, "runtime", "kimi-code", "home", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const baseConfig = `# Kimi desktop config
[general]
language = "en"

[secondary_model.models]
"byok-kth-kimi/kimi-a" = "A"
"byok-kth-kimi/kimi-b" = "B"
"byok-kth-resp/gpt-c" = "C"
`

// ---------------------------------------------------------------------------
// subagent policy

func TestSubagentForceWritesAManagedBlock(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, err := NewSubagentConfig(stateDir, kimiHome)
	if err != nil {
		t.Fatal(err)
	}
	status := config.Update(SubagentPolicy{Mode: SubagentModeForce,
		ModelAlias: "byok-kth-kimi/kimi-a", Effort: "high"}, SubagentCapabilities{})
	if status.State != NativeStateFileApplied || status.Mode != SubagentModeForce {
		t.Fatalf("status: %+v", status)
	}
	text := readConfig(t, kimiHome)
	for _, want := range []string{
		subagentBlockBegin,
		"force = true",
		`default_model = "byok-kth-kimi/kimi-a"`,
		`default_effort = "high"`,
		subagentBlockEnd,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	// The user's own sections survive byte for byte.
	if !strings.Contains(text, "[general]\nlanguage = \"en\"") {
		t.Fatal("a foreign section was modified")
	}
	if !strings.Contains(text, `"byok-kth-kimi/kimi-b" = "B"`) {
		t.Fatal("a declared model was removed")
	}
}

func TestSubagentDropsForceKeyWhenTheKernelLacksIt(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	supported := false
	status := config.Update(SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-a"},
		SubagentCapabilities{ForceKeySupported: &supported})
	if status.State != NativeStateFileApplied {
		t.Fatalf("status: %+v", status)
	}
	text := readConfig(t, kimiHome)
	if strings.Contains(text, "force = true") {
		t.Fatal("the force key was written for a kernel that dropped it")
	}
	if !strings.Contains(text, `default_model = "byok-kth-kimi/kimi-a"`) {
		t.Fatal("the default_model was not written")
	}
}

func TestSubagentPoolWritesTheModelsTable(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	status := config.Update(SubagentPolicy{Mode: SubagentModePool, ModelAlias: "byok-kth-kimi/kimi-a",
		Models: []SubagentPoolEntry{
			{ModelAlias: "byok-kth-kimi/kimi-a", Description: "Fast"},
			{ModelAlias: "byok-kth-resp/gpt-c", Description: "Deep"},
		}}, SubagentCapabilities{})
	if status.State != NativeStateFileApplied || status.Mode != SubagentModePool {
		t.Fatalf("status: %+v", status)
	}
	text := readConfig(t, kimiHome)
	if !strings.Contains(text, "[secondary_model.models]") {
		t.Fatalf("pool table missing:\n%s", text)
	}
	// The managed table is inside the block, so the aliases appear twice: once
	// from the user's section and once from ours.
	if strings.Count(text, `"byok-kth-kimi/kimi-a"`) < 2 {
		t.Fatalf("pool entry missing:\n%s", text)
	}
}

func TestSubagentInheritRestoresTheOriginalBytes(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := config.Update(SubagentPolicy{Mode: SubagentModeForce,
		ModelAlias: "byok-kth-kimi/kimi-a"}, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	if status := config.Update(SubagentPolicy{Mode: SubagentModeInherit}, SubagentCapabilities{}); status.State != SubagentModeInherit {
		t.Fatalf("inherit: %+v", status)
	}
	if got := readConfig(t, kimiHome); got != baseConfig {
		t.Fatalf("the file was not restored:\n%q\nwant\n%q", got, baseConfig)
	}
}

func TestSubagentBlockedLeavesNoBlock(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	status := config.Update(SubagentPolicy{Mode: SubagentModeBlocked}, SubagentCapabilities{})
	if status.State != SubagentModeBlocked || status.Mode != SubagentModeBlocked {
		t.Fatalf("status: %+v", status)
	}
	if strings.Contains(readConfig(t, kimiHome), subagentBlockBegin) {
		t.Fatal("blocked mode wrote a block")
	}
}

func TestSubagentRefusesAnUndeclaredModel(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	status := config.Update(SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/unknown"},
		SubagentCapabilities{})
	if status.State != NativeStateWaiting || status.Reason != "models_missing" {
		t.Fatalf("status: %+v", status)
	}
	if strings.Contains(readConfig(t, kimiHome), subagentBlockBegin) {
		t.Fatal("a block naming an undeclared model was written")
	}
}

func TestSubagentRefusesAForeignSecondaryModelSection(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig+"\n[secondary_model]\ndefault_model = \"theirs\"\n")
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	status := config.Update(SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-a"},
		SubagentCapabilities{})
	if status.State != NativeStateConflict || status.Reason != "foreign_secondary_model" {
		t.Fatalf("status: %+v", status)
	}
}

func TestSubagentRefusesAModifiedBlock(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := config.Update(SubagentPolicy{Mode: SubagentModeForce,
		ModelAlias: "byok-kth-kimi/kimi-a"}, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	// Edit one managed line: the body hash no longer matches.
	text := readConfig(t, kimiHome)
	text = strings.Replace(text, "force = true", "force = false", 1)
	writeConfig(t, kimiHome, text)
	status := config.Update(SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-b"},
		SubagentCapabilities{})
	if status.State != NativeStateConflict || status.Reason != "block_modified" {
		t.Fatalf("status: %+v", status)
	}
}

func TestSubagentRefusesDuplicateMarkers(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig+subagentBlockBegin+"\n"+subagentBlockEnd+"\n"+subagentBlockBegin+"\n"+subagentBlockEnd+"\n")
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := config.Inspect(); status.State != NativeStateConflict || status.Reason != "duplicate_block" {
		t.Fatalf("status: %+v", status)
	}
}

func TestSubagentRejectsInvalidPolicies(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	cases := []SubagentPolicy{
		{Mode: "nonsense"},
		{Mode: SubagentModeForce},
		{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-a", Effort: "HIGH"},
		{Mode: SubagentModePool, ModelAlias: "byok-kth-kimi/kimi-a"},
		{Mode: SubagentModePool, ModelAlias: "not-in-list",
			Models: []SubagentPoolEntry{{ModelAlias: "byok-kth-kimi/kimi-a"}}},
		{Mode: SubagentModePool, ModelAlias: "byok-kth-kimi/kimi-a",
			Models: []SubagentPoolEntry{{ModelAlias: "byok-kth-kimi/kimi-a", Description: markerWord}}},
	}
	for _, policy := range cases {
		if status := config.Update(policy, SubagentCapabilities{}); status.State != NativeStateConflict {
			t.Fatalf("%+v -> %+v", policy, status)
		}
	}
}

func TestSubagentPreservesCRLFAndMissingTrailingNewline(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	original := "[general]\r\nlanguage = \"en\"\r\n\r\n[secondary_model.models]\r\n\"byok-kth-kimi/kimi-a\" = \"A\""
	writeConfig(t, kimiHome, original)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := config.Update(SubagentPolicy{Mode: SubagentModeForce,
		ModelAlias: "byok-kth-kimi/kimi-a"}, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	text := readConfig(t, kimiHome)
	if !strings.Contains(text, "\r\n") {
		t.Fatal("CRLF was not preserved")
	}
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		t.Fatal("a bare LF was introduced into a CRLF file")
	}
	if status := config.Update(SubagentPolicy{Mode: SubagentModeInherit}, SubagentCapabilities{}); status.State != SubagentModeInherit {
		t.Fatalf("inherit: %+v", status)
	}
	if got := readConfig(t, kimiHome); got != original {
		t.Fatalf("restore mismatch:\n%q\nwant\n%q", got, original)
	}
}

func TestSubagentRequiresAnExplicitHome(t *testing.T) {
	if _, err := NewSubagentConfig(t.TempDir(), ""); err == nil {
		t.Fatal("an empty kimi home was accepted")
	}
	if _, err := NewSubagentConfig(t.TempDir(), "relative"); err == nil {
		t.Fatal("a relative kimi home was accepted")
	}
	if _, err := NewSubagentConfig("", t.TempDir()); err == nil {
		t.Fatal("an empty state dir was accepted")
	}
}

func TestSubagentRefusesASymlinkedConfig(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	realPath := filepath.Join(kimiHome, "real.toml")
	configPath := filepath.Join(kimiHome, "runtime", "kimi-code", "home", "config.toml")
	if err := os.Rename(configPath, realPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, configPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	status := config.Update(SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-a"},
		SubagentCapabilities{})
	if status.State != NativeStateConflict || status.Reason != "config_symlink" {
		t.Fatalf("status: %+v", status)
	}
}

func TestSubagentInspectReportsWhatIsOnDisk(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := config.Inspect(); status.State != NativeStateInherit {
		t.Fatalf("before: %+v", status)
	}
	if status := config.Update(SubagentPolicy{Mode: SubagentModeForce,
		ModelAlias: "byok-kth-kimi/kimi-a"}, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	status := config.Inspect()
	if status.State != NativeStateFileApplied || status.Mode != SubagentModeForce {
		t.Fatalf("after: %+v", status)
	}
}

// ---------------------------------------------------------------------------
// context policy

func TestContextPolicyWritesTheTriggerRatio(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, err := NewContextPolicyConfig(stateDir, kimiHome)
	if err != nil {
		t.Fatal(err)
	}
	ratio := 0.85
	status := config.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &ratio})
	if status.State != NativeStateFileApplied {
		t.Fatalf("status: %+v", status)
	}
	text := readConfig(t, kimiHome)
	if !strings.Contains(text, "compaction_trigger_ratio = 0.85") {
		t.Fatalf("ratio not written:\n%s", text)
	}
	if !strings.Contains(text, "[loop_control]") {
		t.Fatalf("table header missing:\n%s", text)
	}
	// The user's own sections are untouched.
	if !strings.Contains(text, `"byok-kth-kimi/kimi-b" = "B"`) {
		t.Fatal("a foreign section was modified")
	}
}

func TestContextPolicyRejectsAnOutOfRangeRatio(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewContextPolicyConfig(stateDir, kimiHome)
	for _, ratio := range []float64{0.1, 0.49, 1.0, 1.5} {
		value := ratio
		if status := config.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &value}); status.State != NativeStateConflict {
			t.Fatalf("ratio %v -> %+v", ratio, status)
		}
	}
	// A custom mode with no field is refused too.
	if status := config.Update(ContextPolicy{Mode: contextModeCustom}); status.State != NativeStateConflict {
		t.Fatalf("empty custom -> %+v", status)
	}
	// inherit must not carry fields.
	ratio := 0.8
	if status := config.Update(ContextPolicy{Mode: contextModeInherit, TriggerRatio: &ratio}); status.State != NativeStateConflict {
		t.Fatalf("inherit with a field -> %+v", status)
	}
}

func TestContextPolicyRestoresTheOriginalTable(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	original := baseConfig + "\n[loop_control]\ncompaction_trigger_ratio = 0.70\n"
	writeConfig(t, kimiHome, original)
	config, _ := NewContextPolicyConfig(stateDir, kimiHome)
	ratio := 0.90
	// A value we did not write is refused rather than replaced.
	if status := config.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &ratio}); status.State != NativeStateConflict || status.Reason != "foreign_value" {
		t.Fatalf("foreign value: %+v", status)
	}
}

func TestContextPolicyTableLayoutRoundTrip(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewContextPolicyConfig(stateDir, kimiHome)
	ratio := 0.75
	reserved := int64(8000)
	if status := config.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &ratio,
		ReservedTokens: &reserved}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	text := readConfig(t, kimiHome)
	if !strings.Contains(text, "compaction_trigger_ratio = 0.75") ||
		!strings.Contains(text, "reserved_context_size = 8000") {
		t.Fatalf("values not written:\n%s", text)
	}
	// The same instance may then release its own values.
	status := config.Update(ContextPolicy{Mode: contextModeInherit})
	if status.State != NativeStateRestored {
		t.Fatalf("release: %+v", status)
	}
	if got := readConfig(t, kimiHome); got != baseConfig {
		t.Fatalf("table not restored:\n%q\nwant\n%q", got, baseConfig)
	}
}

func TestContextPolicyStateReportsForeignValues(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig+"\n[loop_control]\ncompaction_trigger_ratio = 0.70\n")
	config, _ := NewContextPolicyConfig(stateDir, kimiHome)
	status := config.State()
	if status.State != NativeStateConflict || status.Reason != "foreign_value" {
		t.Fatalf("status: %+v", status)
	}
}

// ---------------------------------------------------------------------------
// services

func TestServicesEnableSearchAndFetch(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, err := NewServicesConfig(stateDir, kimiHome)
	if err != nil {
		t.Fatal(err)
	}
	status := config.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://search.test/search", APIKey: "key-search-123"},
		Fetch:  &ServiceSettings{Enabled: true, BaseURL: "https://fetch.test/fetch", APIKey: "key-fetch-456"},
	})
	if status.State != NativeStateFileApplied {
		t.Fatalf("status: %+v", status)
	}
	text := readConfig(t, kimiHome)
	for _, want := range []string{
		"[services.moonshot_search]", `base_url = "https://search.test/search"`,
		`api_key = "key-search-123"`, "[services.moonshot_fetch]",
		`base_url = "https://fetch.test/fetch"`, `api_key = "key-fetch-456"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if !strings.Contains(text, `"byok-kth-kimi/kimi-b" = "B"`) {
		t.Fatal("a foreign section was modified")
	}
}

func TestServicesDisableRestoresOnlyThatTable(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewServicesConfig(stateDir, kimiHome)
	if status := config.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://search.test/search", APIKey: "key-search-123"},
		Fetch:  &ServiceSettings{Enabled: true, BaseURL: "https://fetch.test/fetch", APIKey: "key-fetch-456"},
	}); status.State != NativeStateFileApplied {
		t.Fatalf("enable: %+v", status)
	}
	// Turn search off; fetch must survive.
	if status := config.Update(ServicesInput{Search: &ServiceSettings{Enabled: false}}); status.State == NativeStateConflict {
		t.Fatalf("disable: %+v", status)
	}
	text := readConfig(t, kimiHome)
	if strings.Contains(text, "[services.moonshot_search]") {
		t.Fatalf("search table survived disable:\n%s", text)
	}
	if !strings.Contains(text, "[services.moonshot_fetch]") {
		t.Fatalf("fetch table was removed:\n%s", text)
	}
}

func TestServicesReuseAKeyOnlyForTheSameEndpoint(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewServicesConfig(stateDir, kimiHome)
	if status := config.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://search.test/search", APIKey: "key-search-123"},
	}); status.State != NativeStateFileApplied {
		t.Fatalf("first: %+v", status)
	}
	// The same endpoint may omit the key: the saved one is reused.
	if status := config.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://search.test/search"},
	}); status.State != NativeStateFileApplied {
		t.Fatalf("reuse: %+v", status)
	}
	if !strings.Contains(readConfig(t, kimiHome), `api_key = "key-search-123"`) {
		t.Fatal("the saved key was not reused")
	}
	// A different endpoint must supply its own key.
	if status := config.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://other.test/search"},
	}); status.State != NativeStateConflict || status.Reason != "invalid_service_key" {
		t.Fatalf("cross-endpoint reuse: %+v", status)
	}
}

func TestServicesRejectInsecureOrIncompleteInput(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewServicesConfig(stateDir, kimiHome)
	cases := []struct {
		input  ServicesInput
		reason string
	}{
		{ServicesInput{Search: &ServiceSettings{Enabled: true, BaseURL: "http://example.test/search", APIKey: "k"}}, "invalid_service_endpoint"},
		{ServicesInput{Search: &ServiceSettings{Enabled: true, BaseURL: "https://x.test/s?token=1", APIKey: "k"}}, "invalid_service_endpoint"},
		{ServicesInput{Search: &ServiceSettings{Enabled: true, BaseURL: "https://x.test/s"}}, "invalid_service_key"},
		{ServicesInput{Search: &ServiceSettings{Enabled: true, BaseURL: "https://x.test/s", APIKey: " padded "}}, "invalid_service_key"},
	}
	for _, item := range cases {
		status := config.Update(item.input)
		if status.State != NativeStateConflict || status.Reason != item.reason {
			t.Fatalf("%+v -> %+v", item.input, status)
		}
	}
	// Plain HTTP is allowed only for the machine itself.
	if status := config.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "http://127.0.0.1:8080/search", APIKey: "local-key"},
	}); status.State != NativeStateFileApplied {
		t.Fatalf("loopback http: %+v", status)
	}
}

func TestServicesRefuseAForeignTable(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig+"\n[services.moonshot_search]\nbase_url = \"https://theirs.test/search\"\napi_key = \"their-key\"\n")
	config, _ := NewServicesConfig(stateDir, kimiHome)
	status := config.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://search.test/search", APIKey: "key-search-123"},
	})
	if status.State != NativeStateConflict || status.Search == nil || status.Search.Reason != "foreign_service" {
		t.Fatalf("status: %+v", status)
	}
	if !strings.Contains(readConfig(t, kimiHome), "https://theirs.test/search") {
		t.Fatal("a foreign table was overwritten")
	}
}

func TestServicesIntentNeverAppearsInTheConfig(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewServicesConfig(stateDir, kimiHome)
	if status := config.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://search.test/search", APIKey: "key-search-123"},
	}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	// The intent file holds the key because the module needs it to reuse; it is
	// owner-only, and the config never gains a second copy of it.
	info, err := os.Stat(filepath.Join(stateDir, servicesIntent))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("intent is group/other readable: %v", info.Mode().Perm())
	}
	if strings.Count(readConfig(t, kimiHome), "key-search-123") != 1 {
		t.Fatal("the key appears more than once in the config")
	}
}

// ---------------------------------------------------------------------------
// MCP manager

type fakeMCPTransport struct {
	mu      sync.Mutex
	servers []MCPServer
	added   []MCPServer
	updated []MCPServer
	removed []string
	tested  []string
	result  MCPTestResult
	err     error
}

func (f *fakeMCPTransport) ListServers(context.Context) ([]MCPServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MCPServer(nil), f.servers...), f.err
}

func (f *fakeMCPTransport) AddServer(_ context.Context, server MCPServer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, server)
	f.servers = append(f.servers, server)
	return f.err
}

func (f *fakeMCPTransport) UpdateServer(_ context.Context, server MCPServer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, server)
	for i, existing := range f.servers {
		if existing.Name == server.Name {
			f.servers[i] = server
		}
	}
	return f.err
}

func (f *fakeMCPTransport) RemoveServer(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	kept := f.servers[:0]
	for _, existing := range f.servers {
		if existing.Name != name {
			kept = append(kept, existing)
		}
	}
	f.servers = kept
	return f.err
}

func (f *fakeMCPTransport) TestServer(_ context.Context, name, _ string) (MCPTestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tested = append(f.tested, name)
	return f.result, f.err
}

func (f *fakeMCPTransport) Close(context.Context) error { return nil }

func TestMCPCRUDRoundTrip(t *testing.T) {
	transport := &fakeMCPTransport{}
	manager, err := NewMCPManager(transport)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	added := manager.Add(ctx, MCPServer{Name: "files", Transport: "stdio", Command: "/usr/bin/mcp-files",
		Args: []string{"--root", "/tmp"}, Env: map[string]string{"API_TOKEN": "tok-abcdefgh"}})
	if !added.OK || added.State != "added" || added.Server == nil {
		t.Fatalf("add: %+v", added)
	}
	if added.Server.Command != "/usr/bin/mcp-files" {
		t.Fatalf("command: %q", added.Server.Command)
	}
	if added.Server.ArgsCount != 2 {
		t.Fatalf("args count: %d", added.Server.ArgsCount)
	}
	if len(added.Server.EnvNames) != 1 || added.Server.EnvNames[0] != "API_TOKEN" {
		t.Fatalf("env names: %v", added.Server.EnvNames)
	}

	// A duplicate name is refused.
	if again := manager.Add(ctx, MCPServer{Name: "files", Transport: "stdio", Command: "/bin/x"}); again.State != "exists" {
		t.Fatalf("duplicate: %+v", again)
	}

	// Update merges: the command survives because the patch omits it.
	enabled := false
	updated := manager.Update(ctx, "files", MCPServer{Enabled: &enabled})
	if !updated.OK || updated.State != "updated" {
		t.Fatalf("update: %+v", updated)
	}
	if len(transport.updated) != 1 || transport.updated[0].Command != "/usr/bin/mcp-files" {
		t.Fatalf("the command was lost on merge: %+v", transport.updated)
	}

	listed := manager.List(ctx)
	if !listed.OK || len(listed.Servers) != 1 {
		t.Fatalf("list: %+v", listed)
	}
	if listed.Servers[0].Enabled == nil || *listed.Servers[0].Enabled {
		t.Fatal("the update did not persist")
	}

	if removed := manager.Remove(ctx, "files"); !removed.OK || removed.State != "removed" {
		t.Fatalf("remove: %+v", removed)
	}
	// Removing an absent server succeeds.
	if again := manager.Remove(ctx, "files"); !again.OK || again.State != "absent" {
		t.Fatalf("remove absent: %+v", again)
	}
}

func TestMCPValidationRefusesUnknownFieldsAndFamilies(t *testing.T) {
	manager, _ := NewMCPManager(&fakeMCPTransport{})
	ctx := context.Background()
	cases := []struct {
		server MCPServer
		field  string
	}{
		{MCPServer{Name: "x", Transport: "stdio"}, "command"},
		{MCPServer{Name: "bad name", Transport: "stdio", Command: "/bin/x"}, "name"},
		{MCPServer{Name: "x", Transport: "carrier-pigeon", Command: "/bin/x"}, "transport"},
		{MCPServer{Name: "x", Transport: "stdio", Command: "/bin/x", URL: "https://x.test"}, "url"},
		{MCPServer{Name: "x", Transport: "http", URL: "ftp://x.test"}, "url"},
		{MCPServer{Name: "x", Transport: "stdio", Command: "/bin/x",
			Env: map[string]string{"1BAD": "v"}}, "env"},
		{MCPServer{Name: "x", Transport: "stdio", Command: "/bin/x", Executor: "remote"}, "executor"},
	}
	for _, item := range cases {
		status := manager.Add(ctx, item.server)
		if status.State != "invalid" || status.Field != item.field {
			t.Fatalf("%+v -> %+v", item.server, status)
		}
	}
}

func TestMCPTestRedactsSecrets(t *testing.T) {
	transport := &fakeMCPTransport{
		servers: []MCPServer{{Name: "srv", Transport: "stdio", Command: "/bin/x",
			Env: map[string]string{"API_TOKEN": "tok-abcdefgh"}}},
		result: MCPTestResult{Success: true,
			Output: "starting with tok-abcdefgh\nAuthorization: Bearer abcdefghijklmnop\n tools: read_file write_file\n"},
	}
	manager, _ := NewMCPManager(transport)
	status := manager.Test(context.Background(), "srv", "")
	if !status.OK || status.State != "connected" {
		t.Fatalf("test: %+v", status)
	}
	if strings.Contains(status.Detail, "tok-abcdefgh") || strings.Contains(status.Detail, "abcdefghijklmnop") {
		t.Fatalf("a secret survived redaction: %s", status.Detail)
	}
	if !strings.Contains(status.Detail, "[redacted]") {
		t.Fatalf("no redaction marker: %s", status.Detail)
	}
	if len(status.Tools) == 0 {
		t.Fatalf("no tools extracted: %+v", status)
	}
	if status.Summary == "" {
		t.Fatal("no summary")
	}
}

func TestMCPTestRefusesAnUnknownServer(t *testing.T) {
	manager, _ := NewMCPManager(&fakeMCPTransport{})
	status := manager.Test(context.Background(), "missing", "")
	if status.State != "not_found" {
		t.Fatalf("status: %+v", status)
	}
	// A relative cwd is refused before anything starts.
	status = manager.Test(context.Background(), "srv", "relative/path")
	if status.State != "invalid" || status.Field != "cwd" {
		t.Fatalf("cwd: %+v", status)
	}
}

func TestMCPTransportFailureIsReportedNotHidden(t *testing.T) {
	transport := &fakeMCPTransport{err: errors.New("native_mcp_kernel_unavailable")}
	manager, _ := NewMCPManager(transport)
	status := manager.List(context.Background())
	if status.OK || status.State != "unavailable" || status.Reason != "native_mcp_kernel_unavailable" {
		t.Fatalf("status: %+v", status)
	}
}

func TestMCPManagerRequiresATransport(t *testing.T) {
	if _, err := NewMCPManager(nil); err == nil {
		t.Fatal("a nil transport was accepted")
	}
}

func TestMCPRedactionKeepsShapeOnly(t *testing.T) {
	server := MCPServer{Name: "remote", Transport: "http",
		URL:     "https://user:password@mcp.test/endpoint?token=leak",
		Headers: map[string]string{"Authorization": "Bearer abcdefghijkl"},
		Args:    []string{"--token", "abcdefghijkl"}}
	view := RedactMCPServer(server)
	if strings.Contains(view.URL, "password") || strings.Contains(view.URL, "leak") || strings.Contains(view.URL, "user:") {
		t.Fatalf("the URL was not reduced: %s", view.URL)
	}
	if len(view.HeaderNames) != 1 || view.HeaderNames[0] != "Authorization" {
		t.Fatalf("header names: %v", view.HeaderNames)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "abcdefghijkl") {
		t.Fatalf("a header value leaked: %s", encoded)
	}
}

func TestMCPFingerprintChangesWithTheList(t *testing.T) {
	a := MCPFingerprint([]MCPServer{{Name: "a", Transport: "stdio", Command: "/bin/a"}})
	b := MCPFingerprint([]MCPServer{{Name: "b", Transport: "stdio", Command: "/bin/b"}})
	if a == "" || a == b {
		t.Fatal("fingerprint does not distinguish lists")
	}
	if MCPFingerprint(nil) == "" {
		t.Fatal("empty fingerprint")
	}
}
