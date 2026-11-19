package kimi

import (
	"encoding/json"
	"errors"
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
	provider := config.providers["byok-kth-kimi"].(map[string]any)
	model := config.models["byok-kth-kimi/probe"].(map[string]any)
	if !providerMatches(provider, wantProvider) || !modelMatches(model, modelEntry(profileRows(profile)[0])) {
		t.Fatal("page profile did not retain its declaration terms")
	}
	if !config.credentialPublished {
		t.Fatal("the legacy profile publishes a credential reference")
	}
	if strings.Contains(fmt.Sprint(config), "private-") {
		t.Fatal("normalized catalog retained private profile fields")
	}
	profile["delisted"] = []any{"byok-kth-kimi/probe"}
	config, err = accountProfileConfig(profile)
	if err != nil || len(config.models) != 0 {
		t.Fatal("delisted model was accepted")
	}
	profile["delisted"] = []any{}
	profile["models"] = append(profile["models"].([]any), profile["models"].([]any)[0])
	if _, err := accountProfileConfig(profile); err == nil {
		t.Fatal("duplicate model alias was accepted")
	}
}

// clawSurface stands in for the page whose preload publishes `clawBridgeAPI`.
// `list` is the only entry point the verification step may reach, and every
// write the same object offers records itself, so a call that should never
// happen fails loudly instead of silently changing a user's sources.
type clawSurface struct {
	list    any
	calls   []string
	refused bool
}

func installClawSurface(t *testing.T, page *runtimePage, snapshot any) *clawSurface {
	t.Helper()
	surface := &clawSurface{list: snapshot}
	write := func(name string) func(any) any {
		return func(any) any {
			surface.calls = append(surface.calls, name)
			surface.refused = true
			return map[string]any{"ok": false, "reasonCode": "test-write-refused"}
		}
	}
	bridge := map[string]any{"byokSources": map[string]any{
		"list": func() any {
			surface.calls = append(surface.calls, "list")
			return map[string]any{"ok": true, "value": surface.list}
		},
		"preview": func(payload any) any {
			return write("preview")(payload)
		},
		"save":         write("save"),
		"discard":      write("discard"),
		"refresh":      write("refresh"),
		"disconnect":   write("disconnect"),
		"applyPending": write("applyPending"),
		"anythingElse": write("anythingElse"),
	}}
	if err := page.runtime.Set("clawBridgeAPI", bridge); err != nil {
		t.Fatal(err)
	}
	return surface
}

// accountStoreSnapshot is what 3.2.28's `byokSources.list` answers for an
// account whose gateway catalog is applied.
func accountStoreSnapshot() map[string]any {
	return map[string]any{
		"scopeToken": "scope-token", "accountRevision": "account-rev-1", "applyState": "idle",
		"sources": []any{map[string]any{
			"id": "byok-kth", "kind": "kth", "displayName": "Kimi Work+",
			"revision": "source-rev-1", "registryUrl": "http://127.0.0.1:9999/v1/models/api.json",
			"registryKeyConfigured": true, "quarantined": false, "notices": []any{},
			"providers": []any{
				map[string]any{"id": "byok-kth-kimi", "displayName": "Kimi Work+",
					"protocol": "kimi", "baseUrl": "http://127.0.0.1:9999/v1",
					"keyConfigured": true, "headersConfigured": false, "headerNames": []any{}},
				map[string]any{"id": "byok-kth-resp", "displayName": "Kimi Work+",
					"protocol": "openai_responses", "baseUrl": "http://127.0.0.1:9999/v1",
					"keyConfigured": true, "headersConfigured": false, "headerNames": []any{}},
			},
			"models": []any{
				map[string]any{"alias": "byok-kth-kimi/kimi-probe-model", "providerId": "byok-kth-kimi",
					"model": "kimi-probe-model", "family": "kimi", "maxContextSize": 131072,
					"maxOutputSize": 8192, "capabilities": []any{"tool_use"}, "status": "available"},
				map[string]any{"alias": "byok-kth-resp/gpt-probe-model", "providerId": "byok-kth-resp",
					"model": "gpt-probe-model", "family": "responses", "maxContextSize": 262144,
					"status": "available"},
			},
		}},
		"models": []any{},
	}
}

func TestAccountModelStoreReadsThePublishedSourceList(t *testing.T) {
	page := newRuntimePage(t)
	surface := installClawSurface(t, page, accountStoreSnapshot())
	result := page.run(t, accountModelStoreV3Expression)
	if result["ok"] != true || result["applyState"] != "idle" {
		t.Fatalf("store read: %#v", result)
	}
	if len(surface.calls) != 1 || surface.calls[0] != "list" {
		t.Fatalf("store read reached %v", surface.calls)
	}
	if surface.refused {
		t.Fatal("a verification read reached a write entry point")
	}
	config, err := accountStoreV3Config(result)
	if err != nil {
		t.Fatal(err)
	}
	if config.credentialPublished {
		t.Fatal("the published list carries no credential reference")
	}
	if len(config.providers) != 2 || len(config.models) != 2 {
		t.Fatalf("store catalog: providers=%d models=%d", len(config.providers), len(config.models))
	}
	if strings.Contains(fmt.Sprint(config), "scope-token") {
		t.Fatal("the scope token was kept")
	}
}

// Only the page's own `available` word counts as applied. Every other status is
// the store saying the model is not usable yet.
func TestAccountModelStoreCountsOnlyAvailableModels(t *testing.T) {
	for _, status := range []string{"pending", "unconfirmed", "delisted", "disconnected", ""} {
		t.Run(status, func(t *testing.T) {
			page := newRuntimePage(t)
			snapshot := accountStoreSnapshot()
			source := snapshot["sources"].([]any)[0].(map[string]any)
			source["models"].([]any)[1].(map[string]any)["status"] = status
			installClawSurface(t, page, snapshot)
			config, err := accountStoreV3Config(page.run(t, accountModelStoreV3Expression))
			if err != nil {
				t.Fatal(err)
			}
			if _, present := config.models["byok-kth-resp/gpt-probe-model"]; present {
				t.Fatalf("status %q was counted as applied", status)
			}
			if len(config.models) != 1 {
				t.Fatalf("other models were dropped: %v", config.models)
			}
		})
	}
}

// A quarantined source is not the user's applied catalog, whatever its models
// still say.
func TestAccountModelStoreRefusesAQuarantinedSource(t *testing.T) {
	page := newRuntimePage(t)
	snapshot := accountStoreSnapshot()
	snapshot["sources"].([]any)[0].(map[string]any)["quarantined"] = true
	installClawSurface(t, page, snapshot)
	result := page.run(t, accountModelStoreV3Expression)
	if result["ok"] != true {
		t.Fatalf("read: %#v", result)
	}
	if _, err := accountStoreV3Config(result); err == nil {
		t.Fatal("a quarantined source was accepted as the applied catalog")
	}
}

func TestAccountModelStoreReadsTheWorkShellList(t *testing.T) {
	page := newRuntimePage(t)
	called := 0
	if err := page.runtime.Set("kimiAgentAPI", map[string]any{"byokSources": map[string]any{
		"list": func() any {
			called++
			return map[string]any{"ok": true, "value": accountStoreSnapshot()}
		},
	}}); err != nil {
		t.Fatal(err)
	}
	result := page.run(t, accountModelStoreV3Expression)
	catalog, err := accountStoreV3Config(result)
	if result["ok"] != true || err != nil || called != 1 || len(catalog.models) != 2 {
		t.Fatalf("Work shell catalog: ok=%v calls=%d models=%d err=%v", result["ok"], called, len(catalog.models), err)
	}
}

func TestAccountModelStoreRequiresTheListEntryPoint(t *testing.T) {
	page := newRuntimePage(t)
	if err := page.runtime.Set("clawBridgeAPI", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if result := page.run(t, accountModelStoreV3Expression); result["ok"] != false {
		t.Fatalf("missing entry point: %#v", result)
	}
}

// The fallback has to be reached: a build without `getByokProfile` still answers
// through `byokSources.list`, and a build with it keeps answering as it did.
func TestAccountModelConfigFallsBackToThePublishedStore(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"
	installClawSurface(t, page, accountStoreSnapshot())
	config, err := accountProfileConfigFallback(t, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.models) != 2 || config.credentialPublished {
		t.Fatalf("fallback catalog: providers=%d models=%d", len(config.providers), len(config.models))
	}

	page.current, page.rpcResult = "ready", modelProfileResult()
	installModelProfileAPI(page)
	config, err = accountProfileConfigFallback(t, page)
	if err != nil {
		t.Fatal(err)
	}
	if !config.credentialPublished {
		t.Fatal("the legacy profile stopped being read first")
	}
}

// accountProfileConfigFallback runs the two official answers in the order the
// implementation uses, without a live CDP pipe.
func accountProfileConfigFallback(t *testing.T, page *runtimePage) (accountModelConfig, error) {
	t.Helper()
	if legacy := page.run(t, accountModelConfigExpression); legacy["ok"] == true {
		profile, _ := legacy["profile"].(map[string]any)
		return accountProfileConfig(profile)
	}
	store := page.run(t, accountModelStoreV3Expression)
	if store["ok"] != true {
		return accountModelConfig{}, errors.New(AccountErrUnavailable)
	}
	return accountStoreV3Config(store)
}
