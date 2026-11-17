package kimi

import (
	"fmt"
	"strings"
	"testing"
)

func TestAccountModelAliasesReadCurrentRuntimeAndOnlyExposeNames(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"
	page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"models": []any{
		map[string]any{"model": "gateway/one", "token": "private-model-value"},
		map[string]any{"model": "gateway/two"},
	}}}
	result := page.run(t, accountModelAliasesExpression)
	if result["ok"] != true || fmt.Sprint(result["aliases"]) != "[gateway/one gateway/two]" {
		t.Fatalf("model names: %#v", result)
	}
	for _, private := range []string{"secret-token-value", "private-model-value", "endpoint", "transport"} {
		if strings.Contains(fmt.Sprint(result), private) {
			t.Fatalf("model query exposed %q", private)
		}
	}
	page.rpcResult = map[string]any{"models": []any{map[string]any{"model": "gateway/new"}}}
	if result := page.run(t, accountModelAliasesExpression); fmt.Sprint(result["aliases"]) != "[gateway/new]" || page.rpcCalls != 2 {
		t.Fatal("model query reused an earlier runtime result")
	}
}

func TestAccountModelAliasesRefuseUnavailableOrMalformedRuntime(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  string
		result any
	}{
		{"not-ready", "starting", map[string]any{"models": []any{}}},
		{"rpc-error", "ready", map[string]any{"ok": false}},
		{"not-a-list", "ready", map[string]any{"models": "wrong"}},
		{"missing-model", "ready", map[string]any{"models": []any{map[string]any{"id": "wrong-field"}}}},
		{"non-string-model", "ready", map[string]any{"models": []any{map[string]any{"model": 1}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := newRuntimePage(t)
			page.current, page.rpcResult = test.state, test.result
			if result := page.run(t, accountModelAliasesExpression); result["ok"] != false || result["aliases"] != nil {
				t.Fatalf("unverified models: %#v", result)
			}
			if test.state != "ready" && page.rpcCalls != 0 {
				t.Fatal("queried a runtime that is not ready")
			}
		})
	}
}
