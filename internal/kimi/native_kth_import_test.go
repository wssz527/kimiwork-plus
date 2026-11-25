package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// The import entry points below are the ones auto-sync calls. Each test drives
// the real expressions through the fake page, so the ordering between the store
// repair, the runtime wait and the official import is what is being checked.

func kthImportActions(t *testing.T, home string, page *fakePage) PageActions {
	t.Helper()
	harness := newHarness(t, page)
	return NewCDPActions(harness.pipe, home, home)
}

func kthImportContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestImportRebindsTheGatewayRouteBeforeTheOfficialImport(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	oldURL, newURL := "http://127.0.0.1:9100/v1", "http://127.0.0.1:9200/v1"
	kthTestWrite(t, home, kthTestStore(account, kthTestAccount(oldURL,
		map[string]any{"gpt": "byok-kth-resp", kimiRouteKey(oldURL): kthProviderID})))
	page := &fakePage{surface: "work", actions: true, importAPI: true}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: newURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("result: %+v", result)
	}
	if page.importCalls != 1 {
		t.Fatalf("imports = %d", page.importCalls)
	}
	bindings := kthTestSourceOf(t, kthTestAccountOf(t, home))["routeBindings"].(map[string]any)
	if bindings[kimiRouteKey(newURL)] != kthProviderID {
		t.Fatalf("the new route was not bound before the import: %v", bindings)
	}
}

func TestImportClearsTheBlockedAliasesBeforeTheOfficialImport(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	document := kthTestAccount(baseURL, nil)
	document["sources"] = []any{}
	document["active"] = map[string]any{"providers": []any{}, "models": []any{
		map[string]any{"alias": "byok-uuid/model-a", "providerId": "byok-uuid"}}}
	document["blockedAliases"] = map[string]any{
		"byok-kth-kimi/kimi-a": "delisted",
		"byok-uuid/model-a":    "disconnected",
	}
	kthTestWrite(t, home, kthTestStore(account, document))
	page := &fakePage{surface: "work", actions: true, importAPI: true}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("result: %+v", result)
	}
	blocked := kthTestAccountOf(t, home)["blockedAliases"].(map[string]any)
	if _, present := blocked["byok-kth-kimi/kimi-a"]; present {
		t.Fatalf("the stale alias survived the import: %v", blocked)
	}
	if _, present := blocked["byok-uuid/model-a"]; !present {
		t.Fatalf("another source's blocked alias was removed: %v", blocked)
	}
	if page.kthDisconnectCalls != 0 {
		t.Fatalf("a source that is not there was disconnected: %d", page.kthDisconnectCalls)
	}
}

func TestImportSettlesAnOrphanProviderBeforeGatewayPreview(t *testing.T) {
	for _, tc := range []struct {
		name, setup, calls, reason string
		state                      string
		deferred                   bool
	}{
		{"applied by recorded ID", "", "apply:rev-Y,preview,save,disconnect", "", ImportStateAccepted, false},
		{"deferred by managed name", `direct.id="replaced-id";direct.displayName=managedName;deferOnce=true;`, "apply:rev-Y,apply:rev-Y,preview,save,disconnect", "", ImportStateAccepted, false},
		{"foreign pending", `foreignPending=true;`, "", "model_provider_reset_pending", ImportStateRequiresRestart, false},
		{"foreign pending with managed name", `foreignPending=true;foreign.displayName=managedName;`, "", "model_provider_reset_pending", ImportStateRequiresRestart, false},
		{"revision changes before apply", `revision="newer-revision";`, "", "model_provider_reset_pending", ImportStateRequiresRestart, false},
		{"store still occupied", `keepOrphan=true;`, "apply:rev-Y", "model_provider_reset_pending", ImportStateRequiresRestart, false},
		{"apply refused", `failApply=true;`, "apply:rev-Y", "native_import_provider_reset_failed", ImportStateFailed, false},
		{"account changes during retry", `deferOnce=true;changeScope=true;`, "apply:rev-Y", NativeAccountChanged, ImportStateAccountChanged, false},
		{"apply stays deferred", `deferAlways=true;`, "apply:rev-Y,apply:rev-Y", "model_provider_reset_pending", ImportStateRequiresRestart, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortDeferredApply(t, 0)
			home := kthTestHome(t)
			accountKey := AccountScope(home)
			baseURL := "http://127.0.0.1:9100/v1"
			store := directStoreFixture(accountKey)
			account := mapField(mapField(store, "accounts"), accountKey)
			active := mapField(account, "active")
			providers := active["providers"].([]any)
			catalog := make([]any, 18)
			for i := range catalog {
				model := fmt.Sprintf("catalog-%d", i)
				catalog[i] = map[string]any{"alias": "prov-1/" + model, "providerId": "prov-1",
					"sourceId": directSourceID, "model": model, "provenance": "catalog"}
			}
			account["revision"] = "rev-Y"
			account["pending"] = map[string]any{"applyState": "prepared", "providers": providers, "models": catalog}
			active["providers"] = append(providers, map[string]any{"id": kthProviderID, "sourceId": kthSourceID})
			blockedAlias := kthAliasPrefix + "old-model"
			active["models"] = append(active["models"].([]any), map[string]any{"alias": blockedAlias, "sourceId": kthSourceID})
			account["blockedAliases"] = map[string]any{blockedAlias: "disconnected", "foreign/model": "delisted"}
			original := kthTestWrite(t, home, store)
			file := filepath.Join(home, NativeWorkStore)
			vm := goja.New()
			if err := vm.Set("managedName", DirectDisplayName); err != nil {
				t.Fatal(err)
			}
			if err := vm.Set("promotePending", func() {
				account["active"] = account["pending"]
				delete(account, "pending")
				account["revision"] = "rev-after-apply"
				raw, err := json.Marshal(store)
				if err == nil {
					err = os.WriteFile(file, raw, 0o600)
				}
				if err != nil {
					t.Error(err)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := vm.Set("canonicalAvailable", func() bool {
				scan := kthStoreState(home, accountKey, baseURL)
				return scan.valid && !scan.canonical && !scan.modeled && len(scan.blocked) == 0 &&
					len(modelRows(mapField(scan.account, "active"))) == 18 && mapField(scan.account, "pending") == nil
			}); err != nil {
				t.Fatal(err)
			}
			_, err := vm.RunString(`
const calls=[];
let revision="rev-Y", scope="scope", gatewayApplied=false, directConnected=true;
let foreignPending=false, deferOnce=false, deferAlways=false, keepOrphan=false, failApply=false, changeScope=false;
const ok=value=>({ok:true,value});
const direct={id:"byok-src-1",kind:"custom",displayName:"Renamed Direct",revision:"direct-rev",
  models:Array.from({length:18},(_,i)=>({model:"catalog-"+i,provenance:"catalog",status:"pending"}))};
const foreign={id:"other-source",kind:"custom",models:[{model:"unconfirmed",status:"pending"}]};
globalThis.clawBridgeAPI={byokSources:{
  list(){return ok({scopeToken:scope,accountRevision:revision,sources:(directConnected?[direct]:[]).concat(foreignPending?[foreign]:[])});},
  applyPending(expected){
    calls.push("apply:"+expected);
    if(expected!==revision||foreignPending||!directConnected)throw new Error("unsafe pending apply");
    if(failApply)return {ok:false,reasonCode:"apply-failed"};
    if(deferOnce||deferAlways){deferOnce=false;if(changeScope)scope="another";return ok({status:"deferred"});}
    if(!keepOrphan)promotePending();
    revision="rev-after-apply";
    for(const model of direct.models)model.status="available";
    return ok({status:"applied"});
  },
  preview(request){
    calls.push("preview");
    if(request.accountRevision!==revision||!directConnected||request.draft.kind!=="kth")throw new Error("unsafe gateway preview");
    const id=canonicalAvailable()?"byok-kth-kimi":"byok-random";
    return ok({previewId:"preview",sourceId:"byok-kth",scopeToken:scope,accountRevision:revision,sourceRevision:null,
      canSave:true,candidates:[{id:"gateway-model"}],source:{providers:[{id,protocol:"kimi"}]}});
  },
  discard(){calls.push("discard");},
  save(){calls.push("save");gatewayApplied=true;return ok({sourceId:"byok-kth",status:"applied"});},
  disconnect(request){
    calls.push("disconnect");
    if(!gatewayApplied||request.sourceId!==direct.id)throw new Error("direct source disconnected prematurely");
    directConnected=false;
    return ok({status:"applied"});
  },
}};
` + tc.setup)
			if err != nil {
				t.Fatal(err)
			}
			page := &fakePage{surface: "work", actions: true, importAPI: true, evaluateAction: executeSourceActions(t, vm)}
			result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
				URL: baseURL, Token: "gateway-token", ExpectedAccountKey: accountKey, DisconnectDirectSourceID: directSourceID,
			})
			accepted := tc.state == ImportStateAccepted
			if err != nil || result.State != tc.state || result.Reason != tc.reason || result.OK != accepted ||
				result.RequiresRestart != (tc.state == ImportStateRequiresRestart) || result.RouteCleanupPending || result.RestartDeferred != tc.deferred {
				t.Errorf("gateway import: %+v, %v", result, err)
			}
			calls, err := vm.RunString(`calls.join(",")`)
			if err != nil || calls.String() != tc.calls {
				t.Errorf("page calls: %v, want %q (%v)", calls, tc.calls, err)
			}
			if accepted {
				blocked := mapField(kthTestAccountOf(t, home), "blockedAliases")
				if _, exists := blocked[blockedAlias]; exists || blocked["foreign/model"] != "delisted" {
					t.Errorf("unexpected blocked aliases: %v", blocked)
				}
			} else if raw, err := os.ReadFile(file); err != nil || string(raw) != string(original) {
				t.Errorf("stopped repair changed the store: %v", err)
			}
		})
	}
}

func TestImportDisconnectsAStuckProviderAndThenImports(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	stuckID := "byok-9f2c1d-kimi"
	kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount(baseURL, stuckID)))

	// The official apply removes the source and blocks the aliases it owned, which
	// is what the next store read must observe.
	disconnected := kthTestStore(account, kthTestAccount(baseURL, nil))
	next := disconnected["accounts"].(map[string]any)[account].(map[string]any)
	next["sources"] = []any{}
	next["active"] = map[string]any{"providers": []any{}, "models": []any{}}
	next["blockedAliases"] = map[string]any{
		"byok-kth-kimi/kimi-a": "disconnected",
		"byok-uuid/model-a":    "delisted",
	}
	document, err := json.Marshal(disconnected)
	if err != nil {
		t.Fatal(err)
	}

	page := &fakePage{surface: "work", actions: true, importAPI: true}
	var writeErr error
	page.onKthDisconnect = func() {
		writeErr = os.WriteFile(filepath.Join(home, NativeWorkStore), document, 0o600)
	}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if !result.OK {
		t.Fatalf("result: %+v", result)
	}
	if page.kthDisconnectCalls != 1 {
		t.Fatalf("disconnects = %d", page.kthDisconnectCalls)
	}
	if page.importCalls != 1 {
		t.Fatalf("imports = %d", page.importCalls)
	}
	blocked := kthTestAccountOf(t, home)["blockedAliases"].(map[string]any)
	if _, present := blocked["byok-kth-kimi/kimi-a"]; present {
		t.Fatalf("the alias the disconnect blocked was not cleared: %v", blocked)
	}
	if _, present := blocked["byok-uuid/model-a"]; !present {
		t.Fatalf("a Direct Mode alias was removed: %v", blocked)
	}
}

func TestImportStopsWhenTheOfficialDisconnectIsDeferred(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	before := kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount(baseURL, "byok-9f2c1d-kimi")))
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		kthDisconnectReply: map[string]any{"ok": true, "status": "deferred"}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateRequiresRestart || !result.RequiresRestart || !result.RestartDeferred {
		t.Fatalf("result: %+v", result)
	}
	if result.Reason != "model_provider_reset_pending" {
		t.Fatalf("reason: %s", result.Reason)
	}
	if page.importCalls != 0 {
		t.Fatalf("the import ran against an unapplied disconnect: %d", page.importCalls)
	}
	after, err := os.ReadFile(filepath.Join(home, NativeWorkStore))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("the store was written while the official apply was deferred")
	}
}

func TestImportStopsWhenTheOfficialDisconnectIsRefused(t *testing.T) {
	for code, want := range map[string]string{
		"foreign-source":   "native_import_foreign_source",
		"byok-busy":        "native_import_provider_reset_failed",
		"contract-changed": NativeContractChanged,
	} {
		t.Run(code, func(t *testing.T) {
			home := kthTestHome(t)
			account := AccountScope(home)
			baseURL := "http://127.0.0.1:9100/v1"
			kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount(baseURL, "byok-9f2c1d-kimi")))
			page := &fakePage{surface: "work", actions: true, importAPI: true,
				kthDisconnectReply: map[string]any{"ok": false, "code": code}}

			result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
				URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
			if err != nil {
				t.Fatal(err)
			}
			if result.Reason != want {
				t.Fatalf("reason = %q, want %q", result.Reason, want)
			}
			if page.importCalls != 0 {
				t.Fatalf("the import ran after a refused disconnect: %d", page.importCalls)
			}
		})
	}
}

func TestImportSkipsTheRepairOnALegacyBuild(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	before := kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount(baseURL, "byok-9f2c1d-kimi")))
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		kthDisconnectReply: map[string]any{"ok": true, "skipped": true}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("result: %+v", result)
	}
	if page.importCalls != 1 {
		t.Fatalf("imports = %d", page.importCalls)
	}
	after, err := os.ReadFile(filepath.Join(home, NativeWorkStore))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a build without byokSources was repaired")
	}
}

func TestImportStopsWhenTheStoreKeepsTheStuckProvider(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	stuck := kthTestStuckAccount(baseURL, "byok-9f2c1d-kimi")
	document, err := json.Marshal(kthTestStore(account, stuck))
	if err != nil {
		t.Fatal(err)
	}
	kthTestWrite(t, home, kthTestStore(account, stuck))

	page := &fakePage{surface: "work", actions: true, importAPI: true}
	var writeErr error
	page.onKthDisconnect = func() {
		// The official API reported an applied disconnect, but the store still
		// holds the source.
		writeErr = os.WriteFile(filepath.Join(home, NativeWorkStore), document, 0o600)
	}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if result.State != ImportStateFailed || result.Reason != "native_import_store_changed" {
		t.Fatalf("result: %+v", result)
	}
	if page.kthDisconnectCalls != 1 {
		t.Fatalf("the source was disconnected twice: %d", page.kthDisconnectCalls)
	}
	if page.importCalls != 0 {
		t.Fatalf("the import ran against a store that still mints a random id: %d", page.importCalls)
	}
}

func TestDirectModeImportNeverRepairsTheGatewayStore(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	before := kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount(baseURL, "byok-9f2c1d-kimi")))
	page := &fakePage{surface: "work", actions: true, importAPI: true}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)

	result, err := actions.ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("result: %+v", result)
	}
	if page.kthDisconnectCalls != 0 {
		t.Fatalf("a direct-mode controller disconnected the gateway: %d", page.kthDisconnectCalls)
	}
	after, err := os.ReadFile(filepath.Join(home, NativeWorkStore))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a direct-mode controller wrote the gateway store")
	}
}

// ---------------------------------------------------------------------------
// the startup window

func TestImportHoldsTheWriteUntilDaimonLeavesTheStartupWindow(t *testing.T) {
	previous := settingsReadyStep
	settingsReadyStep = 5 * time.Millisecond
	t.Cleanup(func() { settingsReadyStep = previous })

	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		daimonStates: []string{"provisioning", "spawning", "connecting", "ready"}}
	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("result: %+v", result)
	}
	if page.daimonReads != 4 {
		t.Fatalf("status reads = %d, want one per state", page.daimonReads)
	}
	if page.importCalls != 1 {
		t.Fatalf("imports = %d", page.importCalls)
	}
}

func TestImportWithoutAStatusAPIWaitsForNothing(t *testing.T) {
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true}
	started := time.Now()
	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("result: %+v", result)
	}
	// An unreadable state is a state to leave alone: one read, no polling.
	if page.daimonReads != 1 {
		t.Fatalf("status reads = %d", page.daimonReads)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("a page without the status API cost %s", elapsed)
	}
}

func TestImportDoesNotWaitPastTheBootWindow(t *testing.T) {
	previousStep, previousWindow := settingsReadyStep, daimonBootWindow
	settingsReadyStep, daimonBootWindow = 5*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() {
		settingsReadyStep, daimonBootWindow = previousStep, previousWindow
	})

	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true, daimonStuck: true}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("a bounded wait must still let the import run: %+v", result)
	}
	if page.importCalls != 1 {
		t.Fatalf("imports = %d", page.importCalls)
	}
	if page.daimonReads < 2 {
		t.Fatalf("the wait did not poll: %d", page.daimonReads)
	}
}

func TestDisconnectModelsHoldsTheWriteUntilDaimonLeavesTheStartupWindow(t *testing.T) {
	previous := settingsReadyStep
	settingsReadyStep = 5 * time.Millisecond
	t.Cleanup(func() { settingsReadyStep = previous })

	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		daimonStates: []string{"provisioning", "ready"}}
	result, err := kthImportActions(t, home, page).DisconnectModels(kthImportContext(t),
		DisconnectRequest{ExpectedAccountKey: AccountScope(home)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != "disconnected" {
		t.Fatalf("result: %+v", result)
	}
	if page.daimonReads != 2 {
		t.Fatalf("status reads = %d", page.daimonReads)
	}
	if page.importCalls != 0 {
		t.Fatalf("a disconnect ran an import: %d", page.importCalls)
	}
}

func TestDaimonWaitUsesTheTargetsItIsGiven(t *testing.T) {
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true}
	harness := newHarness(t, page)
	actions := NewCDPActions(harness.pipe, home, home).(*cdpActions)
	ctx := kthImportContext(t)

	targets, err := actions.targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	page.mu.Lock()
	reads := page.targetReads
	page.daimonStates = []string{"ready"}
	page.mu.Unlock()
	if err := actions.waitForDaimonNotBooting(ctx, targets); err != nil {
		t.Fatal(err)
	}
	page.mu.Lock()
	after, probes := page.targetReads, page.daimonReads
	page.mu.Unlock()
	if probes != 1 {
		t.Fatalf("status reads = %d", probes)
	}
	if after != reads {
		t.Fatalf("a supplied Work target still refreshed the target list: %d -> %d", reads, after)
	}
}

func TestDaimonWaitReturnsWhenTheWorkPageIsAbsent(t *testing.T) {
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true, workAvailableAfter: 10}
	harness := newHarness(t, page)
	actions := NewCDPActions(harness.pipe, home, home).(*cdpActions)
	if err := actions.waitForDaimonNotBooting(kthImportContext(t), nil); err != nil {
		t.Fatal(err)
	}
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.daimonReads != 0 {
		t.Fatalf("a page that is not there was probed: %d", page.daimonReads)
	}
}

func TestImportDirectHoldsTheWriteUntilDaimonLeavesTheStartupWindow(t *testing.T) {
	previous := settingsReadyStep
	settingsReadyStep = 5 * time.Millisecond
	t.Cleanup(func() { settingsReadyStep = previous })

	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		daimonStates: []string{"spawning", "ready"}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)
	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("result: %+v", result)
	}
	if page.daimonReads != 2 {
		t.Fatalf("status reads = %d", page.daimonReads)
	}
	if page.directImports != 1 {
		t.Fatalf("direct imports = %d", page.directImports)
	}
}

// ---------------------------------------------------------------------------
// the post-ready reconciliation window

// shortDeferredApply keeps the polls these tests depend on short. It restores
// both globals even when a subtest fails.
func shortDeferredApply(t *testing.T, window time.Duration) {
	t.Helper()
	previousStep, previousWindow := settingsReadyStep, deferredApplyWindow
	settingsReadyStep, deferredApplyWindow = 5*time.Millisecond, window
	t.Cleanup(func() { settingsReadyStep, deferredApplyWindow = previousStep, previousWindow })
}

// TestImportRetriesTheRepairWhenThePreviewIsNotCanonical models the 11ms race:
// Electron's post-restart promote() writes back the blocked aliases between the
// repair and the preview, so the first preview can only offer a random id. That
// preview is dropped, the repair runs again on the restored bytes, and the
// second preview keeps the canonical one.
func TestImportRetriesTheRepairWhenThePreviewIsNotCanonical(t *testing.T) {
	shortDeferredApply(t, 100*time.Millisecond)
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	document := kthTestAccount(baseURL, nil)
	document["sources"] = []any{}
	document["active"] = map[string]any{"providers": []any{}, "models": []any{
		map[string]any{"alias": "byok-uuid/model-a", "providerId": "byok-uuid"}}}
	document["blockedAliases"] = map[string]any{"byok-kth-kimi/kimi-a": "delisted"}
	restored, err := json.Marshal(kthTestStore(account, document))
	if err != nil {
		t.Fatal(err)
	}
	kthTestWrite(t, home, kthTestStore(account, document))

	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{
			{"ok": false, "code": "non-canonical-provider"},
			{"ok": true, "models": float64(2), "requiresRestart": false},
		}}
	var imports int
	page.onImport = func() {
		imports++
		if imports == 1 {
			// Electron writes back the bytes it read before the cleanup.
			if writeErr := os.WriteFile(filepath.Join(home, NativeWorkStore), restored, 0o600); writeErr != nil {
				t.Error(writeErr)
			}
			return
		}
		raw, readErr := os.ReadFile(filepath.Join(home, NativeWorkStore))
		if readErr != nil {
			t.Error(readErr)
			return
		}
		var store map[string]any
		if json.Unmarshal(raw, &store) != nil {
			t.Error("the store was unreadable at the retry")
			return
		}
		blocked, _ := store["accounts"].(map[string]any)[account].(map[string]any)["blockedAliases"].(map[string]any)
		if _, present := blocked["byok-kth-kimi/kimi-a"]; present {
			t.Error("the retry imported while the blocking alias was restored")
		}
	}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("result: %+v", result)
	}
	if page.importCalls != 2 {
		t.Fatalf("imports = %d, want one non-canonical preview and one retry", page.importCalls)
	}
	blocked := kthTestAccountOf(t, home)["blockedAliases"].(map[string]any)
	if _, present := blocked["byok-kth-kimi/kimi-a"]; present {
		t.Fatalf("the restored alias survived the retry: %v", blocked)
	}
}

func TestImportStopsWhenEveryPreviewStaysNonCanonical(t *testing.T) {
	shortDeferredApply(t, 100*time.Millisecond)
	home := kthTestHome(t)
	account := AccountScope(home)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": false, "code": "non-canonical-provider"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.State != ImportStateFailed || result.Reason != "native_import_non_canonical_provider" {
		t.Fatalf("result: %+v", result)
	}
	if page.importCalls != kthRepairPasses {
		t.Fatalf("imports = %d, want %d bounded attempts", page.importCalls, kthRepairPasses)
	}
}

func TestImportAppliesADeferredSaveAfterTheReadyWindow(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": true,
			"restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}, {"ok": true, "status": "applied"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token",
		ExpectedAccountKey: AccountScope(home), DisconnectDirectSourceID: "direct-source"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("result: %+v", result)
	}
	if result.RequiresRestart || result.RestartDeferred || result.RouteCleanupPending {
		t.Fatalf("an applied retry must clear the restart and cleanup claims: %+v", result)
	}
	if page.applyPendingCalls != 2 {
		t.Fatalf("applyPending polls = %d, want the deferred answer and the applied one", page.applyPendingCalls)
	}
	if page.directCleanupCalls != 1 {
		t.Fatalf("direct cleanup calls = %d", page.directCleanupCalls)
	}
	if page.importCalls != 1 {
		t.Fatalf("imports = %d", page.importCalls)
	}
}

func TestImportKeepsTheRestartPromptWhenTheDeferredApplyNeverLands(t *testing.T) {
	shortDeferredApply(t, 30*time.Millisecond)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": true,
			"restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token",
		ExpectedAccountKey: AccountScope(home), DisconnectDirectSourceID: "direct-source"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.RequiresRestart || !result.RestartDeferred || !result.RouteCleanupPending {
		t.Fatalf("a window that never settles must keep the restart prompt: %+v", result)
	}
	if page.applyPendingCalls < 2 {
		t.Fatalf("the retry did not poll: %d", page.applyPendingCalls)
	}
	if page.directCleanupCalls != 0 {
		t.Fatalf("the direct source was disconnected before the apply landed: %d", page.directCleanupCalls)
	}
}

func TestImportStopsWhenTheDeferredApplySeesAnotherAccount(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": true,
			"restartDeferred": true, "applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": false, "code": "account-changed"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token", ExpectedAccountKey: AccountScope(home)})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.State != ImportStateAccountChanged || result.Reason != NativeAccountChanged {
		t.Fatalf("result: %+v", result)
	}
	if result.RequiresRestart || result.RestartDeferred {
		t.Fatalf("another account is not a restart claim: %+v", result)
	}
}

func TestImportKeepsTheRestartPromptWhenTheDeferredRevisionIsStale(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": true,
			"restartDeferred": true, "applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": false, "code": "stale"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token", ExpectedAccountKey: AccountScope(home)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.RequiresRestart || !result.RestartDeferred {
		t.Fatalf("a stale revision must keep the restart prompt: %+v", result)
	}
}

func TestImportReportsAFailedDeferredApply(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": true,
			"restartDeferred": true, "applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "apply-failed"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token", ExpectedAccountKey: AccountScope(home)})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.State != ImportStateFailed || result.Reason != "native_import_apply_failed" {
		t.Fatalf("result: %+v", result)
	}
	if result.RequiresRestart || result.RestartDeferred {
		t.Fatalf("a failed apply keeps its own failure reason: %+v", result)
	}
}

// A deferred reset that applies a moment later continues the same import: the
// disconnect wrote its bytes, the apply completes them, and no Kimi restart is
// needed.
func TestImportContinuesAfterADeferredResetApplies(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount(baseURL, "byok-9f2c1d-kimi")))

	disconnected := kthTestStore(account, kthTestAccount(baseURL, nil))
	next := disconnected["accounts"].(map[string]any)[account].(map[string]any)
	next["sources"] = []any{}
	next["active"] = map[string]any{"providers": []any{}, "models": []any{}}
	next["blockedAliases"] = map[string]any{"byok-kth-kimi/kimi-a": "disconnected"}
	document, err := json.Marshal(disconnected)
	if err != nil {
		t.Fatal(err)
	}
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		kthDisconnectReply: map[string]any{"ok": true, "status": "deferred",
			"applyScopeToken": "s1", "applyRevision": "a2"},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}, {"ok": true, "status": "applied"}}}
	var writeErr error
	page.onKthDisconnect = func() {
		writeErr = os.WriteFile(filepath.Join(home, NativeWorkStore), document, 0o600)
	}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if !result.OK || result.State != ImportStateAccepted || result.RequiresRestart {
		t.Fatalf("result: %+v", result)
	}
	if page.kthDisconnectCalls != 1 || page.importCalls != 1 {
		t.Fatalf("disconnects = %d imports = %d", page.kthDisconnectCalls, page.importCalls)
	}
	if page.applyPendingCalls != 2 {
		t.Fatalf("applyPending polls = %d", page.applyPendingCalls)
	}
	blocked := kthTestAccountOf(t, home)["blockedAliases"].(map[string]any)
	if _, present := blocked["byok-kth-kimi/kimi-a"]; present {
		t.Fatalf("the alias the reset blocked was not cleared: %v", blocked)
	}
}

func TestImportStopsWhenTheDeferredResetNeverApplies(t *testing.T) {
	shortDeferredApply(t, 30*time.Millisecond)
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	before := kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount(baseURL, "byok-9f2c1d-kimi")))
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		kthDisconnectReply: map[string]any{"ok": true, "status": "deferred",
			"applyScopeToken": "s1", "applyRevision": "a2"},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateRequiresRestart || !result.RequiresRestart || !result.RestartDeferred ||
		result.Reason != "model_provider_reset_pending" {
		t.Fatalf("result: %+v", result)
	}
	if page.importCalls != 0 {
		t.Fatalf("the import ran against an unapplied reset: %d", page.importCalls)
	}
	after, err := os.ReadFile(filepath.Join(home, NativeWorkStore))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("the store was written while the official reset was still deferred")
	}
}

func TestImportStopsWhenTheDeferredResetSeesAnotherAccount(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount(baseURL, "byok-9f2c1d-kimi")))
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		kthDisconnectReply: map[string]any{"ok": true, "status": "deferred",
			"applyScopeToken": "s1", "applyRevision": "a2"},
		applyPendingReplies: []map[string]any{{"ok": false, "code": "account-changed"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.State != ImportStateAccountChanged || result.Reason != NativeAccountChanged {
		t.Fatalf("result: %+v", result)
	}
	if page.importCalls != 0 {
		t.Fatalf("the import ran after the account changed: %d", page.importCalls)
	}
}

func TestImportStopsWhenTheDeferredResetCannotApply(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount(baseURL, "byok-9f2c1d-kimi")))
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		kthDisconnectReply: map[string]any{"ok": true, "status": "deferred",
			"applyScopeToken": "s1", "applyRevision": "a2"},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "apply-failed"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: baseURL, Token: "gateway-token", ExpectedAccountKey: account})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.State != ImportStateFailed || result.Reason != "native_import_provider_reset_failed" {
		t.Fatalf("result: %+v", result)
	}
	if page.importCalls != 0 {
		t.Fatalf("the import ran after a failed reset: %d", page.importCalls)
	}
}

// ---------------------------------------------------------------------------
// the same window on the Direct Mode entry point

func TestDirectImportAppliesADeferredSaveAfterTheReadyWindow(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		directImportReplies: []map[string]any{{"ok": true, "sourceId": "byok-src-1", "models": float64(1),
			"requiresRestart": true, "restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}, {"ok": true, "status": "applied"}}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:63501/v1"

	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("result: %+v", result)
	}
	if result.RequiresRestart || result.RestartDeferred || result.RouteCleanupPending {
		t.Fatalf("an applied retry must clear the restart and cleanup claims: %+v", result)
	}
	if page.applyPendingCalls != 2 || page.gatewayCleanupCalls != 1 {
		t.Fatalf("applyPending polls = %d gateway cleanups = %d", page.applyPendingCalls, page.gatewayCleanupCalls)
	}
	if page.directImports != 1 {
		t.Fatalf("direct imports = %d", page.directImports)
	}
}

func TestDirectImportKeepsTheRestartPromptWhenTheDeferredApplyNeverLands(t *testing.T) {
	shortDeferredApply(t, 30*time.Millisecond)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		directImportReplies: []map[string]any{{"ok": true, "sourceId": "byok-src-1", "models": float64(1),
			"requiresRestart": true, "restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:63501/v1"

	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.RequiresRestart || !result.RestartDeferred || !result.RouteCleanupPending {
		t.Fatalf("a window that never settles must keep the restart prompt: %+v", result)
	}
	if page.gatewayCleanupCalls != 0 {
		t.Fatalf("the gateway was disconnected before the apply landed: %d", page.gatewayCleanupCalls)
	}
}

func TestDirectImportStopsWhenTheDeferredApplySeesAnotherAccount(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		directImportReplies: []map[string]any{{"ok": true, "sourceId": "byok-src-1", "models": float64(1),
			"requiresRestart": true, "restartDeferred": true, "applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": false, "code": "account-changed"}}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)

	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.State != ImportStateAccountChanged || result.Reason != NativeAccountChanged {
		t.Fatalf("result: %+v", result)
	}
}

func TestDirectImportReportsAFailedDeferredApply(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		directImportReplies: []map[string]any{{"ok": true, "sourceId": "byok-src-1", "models": float64(1),
			"requiresRestart": true, "restartDeferred": true, "applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": false, "code": "apply-failed"}}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)

	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.State != ImportStateFailed || result.Reason != "native_import_apply_failed" {
		t.Fatalf("result: %+v", result)
	}
	if result.RequiresRestart || result.RestartDeferred {
		t.Fatalf("a failed apply keeps its own failure reason: %+v", result)
	}
}

func TestImportKeepsTheCleanupClaimWhenTheRetriedCleanupFails(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": true,
			"restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "applied"}},
		directCleanupReply:  map[string]any{"ok": false, "code": "disconnect-refused"}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token",
		ExpectedAccountKey: AccountScope(home), DisconnectDirectSourceID: "direct-source"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.RequiresRestart || result.RestartDeferred {
		t.Fatalf("the replacement applied: %+v", result)
	}
	if !result.RouteCleanupPending {
		t.Fatalf("a refused cleanup must stay pending: %+v", result)
	}
	if page.directCleanupCalls != 1 {
		t.Fatalf("direct cleanup calls = %d", page.directCleanupCalls)
	}
}

func TestDirectImportKeepsTheCleanupClaimWhenTheRetriedCleanupFails(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		directImportReplies: []map[string]any{{"ok": true, "sourceId": "byok-src-1", "models": float64(1),
			"requiresRestart": true, "restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "applied"}},
		gatewayCleanupReply: map[string]any{"ok": false, "code": "foreign-source"}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:63501/v1"

	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.RequiresRestart || result.RestartDeferred {
		t.Fatalf("the replacement applied: %+v", result)
	}
	if !result.RouteCleanupPending {
		t.Fatalf("an unowned gateway must keep the cleanup claim: %+v", result)
	}
	if page.gatewayCleanupCalls != 1 {
		t.Fatalf("gateway cleanup calls = %d", page.gatewayCleanupCalls)
	}
}

// A route cleanup answered inside Electron's post-ready window carries the
// revision its disconnect left behind. The import settles it through the same
// bounded retry the save itself uses, so the old Direct Mode source disappears
// without the restart its deferred answer asked for.
func TestImportSettlesADeferredDirectCleanupAfterTheReplacement(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": false,
			"routeCleanupPending": true, "cleanupApplyScopeToken": "s1", "cleanupApplyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}, {"ok": true, "status": "applied"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token",
		ExpectedAccountKey: AccountScope(home), DisconnectDirectSourceID: "direct-source"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("result: %+v", result)
	}
	if result.RequiresRestart || result.RestartDeferred || result.RouteCleanupPending {
		t.Fatalf("an applied cleanup retry must clear every pending claim: %+v", result)
	}
	if page.applyPendingCalls != 2 {
		t.Fatalf("applyPending polls = %d, want the deferred cleanup answer and the applied one", page.applyPendingCalls)
	}
	if page.directCleanupCalls != 0 {
		t.Fatalf("a settled cleanup must not be run again: %d", page.directCleanupCalls)
	}
}

func TestImportKeepsTheCleanupRestartWhenTheDirectCleanupStaysDeferred(t *testing.T) {
	shortDeferredApply(t, 30*time.Millisecond)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": false,
			"routeCleanupPending": true, "cleanupApplyScopeToken": "s1", "cleanupApplyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token",
		ExpectedAccountKey: AccountScope(home), DisconnectDirectSourceID: "direct-source"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.RouteCleanupPending || !result.RequiresRestart || !result.RestartDeferred {
		t.Fatalf("a cleanup that never applies keeps its restart prompt: %+v", result)
	}
	if page.applyPendingCalls < 2 {
		t.Fatalf("the cleanup retry did not poll: %d", page.applyPendingCalls)
	}
}

// The save's own deferred answer is applied, and only then is the old source
// disconnected; that disconnect can land deferred in turn and is settled the
// same way instead of leaving the removal to the next sync.
func TestImportSettlesADeferredDisconnectAfterADeferredSave(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": true,
			"restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}, {"ok": true, "status": "applied"}},
		directCleanupReply: map[string]any{"ok": true, "settled": false,
			"applyScopeToken": "s1", "applyRevision": "r3"}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token",
		ExpectedAccountKey: AccountScope(home), DisconnectDirectSourceID: "direct-source"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.RequiresRestart || result.RestartDeferred || result.RouteCleanupPending {
		t.Fatalf("both the save and the cleanup landed: %+v", result)
	}
	if page.directCleanupCalls != 1 {
		t.Fatalf("direct cleanup calls = %d", page.directCleanupCalls)
	}
	if page.applyPendingCalls != 3 {
		t.Fatalf("applyPending polls = %d, want two for the save and one for the cleanup", page.applyPendingCalls)
	}
}

func TestImportKeepsTheCleanupRestartWhenTheDeferredDisconnectNeverApplies(t *testing.T) {
	shortDeferredApply(t, 30*time.Millisecond)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		importReplies: []map[string]any{{"ok": true, "models": float64(2), "requiresRestart": true,
			"restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		// The save applies on the first poll; the cleanup's own retry is then
		// answered by a busy page, which keeps the disconnect deferred.
		applyPendingReplies: []map[string]any{{"ok": true, "status": "applied"},
			{"ok": false, "code": "byok-busy"}},
		directCleanupReply: map[string]any{"ok": true, "settled": false,
			"applyScopeToken": "s1", "applyRevision": "r3"}}

	result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
		URL: "http://127.0.0.1:9100/v1", Token: "gateway-token",
		ExpectedAccountKey: AccountScope(home), DisconnectDirectSourceID: "direct-source"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.RouteCleanupPending || !result.RequiresRestart || !result.RestartDeferred {
		t.Fatalf("a deferred disconnect must keep its own restart prompt: %+v", result)
	}
	if page.applyPendingCalls != 2 || page.directCleanupCalls != 1 {
		t.Fatalf("applyPending polls = %d cleanups = %d", page.applyPendingCalls, page.directCleanupCalls)
	}
}

func TestImportRequiresRestartWhenCleanupApplyFailsAfterDeferredSave(t *testing.T) {
	for _, mode := range []string{IntegrationModeGateway, IntegrationModeDirect} {
		t.Run(mode, func(t *testing.T) {
			shortDeferredApply(t, time.Second)
			home := kthTestHome(t)
			save := map[string]any{"ok": true, "sourceId": directSourceID, "requiresRestart": true,
				"restartDeferred": true, "routeCleanupPending": true, "applyScopeToken": "s1", "applyRevision": "r2"}
			cleanup := map[string]any{"ok": true, "settled": false, "applyScopeToken": "s1", "applyRevision": "r3"}
			page := &fakePage{surface: "work", actions: true, importAPI: true,
				importReplies: []map[string]any{save}, directImportReplies: []map[string]any{save},
				directCleanupReply: cleanup, gatewayCleanupReply: cleanup,
				applyPendingReplies: []map[string]any{{"ok": true, "status": "applied"}, {"ok": true, "status": "apply-failed"}}}
			ctx := kthImportContext(t)
			var result ImportResult
			var err error
			if mode == IntegrationModeDirect {
				harness := newHarness(t, page)
				actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
				request := validDirectImportRequest()
				request.ExpectedAccountKey = AccountScope(home)
				request.DisconnectGateway = true
				request.DisconnectGatewayURL = "http://127.0.0.1:9100/v1"
				result, err = actions.ImportDirect(ctx, request)
			} else {
				result, err = kthImportActions(t, home, page).ImportModels(ctx, ImportRequest{
					URL: "http://127.0.0.1:9100/v1", Token: "fixture", ExpectedAccountKey: AccountScope(home),
					DisconnectDirectSourceID: directSourceID})
			}
			if err != nil {
				t.Fatal(err)
			}
			if !result.OK || !result.RouteCleanupPending || !result.RequiresRestart || result.RestartDeferred {
				t.Fatalf("failed cleanup must keep its restart prompt without marking it deferred: %+v", result)
			}
			if page.applyPendingCalls != 2 || page.directCleanupCalls+page.gatewayCleanupCalls != 1 {
				t.Fatalf("applyPending calls = %d, direct cleanups = %d, gateway cleanups = %d",
					page.applyPendingCalls, page.directCleanupCalls, page.gatewayCleanupCalls)
			}
		})
	}
}

// The same settlement on the Direct Mode entry point: the gateway disconnect
// deferred by the post-ready window is applied in place.
func TestDirectImportSettlesADeferredGatewayCleanupAfterTheReplacement(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		directImportReplies: []map[string]any{{"ok": true, "sourceId": "byok-src-1", "models": float64(1),
			"requiresRestart": false, "routeCleanupPending": true,
			"cleanupApplyScopeToken": "s1", "cleanupApplyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}, {"ok": true, "status": "applied"}}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:63501/v1"

	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.RequiresRestart || result.RestartDeferred || result.RouteCleanupPending {
		t.Fatalf("an applied cleanup retry must clear every pending claim: %+v", result)
	}
	if page.applyPendingCalls != 2 {
		t.Fatalf("applyPending polls = %d", page.applyPendingCalls)
	}
	if page.gatewayCleanupCalls != 0 {
		t.Fatalf("a settled cleanup must not be run again: %d", page.gatewayCleanupCalls)
	}
}

func TestDirectImportKeepsTheCleanupRestartWhenTheGatewayCleanupStaysDeferred(t *testing.T) {
	shortDeferredApply(t, 30*time.Millisecond)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		directImportReplies: []map[string]any{{"ok": true, "sourceId": "byok-src-1", "models": float64(1),
			"requiresRestart": false, "routeCleanupPending": true,
			"cleanupApplyScopeToken": "s1", "cleanupApplyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:63501/v1"

	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.RouteCleanupPending || !result.RequiresRestart || !result.RestartDeferred {
		t.Fatalf("a cleanup that never applies keeps its restart prompt: %+v", result)
	}
}

func TestDirectImportSettlesADeferredDisconnectAfterADeferredSave(t *testing.T) {
	shortDeferredApply(t, time.Second)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		directImportReplies: []map[string]any{{"ok": true, "sourceId": "byok-src-1", "models": float64(1),
			"requiresRestart": true, "restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "deferred"}, {"ok": true, "status": "applied"}},
		gatewayCleanupReply: map[string]any{"ok": true, "settled": false,
			"applyScopeToken": "s1", "applyRevision": "r3"}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:63501/v1"

	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.RequiresRestart || result.RestartDeferred || result.RouteCleanupPending {
		t.Fatalf("both the save and the cleanup landed: %+v", result)
	}
	if page.gatewayCleanupCalls != 1 || page.applyPendingCalls != 3 {
		t.Fatalf("gateway cleanups = %d applyPending polls = %d",
			page.gatewayCleanupCalls, page.applyPendingCalls)
	}
}

func TestDirectImportKeepsTheCleanupRestartWhenTheDeferredDisconnectNeverApplies(t *testing.T) {
	shortDeferredApply(t, 30*time.Millisecond)
	home := kthTestHome(t)
	page := &fakePage{surface: "work", actions: true, importAPI: true,
		directImportReplies: []map[string]any{{"ok": true, "sourceId": "byok-src-1", "models": float64(1),
			"requiresRestart": true, "restartDeferred": true, "routeCleanupPending": true,
			"applyScopeToken": "s1", "applyRevision": "r2"}},
		applyPendingReplies: []map[string]any{{"ok": true, "status": "applied"},
			{"ok": false, "code": "byok-busy"}},
		gatewayCleanupReply: map[string]any{"ok": true, "settled": false,
			"applyScopeToken": "s1", "applyRevision": "r3"}}
	harness := newHarness(t, page)
	actions := NewCDPActionsForMode(harness.pipe, home, home, IntegrationModeDirect, directSourceID)
	request := validDirectImportRequest()
	request.ExpectedAccountKey = AccountScope(home)
	request.DisconnectGateway = true
	request.DisconnectGatewayURL = "http://127.0.0.1:63501/v1"

	result, err := actions.ImportDirect(kthImportContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.RouteCleanupPending || !result.RequiresRestart || !result.RestartDeferred {
		t.Fatalf("a deferred disconnect must keep its own restart prompt: %+v", result)
	}
	if page.applyPendingCalls != 2 || page.gatewayCleanupCalls != 1 {
		t.Fatalf("applyPending polls = %d cleanups = %d", page.applyPendingCalls, page.gatewayCleanupCalls)
	}
}
