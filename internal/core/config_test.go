package core

import (
	"testing"
	"time"
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
