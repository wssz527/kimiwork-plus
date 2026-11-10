package kimi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The panel reads three layers and must never see a written file presented as an
// adopted policy. These tests pin the mapping the released UI uses.

func TestSubagentBindingNeverClaimsAdoptionFromAFileWrite(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	policy := SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-a"}
	if status := config.Update(policy, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	binding := config.Binding(policy, false)
	if binding.Runtime.State != RuntimeFileApplied {
		t.Fatalf("runtime: %s", binding.Runtime.State)
	}
	if binding.Runtime.Observed != nil {
		t.Fatal("adoption was claimed without evidence")
	}
	// The configured layer carries both aliases, because the panel accepts either.
	if binding.Configured.Model != "byok-kth-kimi/kimi-a" ||
		binding.Configured.ModelAlias != "byok-kth-kimi/kimi-a" {
		t.Fatalf("configured: %+v", binding.Configured)
	}
}

func TestSubagentBindingRaisesOnlyWithEvidence(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	policy := SubagentPolicy{Mode: SubagentModePool, ModelAlias: "byok-kth-kimi/kimi-a",
		Models: []SubagentPoolEntry{{ModelAlias: "byok-kth-kimi/kimi-a", Description: "Fast"}}}
	if status := config.Update(policy, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	binding := config.Binding(policy, true)
	if binding.Runtime.State != RuntimeLoaded || binding.Runtime.Observed == nil ||
		binding.Runtime.Observed.State != RuntimeObservedSchema {
		t.Fatalf("runtime: %+v", binding.Runtime)
	}
	// Even with evidence, the pool rows use the panel's `id` field.
	if len(binding.Configured.Models) != 1 || binding.Configured.Models[0].ID != "byok-kth-kimi/kimi-a" {
		t.Fatalf("pool rows: %+v", binding.Configured.Models)
	}
}

func TestSubagentBindingReportsConflictAndInherit(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig+"\n[secondary_model]\ndefault_model = \"theirs\"\n")
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	binding := config.Binding(SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-a"}, false)
	if binding.Runtime.State != RuntimeConflict {
		t.Fatalf("conflict: %+v", binding.Runtime)
	}

	plainHome, plainState := nativeHome(t)
	writeConfig(t, plainHome, baseConfig)
	plain, _ := NewSubagentConfig(plainState, plainHome)
	inherit := plain.Binding(SubagentPolicy{Mode: SubagentModeInherit}, false)
	if inherit.Runtime.State != RuntimeInherit {
		t.Fatalf("inherit: %+v", inherit.Runtime)
	}
}

func TestSubagentBindingIsJSONRenderable(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewSubagentConfig(stateDir, kimiHome)
	binding := config.Binding(SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-a"}, false)
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"configured", "policy", "runtime"} {
		if _, present := document[key]; !present {
			t.Fatalf("missing %s: %s", key, encoded)
		}
	}
	configured := document["configured"].(map[string]any)
	if configured["mode"] != SubagentModeForce {
		t.Fatalf("configured mode: %v", configured["mode"])
	}
}

func TestContextBindingKeepsTheSavedRequest(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewContextPolicyConfig(stateDir, kimiHome)
	ratio := 0.85
	policy := ContextPolicy{Mode: contextModeCustom, TriggerRatio: &ratio}
	if status := config.Update(policy); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	binding := config.Binding(policy)
	if binding.Policy.TriggerRatio == nil || *binding.Policy.TriggerRatio != 0.85 {
		t.Fatalf("policy: %+v", binding.Policy)
	}
	if binding.State.State != NativeStateFileApplied {
		t.Fatalf("state: %+v", binding.State)
	}
}

func TestServicesBindingSeparatesTheTwoServices(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	config, _ := NewServicesConfig(stateDir, kimiHome)
	if status := config.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://search.test/search", APIKey: "key-search-123"},
	}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	binding := config.Binding()
	if binding.Intent.Search == nil || !binding.Intent.Search.Enabled {
		t.Fatalf("intent search: %+v", binding.Intent)
	}
	if binding.State.Search == nil || binding.State.Search.State != "enabled" {
		t.Fatalf("state search: %+v", binding.State.Search)
	}
	// Fetch was never enabled, so it is disabled rather than unknown.
	if binding.State.Fetch == nil || binding.State.Fetch.State != "disabled" {
		t.Fatalf("state fetch: %+v", binding.State.Fetch)
	}
	// The saved key is not rendered into the reported state.
	encoded, _ := json.Marshal(binding.State)
	if strings.Contains(string(encoded), "key-search-123") {
		t.Fatal("the policy layer leaked a key")
	}
}

func TestMCPBindingDisablesTheProbeOnAFileStore(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	store, _ := NewMCPFileStore(kimiHome)
	manager, _ := NewMCPManager(store)
	binding := manager.Binding(context.Background())
	if binding.State != "ready" {
		t.Fatalf("state: %s (%s)", binding.State, binding.Reason)
	}
	if len(binding.Servers) != 3 {
		t.Fatalf("servers: %d", len(binding.Servers))
	}
	if binding.Probe.Available {
		t.Fatal("a file store reported a usable probe")
	}
	if binding.Probe.Reason != MCPTestUnavailableReason {
		t.Fatalf("probe reason: %s", binding.Probe.Reason)
	}
}

func TestMCPBindingReportsProbeAvailabilityForAProbingTransport(t *testing.T) {
	transport := &fakeMCPTransport{servers: []MCPServer{{Name: "s", Transport: "stdio", Command: "/bin/s"}}}
	manager, _ := NewMCPManager(transport)
	binding := manager.Binding(context.Background())
	// A transport that does not declare ProbeAvailable is assumed capable, which
	// is what the fake is.
	if !binding.Probe.Available {
		t.Fatalf("probe: %+v", binding.Probe)
	}
}

// TestMCPBindingNeverRendersSecrets is the guarantee the panel depends on.
func TestMCPBindingNeverRendersSecrets(t *testing.T) {
	kimiHome := mcpHome(t)
	writeMCPFile(t, kimiHome, kernelMCPSample)
	store, _ := NewMCPFileStore(kimiHome)
	manager, _ := NewMCPManager(store)
	encoded, err := json.Marshal(manager.Binding(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"tok-abcdefgh", "Bearer abcdefghijkl"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("the binding leaked %q: %s", secret, encoded)
		}
	}
}

func TestMCPFileStoreReadsTheRealInstalledShapeWithoutWriting(t *testing.T) {
	// The installed app is only read: no file is created and nothing is written.
	archive := "/Applications/Kimi.app/Contents/Resources/resources/daimon-bundle/app/daimon/dist/kimi-code-v2/kernel-bundle.mjs"
	if _, err := os.Stat(archive); err != nil {
		t.Skip("the installed kernel bundle is not present")
	}
	// The kernel keeps mcp.json in its own home; a fresh temporary home proves
	// the store creates nothing until a caller adds a server.
	kimiHome := mcpHome(t)
	store, _ := NewMCPFileStore(kimiHome)
	servers, err := store.ListServers(context.Background())
	if err != nil || len(servers) != 0 {
		t.Fatalf("fresh home: %v %v", servers, err)
	}
	if _, err := os.Stat(filepath.Join(kimiHome, "runtime", "kimi-code", "home", "mcp.json")); !os.IsNotExist(err) {
		t.Fatal("a read created mcp.json")
	}
}
