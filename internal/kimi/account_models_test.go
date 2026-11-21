package kimi

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type accountModelActions struct {
	*fakeAccountPage
	*changingImportActions
	config   map[string]any
	modelErr error
}

func (a *accountModelActions) accountModelConfig(context.Context, accountPage) (accountModelConfig, error) {
	if a.modelErr != nil {
		return accountModelConfig{}, a.modelErr
	}
	return accountModelConfig{providers: section(a.config, "providers"),
		models: section(a.config, "models"), credentialPublished: true}, nil
}

func accountModelsHarness(t *testing.T, targetProfile bool) (*accountHarness, *accountModelActions, *nativeFixture) {
	t.Helper()
	h := newAccountHarness(t)
	f := &nativeFixture{dir: h.home, state: h.stateDir}
	accounts := newSwitchedAccounts(t)
	const url = "http://127.0.0.1:9999/v1"
	f.seedSwitchedStore(t, accounts, url, url)
	f.seedOldIntent(t, accounts, url)
	config := h.readConfig(t)
	credentials := config["credentials"].(map[string]any)
	credentials["cred-1"] = map[string]any{"apiKey": "local-gateway-token"}
	credentials["kimiWeb"] = map[string]any{"userId": accounts.oldUID}
	h.writeConfig(t, config)
	if !targetProfile {
		store := f.readJSON(t, filepath.Join(h.home, NativeWorkStore))
		delete(store["profiles"].(map[string]any), accounts.newKey)
		f.writeJSON(t, filepath.Join(h.home, NativeWorkStore), store)
	}
	h.controller.settings.GatewayURL = url
	h.controller.settings.GatewayToken = "local-gateway-token"
	h.controller.settings.Models = testCatalog()
	h.page.put("kimi-1", "msh_user_id", accounts.oldUID)
	h.page.runtimeWatch, h.page.runtimeTransition = true, true
	h.page.onActivation = func() {
		config := h.readConfig(t)
		config["credentials"].(map[string]any)["kimiWeb"] = map[string]any{"userId": h.page.user["work"]}
		h.writeConfig(t, config)
	}
	a := &accountModelActions{fakeAccountPage: h.page,
		changingImportActions: &changingImportActions{fakeActions: &fakeActions{
			connected: true, imported: ImportResult{OK: true, Models: 2, RequiresRestart: true},
		}}}
	a.onImport = func(request ImportRequest) {
		if request.ExpectedAccountKey != accounts.newScope || !request.OpenSettings {
			t.Errorf("import was not authorized for the target account")
		}
		store := f.readJSON(t, filepath.Join(h.home, NativeWorkStore))
		store["profiles"].(map[string]any)[accounts.newKey] = map[string]any{"byok-kth": profileRow(url)}
		f.writeJSON(t, filepath.Join(h.home, NativeWorkStore), store)
		a.config = section(h.readConfig(t), "model")
		a.mu.Lock()
		a.runtimeChanged = true
		a.mu.Unlock()
	}
	a.config = section(h.readConfig(t), "model")
	h.controller.actions = a
	bridge, err := StartBridgeHost(h.home, 1, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	h.controller.bridge = bridge
	return h, a, f
}

func activateModelAccount(t *testing.T, h *accountHarness, user string) AccountActivation {
	t.Helper()
	restarts := 0
	restartAccountProcess = func(*Controller, context.Context) error { restarts++; return nil }
	result, err := h.controller.ActivateAccount(context.Background(), AccountSession{
		SchemaVersion: 1, UserID: user, KimiWeb: map[string]any{"userId": user},
	})
	if err != nil || !result.Hot || !result.Verified || result.RolledBack || restarts != 0 {
		t.Fatalf("identity activation: %+v err=%v restarts=%d", result, err, restarts)
	}
	return result
}

func TestAccountModelsImportMissingCatalogOnceWithoutDesktopRestart(t *testing.T) {
	h, a, _ := accountModelsHarness(t, false)
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "ready" || result.ModelsReason != "" || len(a.imports) != 1 {
		t.Fatalf("missing target catalog: state=%s reason=%s imports=%d", result.ModelsState, result.ModelsReason, len(a.imports))
	}
	for _, user := range []string{"account-old", "account-new"} {
		result = activateModelAccount(t, h, user)
		if result.ModelsState != "ready" || len(a.imports) != 1 {
			t.Fatalf("catalog reimported on return: state=%s imports=%d", result.ModelsState, len(a.imports))
		}
	}
}

func TestAccountModelsReuseOnlyMatchingTargetCatalog(t *testing.T) {
	h, a, f := accountModelsHarness(t, true)
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "ready" || len(a.imports) != 0 {
		t.Fatalf("matching target: state=%s imports=%d", result.ModelsState, len(a.imports))
	}
	intent, err := f.integration(t).readIntent()
	if err != nil || intent.AccountDigest != newSwitchedAccounts(t).newDigest {
		t.Fatal("verified catalog did not rebind the integration to its account")
	}
}

func TestAccountModelsDoNotClaimReadyFromFilesAlone(t *testing.T) {
	h, a, _ := accountModelsHarness(t, true)
	a.modelErr = errors.New(AccountErrUnavailable)
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "pending" || result.ModelsReason != "account_models_runtime_unverified" || len(a.imports) != 0 {
		t.Fatalf("unreadable runtime: state=%s reason=%s imports=%d", result.ModelsState, result.ModelsReason, len(a.imports))
	}
}

func TestAccountModelsFailureDoesNotRollBackSuccessfulIdentity(t *testing.T) {
	h, a, _ := accountModelsHarness(t, false)
	a.imported = ImportResult{OK: false, Reason: NativeContractChanged}
	a.onImport = nil
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "failed" || result.ModelsReason != NativeContractChanged || !accountConfigMatches(h.home, "account-new") {
		t.Fatalf("failed catalog affected identity: %+v", result)
	}
}

func TestAccountModelsDoNotStartFirstImportWithoutActiveIntent(t *testing.T) {
	h, a, f := accountModelsHarness(t, false)
	intent, err := f.integration(t).readIntent()
	if err != nil {
		t.Fatal(err)
	}
	intent.Active = false
	if err := f.integration(t).writeIntent(intent); err != nil {
		t.Fatal(err)
	}
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "" || len(a.imports) != 0 {
		t.Fatalf("inactive integration imported a catalog: state=%s imports=%d", result.ModelsState, len(a.imports))
	}
}

func TestReconcileConfirmsRunningCatalogAfterGenerationChanges(t *testing.T) {
	h, a, _ := accountModelsHarness(t, true)
	child, _ := startFakeChild(t)
	h.controller.child = child
	t.Cleanup(func() { _ = child.Stop(time.Second) })
	a.modelErr = errors.New(AccountErrUnavailable)
	result := h.controller.ReconcileWithReadback(context.Background())
	if !result.Applied || result.Adopted {
		t.Fatalf("catalog files alone established adoption: %+v", result)
	}
	a.modelErr = nil
	h.page.user["work"] = "account-new"
	result = h.controller.ReconcileWithReadback(context.Background())
	if !result.Applied || result.Adopted {
		t.Fatalf("catalog was adopted with another page identity: %+v", result)
	}
	h.page.user["work"] = "account-old"
	result = h.controller.ReconcileWithReadback(context.Background())
	if !result.Applied || !result.Adopted || len(a.imports) != 0 {
		t.Fatalf("matching runtime did not confirm adoption: %+v imports=%d", result, len(a.imports))
	}
}

func TestReconcileKeepsDeferredRestartWithMatchingRunningCatalog(t *testing.T) {
	h, a, _ := accountModelsHarness(t, true)
	child, _ := startFakeChild(t)
	h.controller.child = child
	t.Cleanup(func() { _ = child.Stop(time.Second) })
	before := h.controller.ReconcileWithReadback(context.Background())
	if !before.Applied || !before.Adopted {
		t.Fatalf("fixture could not confirm running catalog: %+v", before)
	}
	a.onImport = nil
	a.imported = ImportResult{OK: true, Models: 2, RequiresRestart: true, RestartDeferred: true}
	result, err := h.controller.ImportModelsAuthorized(context.Background())
	if err != nil || !result.Applied || result.Adopted || !result.RequiresRestart || result.Readback.Generation != before.Readback.Generation {
		t.Fatalf("same-generation deferred import: %+v, %v", result, err)
	}
	if polled := h.controller.ReconcileWithReadback(context.Background()); !polled.Applied || polled.Adopted {
		t.Fatalf("an unchanged catalog and ready runtime cleared deferred restart: %+v", polled)
	}
	a.imported = ImportResult{OK: true, Models: 2}
	if result, err = h.controller.ImportModelsAuthorized(context.Background()); err != nil || !result.Applied || result.RequiresRestart {
		t.Fatalf("successful reimport: %+v, %v", result, err)
	}
	if polled := h.controller.ReconcileWithReadback(context.Background()); !polled.Adopted {
		t.Fatalf("running catalog could not recover after successful reimport: %+v", polled)
	}
}

func TestAccountModelsRejectPartialRuntimeCatalog(t *testing.T) {
	h, a, _ := accountModelsHarness(t, true)
	delete(section(a.config, "models"), "byok-kth-resp/gpt-probe-model")
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "pending" || len(a.imports) != 0 {
		t.Fatalf("partial runtime catalog: state=%s imports=%d", result.ModelsState, len(a.imports))
	}
}

func TestAccountModelsRejectDifferentDaemonModelOrProvider(t *testing.T) {
	for _, test := range []struct {
		name    string
		section string
		id      string
		field   string
		value   any
	}{
		{"provider-url", "providers", "byok-kth-kimi", "baseUrl", "https://other.example/v1"},
		{"provider-type", "providers", "byok-kth-kimi", "type", "openai-responses"},
		{"provider-credential", "providers", "byok-kth-kimi", "credential", "other-credential"},
		{"model-id", "models", "byok-kth-kimi/kimi-probe-model", "model", "other-model"},
		{"context-size", "models", "byok-kth-kimi/kimi-probe-model", "maxContextSize", float64(1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, a, _ := accountModelsHarness(t, true)
			row := section(a.config, test.section)[test.id].(map[string]any)
			row[test.field] = test.value
			result := activateModelAccount(t, h, "account-new")
			if result.ModelsState != "pending" || len(a.imports) != 0 {
				t.Fatalf("different daemon catalog: state=%s imports=%d", result.ModelsState, len(a.imports))
			}
		})
	}
}

func TestAccountModelsRequireFreshRuntimeAfterImport(t *testing.T) {
	h, a, _ := accountModelsHarness(t, false)
	install := a.onImport
	a.onImport = func(request ImportRequest) {
		install(request)
		a.mu.Lock()
		a.runtimeChanged = false
		a.mu.Unlock()
	}
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "pending" || result.ModelsReason != "account_models_runtime_unverified" || len(a.imports) != 1 {
		t.Fatalf("import without a runtime transition: state=%s reason=%s imports=%d", result.ModelsState, result.ModelsReason, len(a.imports))
	}
}

func TestAccountModelsRefreshDifferentCredentialThroughOfficialImport(t *testing.T) {
	h, a, _ := accountModelsHarness(t, true)
	config := h.readConfig(t)
	config["credentials"].(map[string]any)["cred-1"] = map[string]any{"apiKey": "other-token"}
	h.writeConfig(t, config)
	install := a.onImport
	a.onImport = func(request ImportRequest) {
		install(request)
		config := h.readConfig(t)
		config["credentials"].(map[string]any)["cred-1"] = map[string]any{"apiKey": "local-gateway-token"}
		h.writeConfig(t, config)
	}
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "ready" || len(a.imports) != 1 {
		t.Fatalf("credential refresh: state=%s imports=%d", result.ModelsState, len(a.imports))
	}
}

func TestAccountModelsPreserveForeignGeneratedProvider(t *testing.T) {
	h, a, f := accountModelsHarness(t, true)
	config := h.readConfig(t)
	config["model"].(map[string]any)["providers"].(map[string]any)["byok-kth-kimi"].(map[string]any)["baseUrl"] = "https://other.example/v1"
	h.writeConfig(t, config)
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "failed" || result.ModelsReason != "foreign_provider" || len(a.imports) != 0 {
		t.Fatalf("foreign provider: state=%s reason=%s imports=%d", result.ModelsState, result.ModelsReason, len(a.imports))
	}
	intent, _ := f.integration(t).readIntent()
	if intent.AccountDigest != newSwitchedAccounts(t).oldDigest {
		t.Fatal("foreign provider moved ownership")
	}
}

func TestAccountModelsWaitForExistingPendingImport(t *testing.T) {
	h, a, f := accountModelsHarness(t, true)
	store := f.readJSON(t, filepath.Join(h.home, NativeWorkStore))
	profile := store["profiles"].(map[string]any)[newSwitchedAccounts(t).newScope].(map[string]any)["byok-kth"].(map[string]any)
	profile["pending"] = map[string]any{"rev": 2}
	f.writeJSON(t, filepath.Join(h.home, NativeWorkStore), store)
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "pending" || result.ModelsReason != "native_profile_pending" || len(a.imports) != 0 {
		t.Fatalf("pending native transaction: state=%s reason=%s imports=%d", result.ModelsState, result.ModelsReason, len(a.imports))
	}
}

func TestAccountModelsRejectAccountChangeDuringImport(t *testing.T) {
	h, a, f := accountModelsHarness(t, false)
	install := a.onImport
	a.onImport = func(request ImportRequest) {
		install(request)
		h.page.put("kimi-1", "msh_user_id", "account-old")
		config := h.readConfig(t)
		config["credentials"].(map[string]any)["kimiWeb"] = map[string]any{"userId": "account-old"}
		h.writeConfig(t, config)
	}
	result := activateModelAccount(t, h, "account-new")
	if result.ModelsState != "failed" || result.ModelsReason != NativeAccountChanged {
		t.Fatalf("changed account accepted: state=%s reason=%s", result.ModelsState, result.ModelsReason)
	}
	intent, _ := f.integration(t).readIntent()
	if intent.AccountDigest != newSwitchedAccounts(t).oldDigest {
		t.Fatal("changed account moved ownership")
	}
}
