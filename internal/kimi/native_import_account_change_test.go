package kimi

import (
	"context"
	"path/filepath"
	"testing"
)

type changingImportActions struct {
	*fakeActions
	onImport func(ImportRequest)
}

func (a *changingImportActions) ImportModels(ctx context.Context, request ImportRequest) (ImportResult, error) {
	result, err := a.fakeActions.ImportModels(ctx, request)
	if a.onImport != nil {
		a.onImport(request)
	}
	return result, err
}

func TestControllerImportRejectsAccountChangedWhileWaiting(t *testing.T) {
	accounts := newSwitchedAccounts(t)
	profile, home := nativeProfile(t, accounts.newUID)
	fixture := &nativeFixture{dir: home, state: filepath.Dir(profile)}
	const url = "http://127.0.0.1:9999/v1"
	fixture.seedSwitchedStore(t, accounts, url, url)
	fixture.seedOldIntent(t, accounts, url)
	setIdentity := func(userID string) {
		config := fixture.readJSON(t, filepath.Join(home, NativeWorkConfig))
		config["credentials"].(map[string]any)["kimiWeb"] = map[string]any{"userId": userID}
		fixture.writeJSON(t, filepath.Join(home, NativeWorkConfig), config)
	}
	setIdentity(accounts.newUID)
	controller, err := NewController(ControllerSettings{StateDir: fixture.state,
		UserDataDir: profile, GatewayURL: url, Models: testCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	actions := &changingImportActions{
		fakeActions: &fakeActions{connected: true, imported: ImportResult{OK: true, Models: 2}},
		onImport: func(request ImportRequest) {
			if request.ExpectedAccountKey != accounts.newScope {
				t.Errorf("import expected scope %q, want %q", request.ExpectedAccountKey, accounts.newScope)
			}
			setIdentity(accounts.oldUID)
		},
	}
	bridge, err := StartBridgeHost(home, 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	controller.mu.Lock()
	controller.bridge = bridge
	controller.mu.Unlock()

	result, err := controller.ImportModelsAuthorized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateAccountChanged || result.Applied || result.Adopted {
		t.Fatalf("an import for a different account was accepted: %+v", result)
	}
	if result.Reason != NativeAccountChanged {
		t.Fatalf("reason %q, want %q", result.Reason, NativeAccountChanged)
	}
}
