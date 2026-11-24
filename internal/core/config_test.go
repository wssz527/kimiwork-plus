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
