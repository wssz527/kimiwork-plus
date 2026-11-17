package kimi

import (
	"fmt"
	"strings"
	"testing"
)

func modelConfigResult() map[string]any {
	return map[string]any{"exists": true, "value": map[string]any{
		"providers": map[string]any{"byok-kth-kimi": map[string]any{
			"type": "kimi", "baseUrl": "http://127.0.0.1:9999/v1", "credential": "byok-kth", "apiKey": "private-provider-value",
		}},
		"models": map[string]any{"byok-kth-kimi/probe": map[string]any{
			"provider": "byok-kth-kimi", "model": "probe", "maxContextSize": 100000, "token": "private-model-value",
		}},
	}}
}

func installModelConfigRPC(t *testing.T, page *runtimePage) {
	t.Helper()
	page.api["rpc"] = func(method string, params map[string]any) any {
		page.rpcCalls++
		if method == "conversations.getKimiModelList" {
			return map[string]any{"models": []any{map[string]any{"model": "k3-agent"}}}
		}
		if method != "config.get" || params["key"] != "model" {
			t.Fatalf("unexpected query: %s", method)
		}
		return page.rpcResult
	}
}

func TestAccountModelCatalogUsesBYOKConfigRatherThanNativeKimiList(t *testing.T) {
	page := newRuntimePage(t)
	page.current, page.rpcResult = "ready", modelConfigResult()
	installModelConfigRPC(t, page)
	result := page.run(t, accountModelConfigExpression)
	catalog, _ := result["catalog"].(map[string]any)
	if result["ok"] != true || catalog == nil || len(section(catalog, "models")) != 1 {
		t.Fatalf("BYOK catalog was not read: %#v", result)
	}
	for _, private := range []string{"secret-token-value", "private-provider-value", "private-model-value", "endpoint", "transport"} {
		if strings.Contains(fmt.Sprint(result), private) {
			t.Fatalf("model query exposed %q", private)
		}
	}
	page.rpcResult = map[string]any{"ok": true, "result": modelConfigResult()}
	if result := page.run(t, accountModelConfigExpression); result["ok"] != true || page.rpcCalls != 2 {
		t.Fatal("model query reused an earlier daemon result")
	}
}

func TestAccountModelConfigRefusesUnavailableOrMalformedDaemon(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  string
		result any
	}{
		{"not-ready", "starting", modelConfigResult()},
		{"rpc-error", "ready", map[string]any{"ok": false}},
		{"missing-config", "ready", map[string]any{"exists": false}},
		{"not-a-record", "ready", map[string]any{"exists": true, "value": "wrong"}},
		{"missing-providers", "ready", map[string]any{"exists": true, "value": map[string]any{"models": map[string]any{}}}},
		{"invalid-provider", "ready", map[string]any{"exists": true, "value": map[string]any{
			"providers": map[string]any{}, "models": map[string]any{"byok-kth-kimi/probe": map[string]any{"provider": 1}},
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := newRuntimePage(t)
			page.current, page.rpcResult = test.state, test.result
			installModelConfigRPC(t, page)
			if result := page.run(t, accountModelConfigExpression); result["ok"] != false || result["catalog"] != nil {
				t.Fatalf("unverified catalog: %#v", result)
			}
			if test.state != "ready" && page.rpcCalls != 0 {
				t.Fatal("queried a daemon that is not ready")
			}
		})
	}
}

func TestAccountModelConfigRefusesReadinessLostDuringRead(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"
	page.api["rpc"] = func(string, map[string]any) any {
		page.current = "switching"
		return modelConfigResult()
	}
	if result := page.run(t, accountModelConfigExpression); result["ok"] != false {
		t.Fatalf("accepted a daemon that stopped being ready: %#v", result)
	}
}
