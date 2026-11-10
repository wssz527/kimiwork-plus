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
