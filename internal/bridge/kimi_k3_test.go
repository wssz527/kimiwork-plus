package bridge

import (
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

func TestKimiK3CompatibilityRequiresReadbackFromFixedProvider(t *testing.T) {
	const id = "group/kimi-example"
	d := core.Document{"models": []any{map[string]any{"id": id, "protocol": protocol.ProtocolChat}}, "kimiK3Compatibility": true}
	idForKimi := protocol.KimiCompatibleModelID(id)
	for _, tc := range []struct {
		alias   string
		adopted bool
		want    string
	}{
		{"byok-kth-kimi/" + id, true, "needs_sync"},
		{"byok-foreign/" + idForKimi, true, "provider_mismatch"},
		{"byok-kth-kimi/" + idForKimi, false, "requires_restart"},
		{"byok-kth-kimi/" + idForKimi, true, "ready"},
	} {
		view := &kimi.IntegrationResult{Applied: true, Adopted: tc.adopted, Aliases: map[string]string{id: tc.alias}}
		if got := kimiK3State(d, view, true); got != tc.want {
			t.Errorf("alias %s: got %s, want %s", tc.alias, got, tc.want)
		}
	}
	d["kimiK3Compatibility"] = false
	if got := kimiK3State(d, &kimi.IntegrationResult{Applied: true, Aliases: map[string]string{id: "byok-kth-kimi/" + idForKimi}}, false); got != "needs_sync" {
		t.Fatalf("disabled with previous alias: %q", got)
	}
	if got := kimiK3State(d, &kimi.IntegrationResult{State: kimi.IntegrationNeedsImport, Reason: "models_missing"}, false); got != "needs_sync" {
		t.Fatalf("disabled with pending models_missing: %q", got)
	}
	if got := kimiK3State(d, &kimi.IntegrationResult{State: kimi.IntegrationNeedsImport, Reason: "profile_missing"}, false); got != "" {
		t.Fatalf("disabled and disconnected: %q", got)
	}
	if got := kimiK3State(d, nil, false); got != "" {
		t.Fatalf("disabled before check: %q", got)
	}

	// A Responses row is imported into the same `kimi` family under its generated
	// id, so the state reads it exactly like a Chat row.
	responses := core.Document{"models": []any{map[string]any{"id": "vendor/gpt", "protocol": protocol.ProtocolResponses}}}
	responsesID := protocol.KimiCompatibleModelID("vendor/gpt")
	view := &kimi.IntegrationResult{Applied: true, Adopted: true, Aliases: map[string]string{"vendor/gpt": "byok-kth-kimi/" + responsesID}}
	if got := kimiK3State(responses, view, true); got != "ready" {
		t.Fatalf("responses row state: %q", got)
	}
	if got := kimiK3State(responses, &kimi.IntegrationResult{Applied: true, Adopted: true,
		Aliases: map[string]string{"vendor/gpt": "byok-kth-resp/vendor/gpt"}}, true); got != "needs_sync" {
		t.Fatalf("responses row still on the old family: %q", got)
	}
}

func TestKimiK3CompatibilitySettingPersistsAndAppliesOnlyInGatewayMode(t *testing.T) {
	fixture := newDirectHTTPFixture(t)
	status, state := fixture.call(t, "GET", "/api/state", nil)
	if status != 200 || state[core.KimiK3CompatibilityKey] != true {
		t.Fatalf("default state = %d, %v", status, state[core.KimiK3CompatibilityKey])
	}
	// The gateway default is on; a stored `false` is what the compatibility
	// regression path uses to get the older two-group catalog back.
	status, state = fixture.call(t, "POST", "/api/kimi-k3-compatibility", core.Document{"revision": fixture.revision(t), "enabled": false})
	if status != 200 || core.Object(state["state"])[core.KimiK3CompatibilityKey] != false {
		t.Fatalf("disable = %d, %v", status, state)
	}
	status, state = fixture.call(t, "GET", "/api/state", nil)
	if status != 200 || state[core.KimiK3CompatibilityKey] != false {
		t.Fatalf("stored disable = %d, %v", status, state[core.KimiK3CompatibilityKey])
	}
	status, state = fixture.call(t, "POST", "/api/kimi-k3-compatibility", core.Document{"revision": fixture.revision(t), "enabled": true})
	if status != 200 || core.Object(state["state"])[core.KimiK3CompatibilityKey] != true {
		t.Fatalf("enable = %d, %v", status, state)
	}
	status, state = fixture.call(t, "POST", "/api/integration-mode", core.Document{"revision": fixture.revision(t), "mode": core.IntegrationModeDirect})
	if status != 200 || core.Object(state["state"])[core.KimiK3CompatibilityKey] != false {
		t.Fatalf("direct mode = %d, %v", status, state)
	}
	status, state = fixture.call(t, "POST", "/api/kimi-k3-compatibility", core.Document{"revision": fixture.revision(t), "enabled": false})
	if status != 400 || core.Object(state["error"])["code"] != "invalid_kimi_k3_compatibility" {
		t.Fatalf("direct opt-in result = %d, %v", status, state)
	}
	status, state = fixture.call(t, "POST", "/api/integration-mode", core.Document{"revision": fixture.revision(t), "mode": core.IntegrationModeGateway})
	if status != 200 || core.Object(state["state"])[core.KimiK3CompatibilityKey] != true {
		t.Fatalf("back to gateway = %d, %v", status, state)
	}
}
