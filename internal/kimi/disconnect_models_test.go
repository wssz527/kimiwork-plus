package kimi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

func TestHasManagedModelSourcesFindsOfficialSourcesWithoutIntent(t *testing.T) {
	const user = "test-current-account"
	account := sha256Hex(user)
	for _, tc := range []struct {
		name  string
		store func(string) map[string]any
	}{
		{"direct", directStoreFixture},
		{"gateway", func(scope string) map[string]any {
			return kthTestStore(scope, kthTestAccount("http://127.0.0.1:9100/v1", nil))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newNativeFixture(t)
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
			writeIdentity(t, fixture, user)
			controller := &Controller{settings: ControllerSettings{
				StateDir: fixture.state, UserDataDir: filepath.Dir(fixture.dir), KimiHome: fixture.dir,
				GatewayURL: "http://127.0.0.1:9200/v1",
			}}
			storePath := filepath.Join(fixture.dir, NativeWorkStore)
			fixture.writeJSON(t, storePath, tc.store("other-account"))
			if controller.HasManagedModelSources() {
				t.Fatal("a different account's source was detected as current")
			}
			fixture.writeJSON(t, storePath, tc.store(account))
			if !controller.HasManagedModelSources() {
				t.Fatal("the current account's source was missed")
			}
			current := controller.ReconcileWithReadback(context.Background())
			if current.State != IntegrationWaitingConfig || !controller.HasManagedModelSources() {
				t.Fatalf("an empty gateway catalog did not preserve the source for authorized removal: %+v", current)
			}
		})
	}
}

func TestHasManagedModelSourcesIgnoresForeignPendingDisconnect(t *testing.T) {
	const user = "test-current-account"
	fixture := newNativeFixture(t)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
	writeIdentity(t, fixture, user)
	controller := &Controller{settings: ControllerSettings{
		StateDir: fixture.state, UserDataDir: filepath.Dir(fixture.dir), KimiHome: fixture.dir,
	}}
	account := kthTestAccount("https://foreign.example/v1", nil)
	source := account["sources"].([]any)[0].(map[string]any)
	source["status"] = "disconnected"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), kthTestStore(sha256Hex(user), account))
	if controller.HasManagedModelSources() {
		t.Fatal("a pending disconnect of a foreign source was detected as managed")
	}
}

func runDisconnectExpression(t *testing.T, setup string) (*goja.Runtime, *goja.Object) {
	t.Helper()
	vm := goja.New()
	if _, err := vm.RunString(setup); err != nil {
		t.Fatal(err)
	}
	expression := `(` + disconnectModelsExpression("direct-1") + `).then(result=>globalThis.disconnectResult=result,error=>globalThis.disconnectError=String(error&&error.stack||error));`
	if _, err := vm.RunString(expression); err != nil {
		t.Fatal(err)
	}
	if failure := vm.Get("disconnectError"); failure != nil {
		t.Fatalf("page expression rejected: %v", failure)
	}
	result := vm.Get("disconnectResult")
	if result == nil {
		t.Fatal("page expression returned no result")
	}
	return vm, result.ToObject(vm)
}

const disconnectSourceFixture = `
const calls=[];
const state={scopeToken:"scope",accountRevision:"r1",applyState:"idle",sources:[
  {id:"byok-kth",kind:"kth",revision:"g1",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"},
  {id:"direct-1",kind:"custom",displayName:"Renamed direct source",revision:"d1"},
  {id:"foreign",kind:"custom",displayName:"My provider",revision:"f1"}
]};
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok(state);},
  disconnect(request){
    calls.push("disconnect:"+request.sourceId+":"+request.accountRevision+":"+request.sourceRevision);
    if(request.sourceId!=="foreign"){
      const source=state.sources.find(candidate=>candidate.id===request.sourceId);
      source.status="disconnected";
      state.accountRevision="r"+(2+calls.filter(call=>call.startsWith("disconnect:")).length);
    }
    return ok({sourceId:request.sourceId,accountRevision:state.accountRevision,status:"applied"});
  },
  preview(request){
    calls.push("preview");
    return ok({scopeToken:state.scopeToken,accountRevision:state.accountRevision,
      sourceRevision:request.sourceRevision,sourceId:"byok-kth",previewId:"p1",canSave:true,candidates:[{id:"m1"}]});
  },
  save(request){
    calls.push("save");
    state.sources.find(source=>source.id==="byok-kth").status="available";
    state.accountRevision="r9";
    return ok({sourceId:"byok-kth",accountRevision:"r9",status:"pending"});
  },
  applyPending(revision){calls.push("apply:"+revision);state.applyState="idle";return ok({accountRevision:revision,status:"applied"});}
}};
`

func TestDisconnectModelsRemovesOnlyManagedSourcesAndAllowsReimport(t *testing.T) {
	vm, result := runDisconnectExpression(t, disconnectSourceFixture)
	if !result.Get("ok").ToBoolean() || result.Get("sources").ToInteger() != 2 {
		t.Fatalf("disconnect result: %v", result)
	}
	calls, _ := vm.RunString(`calls.join(",")`)
	want := "list,list,disconnect:byok-kth:r1:g1,list,disconnect:direct-1:r3:d1,list"
	if calls.String() != want {
		t.Fatalf("calls = %s, want %s", calls.String(), want)
	}
	foreign, _ := vm.RunString(`state.sources.find(source=>source.id==="foreign").status==="disconnected"`)
	if foreign.ToBoolean() {
		t.Fatal("a foreign source was disconnected")
	}

	if _, err := vm.RunString(`(` + disconnectModelsExpression("direct-1") + `).then(result=>globalThis.secondResult=result);`); err != nil {
		t.Fatal(err)
	}
	second := vm.Get("secondResult").ToObject(vm)
	if !second.Get("ok").ToBoolean() || second.Get("sources").ToInteger() != 0 {
		t.Fatalf("idempotent disconnect: %v", second)
	}
	before, _ := vm.RunString(`calls.filter(call=>call.startsWith("disconnect:")).length`)
	if before.ToInteger() != 2 {
		t.Fatalf("disconnect calls before reimport = %d", before.ToInteger())
	}

	if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:9100/v1", "") + `).then(result=>globalThis.importResult=result);`); err != nil {
		t.Fatal(err)
	}
	imported := vm.Get("importResult").ToObject(vm)
	if !imported.Get("ok").ToBoolean() {
		t.Fatalf("reimport after disconnect failed: %v", imported)
	}
	status, _ := vm.RunString(`state.sources.find(source=>source.id==="byok-kth").status`)
	if status.String() != "available" {
		t.Fatalf("gateway was not reactivated: %s", status.String())
	}
}

func TestDisconnectModelsCleanAccountWithNullRevisionIsIdempotent(t *testing.T) {
	_, result := runDisconnectExpression(t, `
globalThis.clawBridgeAPI={byokSources:{
  list(){return {ok:true,value:{scopeToken:"scope",accountRevision:null,applyState:"idle",sources:[],models:[]}};},
  disconnect(){throw new Error("should not disconnect");}
}};
`)
	if !result.Get("ok").ToBoolean() || result.Get("sources").ToInteger() != 0 {
		t.Fatalf("clean account disconnect: %v", result)
	}
}

func TestDisconnectModelsChecksApplyStatusAndApplyState(t *testing.T) {
	t.Run("deferred disconnect retries via applyPending without restarting Kimi", func(t *testing.T) {
		vm, result := runDisconnectExpression(t, disconnectSourceFixture+`
state.sources=state.sources.filter(source=>source.id==="byok-kth");
clawBridgeAPI.byokSources.disconnect=request=>{
  state.sources=[];
  state.accountRevision="r2";
  state.applyState="prepared";
  return ok({sourceId:request.sourceId,accountRevision:"r2",status:"deferred"});
};
`)
		if result.Get("ok").ToBoolean() || result.Get("code").String() != "deferred" || result.Get("sources").ToInteger() != 1 {
			t.Fatalf("deferred disconnect: %v", result)
		}
		if _, err := vm.RunString(`(` + disconnectModelsExpression("direct-1") + `).then(result=>globalThis.retryResult=result);`); err != nil {
			t.Fatal(err)
		}
		retry := vm.Get("retryResult").ToObject(vm)
		if !retry.Get("ok").ToBoolean() || retry.Get("sources").ToInteger() != 0 {
			t.Fatalf("retry after deferred disconnect: %v", retry)
		}
		calls, _ := vm.RunString(`calls.join(",")`)
		if !strings.Contains(calls.String(), "apply:r2") {
			t.Fatalf("retry did not call applyPending(r2): %s", calls.String())
		}
	})
	t.Run("apply-failed disconnect retries via applyPending when retried", func(t *testing.T) {
		vm, result := runDisconnectExpression(t, disconnectSourceFixture+`
state.sources=state.sources.filter(source=>source.id==="byok-kth");
clawBridgeAPI.byokSources.disconnect=request=>{
  state.sources=[];
  state.accountRevision="r2";
  state.applyState="apply-failed";
  return ok({sourceId:request.sourceId,accountRevision:"r2",status:"failed"});
};
`)
		if result.Get("ok").ToBoolean() || result.Get("code").String() != "apply-failed" || result.Get("sources").ToInteger() != 1 {
			t.Fatalf("failed apply disconnect: %v", result)
		}
		if _, err := vm.RunString(`(` + disconnectModelsExpression("direct-1") + `).then(result=>globalThis.retryFailedResult=result);`); err != nil {
			t.Fatal(err)
		}
		retry := vm.Get("retryFailedResult").ToObject(vm)
		if !retry.Get("ok").ToBoolean() || retry.Get("sources").ToInteger() != 0 {
			t.Fatalf("retry after apply-failed disconnect: %v", retry)
		}
		calls, _ := vm.RunString(`calls.join(",")`)
		if !strings.Contains(calls.String(), "apply:r2") {
			t.Fatalf("retry did not call applyPending(r2): %s", calls.String())
		}
	})
	t.Run("pending disconnect settles through applyPending", func(t *testing.T) {
		vm, result := runDisconnectExpression(t, disconnectSourceFixture+`
state.sources=state.sources.filter(source=>source.id==="byok-kth");
clawBridgeAPI.byokSources.disconnect=request=>{
  state.sources=[];
  state.accountRevision="r2";
  state.applyState="prepared";
  return ok({sourceId:request.sourceId,accountRevision:"r2",status:"pending"});
};
`)
		if !result.Get("ok").ToBoolean() || result.Get("sources").ToInteger() != 1 {
			t.Fatalf("pending disconnect: %v", result)
		}
		calls, _ := vm.RunString(`calls.join(",")`)
		if !strings.Contains(calls.String(), "apply:r2") {
			t.Fatalf("expected applyPending call, got %s", calls.String())
		}
	})
	t.Run("foreign pending source does not block clean idempotent disconnect", func(t *testing.T) {
		_, result := runDisconnectExpression(t, `
globalThis.clawBridgeAPI={byokSources:{
  list(){return {ok:true,value:{scopeToken:"scope",accountRevision:"r1",applyState:"prepared",sources:[
    {id:"foreign",kind:"custom",displayName:"My provider",revision:"f1",models:[{alias:"a",status:"pending"}]}
  ]}};},
  disconnect(){throw new Error("should not disconnect");}
}};
`)
		if !result.Get("ok").ToBoolean() || result.Get("sources").ToInteger() != 0 {
			t.Fatalf("foreign pending disconnect: %v", result)
		}
	})
}

func TestDisconnectModelsDetectsScopeTokenChangeDuringDisconnect(t *testing.T) {
	t.Run("mid-loop account switch stops before disconnecting second account", func(t *testing.T) {
		vm, result := runDisconnectExpression(t, disconnectSourceFixture+`
let listCount=0;
clawBridgeAPI.byokSources.list=()=>{
  listCount++;
  if(listCount>=2)state.scopeToken="other-account";
  return ok(state);
};
`)
		if result.Get("ok").ToBoolean() || result.Get("code").String() != "account-changed" {
			t.Fatalf("mid-loop switch: %v", result)
		}
		calls, _ := vm.RunString(`calls.filter(call=>call.startsWith("disconnect:")).length`)
		if calls.ToInteger() != 0 {
			t.Fatal("disconnected after scopeToken changed")
		}
	})
	t.Run("final readback detects account switch", func(t *testing.T) {
		_, result := runDisconnectExpression(t, disconnectSourceFixture+`
state.sources=state.sources.filter(source=>source.id==="byok-kth");
let listCount=0;
clawBridgeAPI.byokSources.list=()=>{
  listCount++;
  if(listCount>=3)state.scopeToken="other-account";
  return ok(state);
};
`)
		if result.Get("ok").ToBoolean() || result.Get("code").String() != "account-changed" || result.Get("sources").ToInteger() != 1 {
			t.Fatalf("readback switch: %v", result)
		}
	})
}

func TestDecodeDisconnectAnswerPreservesSourcesAndNormalizesReasonCodes(t *testing.T) {
	validCode := regexp.MustCompile(`^[a-z][a-z0-9_]{0,100}$`)
	cases := []struct {
		name       string
		answer     map[string]any
		wantOK     bool
		wantState  string
		wantReason string
		wantCount  int
	}{
		{
			name:       "partial failure keeps disconnected count and normalizes hyphens",
			answer:     map[string]any{"ok": false, "code": "byok-source-stale", "sources": float64(1)},
			wantState:  ImportStateFailed,
			wantReason: "model_disconnect_byok_source_stale",
			wantCount:  1,
		},
		{
			name:       "source-conflict matches frontend key",
			answer:     map[string]any{"ok": false, "code": "source-conflict"},
			wantState:  ImportStateFailed,
			wantReason: "model_disconnect_source_conflict",
		},
		{
			name:       "deferred uses requires-restart state and keeps count",
			answer:     map[string]any{"ok": false, "code": "deferred", "sources": float64(2)},
			wantState:  ImportStateRequiresRestart,
			wantReason: "model_disconnect_deferred",
			wantCount:  2,
		},
		{
			name:       "account-changed maps to NativeAccountChanged",
			answer:     map[string]any{"ok": false, "code": "account-changed", "sources": float64(1)},
			wantState:  ImportStateAccountChanged,
			wantReason: NativeAccountChanged,
			wantCount:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeDisconnectAnswer(tc.answer, true)
			if got.OK != tc.wantOK || got.State != tc.wantState || got.Reason != tc.wantReason || got.Sources != tc.wantCount {
				t.Fatalf("got %+v, want ok=%v state=%s reason=%s sources=%d", got, tc.wantOK, tc.wantState, tc.wantReason, tc.wantCount)
			}
			if !validCode.MatchString(got.Reason) {
				t.Fatalf("reason %q is not a valid underscore code", got.Reason)
			}
		})
	}
}

func TestDisconnectModelsReportsPartialFailureAndRetries(t *testing.T) {
	vm, result := runDisconnectExpression(t, disconnectSourceFixture+`
const originalDisconnect=clawBridgeAPI.byokSources.disconnect;
clawBridgeAPI.byokSources.disconnect=request=>request.sourceId==="direct-1"?{ok:false,reasonCode:"busy"}:originalDisconnect(request);
`)
	if result.Get("ok").ToBoolean() || result.Get("sources").ToInteger() != 1 || result.Get("code").String() != "busy" {
		t.Fatalf("partial failure: %v", result)
	}
	if _, err := vm.RunString(`clawBridgeAPI.byokSources.disconnect=originalDisconnect;`); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`(` + disconnectModelsExpression("direct-1") + `).then(result=>globalThis.retryResult=result);`); err != nil {
		t.Fatal(err)
	}
	retry := vm.Get("retryResult").ToObject(vm)
	if !retry.Get("ok").ToBoolean() || retry.Get("sources").ToInteger() != 1 {
		t.Fatalf("retry after partial failure: %v", retry)
	}
}

func TestDisconnectModelsRefusesAmbiguousOrUnverifiedChanges(t *testing.T) {
	t.Run("source id changed kind", func(t *testing.T) {
		vm, result := runDisconnectExpression(t, disconnectSourceFixture+`
state.sources.find(source=>source.id==="direct-1").kind="kth";
`)
		if result.Get("ok").ToBoolean() || result.Get("code").String() != "source-conflict" {
			t.Fatalf("source conflict: %v", result)
		}
		calls, _ := vm.RunString(`calls.filter(call=>call.startsWith("disconnect:")).length`)
		if calls.ToInteger() != 0 {
			t.Fatal("a conflicting source was disconnected")
		}
	})
	t.Run("official reply is not accepted as evidence", func(t *testing.T) {
		_, result := runDisconnectExpression(t, disconnectSourceFixture+`
clawBridgeAPI.byokSources.disconnect=request=>({ok:true,value:{sourceId:request.sourceId,accountRevision:"r2",status:"applied"}});
`)
		if result.Get("ok").ToBoolean() || result.Get("code").String() != "not-verified" {
			t.Fatalf("unverified disconnect: %v", result)
		}
	})
	t.Run("remote gateway is left alone", func(t *testing.T) {
		vm, result := runDisconnectExpression(t, disconnectSourceFixture+`
state.sources=state.sources.filter(source=>source.id==="byok-kth");
state.sources[0].registryUrl="https://gateway.example/v1/models/api.json";
`)
		if !result.Get("ok").ToBoolean() || result.Get("sources").ToInteger() != 0 {
			t.Fatalf("foreign gateway: %v", result)
		}
		calls, _ := vm.RunString(`calls.filter(call=>call.startsWith("disconnect:")).length`)
		if calls.ToInteger() != 0 {
			t.Fatal("a remote gateway was disconnected")
		}
	})
	t.Run("remote provider inside a loopback registry is left alone", func(t *testing.T) {
		vm, result := runDisconnectExpression(t, disconnectSourceFixture+`
state.sources=state.sources.filter(source=>source.id==="byok-kth");
state.sources[0].providers=[{baseUrl:"https://user-provider.example/v1"}];
`)
		if !result.Get("ok").ToBoolean() || result.Get("sources").ToInteger() != 0 {
			t.Fatalf("modified gateway: %v", result)
		}
		calls, _ := vm.RunString(`calls.filter(call=>call.startsWith("disconnect:")).length`)
		if calls.ToInteger() != 0 {
			t.Fatal("a gateway with a user provider was disconnected")
		}
	})
}

func TestControllerDisconnectClearsIntentWithoutDeletingCatalog(t *testing.T) {
	profile, home := nativeProfile(t, "user-1")
	seedNativeProfile(t, home, "http://127.0.0.1:9999/v1", "cred-1")
	controller, actions := controllerFor(t, profile, nil)
	controller.reconcile(false, "")
	actions.removed = DisconnectResult{OK: true, State: "disconnected", Sources: 1}
	result, err := controller.DisconnectModelsAuthorized(context.Background())
	if err != nil || !result.OK || result.State != "disconnected" {
		t.Fatalf("disconnect: %+v %v", result, err)
	}
	intent, err := readIntentFile(filepath.Dir(profile))
	if err != nil || intent == nil || intent.Active {
		t.Fatalf("intent after disconnect: %+v %v", intent, err)
	}
	if controller.integration == nil || controller.integration.State != IntegrationDisabled {
		t.Fatalf("controller integration: %+v", controller.integration)
	}
	if len(controller.Settings().Models) != 2 {
		t.Fatal("the local model catalog was deleted")
	}
	if _, err := controller.ImportModelsAuthorized(context.Background()); err != nil {
		t.Fatalf("reimport after disconnect: %v", err)
	}
	if len(actions.imports) != 1 {
		t.Fatalf("reimport calls = %d", len(actions.imports))
	}
}

func TestDisconnectForAnotherAccountPreservesOriginalIntentAndAllowsCrossAccountRebind(t *testing.T) {
	fixture := newNativeFixture(t)
	accounts := newSwitchedAccounts(t)
	fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:9999/v1", "http://127.0.0.1:9999/v1")
	fixture.seedOldIntent(t, accounts, "http://127.0.0.1:9999/v1")

	integration := fixture.integration(t)
	// Disconnecting account B must not deactivate account A's active intent.
	other := integration.DisconnectForAccount(accounts.newScope)
	if other.State != IntegrationDisabled {
		t.Fatalf("disconnect account B: %+v", other)
	}
	intent, err := integration.readIntent()
	if err != nil || intent == nil || !intent.Active || intent.AccountDigest != accounts.oldDigest {
		t.Fatalf("account A intent was altered by disconnecting account B: %+v %v", intent, err)
	}

	// Disconnecting account A marks account A's intent inactive, and an explicit
	// confirmed import on account B can then rebind instead of sticking on account_changed.
	mine := integration.DisconnectForAccount(accounts.oldScope)
	if mine.State != IntegrationDisabled {
		t.Fatalf("disconnect account A: %+v", mine)
	}
	intent, err = integration.readIntent()
	if err != nil || intent == nil || intent.Active {
		t.Fatalf("account A intent still active: %+v %v", intent, err)
	}
	rebound := integration.Reconcile(ReconcileOptions{
		Enabled:            true,
		AllowAccountRebind: true,
		AccountScope:       accounts.newScope,
		URL:                "http://127.0.0.1:9999/v1",
		Models:             testCatalog(),
	})
	if rebound.State != IntegrationReady && rebound.State != IntegrationRepaired {
		t.Fatalf("rebind after account A disconnect failed: %+v", rebound)
	}
	intent, err = integration.readIntent()
	if err != nil || intent == nil || !intent.Active || intent.AccountDigest != accounts.newDigest {
		t.Fatalf("rebound intent: %+v %v", intent, err)
	}
}

func TestDirectRebindAfterDisconnectAndEmptyConfigDisconnect(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	kimiDir := filepath.Join(dir, "kimi")
	if err := os.MkdirAll(kimiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Empty config.json with no "model" section must not fail idempotent disconnect.
	if err := os.WriteFile(filepath.Join(kimiDir, NativeWorkConfig), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	integration, err := NewNativeIntegration(stateDir, kimiDir, IntegrationModeDirect)
	if err != nil {
		t.Fatal(err)
	}
	if res := integration.DisconnectForAccount("account-a"); res.State != IntegrationDisabled {
		t.Fatalf("empty config disconnect without intent: %+v", res)
	}

	accounts := newSwitchedAccounts(t)
	if err := integration.writeIntent(&nativeIntent{
		SchemaVersion: NativeWorkSchema, SourceID: integration.sourceID, OwnerID: integration.ownerID,
		KimiHome: kimiDir, Mode: IntegrationModeDirect, DirectSourceID: "custom-a",
		AccountDigest: accounts.oldDigest, ProfileKeyDigest: accounts.oldDigest, Active: true, Generation: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if res := integration.DisconnectForAccount(accounts.oldScope); res.State != IntegrationDisabled {
		t.Fatalf("v3 direct disconnect on empty config: %+v", res)
	}

	// Now account B imports a direct source and reconciles with AllowAccountRebind.
	spec := directSpecFixture()
	storeData, err := json.Marshal(directStoreFixture(accounts.newKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiDir, NativeWorkStore), storeData, 0o600); err != nil {
		t.Fatal(err)
	}
	configData, err := json.Marshal(directConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiDir, NativeWorkConfig), configData, 0o600); err != nil {
		t.Fatal(err)
	}
	rebound := integration.Reconcile(ReconcileOptions{
		Enabled:            true,
		Mode:               IntegrationModeDirect,
		AllowAccountRebind: true,
		AccountScope:       accounts.newScope,
		DirectSpec:         spec,
		DirectSourceID:     directSourceID,
	})
	if rebound.State != IntegrationReady || !rebound.Applied || rebound.DirectSourceID != directSourceID {
		t.Fatalf("direct rebind after disconnect failed: %+v", rebound)
	}
}

func TestDisconnectPropagatesIntentWriteFailure(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	kimiDir := filepath.Join(dir, "kimi")
	if err := os.MkdirAll(kimiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	integration, err := NewNativeIntegration(stateDir, kimiDir, IntegrationModeGateway)
	if err != nil {
		t.Fatal(err)
	}
	if err := integration.writeIntent(&nativeIntent{
		SchemaVersion: NativeWorkSchema, SourceID: integration.sourceID, OwnerID: integration.ownerID,
		KimiHome: kimiDir, Mode: IntegrationModeGateway, Active: true, Generation: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	if res := integration.Disconnect(); res.State != IntegrationConflict || res.Reason != "intent_unavailable" {
		t.Fatalf("expected intent write conflict, got %+v", res)
	}
}

func TestPreviousDirectSourceIDIgnoresOtherAccountIntent(t *testing.T) {
	profile, home := nativeProfile(t, "user-b")
	stateDir := filepath.Dir(profile)
	integration, err := NewNativeIntegration(stateDir, home, IntegrationModeDirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := integration.writeIntent(&nativeIntent{
		SchemaVersion: NativeWorkSchema, SourceID: integration.sourceID, OwnerID: integration.ownerID,
		KimiHome: home, Mode: IntegrationModeDirect, DirectSourceID: "custom-a",
		AccountDigest: sha256Hex(sha256Hex("user-a")), Active: true, Generation: 1,
	}); err != nil {
		t.Fatal(err)
	}
	controller, _ := controllerFor(t, profile, nil)
	settings := controller.Settings()
	settings.IntegrationMode = IntegrationModeDirect
	if got := controller.previousDirectSourceID(settings); got != "" {
		t.Fatalf("leaked other account direct source id: %q", got)
	}
}

func TestDisconnectedStatusRejectedByRuntimeAndDirectCatalog(t *testing.T) {
	raw := map[string]any{
		"sources": []any{
			map[string]any{
				"id": "byok-kth", "kind": "kth", "status": "disconnected",
				"providers": []any{map[string]any{"id": "p1", "protocol": "openai_responses", "baseUrl": "http://127.0.0.1:9100/v1"}},
				"models":    []any{map[string]any{"alias": "m1", "providerId": "p1", "model": "m1", "status": "available"}},
			},
		},
	}
	if _, err := accountStoreV3ConfigFor(raw, IntegrationModeGateway, ""); err == nil {
		t.Fatal("accountStoreV3ConfigFor accepted a disconnected source")
	}

	spec := directSpecFixture()
	fixture := seedDirectFixture(t, sha256Hex("user-1"))
	store := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkStore))
	account := store["accounts"].(map[string]any)[sha256Hex("user-1")].(map[string]any)
	account["sources"].([]any)[0].(map[string]any)["status"] = "disconnected"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
	integration := fixture.integration(t)
	_, reason, needsImport := integration.directCatalog(ControllerSettings{IntegrationMode: IntegrationModeDirect, DirectSpec: spec}, sha256Hex("user-1"))
	if reason != "profile_delisted" || !needsImport {
		t.Fatalf("directCatalog accepted disconnected source: reason=%q needsImport=%v", reason, needsImport)
	}
}

func TestDisconnectExpressionContainsNoProviderCredential(t *testing.T) {
	expression := disconnectModelsExpression("direct-1")
	for _, forbidden := range []string{"apiKey", "registryKey", "Authorization"} {
		if strings.Contains(expression, forbidden) {
			t.Fatalf("disconnect expression contains %s", forbidden)
		}
	}
}
