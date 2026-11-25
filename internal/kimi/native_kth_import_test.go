package kimi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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
