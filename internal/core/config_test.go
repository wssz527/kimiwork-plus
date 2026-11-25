package core

import (
	"strings"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

func TestPanelModelConstraints(t *testing.T) {
	if err := Validate(Document{"connections": []any{}, "models": []any{Document{"id": "alpha", "protocol": "responses", "contextWindow": float64(32000)}}}); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []Document{{"id": " alpha"}, {"reasoning": true, "reasoningEfforts": []any{"High"}}, {"reasoning": false, "reasoningEfforts": []any{"high"}}, {"defaultEffort": "high", "reasoningEfforts": []any{"high"}}, {"inputModalities": []any{"image"}}, {"inputModalities": []any{}}} {
		m := Document{"id": "alpha", "protocol": "responses", "contextWindow": float64(32000)}
		for k, v := range extra {
			m[k] = v
		}
		if Validate(Document{"connections": []any{}, "models": []any{m}}) == nil {
			t.Fatalf("accepted %#v", extra)
		}
	}
}
func TestIntegrationModeDefaultsToGateway(t *testing.T) {
	for _, d := range []Document{{}, {"integrationMode": nil}, {"integrationMode": ""}, {"integrationMode": "unknown"}, {"integrationMode": "Gateway"}} {
		if mode := IntegrationMode(d); mode != IntegrationModeGateway {
			t.Fatalf("mode for %#v = %q", d, mode)
		}
	}
	if mode := IntegrationMode(Document{"integrationMode": IntegrationModeDirect}); mode != IntegrationModeDirect {
		t.Fatalf("explicit direct mode = %q", mode)
	}
}

func TestSetIntegrationModeRejectsUnknownValues(t *testing.T) {
	d := Document{}
	for _, mode := range []string{"", "Direct", "direct ", "profiles", "gateway-direct"} {
		if err := SetIntegrationMode(d, mode); err == nil || err.Error() != "invalid_integration_mode" {
			t.Fatalf("accepted %q: %v", mode, err)
		}
		if _, exists := d["integrationMode"]; exists {
			t.Fatalf("%q was written despite the rejection", mode)
		}
	}
	if err := SetIntegrationMode(d, IntegrationModeDirect); err != nil {
		t.Fatal(err)
	}
	if d["integrationMode"] != IntegrationModeDirect {
		t.Fatalf("document = %#v", d)
	}
	if err := SetIntegrationMode(d, IntegrationModeGateway); err != nil {
		t.Fatal(err)
	}
	if d["integrationMode"] != IntegrationModeGateway {
		t.Fatalf("document = %#v", d)
	}
}

func TestValidateIntegrationMode(t *testing.T) {
	base := func() Document {
		return Document{"connections": []any{}, "models": []any{}}
	}
	valid := base()
	valid["integrationMode"] = IntegrationModeDirect
	if err := Validate(valid); err != nil {
		t.Fatal(err)
	}
	unset := base()
	if err := Validate(unset); err != nil {
		t.Fatal(err)
	}
	null := base()
	null["integrationMode"] = nil
	if err := Validate(null); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{"", "profile", float64(1), true, []any{IntegrationModeDirect}} {
		d := base()
		d["integrationMode"] = value
		if err := Validate(d); err == nil || err.Error() != "invalid_integration_mode" {
			t.Fatalf("accepted %#v: %v", value, err)
		}
	}
}

func TestTranslationTimeoutCompatibility(t *testing.T) {
	for _, tc := range []struct {
		raw            string
		normal, custom time.Duration
	}{{"", 20, 60}, {"250", 1, 3}, {"40000", 40, 120}, {"99999999", 120, 120}, {"NaN", 20, 60}, {"-1", 20, 60}} {
		t.Setenv("KIMI_WORK_TRANSLATION_TIMEOUT_MS", tc.raw)
		translator := NewTranslator()
		if translator.timeout(false) != tc.normal*time.Second || translator.timeout(true) != tc.custom*time.Second {
			t.Fatal(tc.raw, translator)
		}
	}
}

func TestKimiK3CompatibilityIsAnEffectiveGatewayOptIn(t *testing.T) {
	for name, tc := range map[string]struct {
		document Document
		want     bool
	}{
		"absent defaults on":   {Document{}, true},
		"false turns it off":   {Document{"kimiK3Compatibility": false}, false},
		"true":                 {Document{"kimiK3Compatibility": true}, true},
		"null defaults on":     {Document{"kimiK3Compatibility": nil}, true},
		"string defaults on":   {Document{"kimiK3Compatibility": "true"}, true},
		"direct ignores true":  {Document{"integrationMode": IntegrationModeDirect, "kimiK3Compatibility": true}, false},
		"gateway keeps true":   {Document{"integrationMode": IntegrationModeGateway, "kimiK3Compatibility": true}, true},
		"gateway keeps false":  {Document{"integrationMode": IntegrationModeGateway, "kimiK3Compatibility": false}, false},
		"direct ignores false": {Document{"integrationMode": IntegrationModeDirect, "kimiK3Compatibility": false}, false},
	} {
		if got := KimiK3Compatibility(tc.document); got != tc.want {
			t.Fatalf("%s: %v", name, got)
		}
	}
}

func TestValidateKimiK3Compatibility(t *testing.T) {
	base := func() Document { return Document{"connections": []any{}, "models": []any{}} }
	for _, value := range []any{true, false, nil} {
		d := base()
		d[KimiK3CompatibilityKey] = value
		if err := Validate(d); err != nil {
			t.Fatalf("%#v: %v", value, err)
		}
	}
	for _, value := range []any{"true", "on", float64(1), []any{true}, Document{}} {
		d := base()
		d[KimiK3CompatibilityKey] = value
		if err := Validate(d); err == nil || err.Error() != "invalid_kimi_k3_compatibility" {
			t.Fatalf("accepted %#v: %v", value, err)
		}
	}
}

func TestValidateRefusesKimiCompatibleIDCollision(t *testing.T) {
	model := func(id, protocol string) Document {
		return Document{"id": id, "protocol": protocol, "contextWindow": float64(32000)}
	}
	shadow := protocol.KimiCompatibleModelID("kimi-alpha")
	if shadow == "kimi-alpha" {
		t.Fatal("the generated id equals its logical id")
	}
	// The shadowing row is legitimate on its own: the name starts with "kimi-"
	// and is a short, control-free id.
	models := []any{model("kimi-alpha", "chat"), model("vendor/kimi-beta", "chat"), model(shadow, "chat")}
	// It is refused with the opt-in off, on and absent alike: turning the
	// option on later must not reinterpret a name an old alias already used.
	for name, flag := range map[string]any{"absent": nil, "off": false, "on": true} {
		d := Document{"connections": []any{}, "models": models}
		if flag != nil {
			d[KimiK3CompatibilityKey] = flag
		}
		if err := Validate(d); err == nil || err.Error() != "kimi_k3_model_id_conflict" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A Responses row is offered under its generated id too, so its shadow is
	// refused exactly the same way.
	responses := []any{model("vendor/gpt", "responses"), model(protocol.KimiCompatibleModelID("vendor/gpt"), "chat")}
	if err := Validate(Document{"connections": []any{}, "models": responses}); err == nil || err.Error() != "kimi_k3_model_id_conflict" {
		t.Fatalf("responses shadow: %v", err)
	}
	// Without the shadowing row the same catalog is accepted, including long
	// and nested ids whose generated forms stay legal.
	long := Document{"connections": []any{}, "models": []any{
		model("kimi-alpha", "chat"),
		model("vendor/gpt", "responses"),
		model("vendor/subgroup/kimi-"+strings.Repeat("n", 480), "chat"),
	}}
	if err := Validate(long); err != nil {
		t.Fatal(err)
	}
}

// A connection saved before a protocol was part of the contract carries none,
// and that document must keep loading: the gateway still serves it with the
// per-model routing it always had. Only a save that adds or edits a connection
// is asked for a choice.
func TestValidateKeepsLegacyConnectionsWithoutAProtocol(t *testing.T) {
	for name, extra := range map[string]Document{
		"absent": {},
		"null":   {"protocols": nil},
		"empty":  {"protocols": []any{}},
	} {
		row := Document{"id": "vendor", "name": "Vendor", "baseURL": "https://vendor.example/v1"}
		for key, value := range extra {
			row[key] = value
		}
		if err := Validate(Document{"connections": []any{row}, "models": []any{}}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestValidateReadsOneProtocolAndKeepsLegacyLists(t *testing.T) {
	connection := func(protocols any) Document {
		return Document{"connections": []any{Document{"id": "vendor", "name": "Vendor",
			"baseURL": "https://vendor.example/v1", "protocols": protocols}}, "models": []any{}}
	}
	for _, protocol := range []string{ConnectionProtocolChat, ConnectionProtocolResponses} {
		if err := Validate(connection([]any{protocol})); err != nil {
			t.Fatalf("%s: %v", protocol, err)
		}
	}
	// An older release could store both wires on one connection. That document
	// must still load, with the routing it always had, or an upgrade would refuse
	// to start on a file the user never touched. The save path is what refuses
	// to write such a row again.
	for name, protocols := range map[string]any{
		"both wires":     []any{"chat", "responses"},
		"the same twice": []any{"chat", "chat"},
	} {
		if err := Validate(connection(protocols)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, protocols := range map[string]any{
		"unknown":       []any{"anthropic"},
		"not a list":    "chat",
		"wrong element": []any{float64(1)},
	} {
		if err := Validate(connection(protocols)); err == nil || err.Error() != "invalid_connection_protocols" {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestMergeConnectionsRequiresAProtocolForANewConnection(t *testing.T) {
	d := Document{"connections": []any{}, "models": []any{}}
	row := func(extra Document) Document {
		out := Document{"id": "vendor", "name": "Vendor", "baseURL": "https://vendor.example/v1"}
		for key, value := range extra {
			out[key] = value
		}
		return out
	}
	if _, err := MergeConnections(d, []any{row(nil)}); err == nil || err.Error() != "connection_protocol_required" {
		t.Fatalf("a new connection without a protocol was accepted: %v", err)
	}
	if _, err := MergeConnections(d, []any{row(Document{"protocols": []any{}})}); err == nil || err.Error() != "connection_protocol_required" {
		t.Fatalf("an empty declaration was accepted for a new connection: %v", err)
	}
	for name, protocols := range map[string]any{
		"both wires": []any{"chat", "responses"},
		"unknown":    []any{"anthropic"},
		"not a list": "chat",
	} {
		if _, err := MergeConnections(d, []any{row(Document{"protocols": protocols})}); err == nil || err.Error() != "invalid_connection_protocols" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(Rows(d["connections"])) != 0 {
		t.Fatalf("a refused save wrote connections: %#v", d["connections"])
	}
	if _, err := MergeConnections(d, []any{row(Document{"protocols": []any{ConnectionProtocolResponses}})}); err != nil {
		t.Fatal(err)
	}
	if err := Validate(d); err != nil {
		t.Fatal(err)
	}
	if got := ConnectionProtocol(Object(Rows(d["connections"])[0])); got != ConnectionProtocolResponses {
		t.Fatalf("stored protocol = %q", got)
	}
}

func TestMergeConnectionsKeepsADeclaredProtocolUnlessItChanges(t *testing.T) {
	d := Document{
		"connections": []any{Document{"id": "vendor", "name": "Vendor", "baseURL": "https://vendor.example/v1",
			"apiKeyEnv": "VENDOR_KEY", "protocols": []any{ConnectionProtocolChat}}},
		"models": []any{},
	}
	edit := func(extra Document) Document {
		out := Document{"id": "vendor", "name": "Vendor", "baseURL": "https://vendor.example/v1", "apiKeyEnv": "VENDOR_KEY"}
		for key, value := range extra {
			out[key] = value
		}
		return out
	}
	// An edit that does not restate the protocol — the shape an older panel sends
	// for a rename — keeps the declaration instead of silently widening the
	// connection to every wire.
	for name, extra := range map[string]Document{"omitted": nil, "empty list": {"protocols": []any{}}} {
		if _, err := MergeConnections(d, []any{edit(extra)}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		stored := Object(Rows(d["connections"])[0])
		if got := ConnectionProtocol(stored); got != ConnectionProtocolChat {
			t.Fatalf("%s: declaration = %q", name, got)
		}
		if stored["name"] != "Vendor" {
			t.Fatalf("%s: the edit was not applied: %#v", name, stored)
		}
	}
	// The user's explicit choice is what moves it.
	if _, err := MergeConnections(d, []any{edit(Document{"protocols": []any{ConnectionProtocolResponses}})}); err != nil {
		t.Fatal(err)
	}
	if got := ConnectionProtocol(Object(Rows(d["connections"])[0])); got != ConnectionProtocolResponses {
		t.Fatalf("protocol change = %q", got)
	}
}

func TestMergeConnectionsLeavesALegacyConnectionUndeclared(t *testing.T) {
	d := Document{
		"connections": []any{Document{"id": "legacy", "name": "Legacy",
			"baseURL": "https://legacy.example/v1", "apiKeyEnv": "LEGACY_KEY"}},
		"models": []any{},
	}
	if _, err := MergeConnections(d, []any{Document{"id": "legacy", "name": "Legacy Renamed",
		"baseURL": "https://legacy.example/v1", "apiKeyEnv": "LEGACY_KEY"}}); err != nil {
		t.Fatalf("an unrelated edit of a legacy connection was refused: %v", err)
	}
	row := Object(Rows(d["connections"])[0])
	if _, exists := row["protocols"]; exists {
		t.Fatalf("a protocol was invented for a legacy connection: %#v", row)
	}
	if row["name"] != "Legacy Renamed" {
		t.Fatalf("the edit was not applied: %#v", row)
	}
	if err := Validate(d); err != nil {
		t.Fatal(err)
	}
}

// A legacy row stored with both wires is not this save's statement, so an
// unrelated edit keeps it as it is instead of blocking the whole document.
func TestMergeConnectionsCarriesALegacyProtocolListForward(t *testing.T) {
	d := Document{
		"connections": []any{Document{"id": "legacy", "name": "Legacy", "baseURL": "https://legacy.example/v1",
			"apiKeyEnv": "LEGACY_KEY", "protocols": []any{"chat", "responses"}}},
		"models": []any{},
	}
	if _, err := MergeConnections(d, []any{Document{"id": "legacy", "name": "Legacy Renamed",
		"baseURL": "https://legacy.example/v1", "apiKeyEnv": "LEGACY_KEY"}}); err != nil {
		t.Fatalf("an unrelated edit was refused: %v", err)
	}
	row := Object(Rows(d["connections"])[0])
	if len(Rows(row["protocols"])) != 2 || row["name"] != "Legacy Renamed" {
		t.Fatalf("legacy protocols were not carried forward: %#v", row)
	}
	if err := Validate(d); err != nil {
		t.Fatalf("the carried document no longer loads: %v", err)
	}
	next := Document{"id": "legacy", "name": "Legacy", "baseURL": "https://legacy.example/v1",
		"apiKeyEnv": "LEGACY_KEY", "protocols": []any{ConnectionProtocolChat}}
	d["models"] = []any{Document{"id": "legacy/resp-model", "connectionId": "legacy", "protocol": "responses", "contextWindow": 32000}}
	if _, err := MergeConnections(d, []any{next}); ErrorCode(err) != "invalid_connection_protocol_in_use" {
		t.Fatalf("narrowing a live Responses route to Chat: %v", err)
	}
	if got := Rows(Object(Rows(d["connections"])[0])["protocols"]); len(got) != 2 {
		t.Fatalf("a rejected change modified the stored protocols: %v", got)
	}
	d["models"] = []any{}
	if _, err := MergeConnections(d, []any{next}); err != nil {
		t.Fatal(err)
	}
	if got := ConnectionProtocol(Object(Rows(d["connections"])[0])); got != ConnectionProtocolChat {
		t.Fatalf("explicit choice = %q", got)
	}
}
