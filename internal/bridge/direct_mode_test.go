package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/byokdns"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func directConnection(id, name, preset, baseURL string, protocols ...string) map[string]any {
	rows := make([]any, 0, len(protocols))
	for _, protocol := range protocols {
		rows = append(rows, protocol)
	}
	return map[string]any{"id": id, "name": name, "providerPresetId": preset, "baseURL": baseURL, "apiKeyEnv": core.KeyEnv(id), "protocols": rows}
}

func directModel(id, connectionID, protocol string, extra ...any) map[string]any {
	row := map[string]any{"id": id, "protocol": protocol, "connectionId": connectionID,
		"contextWindow": float64(128000), "tools": true}
	for index := 0; index+1 < len(extra); index += 2 {
		row[extra[index].(string)] = extra[index+1]
	}
	return row
}

func directDocument(connections []any, models []any) core.Document {
	if connections == nil {
		connections = []any{}
	}
	if models == nil {
		models = []any{}
	}
	return core.Document{"connections": connections, "models": models}
}

func planModelIDs(plan directPlan) map[string]string {
	out := map[string]string{}
	for _, provider := range plan.providers {
		for _, model := range provider.models {
			out[model.ID] = provider.id
		}
	}
	return out
}

func skippedReasons(plan directPlan) map[string]string {
	out := map[string]string{}
	for _, row := range plan.skipped {
		out[core.String(row["id"])] = core.String(row["reason"])
	}
	return out
}

func TestDirectProtocolMapping(t *testing.T) {
	for _, test := range []struct {
		wire, preset, want string
	}{
		{"chat", "kimi-code-cn", "kimi"},
		{"chat", "kimi-code-global", "kimi"},
		{"chat", "deepseek", "openai"},
		{"chat", "", "openai"},
		{"responses", "deepseek", "openai_responses"},
		{"anthropic", "", "anthropic"},
		{"google-genai", "", "google-genai"},
		{"kimi", "", "kimi"},
		{"realtime", "", ""},
		{"", "", ""},
	} {
		if got := directProtocolFor(test.wire, test.preset); got != test.want {
			t.Fatalf("directProtocolFor(%q, %q) = %q, want %q", test.wire, test.preset, got, test.want)
		}
	}
}

func TestDirectPlanSplitsOneConnectionPerProtocol(t *testing.T) {
	document := directDocument(
		[]any{
			directConnection("magpie", "Magpie", "deepseek", "https://magpie.example/v1", "chat", "responses"),
			directConnection("glm", "GLM", "zhipu-api-cn", "https://glm.example/v1", "chat"),
		},
		[]any{
			directModel("deepseek-v4-flash", "magpie", "responses", "upstreamModelId", "deepseek-v4-flash"),
			directModel("kimi-glm-chat", "glm", "chat"),
		})
	// A chat model on the same connection as a responses model splits the
	// connection; the id carries the protocol so the split is stable.
	document["models"] = append(core.Rows(document["models"]),
		directModel("qwen3.7-plus", "magpie", "responses"))
	plan := buildDirectPlan(document)
	ids := planModelIDs(plan)
	if ids["deepseek-v4-flash"] != "kwp-magpie-openai_responses" || ids["qwen3.7-plus"] != "kwp-magpie-openai_responses" {
		t.Fatalf("responses provider = %+v", ids)
	}
	if ids["kimi-glm-chat"] != "kwp-glm-openai" {
		t.Fatalf("chat provider = %+v", ids)
	}
	if plan.eligible() != 3 || len(plan.skipped) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestDirectPlanLabelsOneConnectionServingTwoProtocols(t *testing.T) {
	document := directDocument(
		[]any{directConnection("mixed", "Mixed", "deepseek", "https://mixed.example/v1", "chat", "responses")},
		[]any{
			directModel("kimi-mixed", "mixed", "chat"),
			directModel("mixed-responses", "mixed", "responses"),
		})
	plan := buildDirectPlan(document)
	if len(plan.providers) != 2 {
		t.Fatalf("providers = %+v", plan.providers)
	}
	names := map[string]string{}
	for _, provider := range plan.providers {
		names[provider.protocol] = provider.displayName
	}
	if names["openai"] != "Mixed (Chat)" || names["openai_responses"] != "Mixed (Responses)" {
		t.Fatalf("display names = %+v", names)
	}
}

func TestDirectPlanKeepsSingleProtocolNameBare(t *testing.T) {
	document := directDocument(
		[]any{directConnection("solo", "Solo", "deepseek", "https://solo.example/v1", "chat")},
		[]any{directModel("deepseek-v4-flash", "solo", "responses")})
	plan := buildDirectPlan(document)
	if len(plan.providers) != 1 || plan.providers[0].displayName != "Solo" {
		t.Fatalf("providers = %+v", plan.providers)
	}
}

func TestDirectPlanSkipsIneligibleModels(t *testing.T) {
	document := directDocument(
		[]any{
			directConnection("ok", "Ok", "deepseek", "https://ok.example/v1", "chat"),
			directConnection("suffix", "Suffix", "deepseek", "https://suffix.example/v1/chat/completions", "chat"),
			directConnection("insecure", "Insecure", "deepseek", "http://public.example/v1", "chat"),
			directConnection("weird", "Weird", "deepseek", "https://weird.example/v1", "chat"),
		},
		[]any{
			directModel("no-connection", "", "responses"),
			directModel("missing-connection", "ghost", "responses"),
			directModel("bad-base", "suffix", "responses"),
			directModel("insecure-base", "insecure", "responses"),
			directModel("no-tools", "ok", "responses", "tools", false),
			directModel("omitted-tools", "ok", "responses", "tools", nil),
			directModel("no-window", "ok", "responses", "contextWindow", float64(0)),
			directModel("unsupported", "weird", "responses", "upstreamProtocol", "realtime"),
		})
	for _, value := range core.Rows(document["models"]) {
		if core.String(core.Object(value)["id"]) == "omitted-tools" {
			delete(core.Object(value), "tools")
		}
	}
	plan := buildDirectPlan(document)
	reasons := skippedReasons(plan)
	for id, want := range map[string]string{
		"no-connection":      directSkipMissingConnection,
		"missing-connection": directSkipMissingConnection,
		"bad-base":           directSkipInvalidBaseURL,
		"insecure-base":      directSkipInvalidBaseURL,
		"no-tools":           directSkipToolsRequired,
		"omitted-tools":      directSkipToolsRequired,
		"no-window":          directSkipContextWindow,
		"unsupported":        directSkipUnsupportedProtocol,
	} {
		if reasons[id] != want {
			t.Fatalf("%s skipped as %q, want %q (%+v)", id, reasons[id], want, plan.skipped)
		}
	}
	if plan.eligible() != 0 {
		t.Fatalf("eligible = %d", plan.eligible())
	}
	if plan.total != 8 {
		t.Fatalf("total = %d", plan.total)
	}
}

func TestDirectPlanReportsSecondEndpointOnSameProtocol(t *testing.T) {
	document := directDocument(
		[]any{
			directConnection("first", "First", "deepseek", "https://shared.example/v1", "chat"),
			directConnection("second", "Second", "deepseek", "https://shared.example/v1/", "chat"),
			directConnection("other", "Other", "deepseek", "https://shared.example/v1", "chat"),
		},
		[]any{
			directModel("kimi-first", "first", "chat"),
			directModel("kimi-second", "second", "chat"),
			directModel("kimi-other", "other", "chat"),
		})
	plan := buildDirectPlan(document)
	ids := planModelIDs(plan)
	if ids["kimi-first"] != "kwp-first-openai" || len(ids) != 1 {
		t.Fatalf("first endpoint won: %+v", ids)
	}
	reasons := skippedReasons(plan)
	if reasons["kimi-second"] != directSkipDuplicateEndpoint || reasons["kimi-other"] != directSkipDuplicateEndpoint {
		t.Fatalf("conflicts = %+v", reasons)
	}
	if plan.duplicates != 2 {
		t.Fatalf("duplicates = %d", plan.duplicates)
	}
}

func TestDirectPlanAllowsSameEndpointOnDifferentProtocols(t *testing.T) {
	document := directDocument(
		[]any{
			directConnection("chat", "Shared", "deepseek", "https://shared.example/v1", "chat"),
			directConnection("responses", "Shared", "deepseek", "https://shared.example/v1", "responses"),
		},
		[]any{
			directModel("kimi-chat", "chat", "chat"),
			directModel("shared-responses", "responses", "responses"),
		})
	plan := buildDirectPlan(document)
	if plan.eligible() != 2 || len(plan.skipped) != 0 {
		t.Fatalf("plan = %+v", plan.skipped)
	}
}

func TestDirectPlanTreatsAnthropicV1AsDuplicate(t *testing.T) {
	document := directDocument(
		[]any{
			directConnection("a", "A", "", "https://anthropic.example/v1", "chat"),
			directConnection("b", "B", "", "https://anthropic.example", "chat"),
		},
		[]any{
			directModel("kimi-a", "a", "responses", "upstreamProtocol", "anthropic"),
			directModel("kimi-b", "b", "responses", "upstreamProtocol", "anthropic"),
		})
	plan := buildDirectPlan(document)
	reasons := skippedReasons(plan)
	if reasons["kimi-b"] != directSkipDuplicateEndpoint {
		t.Fatalf("anthropic /v1 was not normalised away: %+v", reasons)
	}
}

func TestDirectModelCarriesCapabilitiesAndLimits(t *testing.T) {
	document := directDocument(
		[]any{directConnection("ok", "Ok", "deepseek", "https://ok.example/v1", "chat")},
		[]any{directModel("deepseek-v4-flash", "ok", "responses",
			"upstreamModelId", "deepseek-v4-flash-upstream",
			"name", "DeepSeek V4.1 Flash",
			"maxOutputTokens", float64(8192),
			"reasoning", true,
			"inputModalities", []any{"text", "image", "video"},
			"reasoningEfforts", []any{"low", "high"},
			"defaultEffort", "high")})
	plan := buildDirectPlan(document)
	model := plan.providers[0].models[0]
	if model.ID != "deepseek-v4-flash" || model.Model != "deepseek-v4-flash-upstream" {
		t.Fatalf("model = %+v", model)
	}
	if model.DisplayName != "DeepSeek V4.1 Flash" || model.MaxContextSize != 128000 || model.MaxOutputSize != 8192 {
		t.Fatalf("limits = %+v", model)
	}
	want := []string{"tool_use", "thinking", "image_in", "video_in"}
	if len(model.Capabilities) != len(want) {
		t.Fatalf("capabilities = %+v", model.Capabilities)
	}
	for index, value := range want {
		if model.Capabilities[index] != value {
			t.Fatalf("capabilities = %+v", model.Capabilities)
		}
	}
	if len(model.SupportEfforts) != 2 || model.DefaultEffort != "high" {
		t.Fatalf("efforts = %+v", model)
	}
}

func TestDirectModelDropsUnknownDefaultEffort(t *testing.T) {
	document := directDocument(
		[]any{directConnection("ok", "Ok", "deepseek", "https://ok.example/v1", "chat")},
		[]any{directModel("deepseek-v4-flash", "ok", "responses",
			"reasoningEfforts", []any{"low"}, "defaultEffort", "high")})
	model := buildDirectPlan(document).providers[0].models[0]
	if model.DefaultEffort != "" || len(model.SupportEfforts) != 1 {
		t.Fatalf("efforts = %+v", model)
	}
}

func TestDirectSpecIsNilInGatewayMode(t *testing.T) {
	document := directDocument(
		[]any{directConnection("ok", "Ok", "deepseek", "https://ok.example/v1", "chat")},
		[]any{directModel("deepseek-v4-flash", "ok", "responses")})
	if spec := buildDirectSpec(document); spec != nil {
		t.Fatalf("gateway mode built a direct spec: %+v", spec)
	}
	document["integrationMode"] = core.IntegrationModeDirect
	spec := buildDirectSpec(document)
	if spec == nil || len(spec.Providers) != 1 || len(spec.Models) != 1 || spec.DisplayName != kimi.DirectDisplayName {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Providers[0].ID != "kwp-ok-openai_responses" || spec.Providers[0].Protocol != "openai_responses" {
		t.Fatalf("provider = %+v", spec.Providers[0])
	}
	// The spec must never carry a key, so the import shape is what adds one.
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || containsText([]string{string(encoded)}, "apiKey") {
		t.Fatalf("spec mentions a key field: %s", encoded)
	}
}

func TestDirectImportRequestReadsKeysAndFlags(t *testing.T) {
	document := directDocument(
		[]any{
			directConnection("a", "A", "deepseek", "https://a.example/v1", "chat"),
			directConnection("b", "B", "deepseek", "https://b.example/v1", "chat"),
		},
		[]any{
			directModel("kimi-a", "a", "chat"),
			directModel("kimi-b", "b", "chat"),
		})
	document["integrationMode"] = core.IntegrationModeDirect
	keys := testKeys{"KIMI_WORK_A_KEY": "key-a", "KIMI_WORK_B_KEY": "key-b"}
	request, skipped, err := directImportRequest(document, keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 || len(request.Providers) != 2 || len(request.Models) != 2 {
		t.Fatalf("request = %+v skipped = %+v", request, skipped)
	}
	if request.DisplayName != kimi.DirectDisplayName || !request.DisconnectGateway || !request.OpenSettings {
		t.Fatalf("flags = %+v", request)
	}
	keysByID := map[string]string{}
	for _, provider := range request.Providers {
		keysByID[provider.ID] = provider.APIKey
	}
	if keysByID["kwp-a-openai"] != "key-a" || keysByID["kwp-b-openai"] != "key-b" {
		t.Fatalf("keys = %+v", keysByID)
	}
}

func TestDirectImportRequestSkipsProviderWithoutKey(t *testing.T) {
	document := directDocument(
		[]any{
			directConnection("a", "A", "deepseek", "https://a.example/v1", "chat"),
			directConnection("b", "B", "deepseek", "https://b.example/v1", "chat"),
		},
		[]any{
			directModel("kimi-a", "a", "chat"),
			directModel("kimi-b", "b", "chat"),
		})
	document["integrationMode"] = core.IntegrationModeDirect
	request, skipped, err := directImportRequest(document, testKeys{"KIMI_WORK_A_KEY": "key-a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Providers) != 1 || len(request.Models) != 1 || request.Models[0].ID != "kimi-a" {
		t.Fatalf("request = %+v", request)
	}
	reasons := map[string]string{}
	for _, row := range skipped {
		reasons[core.String(row["id"])] = core.String(row["reason"])
	}
	if reasons["kimi-b"] != directSkipMissingAPIKey {
		t.Fatalf("skipped = %+v", skipped)
	}
}

func TestDirectImportRequestFallsBackToVisibleCredential(t *testing.T) {
	document := directDocument(
		[]any{directConnection("a", "A", "deepseek", "https://a.example/v1", "chat")},
		[]any{directModel("kimi-a", "a", "chat")})
	document["integrationMode"] = core.IntegrationModeDirect
	visibleCalls := 0
	request, _, err := directImportRequest(document, testKeys{}, func(KeyStore, string) (string, error) {
		visibleCalls++
		return "recovered-key", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if visibleCalls != 1 || len(request.Providers) != 1 || request.Providers[0].APIKey != "recovered-key" {
		t.Fatalf("request = %+v visibleCalls = %d", request, visibleCalls)
	}
}

func TestDirectImportRequestRefusesWithoutEligibleModel(t *testing.T) {
	gateway := directDocument(
		[]any{directConnection("a", "A", "deepseek", "https://a.example/v1", "chat")},
		[]any{directModel("kimi-a", "a", "chat", "tools", false)})
	gateway["integrationMode"] = core.IntegrationModeDirect
	if _, _, err := directImportRequest(gateway, testKeys{"KIMI_WORK_A_KEY": "key"}, nil); err == nil || err.Error() != "direct_no_eligible_models" {
		t.Fatalf("err = %v", err)
	}

	conflict := directDocument(
		[]any{
			directConnection("a", "A", "deepseek", "https://shared.example/v1", "chat"),
			directConnection("b", "B", "deepseek", "https://shared.example/v1", "chat"),
		},
		[]any{
			directModel("kimi-a", "a", "chat"),
			directModel("kimi-b", "b", "chat"),
		})
	conflict["integrationMode"] = core.IntegrationModeDirect
	// One endpoint still has an eligible model, so that one is imported and the
	// conflict is reported per model instead of failing the whole import.
	request, skipped, err := directImportRequest(conflict, testKeys{"KIMI_WORK_A_KEY": "k", "KIMI_WORK_B_KEY": "k"}, nil)
	if err != nil || len(request.Models) != 1 || len(skipped) != 1 {
		t.Fatalf("request = %+v skipped = %+v err = %v", request, skipped, err)
	}

	missingKey := directDocument(
		[]any{directConnection("a", "A", "deepseek", "https://a.example/v1", "chat")},
		[]any{directModel("kimi-a", "a", "chat")})
	missingKey["integrationMode"] = core.IntegrationModeDirect
	if _, _, err := directImportRequest(missingKey, testKeys{}, nil); err == nil || err.Error() != "direct_missing_api_key" {
		t.Fatalf("err = %v", err)
	}
}

func TestDirectBaseURLRejectsOperationSuffixes(t *testing.T) {
	for _, value := range []string{
		"https://api.example/v1/chat/completions",
		"https://api.example/v1/responses",
		"https://api.example/v1/messages",
		"http://api.example/v1",
		"https://user:pass@api.example/v1",
		"https://api.example/v1?x=1",
		"https://api.example/v1#f",
	} {
		if _, ok := directBaseURL(value); ok {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, value := range []string{"https://api.example/v1", "https://api.example", "http://127.0.0.1:8080/v1", "https://api.example/anthropic"} {
		if _, ok := directBaseURL(value); !ok {
			t.Fatalf("refused %q", value)
		}
	}
}

func TestDirectModeRowShape(t *testing.T) {
	document := directDocument(
		[]any{directConnection("a", "A", "deepseek", "https://a.example/v1", "chat")},
		[]any{directModel("kimi-a", "a", "chat")})
	plan := buildDirectPlan(document)
	row := directModeRow(core.IntegrationModeGateway, true, true, nil, core.Document{}, plan)
	for _, key := range []string{"mode", "supported", "staticHostsReady", "eligibleModels", "totalModels", "skippedModels", "activeFeaturesBypassed", "readyForZeroGateway"} {
		if _, present := row[key]; !present {
			t.Fatalf("row is missing %q: %+v", key, row)
		}
	}
	if row["mode"] != core.IntegrationModeGateway || row["supported"] != true || row["eligibleModels"] != 1 || row["totalModels"] != 1 {
		t.Fatalf("row = %+v", row)
	}
	if skipped, ok := row["skippedModels"].([]core.Document); !ok || skipped == nil || len(skipped) != 0 {
		t.Fatalf("skippedModels must be a non-nil empty slice: %#v", row["skippedModels"])
	}
	if bypassed, ok := row["activeFeaturesBypassed"].([]string); !ok || bypassed == nil || len(bypassed) != 0 {
		t.Fatalf("activeFeaturesBypassed must be a non-nil empty slice: %#v", row["activeFeaturesBypassed"])
	}
	if row["readyForZeroGateway"] != false {
		t.Fatalf("gateway mode claimed zero-gateway readiness: %+v", row)
	}
}

func TestDirectModeRowReadyOnlyWhenFullySettled(t *testing.T) {
	document := directDocument(nil, nil)
	integration := core.Document{"mode": kimi.IntegrationModeDirect, "state": kimi.IntegrationReady, "adopted": true}
	ready := directModeRow(core.IntegrationModeDirect, true, true, nil, integration, buildDirectPlan(document))
	if ready["readyForZeroGateway"] != true {
		t.Fatalf("row = %+v", ready)
	}
	for name, mutate := range map[string]func(core.Document){
		"not-adopted": func(d core.Document) { d["adopted"] = false },
		"route-cleanup-pending": func(d core.Document) {
			d["nativeImport"] = map[string]any{"routeCleanupPending": true}
		},
		"wrong-mode":           func(d core.Document) { d["mode"] = kimi.IntegrationModeGateway },
		"not-ready":            func(d core.Document) { d["state"] = kimi.IntegrationNeedsImport },
		"static-hosts-missing": func(core.Document) {},
	} {
		candidate := core.Document{}
		for key, value := range integration {
			candidate[key] = value
		}
		mutate(candidate)
		staticHosts := name != "static-hosts-missing"
		row := directModeRow(core.IntegrationModeDirect, true, staticHosts, nil, candidate, buildDirectPlan(document))
		if row["readyForZeroGateway"] != false {
			t.Fatalf("%s still claimed readiness: %+v", name, row)
		}
	}
}

func TestDirectSupportedRejectsLegacyStore(t *testing.T) {
	report := kimi.CompatibilityReport{
		Status: kimi.CompatStatusDegraded,
		Checks: kimi.CompatibilityChecks{
			StoreFormat: kimi.StoreFormatCompatCheck{Status: kimi.CheckStatusPass, Format: kimi.StoreFormatProfiles},
			AccountAPI:  kimi.AccountAPICompatCheck{Status: kimi.CheckStatusPass, ByokSourcesList: true},
		},
	}
	if directSupported(report) {
		t.Fatal("a legacy profiles store cannot hold a custom source")
	}
	report.Status = kimi.CompatStatusVerified
	report.Checks.StoreFormat = kimi.StoreFormatCompatCheck{Status: kimi.CheckStatusPass, Format: kimi.StoreFormatAccounts}
	report.Checks.AccountAPI = kimi.AccountAPICompatCheck{Status: kimi.CheckStatusPass, ByokSourcesList: false}
	if directSupported(report) {
		t.Fatal("a build without byokSources.list cannot carry a direct import")
	}
	report.Checks.AccountAPI = kimi.AccountAPICompatCheck{Status: kimi.CheckStatusPass, ByokSourcesList: true}
	if !directSupported(report) {
		t.Fatal("v3 store with byokSources.list was refused")
	}
	// An answer that could not be produced is not evidence of a missing surface.
	report.Checks.AccountAPI = kimi.AccountAPICompatCheck{Status: kimi.CheckStatusUnavailable}
	if !directSupported(report) {
		t.Fatal("an unavailable check blocked direct mode")
	}
	report.Checks.StoreFormat = kimi.StoreFormatCompatCheck{Status: kimi.CheckStatusAbsent, Format: kimi.StoreFormatAbsent}
	if !directSupported(report) {
		t.Fatal("a store that does not exist yet blocked direct mode")
	}
}

func TestDirectSupportedAllowsInitialAndCheckingReports(t *testing.T) {
	if !directSupported(kimi.CompatibilityReport{}) {
		t.Fatal("empty compatibility report reported unsupported")
	}
	if !directSupported(initialCompatibilityReport()) {
		t.Fatal("initial checking report reported unsupported")
	}
	// Even if a completed report left AccountAPI.Status empty, do not assume unsupported.
	completedEmptyAPI := kimi.CompatibilityReport{
		Status: kimi.CompatStatusVerified,
		Checks: kimi.CompatibilityChecks{
			StoreFormat: kimi.StoreFormatCompatCheck{Status: kimi.CheckStatusPass, Format: kimi.StoreFormatAccounts},
			AccountAPI:  kimi.AccountAPICompatCheck{Status: "", ByokSourcesList: false},
		},
	}
	if !directSupported(completedEmptyAPI) {
		t.Fatal("empty AccountAPI.Status reported unsupported")
	}
	n := newDirectTestNative(t, false)
	n.compatibility = initialCompatibilityReport()
	row := n.directModeState(directDocument(nil, nil), core.Document{}, core.Document{})
	if row["supported"] != true {
		t.Fatalf("startup directModeState flashed supported=false: %+v", row)
	}
}

func newDirectTestNative(t *testing.T, direct bool) *Native {
	t.Helper()
	directory := t.TempDir()
	stateDir := filepath.Join(directory, "state")
	profile := filepath.Join(directory, "profile")
	for _, dir := range []string{stateDir, profile} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	controller := &kimi.Controller{}
	mode := kimi.IntegrationModeGateway
	if direct {
		mode = kimi.IntegrationModeDirect
	}
	if err := controller.Configure(kimi.ControllerSettings{StateDir: stateDir, UserDataDir: profile,
		IntegrationMode: mode, Installation: kimi.Installation{}}); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := newIsolatedTestPolicies(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(policies.home, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Native{
		controller: controller,
		service:    &Service{Store: store, StateDir: stateDir, Keys: CacheKeys(testKeys{})},
		policies:   policies,
		probe:      &byokdns.Probe{Lookup: func(context.Context, string) (string, error) { return "10.1.2.3", nil }},
		hostsPath:  filepath.Join(directory, "hosts"),
		// The resolver path is scoped to the test: the machine's real
		// /etc/resolver entry belongs to whatever release installed it, and a test
		// must neither read it nor remove it.
		resolverPath: filepath.Join(directory, "resolver", byokdns.Domain),
	}
	if err := n.initEnhancements(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.web.Close() })
	return n
}

func TestNativeIsDirectModeFollowsTheController(t *testing.T) {
	if (&Native{}).isDirectMode() {
		t.Fatal("a native without a controller claimed direct mode")
	}
	if newDirectTestNative(t, false).isDirectMode() {
		t.Fatal("gateway configuration reported direct mode")
	}
	if !newDirectTestNative(t, true).isDirectMode() {
		t.Fatal("direct configuration reported gateway mode")
	}
}

func TestNativeDirectModeBypassedFeatures(t *testing.T) {
	n := newDirectTestNative(t, true)
	document := directDocument(nil, nil)
	search := core.Document{}
	if bypassed := n.directBypassedFeatures(document, search); len(bypassed) != 0 {
		t.Fatalf("bypassed = %+v", bypassed)
	}
	document["budget"] = map[string]any{"enabled": true}
	document["models"] = []any{map[string]any{"id": "kimi-a", "failover": []any{map[string]any{"connectionId": "b"}}}}
	if err := os.WriteFile(filepath.Join(n.service.StateDir, "shared-instructions.md"), []byte("# rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if result := n.policies.services.Update(kimi.ServicesInput{Search: &kimi.ServiceSettings{Enabled: enabled, BaseURL: "https://search.example", APIKey: "search-key"}}); result.State == kimi.NativeStateConflict {
		t.Fatalf("services update = %+v", result)
	}
	search = core.Object(n.policies.State(document)["search"])
	bypassed := n.directBypassedFeatures(document, search)
	want := []string{"budget", "failover", "shared_instructions", "web_tools"}
	if len(bypassed) != len(want) {
		t.Fatalf("bypassed = %+v", bypassed)
	}
	for index, value := range want {
		if bypassed[index] != value {
			t.Fatalf("bypassed = %+v", bypassed)
		}
	}
}

func TestNativeBYOKSetupRequiresStaticHostsOnlyInDirectMode(t *testing.T) {
	gateway := newDirectTestNative(t, false)
	if gateway.byokSetup().RequireStaticHosts {
		t.Fatal("gateway mode asked for a static hosts entry")
	}
	direct := newDirectTestNative(t, true)
	setup := direct.byokSetup()
	if !setup.RequireStaticHosts {
		t.Fatal("direct mode did not ask for a static hosts entry")
	}
	if setup.HostsPath != direct.hostsPath {
		t.Fatalf("hosts path = %q", setup.HostsPath)
	}
}

func TestNativeBYOKStateReportsStaticHosts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the static hosts answer is macOS-only")
	}
	n := newDirectTestNative(t, true)
	flag := n.byokFlagFile()
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flag, []byte(`{"revealed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := core.Object(n.byokState())
	if state["revealed"] != true {
		t.Fatalf("state = %+v", state)
	}
	dns := core.Object(state["dns"])
	if dns["staticHosts"] != false {
		t.Fatalf("an empty hosts file reported a static entry: %+v", dns)
	}
	if err := os.WriteFile(n.hostsPath, []byte(byokdns.HostsMarker+"\n"+byokdns.HostsEntry()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dns = core.Object(core.Object(n.byokState())["dns"])
	if dns["staticHosts"] != true || dns["state"] != "hosts" {
		t.Fatalf("dns = %+v", dns)
	}
}

func TestNativePrepareDirectReusesStaticHostsWithoutTheListener(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the static hosts path is macOS-only")
	}
	n := newDirectTestNative(t, true)
	n.dns = &fakeBYOKDNS{err: byokdns.ErrUnavailable}
	flag := n.byokFlagFile()
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flag, []byte(`{"revealed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.hostsPath, []byte(byokdns.HostsMarker+"\n"+byokdns.HostsEntry()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	n.admin = func(_ context.Context, _, report string) error {
		prompts++
		if report != "" {
			return os.WriteFile(report, []byte("done\n"), 0o600)
		}
		return nil
	}
	if err := n.prepareBYOK(context.Background()); err != nil {
		t.Fatalf("prepare failed with a ready static entry: %v", err)
	}
	if prompts != 0 {
		t.Fatalf("a ready static entry still asked for the administrator %d times", prompts)
	}
	if n.dns.Port() != 0 {
		t.Fatal("the listener was left running in direct mode")
	}
}

func TestNativePrepareDirectWritesStaticHostsOnce(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the static hosts path is macOS-only")
	}
	n := newDirectTestNative(t, true)
	n.dns = &fakeBYOKDNS{err: byokdns.ErrUnavailable}
	flag := n.byokFlagFile()
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flag, []byte(`{"revealed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	n.admin = func(_ context.Context, _ string, _ string) error {
		prompts++
		return writeStaticHosts(n.hostsPath)
	}
	if err := n.prepareBYOK(context.Background()); err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	if prompts != 1 {
		t.Fatalf("administrator prompts = %d", prompts)
	}
	state := core.Object(core.Object(n.byokState())["dns"])
	if state["staticHosts"] != true {
		t.Fatalf("dns = %+v", state)
	}
	if err := n.prepareBYOK(context.Background()); err != nil {
		t.Fatalf("second prepare failed: %v", err)
	}
	if prompts != 1 {
		t.Fatalf("second prepare asked again: %d prompts", prompts)
	}
}

func writeStaticHosts(path string) error {
	content, _, err := byokdns.UpdateHosts(nil, "prepare")
	if err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o600)
}

func TestNativeCleanupOrphanedLegacyToleratesMissingWorkHome(t *testing.T) {
	n := newDirectTestNative(t, true)
	n.cleanupOrphanedLegacy()
	if err := n.controller.Configure(kimi.ControllerSettings{}); err == nil {
		t.Fatal("an empty state directory was accepted")
	}
}

func TestDirectImportRefusesWithoutAStore(t *testing.T) {
	n := &Native{}
	if _, err := n.directImportFor(); err == nil {
		t.Fatal("an import without a store succeeded")
	}
}

func TestDirectImportErrorKeepsTheCode(t *testing.T) {
	document := directDocument(
		[]any{directConnection("a", "A", "deepseek", "https://a.example/v1", "chat")},
		[]any{directModel("kimi-a", "a", "chat", "tools", false)})
	document["integrationMode"] = core.IntegrationModeDirect
	_, _, err := directImportRequest(document, testKeys{"KIMI_WORK_A_KEY": "k"}, nil)
	if core.ErrorCode(err) != "direct_no_eligible_models" {
		t.Fatalf("code = %q", core.ErrorCode(err))
	}
	if errors.Is(err, kimi.ErrInvalidModels) {
		t.Fatalf("unexpected wrapped error: %v", err)
	}
}

func TestDirectPlanExcludesMissingCredentialsEverywhere(t *testing.T) {
	document := directDocument(
		[]any{
			directConnection("a", "A", "deepseek", "https://a.example/v1", "chat"),
			directConnection("b", "B", "deepseek", "https://b.example/v1", "chat"),
		},
		[]any{
			directModel("kimi-a", "a", "chat"),
			directModel("kimi-b", "b", "chat"),
		})
	document["integrationMode"] = core.IntegrationModeDirect
	available := map[string]bool{"a": true, "b": false}
	plan := buildDirectPlan(document, available)
	if plan.eligible() != 1 || skippedReasons(plan)["kimi-b"] != directSkipMissingAPIKey {
		t.Fatalf("plan = %+v", plan)
	}
	spec := buildDirectSpec(document, available)
	if len(spec.Models) != 1 || spec.Models[0].ID != "kimi-a" {
		t.Fatalf("spec = %+v", spec)
	}

	n := newDirectTestNative(t, true)
	n.service.Keys = CacheKeys(testKeys{"KIMI_WORK_A_KEY": "key-a"})
	if got := n.credentialAvailability(document); !got["a"] || got["b"] {
		t.Fatalf("availability = %+v", got)
	}
}

func TestNativeConfigureDirectStopsDNSOnlyAfterStaticResolutionIsReady(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the DNS listener is macOS-only")
	}
	n := newDirectTestNative(t, false)
	n.dns = &fakeBYOKDNS{port: byokdns.DNSPort}
	document := directDocument(nil, nil)
	document["integrationMode"] = core.IntegrationModeDirect
	if err := n.Configure(document); err != nil {
		t.Fatal(err)
	}
	if n.dns.Port() != byokdns.DNSPort {
		t.Fatal("saving the direct preference stopped the gateway's DNS before hosts was ready")
	}
	if err := writeStaticHosts(n.hostsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(n.resolverPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.resolverPath, []byte("legacy resolver\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.Configure(document); err != nil {
		t.Fatal(err)
	}
	if n.dns.Port() != byokdns.DNSPort {
		t.Fatal("DNS stopped while the legacy resolver still needed it")
	}
	if err := os.Remove(n.resolverPath); err != nil {
		t.Fatal(err)
	}
	if err := n.Configure(document); err != nil {
		t.Fatal(err)
	}
	if n.dns.Port() != 0 {
		t.Fatal("DNS stayed running after static resolution was ready")
	}
}

func TestDirectModeBaseDoesNotAssumeDesktopHosts(t *testing.T) {
	row := directModeBase(directDocument(nil, nil))
	if (runtime.GOOS == "darwin" || runtime.GOOS == "windows") && row["staticHostsReady"] != false {
		t.Fatalf("unchecked desktop hosts reported ready: %+v", row)
	}
}

func TestNativeGatewayModePublishesRemainingDirectCleanup(t *testing.T) {
	n := newDirectTestNative(t, false)
	document := directDocument(nil, nil)
	integration := core.Document{"mode": kimi.IntegrationModeGateway, "state": kimi.IntegrationReady,
		"applied": true, "routeCleanupPending": true}
	if row := n.directModeState(document, integration, core.Document{}); row["routeCleanupPending"] != true {
		t.Fatalf("reconcile cleanup evidence was not published: %+v", row)
	}
	delete(integration, "routeCleanupPending")
	if row := n.directModeState(document, integration, core.Document{}); row["routeCleanupPending"] != false {
		t.Fatalf("cleared cleanup evidence was still published: %+v", row)
	}
}

func TestNativeDirectModeCleanupRequiresAnAppliedReplacement(t *testing.T) {
	n := newDirectTestNative(t, true)
	settings := n.controller.Settings()
	settings.GatewayURL = "http://127.0.0.1:9100/v1"
	if err := n.controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	home, err := n.controller.WorkHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	store := []byte(`{"v":3,"accounts":{"account":{"revision":"a1","sources":[{"id":"byok-kth","kind":"kth","registryUrl":"http://127.0.0.1:9100/v1/models/api.json","desired":{"providers":[],"models":[]}}],"active":{"providers":[],"models":[]},"blockedAliases":{}}},"ownedResources":{}}`)
	if err := os.WriteFile(filepath.Join(home, "byok-providers.json"), store, 0o600); err != nil {
		t.Fatal(err)
	}
	document := directDocument(nil, nil)
	document["integrationMode"] = core.IntegrationModeDirect
	integration := core.Document{"mode": kimi.IntegrationModeDirect, "state": kimi.IntegrationNeedsImport, "applied": false}
	if row := n.directModeState(document, integration, core.Document{}); row["routeCleanupPending"] != false {
		t.Fatalf("an unwritten replacement claimed cleanup work: %+v", row)
	}
	current := n.integrationForState(&kimi.IntegrationResult{Mode: kimi.IntegrationModeDirect,
		State: kimi.IntegrationReady, Applied: true, Adopted: true})
	receipt := settledNativeImportReceipt(current, kimi.ImportResult{OK: true,
		State: kimi.ImportStateRequiresRestart, Reason: kimi.NativeRequiresRestart,
		Applied: true, RequiresRestart: true, RestartDeferred: true, RouteCleanupPending: true})
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var published core.Document
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatal(err)
	}
	integration["state"] = kimi.IntegrationReady
	integration["applied"] = true
	integration["adopted"] = true
	integration["routeCleanupPending"] = current.RouteCleanupPending
	integration["nativeImport"] = published
	if !current.RouteCleanupPending || published["routeCleanupPending"] != true {
		t.Fatalf("the remaining old gateway was cleared before disconnect: %+v %+v", current, published)
	}
	if row := n.directModeState(document, integration, core.Document{}); row["routeCleanupPending"] != true {
		t.Fatalf("the remaining old gateway was not reported after apply: %+v", row)
	}

	store = []byte(`{"v":3,"accounts":{"account":{"revision":"a1","sources":[],"active":{"providers":[],"models":[]},"blockedAliases":{}}},"ownedResources":{}}`)
	if err := os.WriteFile(filepath.Join(home, "byok-providers.json"), store, 0o600); err != nil {
		t.Fatal(err)
	}
	current = n.integrationForState(current)
	receipt = settledNativeImportReceipt(current, receipt)
	if current.RouteCleanupPending || receipt.RouteCleanupPending || receipt.RequiresRestart || receipt.RestartDeferred {
		t.Fatalf("settled direct state kept stale pending evidence: %+v %+v", current, receipt)
	}
}
