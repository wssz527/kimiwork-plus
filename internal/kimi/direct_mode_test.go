package kimi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// ---------------------------------------------------------------------------
// fixtures
//
// The direct store below uses the 3.2.28 field names and the shape a custom
// source takes after `byokSources.save()` applied it.

const directSourceID = "byok-src-1"

func directStoreFixture(key string) map[string]any {
	return map[string]any{
		"v": 3,
		"accounts": map[string]any{
			key: map[string]any{
				"revision": "account-rev-1",
				"sources": []any{map[string]any{
					"id": directSourceID, "kind": "custom", "displayName": DirectDisplayName,
					"revision": "source-rev-1", "discoveryMode": "manual-models",
				}},
				"active": map[string]any{
					"rev": "active-rev-1",
					"providers": []any{map[string]any{
						"id": "prov-1", "sourceId": directSourceID, "protocol": "openai",
						"baseUrl": "https://api.example.com/v1", "credentialRef": "byok-cred-1",
						"credentialVersion": "cv-1", "publicHeaders": map[string]any{},
					}},
					"models": []any{map[string]any{
						"alias": "byok-uuid/model-a", "providerId": "prov-1",
						"model": "model-a", "family": "gpt", "maxContextSize": 128000,
						"maxOutputSize": 8192, "capabilities": []any{"tool_use"}, "sourceId": directSourceID,
					}},
				},
				"blockedAliases": map[string]any{},
			},
		},
		"ownedResources": map[string]any{
			"providerIds": []any{"prov-1"}, "modelAliases": []any{"byok-uuid/model-a"},
			"credentialRefs": []any{"byok-cred-1"},
		},
	}
}

func directConfigFixture() map[string]any {
	return map[string]any{
		"model": map[string]any{
			"providers": map[string]any{
				"prov-1": map[string]any{"type": "openai", "baseUrl": "https://api.example.com/v1",
					"credential": "byok-cred-1"},
			},
			"models": map[string]any{
				"byok-uuid/model-a": map[string]any{"provider": "prov-1", "model": "model-a",
					"maxContextSize": float64(128000), "maxOutputSize": float64(8192),
					"capabilities": []any{"tool_use"}},
			},
		},
		"credentials": map[string]any{"byok-cred-1": map[string]any{"apiKey": "sk-direct-test-key"}},
	}
}

func directSpecFixture() *DirectSpec {
	return &DirectSpec{
		Providers: []DirectProviderSpec{{ID: "prov-1", DisplayName: "Example",
			Protocol: "openai", BaseURL: "https://api.example.com/v1"}},
		Models: []DirectModel{{ID: "kimi-a", ProviderID: "prov-1", Model: "model-a",
			DisplayName: "Model A", MaxContextSize: 128000, MaxOutputSize: 8192, Capabilities: []string{"tool_use"}}},
	}
}

func seedDirectFixture(t *testing.T, key string) *nativeFixture {
	t.Helper()
	fixture := newNativeFixture(t)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), directStoreFixture(key))
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
	return fixture
}

func writeIdentity(t *testing.T, fixture *nativeFixture, userID string) {
	t.Helper()
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	config["credentials"].(map[string]any)["kimiWeb"] = map[string]any{"userId": userID}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)
}

// ---------------------------------------------------------------------------
// the page expression

const directExpressionMock = `
const calls=[];
let gatewayConnected=true;
const gatewayRegistry=globalThis.gatewayRegistry||"http://127.0.0.1:9100/v1/models/api.json";
const gatewayBase=gatewayRegistry.replace(/\/models\/api\.json$/,"");
const kth={id:"byok-kth",kind:"kth",revision:"k1",displayName:"KimiWork +",
  registryUrl:gatewayRegistry,
  providers:(globalThis.extraGatewayProviders||[]).concat([{id:"byok-kth-resp",protocol:"openai_responses",baseUrl:gatewayBase}])};
if(Array.isArray(globalThis.gatewayModels))kth.models=globalThis.gatewayModels;
if(globalThis.gatewayQuarantined)kth.quarantined=true;
if(globalThis.gatewayStatus)kth.status=globalThis.gatewayStatus;
const custom={id:"byok-src-1",kind:"custom",displayName:"KimiWork + 直连",revision:"s1",
  providers:[{id:"byok-prov-existing",protocol:"openai",baseUrl:"https://api.example.com/v1"}],
  models:[{providerId:"byok-prov-existing",model:"model-a",alias:"byok-prov-existing/model-a",status:"available"}]};
const ok=value=>({ok:true,value});
let saved=false;
let saveCount=0;
globalThis.seenDraft=null;
globalThis.seenPreviewRequest=null;
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");
    const providerId=globalThis.rejectClearedEffort&&saved?"byok-prov-fresh":"byok-prov-existing";
    custom.providers[0].id=providerId;
    custom.models=[{providerId,model:"model-a",alias:providerId+"/model-a",status:"available"}];
    if(globalThis.pendingCatalog&&saveCount===1)custom.models.push({providerId,model:"catalog-extra",alias:providerId+"/catalog-extra",status:"pending",provenance:"catalog"});
    return ok({scopeToken:"account-1",accountRevision:saved?"r2":"r1",applyState:globalThis.snapshotApplyState||"applied",
    sources:(gatewayConnected?[kth]:[]).concat([custom])});},
  preview(request){
    calls.push("preview");
    globalThis.seenDraft=request.draft;
    globalThis.seenPreviewRequest=request;
    if(globalThis.rejectClearedEffort&&request.draft.providers[0].id==="byok-prov-existing"&&
       !request.draft.models[0].defaultEffort)return {ok:false,reasonCode:"byok-invalid-source"};
    return ok({scopeToken:"account-1",accountRevision:saved?"r2":"r1",sourceRevision:"s1",sourceId:"byok-src-1",
      previewId:"p1",canSave:true,candidates:[]});
  },
  discard(id){calls.push("discard:"+id);},
  save(request){calls.push("save");if(globalThis.failSave)return {ok:false,reasonCode:"save-refused"};saveCount++;saved=true;
    return ok({sourceId:"byok-src-1",accountRevision:"r2",status:globalThis.pendingCatalog&&saveCount>1?"applied":"pending"});},
  applyPending(revision){calls.push("apply:"+revision);return ok({status:"applied"});},
  disconnect(request){calls.push("disconnect:"+request.sourceId);if(globalThis.failDisconnect||(globalThis.pendingCatalog&&saveCount===1))return {ok:false,reasonCode:"disconnect-refused"};gatewayConnected=false;return ok({});},
}};
`

func runDirectExpression(t *testing.T, request DirectImportRequest, setup string) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	if _, err := vm.RunString(setup + directExpressionMock); err != nil {
		t.Fatal(err)
	}
	expression, err := directImportExpression(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`(` + expression + `).then(result=>globalThis.directResult=result);`); err != nil {
		t.Fatal(err)
	}
	return vm
}

func validDirectImportRequest() DirectImportRequest {
	return DirectImportRequest{
		DisplayName:      DirectDisplayName,
		ExistingSourceID: directSourceID,
		Providers: []DirectProvider{{ID: "prov-1", DisplayName: "Example", Protocol: "openai",
			BaseURL: "https://api.example.com/v1", APIKey: "sk-direct-test-key"}},
		Models: []DirectModel{{ID: "kimi-a", ProviderID: "prov-1", Model: "model-a",
			DisplayName: "Model A", MaxContextSize: 128000, Capabilities: []string{"tool_use"}}},
	}
}

func TestDirectImportRetriesClearedEffortWithFreshProvider(t *testing.T) {
	vm := runDirectExpression(t, validDirectImportRequest(), `globalThis.rejectClearedEffort=true;`)
	result := vm.Get("directResult").ToObject(vm)
	if !result.Get("ok").ToBoolean() || result.Get("sourceId").String() != directSourceID {
		t.Fatalf("direct result: %v", result)
	}
	draft := vm.Get("seenDraft").ToObject(vm)
	provider := draft.Get("providers").ToObject(vm).Get("0").ToObject(vm)
	model := draft.Get("models").ToObject(vm).Get("0").ToObject(vm)
	if provider.Get("id").String() != "prov-1" || model.Get("providerId").String() != "prov-1" {
		t.Fatalf("retry draft: %v", draft)
	}
	calls, err := vm.RunString(`calls.join(",")`)
	if err != nil || calls.String() != "list,preview,preview,save,list,apply:r2" {
		t.Fatalf("want one save after validated retry, got %v (%v)", calls, err)
	}
}

func TestDirectImportCleansGatewayAfterOfficiallyAssignedProviderID(t *testing.T) {
	for _, tc := range []struct {
		name, extra, calls string
	}{
		{"immediate", "", "list,preview,preview,save,list,apply:r2,list,disconnect:byok-kth"},
		{"catalog refresh", `globalThis.pendingCatalog=true;`, "list,preview,preview,save,list,apply:r2,list,disconnect:byok-kth,list,preview,save,list,disconnect:byok-kth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := validDirectImportRequest()
			request.DisconnectGateway = true
			request.DisconnectGatewayURL = "http://127.0.0.1:9100/v1"
			vm := runDirectExpression(t, request, `globalThis.rejectClearedEffort=true;`+tc.extra)
			result := vm.Get("directResult").ToObject(vm)
			if !result.Get("ok").ToBoolean() || !result.Get("gatewayDisconnected").ToBoolean() || result.Get("routeCleanupPending").ToBoolean() {
				t.Fatalf("unexpected direct result: %v", result.Export())
			}
			calls, err := vm.RunString(`calls.join(",")`)
			if err != nil || calls.String() != tc.calls {
				t.Fatalf("unexpected calls: %v (%v)", calls, err)
			}
			if tc.extra != "" {
				draft := vm.Get("seenDraft").ToObject(vm)
				provider := draft.Get("providers").ToObject(vm).Get("0").ToObject(vm)
				if provider.Get("id").String() != "byok-prov-fresh" {
					t.Fatalf("re-save must reuse official provider ID: %v", provider.Export())
				}
			}
		})
	}
}

// The manual-models draft is the exact contract with Kimi 3.2.28: a custom
// source, no discovery fields, an explicit "clear headers", and an empty
// candidates list that must still be saveable.
func TestDirectImportExpressionBuildsTheManualModelsDraft(t *testing.T) {
	vm := runDirectExpression(t, validDirectImportRequest(), "")
	result := vm.Get("directResult").ToObject(vm)
	if !result.Get("ok").ToBoolean() || result.Get("sourceId").String() != directSourceID ||
		result.Get("models").ToInteger() != 1 {
		t.Fatalf("direct result: %v", result)
	}
	if result.Get("requiresRestart").ToBoolean() {
		t.Fatal("an applied save was reported as needing a restart")
	}
	draft := vm.Get("seenDraft").ToObject(vm)
	if draft.Get("kind").String() != "custom" || draft.Get("displayName").String() != DirectDisplayName {
		t.Fatalf("draft head: %v", draft)
	}
	providers := draft.Get("providers").ToObject(vm)
	provider := providers.Get("0").ToObject(vm)
	// The existing provider is reused by (protocol, baseUrl): this is what keeps
	// Kimi's provider ids and aliases stable across syncs.
	if provider.Get("id").String() != "byok-prov-existing" {
		t.Fatalf("provider id = %q, want the existing id", provider.Get("id"))
	}
	if provider.Get("headers").ToObject(vm).Get("mode").String() != "clear" {
		t.Fatal("headers were not cleared")
	}
	if provider.Get("key").ToObject(vm).Get("mode").String() != "set" {
		t.Fatal("key was not set")
	}
	for _, key := range provider.Keys() {
		if key == "apiVariant" {
			t.Fatal("apiVariant was sent for a non google-genai provider")
		}
	}
	models := draft.Get("models").ToObject(vm)
	model := models.Get("0").ToObject(vm)
	declaredContextSize := model.Get("declaredContextSize")
	if model.Get("providerId").String() != "byok-prov-existing" ||
		model.Get("provenance").String() != "manual" ||
		declaredContextSize == nil || declaredContextSize.ToInteger() != 128000 {
		t.Fatalf("model row: %v", model)
	}
	for _, key := range model.Keys() {
		if key == "maxOutputSize" {
			t.Fatal("an unset maxOutputSize was sent")
		}
	}
	encoded, err := vm.RunString(`JSON.stringify(globalThis.seenDraft)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"selectedCandidates", "routeKeys", "reuseRegistryCredential", "discover"} {
		if strings.Contains(encoded.String(), forbidden) {
			t.Fatalf("draft carried %q", forbidden)
		}
	}
	// The preview request is the snapshot's own revisions, never a guessed one.
	previewRequest := vm.Get("seenPreviewRequest").ToObject(vm)
	if previewRequest.Get("accountRevision").String() != "r1" ||
		previewRequest.Get("sourceRevision").String() != "s1" ||
		previewRequest.Get("sourceId").String() != directSourceID {
		t.Fatalf("preview request: %v", previewRequest)
	}
	calls, err := vm.RunString(`calls.join(",")`)
	if err != nil || calls.String() != "list,preview,save,list,apply:r2" {
		t.Fatalf("calls = %v, err=%v", calls, err)
	}
}

func TestDirectImportExpressionRejectsABuildWithoutByokSources(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(`globalThis.clawBridgeAPI={};`); err != nil {
		t.Fatal(err)
	}
	expression, err := directImportExpression(validDirectImportRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`(` + expression + `).then(result=>globalThis.directResult=result);`); err != nil {
		t.Fatal(err)
	}
	result := vm.Get("directResult").ToObject(vm)
	if result.Get("ok").ToBoolean() || result.Get("code").String() != "direct-mode-unsupported" {
		t.Fatalf("unsupported build: ok=%v code=%v", result.Get("ok"), result.Get("code"))
	}
}

func TestDirectImportExpressionDisconnectsOnlyOurLoopbackGateway(t *testing.T) {
	request := validDirectImportRequest()
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:9100/v1"

	t.Run("loopback gateway is disconnected and the snapshot re-read", func(t *testing.T) {
		vm := runDirectExpression(t, request, "")
		result := vm.Get("directResult").ToObject(vm)
		if !result.Get("ok").ToBoolean() || !result.Get("gatewayDisconnected").ToBoolean() {
			t.Fatalf("direct result: %v", result)
		}
		calls, err := vm.RunString(`calls.join(",")`)
		if err != nil || calls.String() != "list,preview,save,list,apply:r2,list,disconnect:byok-kth" {
			t.Fatalf("calls = %v, err=%v", calls, err)
		}
	})

	t.Run("older loopback providers do not block cleanup", func(t *testing.T) {
		vm := runDirectExpression(t, request,
			`globalThis.extraGatewayProviders=[{id:"byok-kth-kimi",protocol:"kimi",baseUrl:"http://127.0.0.1:49958/v1"}];`)
		result := vm.Get("directResult").ToObject(vm)
		calls, err := vm.RunString(`calls.join(",")`)
		if !result.Get("ok").ToBoolean() || !result.Get("gatewayDisconnected").ToBoolean() ||
			err != nil || !strings.Contains(calls.String(), "disconnect") {
			t.Fatalf("stale loopback providers blocked cleanup: result=%v calls=%v err=%v", result, calls, err)
		}
	})

	t.Run("a remote provider in an owned registry is never disconnected", func(t *testing.T) {
		vm := runDirectExpression(t, request,
			`globalThis.extraGatewayProviders=[{id:"remote",protocol:"openai",baseUrl:"https://gateway.example/v1"}];`)
		result := vm.Get("directResult").ToObject(vm)
		calls, err := vm.RunString(`calls.join(",")`)
		if err != nil || strings.Contains(calls.String(), "disconnect") {
			t.Fatalf("a remote provider was disconnected: result=%v calls=%v", result, calls)
		}
	})

	t.Run("a remote gateway is never disconnected", func(t *testing.T) {
		vm := runDirectExpression(t, request,
			`globalThis.gatewayRegistry="https://gateway.example/v1/models/api.json";`)
		result := vm.Get("directResult").ToObject(vm)
		if !result.Get("ok").ToBoolean() || result.Get("gatewayDisconnected").ToBoolean() {
			t.Fatalf("direct result: %v", result)
		}
		calls, err := vm.RunString(`calls.join(",")`)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(calls.String(), "disconnect") {
			t.Fatalf("a remote gateway was disconnected: %v", calls)
		}
	})

	t.Run("a different loopback gateway is never disconnected", func(t *testing.T) {
		vm := runDirectExpression(t, request,
			`globalThis.gatewayRegistry="http://127.0.0.1:9200/v1/models/api.json";`)
		calls, err := vm.RunString(`calls.join(",")`)
		if err != nil || strings.Contains(calls.String(), "disconnect") {
			t.Fatalf("a different local gateway was disconnected: %v", calls)
		}
	})
}

func TestDirectImportExpressionChecksOtherSourcesForPendingModels(t *testing.T) {
	for _, tc := range []struct {
		name       string
		applyState string
		models     string
		wantApply  bool
	}{
		{name: "idle state with settled models", applyState: "idle", models: `[{status:"available"}]`, wantApply: true},
		{name: "prepared state with settled models", applyState: "prepared", models: `[{status:"available"}]`, wantApply: true},
		{name: "missing model evidence uses the pending state", applyState: "prepared"},
		{name: "another pending model blocks apply", applyState: "prepared", models: `[{status:"pending"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := `globalThis.snapshotApplyState="` + tc.applyState + `";`
			if tc.models != "" {
				setup += `globalThis.gatewayModels=` + tc.models + `;`
			}
			vm := runDirectExpression(t, validDirectImportRequest(), setup)
			result := vm.Get("directResult").ToObject(vm)
			if !result.Get("ok").ToBoolean() || result.Get("requiresRestart").ToBoolean() == tc.wantApply {
				t.Fatalf("apply = %v, result = %v", tc.wantApply, result)
			}
			calls, err := vm.RunString(`calls.join(",")`)
			want := "list,preview,save"
			if tc.wantApply {
				want += ",list,apply:r2"
			}
			if err != nil || calls.String() != want {
				t.Fatalf("calls = %v, want %s, err=%v", calls, want, err)
			}
		})
	}
}

func TestDirectImportExpressionIgnoresDisconnectedOrQuarantinedGateway(t *testing.T) {
	request := validDirectImportRequest()
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:9100/v1"
	for _, setup := range []string{`globalThis.gatewayQuarantined=true;`, `globalThis.gatewayStatus="disconnected";`} {
		vm := runDirectExpression(t, request, setup)
		result := vm.Get("directResult").ToObject(vm)
		if !result.Get("ok").ToBoolean() || result.Get("routeCleanupPending").ToBoolean() {
			t.Fatalf("inactive gateway required cleanup: %v", result)
		}
		calls, err := vm.RunString(`calls.join(",")`)
		if err != nil || strings.Contains(calls.String(), "disconnect") {
			t.Fatalf("inactive gateway was disconnected again: %v, err=%v", calls, err)
		}
	}
}

// ---------------------------------------------------------------------------
// request validation

func directValidationActions(t *testing.T) (*cdpActions, string) {
	t.Helper()
	home := t.TempDir()
	config, _ := json.Marshal(map[string]any{"credentials": map[string]any{
		"kimiWeb": map[string]any{"userId": "direct-account"}}})
	if err := os.WriteFile(filepath.Join(home, NativeWorkConfig), config, 0o600); err != nil {
		t.Fatal(err)
	}
	return &cdpActions{kimiHome: home, kimiDir: home}, AccountScope(home)
}

func TestImportDirectRefusesInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*DirectImportRequest)
	}{
		{"http on a remote host", func(r *DirectImportRequest) {
			r.Providers[0].BaseURL = "http://api.example.com/v1"
		}},
		{"unknown protocol", func(r *DirectImportRequest) { r.Providers[0].Protocol = "bedrock" }},
		{"empty key", func(r *DirectImportRequest) { r.Providers[0].APIKey = "" }},
		{"key with a line break", func(r *DirectImportRequest) { r.Providers[0].APIKey = "sk-a\nb" }},
		{"key with a carriage return", func(r *DirectImportRequest) { r.Providers[0].APIKey = "sk-a\rb" }},
		{"base url with userinfo", func(r *DirectImportRequest) {
			r.Providers[0].BaseURL = "https://user:key@api.example.com/v1"
		}},
		{"base url with a query", func(r *DirectImportRequest) {
			r.Providers[0].BaseURL = "https://api.example.com/v1?key=1"
		}},
		{"base url naming chat completions", func(r *DirectImportRequest) {
			r.Providers[0].BaseURL = "https://api.example.com/v1/chat/completions"
		}},
		{"duplicate provider ids", func(r *DirectImportRequest) {
			r.Providers = append(r.Providers, r.Providers[0])
		}},
		{"no providers", func(r *DirectImportRequest) { r.Providers = nil }},
		{"no models", func(r *DirectImportRequest) { r.Models = nil }},
		{"unknown provider reference", func(r *DirectImportRequest) { r.Models[0].ProviderID = "missing" }},
		{"model without tool use", func(r *DirectImportRequest) {
			r.Models[0].Capabilities = []string{"thinking"}
		}},
		{"unknown capability", func(r *DirectImportRequest) {
			r.Models[0].Capabilities = []string{"tool_use", "audio_in"}
		}},
		{"zero context size", func(r *DirectImportRequest) { r.Models[0].MaxContextSize = 0 }},
		{"negative output size", func(r *DirectImportRequest) { r.Models[0].MaxOutputSize = -1 }},
		{"default effort not supported", func(r *DirectImportRequest) {
			r.Models[0].SupportEfforts = []string{"low"}
			r.Models[0].DefaultEffort = "high"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actions, _ := directValidationActions(t)
			request := validDirectImportRequest()
			tc.mutate(&request)
			result, err := actions.ImportDirect(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result.State != ImportStateFailed || result.Reason != "native_bridge_invalid_request" {
				t.Fatalf("invalid request was accepted: %+v", result)
			}
		})
	}
}

func TestImportDirectRefusesAnUnverifiedOrWrongAccount(t *testing.T) {
	valid := validDirectImportRequest()

	t.Run("no profile identity", func(t *testing.T) {
		actions := &cdpActions{kimiHome: t.TempDir(), kimiDir: t.TempDir()}
		result, err := actions.ImportDirect(context.Background(), valid)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != ImportStateWaitingLogin {
			t.Fatalf("unsigned-in profile: %+v", result)
		}
	})

	t.Run("relative home", func(t *testing.T) {
		actions := &cdpActions{kimiHome: "relative", kimiDir: "relative"}
		result, err := actions.ImportDirect(context.Background(), valid)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != ImportStateWaitingLogin {
			t.Fatalf("relative home: %+v", result)
		}
	})

	t.Run("another account", func(t *testing.T) {
		actions, _ := directValidationActions(t)
		request := valid
		request.ExpectedAccountKey = "someone-else"
		result, err := actions.ImportDirect(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != ImportStateAccountChanged || result.Reason != NativeAccountChanged {
			t.Fatalf("wrong account: %+v", result)
		}
	})
}

// ---------------------------------------------------------------------------
// reconcile and readback

func TestDirectReconcileVerifiesTheManagedSource(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("direct-account")
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), directStoreFixture(key))
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
	before := fixture.rawConfig(t)
	spec := directSpecFixture()

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		Mode: IntegrationModeDirect, DirectSpec: spec, AccountScope: key})
	if result.State != IntegrationReady || result.Reason != "" {
		t.Fatalf("direct reconcile: %s/%s", result.State, result.Reason)
	}
	if !result.Applied || !result.Imported || result.Changed {
		t.Fatalf("direct result: %+v", result)
	}
	if result.Mode != IntegrationModeDirect || result.DirectSourceID != directSourceID {
		t.Fatalf("direct identity: %+v", result)
	}
	if result.Aliases["kimi-a"] != "byok-uuid/model-a" || result.CredentialRef != "byok-cred-1" {
		t.Fatalf("direct aliases: %+v", result)
	}
	if after := fixture.rawConfig(t); after != before {
		t.Fatal("direct reconcile rewrote the generated config")
	}

	readback := fixture.integration(t).ReadbackMode(key, IntegrationModeDirect, "")
	if readback.Mode != IntegrationModeDirect || readback.DirectSourceID != directSourceID ||
		readback.Models != 1 || readback.CredentialRef != "byok-cred-1" {
		t.Fatalf("direct readback: %+v", readback)
	}
	if len(readback.Aliases) != 1 || readback.Aliases[0] != "byok-uuid/model-a" {
		t.Fatalf("direct readback aliases: %v", readback.Aliases)
	}
	if !readback.HoldsDirect(spec) {
		t.Fatalf("direct readback does not hold: %+v", readback)
	}

	// The intent is the record every later sync trusts. It must name the source
	// and the aliases and never carry a key.
	intentBytes, err := os.ReadFile(filepath.Join(fixture.state, NativeWorkIntent))
	if err != nil {
		t.Fatal(err)
	}
	intent := string(intentBytes)
	for _, secret := range []string{"sk-direct-test-key", "apiKey"} {
		if strings.Contains(intent, secret) {
			t.Fatalf("intent carries %q", secret)
		}
	}
	if !strings.Contains(intent, `"mode": "direct"`) || !strings.Contains(intent, directSourceID) {
		t.Fatalf("intent does not record the direct source: %s", intent)
	}
}

func TestDirectDisconnectedSourceNeedsExplicitReimport(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("direct-account")
	store := directStoreFixture(key)
	account := store["accounts"].(map[string]any)[key].(map[string]any)
	account["sources"].([]any)[0].(map[string]any)["status"] = "disconnected"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		Mode: IntegrationModeDirect, DirectSpec: directSpecFixture(), AccountScope: key})
	if result.State != IntegrationNeedsImport || result.Reason != "profile_delisted" {
		t.Fatalf("disconnected direct source was revived: %+v", result)
	}
	readback := fixture.integration(t).ReadbackMode(key, IntegrationModeDirect, directSourceID)
	if readback.Active || readback.HoldsDirect(directSpecFixture()) {
		t.Fatalf("disconnected direct readback still holds: %+v", readback)
	}
}

func TestDirectReconcileReportsAMissingModel(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("direct-account")
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), directStoreFixture(key))
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
	spec := directSpecFixture()
	spec.Models[0].Model = "model-b"

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		Mode: IntegrationModeDirect, DirectSpec: spec, AccountScope: key})
	if result.State != IntegrationNeedsImport || result.Reason != "models_missing" {
		t.Fatalf("missing model: %s/%s", result.State, result.Reason)
	}
	if len(result.MissingModels) != 1 || result.MissingModels[0] != "kimi-a" {
		t.Fatalf("missing models: %v", result.MissingModels)
	}
}

func TestDirectReconcileReportsAChangedBaseURL(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("direct-account")
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), directStoreFixture(key))
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
	spec := directSpecFixture()
	spec.Providers[0].BaseURL = "https://api.other.example/v1"

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		Mode: IntegrationModeDirect, DirectSpec: spec, AccountScope: key})
	if result.State != IntegrationNeedsImport || result.Reason != "models_missing" {
		t.Fatalf("changed endpoint: %s/%s", result.State, result.Reason)
	}
}

func TestDirectReconcileWaitsForGeneratedConfig(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("direct-account")
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), directStoreFixture(key))

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		Mode: IntegrationModeDirect, DirectSpec: directSpecFixture(), AccountScope: key})
	if result.State != IntegrationWaitingConfig || result.Reason != "config_missing" {
		t.Fatalf("missing config: %s/%s", result.State, result.Reason)
	}
}

func TestDirectReconcileRequiresAV3Store(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("direct-account")

	t.Run("missing store", func(t *testing.T) {
		result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
			Mode: IntegrationModeDirect, DirectSpec: directSpecFixture(), AccountScope: key})
		if result.State != IntegrationNeedsImport || result.Reason != "profile_store_missing" {
			t.Fatalf("missing store: %s/%s", result.State, result.Reason)
		}
	})

	t.Run("legacy store", func(t *testing.T) {
		legacy := newNativeFixture(t)
		legacy.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
		result := legacy.integration(t).Reconcile(ReconcileOptions{Enabled: true,
			Mode: IntegrationModeDirect, DirectSpec: directSpecFixture(), AccountScope: key})
		if result.State != IntegrationConflict || result.Reason != "direct_mode_unsupported" {
			t.Fatalf("legacy store: %s/%s", result.State, result.Reason)
		}
	})
}

func TestDirectReconcileNormalizesAnthropicV1(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("direct-account")
	store := directStoreFixture(key)
	account := store["accounts"].(map[string]any)[key].(map[string]any)
	provider := account["active"].(map[string]any)["providers"].([]any)[0].(map[string]any)
	provider["protocol"] = "anthropic"
	provider["baseUrl"] = "https://api.anthropic.com"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	config := directConfigFixture()
	declared := config["model"].(map[string]any)["providers"].(map[string]any)["prov-1"].(map[string]any)
	declared["type"] = "anthropic"
	declared["baseUrl"] = "https://api.anthropic.com"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)
	spec := directSpecFixture()
	spec.Providers[0].Protocol = "anthropic"
	spec.Providers[0].BaseURL = "https://api.anthropic.com/v1"

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		Mode: IntegrationModeDirect, DirectSpec: spec, AccountScope: key})
	if result.State != IntegrationReady {
		t.Fatalf("anthropic /v1: %s/%s", result.State, result.Reason)
	}
}

func TestDirectReconcileHonoursBlockedAliases(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("direct-account")
	store := directStoreFixture(key)
	account := store["accounts"].(map[string]any)[key].(map[string]any)
	account["blockedAliases"] = map[string]any{"byok-uuid/model-a": "delisted"}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		Mode: IntegrationModeDirect, DirectSpec: directSpecFixture(), AccountScope: key})
	if result.State != IntegrationNeedsImport || result.Reason != "models_missing" {
		t.Fatalf("blocked alias: %s/%s", result.State, result.Reason)
	}
}

func TestDirectCatalogReadsTheCustomSource(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("direct-account")
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), directStoreFixture(key))
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
	if result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		Mode: IntegrationModeDirect, DirectSpec: directSpecFixture(), AccountScope: key}); result.State != IntegrationReady {
		t.Fatalf("seed reconcile: %s/%s", result.State, result.Reason)
	}
	settings := ControllerSettings{IntegrationMode: IntegrationModeDirect, DirectSpec: directSpecFixture()}
	entries, reason, needsImport := fixture.integration(t).accountCatalog(settings, key)
	if reason != "" || needsImport || len(entries) != 1 {
		t.Fatalf("direct catalog: reason=%q needsImport=%v entries=%d", reason, needsImport, len(entries))
	}
	if entries[0].alias != "byok-uuid/model-a" || entries[0].provider["type"] != "openai" ||
		entries[0].provider["baseUrl"] != "https://api.example.com/v1" {
		t.Fatalf("direct catalog entry: %+v", entries[0])
	}
}

// ---------------------------------------------------------------------------
// controller

func TestControllerImportDirectAuthorizedGradesWithReadback(t *testing.T) {
	h := newAccountHarness(t)
	key := sha256Hex("direct-account")
	fixture := &nativeFixture{dir: h.home, state: h.stateDir}
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkStore), directStoreFixture(key))
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkConfig), directConfigFixture())
	writeIdentity(t, fixture, "direct-account")
	// The previous mode was the gateway: its intent must not be mistaken for a
	// direct source, and the switch still has to produce a direct intent.
	fixture.writeJSON(t, filepath.Join(h.stateDir, NativeWorkIntent), map[string]any{
		"schemaVersion": NativeWorkSchema, "sourceID": "gateway", "ownerID": "owner",
		"kimiHome": h.home, "url": "http://127.0.0.1:9999/v1",
		"accountDigest": sha256Hex(key), "active": true, "generation": 2,
	})
	h.controller.settings.IntegrationMode = IntegrationModeDirect
	h.controller.settings.DirectSpec = directSpecFixture()
	h.controller.settings.GatewayURL = "http://127.0.0.1:9999/v1"

	actions := &fakeActions{connected: true, direct: ImportResult{OK: true, SourceID: directSourceID, Models: 1}}
	host, err := StartBridgeHost(h.home, 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	h.controller.mu.Lock()
	h.controller.bridge = host
	h.controller.mu.Unlock()

	switchRequest := validDirectImportRequest()
	// No source is named here: the controller must not read one out of a gateway
	// intent, and the page creates the first direct source.
	switchRequest.ExistingSourceID = ""
	switchRequest.DisconnectGateway = true
	result, err := h.controller.ImportDirectAuthorized(context.Background(), switchRequest)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateApplied || !result.Applied {
		t.Fatalf("direct import was not applied: %+v", result)
	}
	actions.mu.Lock()
	defer actions.mu.Unlock()
	if len(actions.directs) != 1 {
		t.Fatalf("direct imports forwarded: %d", len(actions.directs))
	}
	forwarded := actions.directs[0]
	if forwarded.ExpectedAccountKey != key || forwarded.DisplayName != DirectDisplayName {
		t.Fatalf("direct request: %+v", forwarded)
	}
	if forwarded.Providers[0].APIKey != "sk-direct-test-key" {
		t.Fatal("the provider key did not reach the page")
	}
	if forwarded.ExistingSourceID != "" {
		t.Fatalf("a gateway intent was reused as a direct source: %q", forwarded.ExistingSourceID)
	}
	if forwarded.DisconnectGatewayURL != "http://127.0.0.1:9999/v1" {
		t.Fatalf("cleanup did not bind the configured gateway URL: %q", forwarded.DisconnectGatewayURL)
	}
	intent, err := os.ReadFile(filepath.Join(h.stateDir, NativeWorkIntent))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(intent), `"mode": "direct"`) ||
		!strings.Contains(string(intent), `"directSourceId": "`+directSourceID+`"`) {
		t.Fatalf("gateway -> direct switch did not record a direct intent: %s", intent)
	}
}

// A Direct Mode sync that keeps the same model set only changes the credential,
// so its intent generation also stays put. A deferred answer there must withdraw
// adoption exactly as the gateway path does.
func TestDeferredDirectImportWithdrawsAdoption(t *testing.T) {
	h := newAccountHarness(t)
	key := sha256Hex("direct-account")
	fixture := &nativeFixture{dir: h.home, state: h.stateDir}
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkStore), directStoreFixture(key))
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkConfig), directConfigFixture())
	writeIdentity(t, fixture, "direct-account")
	h.controller.settings.IntegrationMode = IntegrationModeDirect
	h.controller.settings.DirectSpec = directSpecFixture()
	h.controller.settings.AccountScope = key
	h.controller.settings.GatewayURL = "http://127.0.0.1:9999/v1"

	actions := &fakeActions{connected: true,
		direct: ImportResult{OK: true, SourceID: directSourceID, Models: 1}}
	host, err := StartBridgeHost(h.home, 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	child, _ := startFakeChild(t)
	t.Cleanup(func() { _ = child.Stop(3 * time.Second) })

	seeded := h.controller.ReconcileWithReadback(context.Background())
	if !seeded.Applied {
		t.Fatalf("direct fixture was not applied: %+v", seeded)
	}
	generation := h.controller.currentGeneration()
	h.controller.mu.Lock()
	h.controller.child = child
	h.controller.bridge = host
	h.controller.loadedGeneration = generation
	h.controller.loadedKnown = true
	h.controller.mu.Unlock()
	if adopted := h.controller.ReconcileWithReadback(context.Background()); !adopted.Applied || !adopted.Adopted {
		t.Fatalf("direct fixture did not start out adopted: %+v", adopted)
	}

	actions.direct = ImportResult{OK: true, SourceID: directSourceID, Models: 1,
		RequiresRestart: true, RestartDeferred: true}
	result, err := h.controller.ImportDirectAuthorized(context.Background(), validDirectImportRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateRequiresRestart || result.Adopted {
		t.Fatalf("a deferred direct import kept adoption: %+v", result)
	}
	status := h.controller.Status(context.Background())
	if status.Integration == nil || status.Integration.Adopted {
		t.Fatalf("status integration: %+v", status.Integration)
	}
}

func TestControllerGatewayImportDisconnectsTheDirectSource(t *testing.T) {
	h := newAccountHarness(t)
	fixture := &nativeFixture{dir: h.home, state: h.stateDir}
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkConfig), map[string]any{
		"model":       map[string]any{"providers": map[string]any{}, "models": map[string]any{}},
		"credentials": map[string]any{"cred-1": map[string]any{"apiKey": "gateway-token"}},
	})
	writeIdentity(t, fixture, "gateway-account")
	fixture.writeJSON(t, filepath.Join(h.stateDir, NativeWorkIntent), map[string]any{
		"schemaVersion": NativeWorkSchema, "sourceID": "gateway", "ownerID": "owner",
		"kimiHome": h.home, "mode": IntegrationModeDirect, "directSourceId": "byok-src-9",
		"accountDigest": sha256Hex(sha256Hex("gateway-account")), "active": true, "generation": 3,
	})
	h.controller.settings.GatewayURL = "http://127.0.0.1:9999/v1"
	h.controller.settings.GatewayToken = "gateway-token"
	h.controller.settings.Models = testCatalog()

	actions := &fakeActions{connected: true, imported: ImportResult{OK: false, Reason: "stop_after_import"}}
	host, err := StartBridgeHost(h.home, 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	h.controller.mu.Lock()
	h.controller.bridge = host
	h.controller.mu.Unlock()

	if _, err := h.controller.ImportModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	actions.mu.Lock()
	defer actions.mu.Unlock()
	if len(actions.imports) != 1 {
		t.Fatalf("gateway imports: %d", len(actions.imports))
	}
	if actions.imports[0].DisconnectDirectSourceID != "byok-src-9" {
		t.Fatalf("direct source not disconnected: %+v", actions.imports[0])
	}
}

func TestControllerGatewayImportRecoversDirectSourceAfterIntentReplacement(t *testing.T) {
	h := newAccountHarness(t)
	key := sha256Hex("gateway-account")
	fixture := &nativeFixture{dir: h.home, state: h.stateDir}
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkStore), directStoreFixture(key))
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkConfig), directConfigFixture())
	writeIdentity(t, fixture, "gateway-account")
	fixture.writeJSON(t, filepath.Join(h.stateDir, NativeWorkIntent), map[string]any{
		"schemaVersion": NativeWorkSchema, "sourceID": "gateway", "ownerID": "owner",
		"kimiHome": h.home, "url": "http://127.0.0.1:9999/v1",
		"accountDigest": sha256Hex(key), "active": true, "generation": 4,
	})
	h.controller.settings.GatewayURL = "http://127.0.0.1:9999/v1"
	h.controller.settings.GatewayToken = "gateway-token"
	h.controller.settings.Models = testCatalog()

	actions := &fakeActions{connected: true, imported: ImportResult{OK: false, Reason: "stop_after_import"}}
	host, err := StartBridgeHost(h.home, 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	h.controller.mu.Lock()
	h.controller.bridge = host
	h.controller.mu.Unlock()

	if got := h.controller.previousDirectSourceID(h.controller.settings); got != directSourceID {
		t.Fatalf("remaining direct source = %q", got)
	}
	if _, err := h.controller.ImportModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	actions.mu.Lock()
	defer actions.mu.Unlock()
	if len(actions.imports) != 1 || actions.imports[0].DisconnectDirectSourceID != directSourceID {
		t.Fatalf("retry did not name the remaining direct source: %+v", actions.imports)
	}
}

func TestGatewayReconcileCarriesDirectCleanupFromAccountSnapshot(t *testing.T) {
	fixture := newNativeFixture(t)
	const scope = "account"
	const gateway = "http://127.0.0.1:9999/v1"
	store := directStoreFixture(scope)
	account := store["accounts"].(map[string]any)[scope].(map[string]any)
	account["sources"] = append(account["sources"].([]any), map[string]any{
		"id": "byok-kth", "kind": "kth", "registryUrl": gateway + "/models/api.json",
	})
	active := account["active"].(map[string]any)
	active["providers"] = append(active["providers"].([]any), map[string]any{
		"id": "byok-kth-resp", "sourceId": "byok-kth", "protocol": "openai_responses",
		"baseUrl": gateway, "credentialRef": "cred-gw",
	})
	active["models"] = append(active["models"].([]any), map[string]any{
		"alias": "byok-kth-resp/gpt-probe-model", "providerId": "byok-kth-resp",
		"model": "gpt-probe-model", "family": "responses", "maxContextSize": 262144, "sourceId": "byok-kth",
	})
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	config := directConfigFixture()
	model := config["model"].(map[string]any)
	model["providers"].(map[string]any)["byok-kth-resp"] = map[string]any{
		"type": "openai_responses", "baseUrl": gateway, "credential": "cred-gw",
	}
	model["models"].(map[string]any)["byok-kth-resp/gpt-probe-model"] = map[string]any{
		"provider": "byok-kth-resp", "model": "gpt-probe-model", "maxContextSize": 262144,
	}
	config["credentials"].(map[string]any)["cred-gw"] = map[string]any{"apiKey": "synthetic-not-a-real-key"}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)
	options := ReconcileOptions{Enabled: true, AccountScope: scope, URL: gateway,
		Models: []Model{{ID: "gpt-probe-model", Protocol: "responses", ContextWindow: 262144}}}
	result := fixture.integration(t).Reconcile(options)
	if result.State != IntegrationReady || !result.RouteCleanupPending {
		t.Fatalf("remaining direct source was not carried by reconcile: %+v", result)
	}
	account["sources"].([]any)[0].(map[string]any)["status"] = "disconnected"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	result = fixture.integration(t).Reconcile(options)
	if result.State != IntegrationReady || result.RouteCleanupPending {
		t.Fatalf("disconnected direct source still required cleanup: %+v", result)
	}
}

func TestControllerRepairIntegrationIsANoOpInDirectMode(t *testing.T) {
	h := newAccountHarness(t)
	h.controller.settings.IntegrationMode = IntegrationModeDirect
	h.controller.settings.DirectSpec = directSpecFixture()
	key := sha256Hex("direct-account")
	fixture := &nativeFixture{dir: h.home, state: h.stateDir}
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkStore), directStoreFixture(key))
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkConfig), directConfigFixture())
	writeIdentity(t, fixture, "direct-account")

	actions := &fakeActions{connected: true, imported: ImportResult{OK: true}}
	host, err := StartBridgeHost(h.home, 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	h.controller.mu.Lock()
	h.controller.bridge = host
	h.controller.mu.Unlock()

	observed := IntegrationResult{State: IntegrationNeedsImport, Reason: "base_url_changed",
		Readback: &ImportReadback{Active: true, AccountVerified: true,
			AccountDigest: key, URL: "http://127.0.0.1:9998/v1"}}
	h.controller.RepairIntegration(context.Background(), observed)
	actions.mu.Lock()
	defer actions.mu.Unlock()
	if len(actions.imports) != 0 {
		t.Fatalf("a direct-mode controller auto-repaired: %d imports", len(actions.imports))
	}
}

func TestControllerImportModelsRefusesInDirectMode(t *testing.T) {
	h := newAccountHarness(t)
	h.controller.settings.IntegrationMode = IntegrationModeDirect
	fixture := &nativeFixture{dir: h.home, state: h.stateDir}
	fixture.writeJSON(t, filepath.Join(h.home, NativeWorkConfig), directConfigFixture())
	writeIdentity(t, fixture, "direct-account")
	actions := &fakeActions{connected: true}
	host, err := StartBridgeHost(h.home, 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	h.controller.mu.Lock()
	h.controller.bridge = host
	h.controller.mu.Unlock()

	result, err := h.controller.ImportModels(context.Background())
	if err == nil || result.Reason != "native_import_direct_mode" {
		t.Fatalf("gateway import in direct mode: %+v %v", result, err)
	}
	if len(actions.imports) != 0 {
		t.Fatal("a gateway import reached the page in direct mode")
	}
}

func TestDirectImportFailurePreservesTheOldGateway(t *testing.T) {
	request := validDirectImportRequest()
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:9100/v1"

	vm := runDirectExpression(t, request, `globalThis.failSave=true;`)
	result := vm.Get("directResult").ToObject(vm)
	calls, _ := vm.RunString(`calls.join(",")`)
	if result.Get("ok").ToBoolean() || calls.String() != "list,preview,save" || !gatewayConnected(vm) {
		t.Fatalf("failed save touched the old gateway: result=%v calls=%v", result, calls)
	}

	vm = runDirectExpression(t, request, `globalThis.failDisconnect=true;`)
	result = vm.Get("directResult").ToObject(vm)
	calls, _ = vm.RunString(`calls.join(",")`)
	if !result.Get("ok").ToBoolean() || !result.Get("routeCleanupPending").ToBoolean() ||
		calls.String() != "list,preview,save,list,apply:r2,list,disconnect:byok-kth,list" || !gatewayConnected(vm) {
		t.Fatalf("failed cleanup was not retryable: result=%v calls=%v", result, calls)
	}
}

func TestDirectImportExpressionSettlesCatalogRefreshBeforeCleanup(t *testing.T) {
	request := validDirectImportRequest()
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:9100/v1"
	vm := runDirectExpression(t, request, `globalThis.pendingCatalog=true;`)
	result := vm.Get("directResult").ToObject(vm)
	calls, _ := vm.RunString(`calls.join(",")`)
	want := "list,preview,save,list,apply:r2,list,disconnect:byok-kth,list,preview,save,list,disconnect:byok-kth"
	if !result.Get("ok").ToBoolean() || !result.Get("gatewayDisconnected").ToBoolean() ||
		result.Get("routeCleanupPending").ToBoolean() || result.Get("requiresRestart").ToBoolean() ||
		calls.String() != want || gatewayConnected(vm) {
		t.Fatalf("catalog refresh blocked cleanup: result=%v calls=%v", result, calls)
	}
}

func TestDirectReconcileIgnoresOnlyUnrequestedCatalogPending(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provenance string
		wantState  string
	}{
		{"catalog discovery", "catalog", IntegrationReady},
		{"requested manual model", "manual", IntegrationPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newNativeFixture(t)
			key := sha256Hex("direct-account")
			store := directStoreFixture(key)
			account := store["accounts"].(map[string]any)[key].(map[string]any)
			account["pending"] = map[string]any{"applyState": "prepared", "models": []any{map[string]any{
				"alias": "byok-uuid/catalog-extra", "sourceId": directSourceID,
				"providerId": "prov-1", "model": "catalog-extra", "provenance": tc.provenance,
			}}}
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
			spec := directSpecFixture()
			if tc.provenance == "manual" {
				spec.Models = append(spec.Models, DirectModel{ID: "kimi-extra", ProviderID: "prov-1",
					Model: "catalog-extra", MaxContextSize: 128000, Capabilities: []string{"tool_use"}})
			}
			result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
				Mode: IntegrationModeDirect, DirectSpec: spec, AccountScope: key})
			if string(result.State) != tc.wantState {
				t.Fatalf("pending %s: %s/%s", tc.provenance, result.State, result.Reason)
			}
		})
	}
}

func gatewayConnected(vm *goja.Runtime) bool {
	connected, err := vm.RunString(`gatewayConnected`)
	return err == nil && connected.ToBoolean()
}

func TestDirectReconcileRejectsForeignAndUntaggedRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(provider, model map[string]any)
	}{
		{"missing source ids", func(provider, model map[string]any) {
			delete(provider, "sourceId")
			delete(model, "sourceId")
		}},
		{"another source on the same endpoint", func(provider, model map[string]any) {
			provider["sourceId"] = "other-source"
			model["sourceId"] = "other-source"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := directStoreFixture("account")
			account := store["accounts"].(map[string]any)["account"].(map[string]any)
			active := account["active"].(map[string]any)
			tc.edit(active["providers"].([]any)[0].(map[string]any), active["models"].([]any)[0].(map[string]any))
			_, rows := directActiveRows(account, directSourceID)
			if len(rows) != 0 {
				t.Fatalf("untrusted rows were admitted: %+v", rows)
			}
		})
	}
}

func TestDirectReconcileChecksAllDeclaredModelTerms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*DirectModel)
	}{
		{"context", func(model *DirectModel) { model.MaxContextSize++ }},
		{"output", func(model *DirectModel) { model.MaxOutputSize = 4096 }},
		{"capabilities", func(model *DirectModel) { model.Capabilities = append(model.Capabilities, "thinking") }},
		{"efforts", func(model *DirectModel) {
			model.SupportEfforts = []string{"low", "high"}
			model.DefaultEffort = "high"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newNativeFixture(t)
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), directStoreFixture("account"))
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
			spec := directSpecFixture()
			spec.Models[0].MaxOutputSize = 8192
			tc.mutate(&spec.Models[0])
			result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
				Mode: IntegrationModeDirect, DirectSpec: spec})
			if result.State != IntegrationNeedsImport || result.Reason != "models_missing" {
				t.Fatalf("changed model declaration passed: %s/%s", result.State, result.Reason)
			}
		})
	}
}

func TestDirectReconcileChecksGeneratedModelTerms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"context", func(model map[string]any) { model["maxContextSize"] = 64000 }},
		{"output", func(model map[string]any) { model["maxOutputSize"] = 4096 }},
		{"capabilities", func(model map[string]any) { model["capabilities"] = []any{"tool_use", "thinking"} }},
		{"efforts", func(model map[string]any) {
			model["supportEfforts"] = []any{"low"}
			model["defaultEffort"] = "low"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newNativeFixture(t)
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), directStoreFixture("account"))
			config := directConfigFixture()
			model := config["model"].(map[string]any)["models"].(map[string]any)["byok-uuid/model-a"].(map[string]any)
			tc.mutate(model)
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)
			spec := directSpecFixture()
			spec.Models[0].MaxOutputSize = 8192
			result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
				Mode: IntegrationModeDirect, DirectSpec: spec})
			if result.State != IntegrationConflict || result.Reason != "foreign_model" {
				t.Fatalf("stale generated declaration passed: %s/%s", result.State, result.Reason)
			}
		})
	}
}

func TestDirectFailureReasonsUsePublicUnderscoredCodes(t *testing.T) {
	for input, want := range map[string]string{
		"direct-mode-unsupported":   "direct_mode_unsupported",
		"direct-preview-refused":    "direct_preview_refused",
		"gateway-disconnect-failed": "direct_gateway_disconnect_failed",
		"foreign-source":            "direct_foreign_source",
		"import-failed":             "direct_import_failed",
		"contract-changed":          NativeContractChanged,
	} {
		if got := directImportFailureReason(input); got != want {
			t.Errorf("%q mapped to %q, want %q", input, got, want)
		}
	}
}

func TestDirectCatalogLimitFailsBeforeCDP(t *testing.T) {
	request := validDirectImportRequest()
	request.Models[0].Model = strings.Repeat("m", maxDirectCatalog+1)
	if got := validateDirectImportRequest(request); got != "direct_catalog_too_large" {
		t.Fatalf("oversized catalog reason = %q", got)
	}
}

func TestCDPActionsIntegrationRouteIsSynchronized(t *testing.T) {
	actions := &cdpActions{sessions: map[string]string{}}
	actions.setIntegrationMode(IntegrationModeDirect, "source-new")
	if mode, source := actions.integrationRoute(); mode != IntegrationModeDirect || source != "source-new" {
		t.Fatalf("route = %s/%s", mode, source)
	}
	actions.setIntegrationMode(IntegrationModeGateway, "must-not-survive")
	if mode, source := actions.integrationRoute(); mode != IntegrationModeGateway || source != "" {
		t.Fatalf("gateway route = %s/%s", mode, source)
	}
}

func TestControllerConfigureUpdatesExistingCDPActions(t *testing.T) {
	h := newAccountHarness(t)
	fixture := &nativeFixture{dir: h.home, state: h.stateDir}
	fixture.writeJSON(t, filepath.Join(h.stateDir, NativeWorkIntent), map[string]any{
		"schemaVersion": NativeWorkSchema, "sourceID": "gateway", "ownerID": "owner",
		"kimiHome": h.home, "mode": IntegrationModeDirect, "directSourceId": "source-new",
		"active": true, "generation": 1,
	})
	actions := &cdpActions{sessions: map[string]string{}}
	h.controller.actions = actions
	settings := h.controller.Settings()
	settings.IntegrationMode = IntegrationModeDirect
	settings.DirectSpec = directSpecFixture()
	if err := h.controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	if mode, source := actions.integrationRoute(); mode != IntegrationModeDirect || source != "source-new" {
		t.Fatalf("configured direct route = %s/%s", mode, source)
	}
	settings.IntegrationMode = IntegrationModeGateway
	settings.DirectSpec = nil
	if err := h.controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	if mode, source := actions.integrationRoute(); mode != IntegrationModeGateway || source != "" {
		t.Fatalf("configured gateway route = %s/%s", mode, source)
	}
}

func TestDirectReconcileReportsARemovedModel(t *testing.T) {
	fixture := newNativeFixture(t)
	store := directStoreFixture("account")
	account := store["accounts"].(map[string]any)["account"].(map[string]any)
	models := account["active"].(map[string]any)["models"].([]any)
	removed := map[string]any{}
	for key, value := range models[0].(map[string]any) {
		removed[key] = value
	}
	removed["alias"] = "byok-uuid/model-removed"
	removed["model"] = "model-removed"
	account["active"].(map[string]any)["models"] = append(models, removed)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		Mode: IntegrationModeDirect, DirectSpec: directSpecFixture()})
	if result.State != IntegrationNeedsImport || result.Reason != "models_removed" {
		t.Fatalf("removed model was accepted: %s/%s", result.State, result.Reason)
	}
}

func TestDirectSourceRemainsReturnsOnlyConnectedManagedSources(t *testing.T) {
	fixture := newNativeFixture(t)
	store := directStoreFixture("account")
	account := store["accounts"].(map[string]any)["account"].(map[string]any)
	source := account["sources"].([]any)[0].(map[string]any)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if got := DirectSourceRemains(fixture.dir, "account", ""); got != directSourceID {
		t.Fatalf("managed direct source = %q", got)
	}
	source["status"] = "disconnected"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if got := DirectSourceRemains(fixture.dir, "account", ""); got != "" {
		t.Fatalf("disconnected source = %q", got)
	}
	delete(source, "status")
	source["quarantined"] = true
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if got := DirectSourceRemains(fixture.dir, "account", ""); got != "" {
		t.Fatalf("quarantined source = %q", got)
	}
	delete(source, "quarantined")
	source["displayName"] = "Another custom source"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if got := DirectSourceRemains(fixture.dir, "account", ""); got != "" {
		t.Fatalf("foreign custom source = %q", got)
	}
}

func TestDirectGatewayRemainsUsesOnlyTheCurrentAccountsOwnedLoopback(t *testing.T) {
	fixture := newNativeFixture(t)
	store := directStoreFixture("account")
	account := store["accounts"].(map[string]any)["account"].(map[string]any)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	scope := sha256Hex("account")
	gateway := "http://127.0.0.1:9100/v1"
	if DirectGatewayRemains(fixture.dir, scope, gateway) {
		t.Fatal("a direct-only source was mistaken for a gateway")
	}
	account["sources"] = append(account["sources"].([]any), map[string]any{
		"id": "byok-kth", "kind": "kth", "registryUrl": "http://127.0.0.1:9100/v1/models/api.json",
	})
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if !DirectGatewayRemains(fixture.dir, scope, gateway) {
		t.Fatal("the owned gateway left after a restart was not found")
	}
	account["sources"].([]any)[1].(map[string]any)["desired"] = map[string]any{
		"providers": []any{map[string]any{"baseUrl": "http://127.0.0.1:49958/v1"}},
	}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if !DirectGatewayRemains(fixture.dir, scope, gateway) {
		t.Fatal("an older loopback provider hid the currently owned gateway")
	}
	account["sources"].([]any)[1].(map[string]any)["registryUrl"] = "https://gateway.example/v1/models/api.json"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if DirectGatewayRemains(fixture.dir, scope, gateway) {
		t.Fatal("a foreign remote gateway was claimed")
	}
	account["sources"].([]any)[1].(map[string]any)["registryUrl"] = "http://127.0.0.1:9200/v1/models/api.json"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if DirectGatewayRemains(fixture.dir, scope, gateway) {
		t.Fatal("a different loopback gateway was claimed")
	}
	account["sources"].([]any)[1].(map[string]any)["registryUrl"] = "http://127.0.0.1:9100/v1/models/api.json"
	account["sources"].([]any)[1].(map[string]any)["desired"] = map[string]any{
		"providers": []any{map[string]any{"baseUrl": "https://gateway.example/v1"}},
	}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if DirectGatewayRemains(fixture.dir, scope, gateway) {
		t.Fatal("a remote provider in the current registry was claimed")
	}
	account["sources"].([]any)[1].(map[string]any)["desired"] = map[string]any{}
	account["sources"].([]any)[1].(map[string]any)["registryUrl"] = "http://127.0.0.1:9100/v1/models/api.json"
	account["sources"].([]any)[1].(map[string]any)["quarantined"] = true
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	if DirectGatewayRemains(fixture.dir, scope, gateway) {
		t.Fatal("a disconnected gateway still required cleanup")
	}
}

func TestControllerConfigureRefusesModeChangeDuringImport(t *testing.T) {
	h := newAccountHarness(t)
	h.controller.importGranted = true
	settings := h.controller.Settings()
	settings.IntegrationMode = IntegrationModeDirect
	if err := h.controller.Configure(settings); err != ErrImportBusy {
		t.Fatalf("mode change during import: %v", err)
	}
}
