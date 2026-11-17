package kimi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func modelProfileResult() map[string]any {
	return map[string]any{"profile": map[string]any{
		"id": "byok-kth", "kind": "kth", "baseUrl": "http://127.0.0.1:9999/v1", "credentialRef": "cred-1",
		"models": []any{map[string]any{
			"alias": "byok-kth-kimi/probe", "providerId": "byok-kth-kimi", "model": "probe", "family": "kimi",
			"maxContextSize": 100000, "maxOutputSize": 1000, "capabilities": []any{"tool_use"}, "token": "private-model-value",
		}},
		"pending": []any{}, "delisted": []any{}, "token": "private-profile-value", "tokenMask": "private-mask-value",
	}}
}

func installModelProfileAPI(page *runtimePage) {
	page.api["getByokProfile"] = func() any {
		page.rpcCalls++
		return page.rpcResult
	}
	page.api["rpc"] = func(string, map[string]any) any {
		panic(page.runtime.NewGoError(fmt.Errorf("unauthorized RPC sender or method")))
	}
}

func TestAccountModelCatalogUsesAllowedPageProfile(t *testing.T) {
	page := newRuntimePage(t)
	page.current, page.rpcResult = "ready", modelProfileResult()
	installModelProfileAPI(page)
	result := page.run(t, accountModelConfigExpression)
	if result["ok"] != true || result["profile"] == nil {
		t.Fatalf("allowed profile was not read: %#v", result)
	}
	for _, private := range []string{"private-profile-value", "private-mask-value", "private-model-value", "secret-token-value", "tokenMask", "endpoint", "transport"} {
		if strings.Contains(fmt.Sprint(result), private) {
			t.Fatalf("profile query exposed %q", private)
		}
	}
	if result := page.run(t, accountModelConfigExpression); result["ok"] != true || page.rpcCalls != 2 {
		t.Fatal("profile query reused an earlier page result")
	}
}

func TestAccountModelProfileRefusesUnavailableOrMalformedPage(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  string
		result any
	}{
		{"not-ready", "starting", modelProfileResult()},
		{"missing-profile", "ready", map[string]any{"profile": nil}},
		{"not-a-record", "ready", map[string]any{"profile": "wrong"}},
		{"wrong-kind", "ready", map[string]any{"profile": map[string]any{"id": "byok-kth", "kind": "other"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := newRuntimePage(t)
			page.current, page.rpcResult = test.state, test.result
			installModelProfileAPI(page)
			if result := page.run(t, accountModelConfigExpression); result["ok"] != false || result["profile"] != nil {
				t.Fatalf("unverified profile: %#v", result)
			}
			if test.state != "ready" && page.rpcCalls != 0 {
				t.Fatal("queried a page whose daemon is not ready")
			}
		})
	}
}

func TestAccountModelProfileRefusesPendingOrMalformedRows(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"pending", func(p map[string]any) { p["pending"] = []any{"byok-kth-kimi/probe"} }},
		{"invalid-pending", func(p map[string]any) { p["pending"] = "wrong" }},
		{"invalid-models", func(p map[string]any) { p["models"] = "wrong" }},
		{"invalid-alias", func(p map[string]any) { p["models"].([]any)[0].(map[string]any)["alias"] = "other/probe" }},
		{"invalid-context", func(p map[string]any) { p["models"].([]any)[0].(map[string]any)["maxContextSize"] = "wrong" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := newRuntimePage(t)
			page.current, page.rpcResult = "ready", modelProfileResult()
			test.mutate(page.rpcResult.(map[string]any)["profile"].(map[string]any))
			installModelProfileAPI(page)
			if result := page.run(t, accountModelConfigExpression); result["ok"] != false {
				t.Fatalf("accepted malformed profile: %#v", result)
			}
		})
	}
}

func TestAccountModelProfileRefusesReadinessLostDuringRead(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"
	page.api["getByokProfile"] = func() any {
		page.current = "switching"
		return modelProfileResult()
	}
	if result := page.run(t, accountModelConfigExpression); result["ok"] != false {
		t.Fatalf("accepted a daemon that stopped being ready: %#v", result)
	}
}

func TestAccountModelProfileConfigReusesDeclarationRules(t *testing.T) {
	payload, err := json.Marshal(modelProfileResult())
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	profile := result["profile"].(map[string]any)
	config, err := accountProfileConfig(profile)
	if err != nil {
		t.Fatal(err)
	}
	wantProvider := map[string]any{"type": "kimi", "baseUrl": "http://127.0.0.1:9999/v1", "credential": "cred-1"}
	provider := section(config, "providers")["byok-kth-kimi"].(map[string]any)
	model := section(config, "models")["byok-kth-kimi/probe"].(map[string]any)
	if !providerMatches(provider, wantProvider) || !modelMatches(model, modelEntry(profileRows(profile)[0])) {
		t.Fatal("page profile did not retain its declaration terms")
	}
	if strings.Contains(fmt.Sprint(config), "private-") {
		t.Fatal("normalized catalog retained private profile fields")
	}
	profile["delisted"] = []any{"byok-kth-kimi/probe"}
	config, err = accountProfileConfig(profile)
	if err != nil || len(section(config, "models")) != 0 {
		t.Fatal("delisted model was accepted")
	}
	profile["delisted"] = []any{}
	profile["models"] = append(profile["models"].([]any), profile["models"].([]any)[0])
	if _, err := accountProfileConfig(profile); err == nil {
		t.Fatal("duplicate model alias was accepted")
	}
}
