package kimi

import (
	"testing"

	"github.com/dop251/goja"
)

func TestAccountBusyExpressionNeverTreatsUnknownEntriesAsIdle(t *testing.T) {
	for _, tt := range []struct {
		name           string
		table          any
		known, running bool
	}{
		{"empty", map[string]any{}, true, false},
		{"official-empty", map[string]any{"statuses": map[string]any{}, "failureIds": map[string]any{}}, true, false},
		{"official-completed", map[string]any{"statuses": map[string]any{"conversation": "completed"}, "failureIds": map[string]any{}}, true, false},
		{"official-running", map[string]any{"statuses": map[string]any{"conversation": "running"}, "failureIds": map[string]any{}}, true, true},
		{"official-blocked", map[string]any{"statuses": map[string]any{"conversation": "blocked"}, "failureIds": map[string]any{"conversation": []string{"failure"}}}, true, false},
		{"official-unknown", map[string]any{"statuses": map[string]any{"conversation": "new-state"}}, false, false},
		{"official-invalid", map[string]any{"statuses": nil}, false, false},
		{"official-invalid-scalar", map[string]any{"statuses": 1}, false, false},
		{"idle", map[string]any{"conversation": map[string]any{"status": "idle"}}, true, false},
		{"running-string", map[string]any{"conversation": "running"}, true, true},
		{"running-flag", []any{map[string]any{"running": true}}, true, true},
		{"unknown-field", map[string]any{"conversation": map[string]any{"newState": "running"}}, false, false},
		{"unknown-string", map[string]any{"conversation": "new-state"}, false, false},
		{"invalid-scalar", 1, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runtime := goja.New()
			if err := runtime.Set("kimiAgentAPI", map[string]any{"getConversationStatuses": func() any { return tt.table }}); err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.RunString("(" + accountWorkBusyExpression + ").then(function(result){globalThis.busyResult=result;});"); err != nil {
				t.Fatal(err)
			}
			result, ok := runtime.Get("busyResult").Export().(map[string]any)
			if !ok || result["known"] != tt.known || result["running"] != tt.running {
				t.Fatalf("unexpected busy evidence: %#v", result)
			}
		})
	}
}
