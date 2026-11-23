package kimi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A real account switch leaves the store with two profiles (one per account) and
// an intent that still records the old one. Kimi's own import then lands the
// models in the new account's profile, but nothing used to move the recorded
// account: the readback kept selecting the old profile and the reconcile kept
// answering `account_changed`, so a successful import was never reported.
//
// The levels are the ones the real files use, verified against a live
// `byok-providers.json` and `native-work-intent.json` pair:
//
//	uid                the identity Kimi signs in with
//	store key          sha256(uid), which is what `AccountScope` answers
//	scope digest       sha256(store key), what the intent records in both
//	                   `accountDigest` and `profileKeyDigest`
type switchedAccounts struct {
	oldUID     string
	newUID     string
	oldScope   string // AccountScope of the previous account, i.e. its store key
	oldKey     string // store key of the previous account
	oldDigest  string // what the intent records: sha256(store key)
	newScope   string // AccountScope of the current account, i.e. its store key
	newKey     string // store key of the current account
	newDigest  string // what the intent records: sha256(store key)
	credential string
}

func newSwitchedAccounts(t *testing.T) switchedAccounts {
	t.Helper()
	return switchedAccountsFor("account-old", "account-new", "cred-1")
}

// switchedAccountsFor builds the same three levels for any pair of identities.
func switchedAccountsFor(oldUID, newUID, credential string) switchedAccounts {
	oldKey, newKey := sha256Hex(oldUID), sha256Hex(newUID)
	return switchedAccounts{oldUID: oldUID, newUID: newUID,
		oldScope: oldKey, oldKey: oldKey, oldDigest: sha256Hex(oldKey),
		newScope: newKey, newKey: newKey, newDigest: sha256Hex(newKey),
		credential: credential}
}

// profileRow is one account's byok profile at `baseURL`.
func profileRow(baseURL string) map[string]any {
	return map[string]any{
		"kind": "kth", "id": "byok-kth", "baseUrl": baseURL, "credentialRef": "cred-1",
		"models": []any{
			map[string]any{"alias": "byok-kth-kimi/kimi-probe-model", "providerId": "byok-kth-kimi",
				"model": "kimi-probe-model", "family": "kimi", "maxContextSize": 131072},
			map[string]any{"alias": "byok-kth-resp/gpt-probe-model", "providerId": "byok-kth-resp",
				"model": "gpt-probe-model", "family": "responses", "maxContextSize": 262144},
		},
	}
}

// seedSwitchedStore writes both account profiles and a generated config that
// already carries the imported models, which is the state right after a native
// import into the current account.
func (f *nativeFixture) seedSwitchedStore(t *testing.T, accounts switchedAccounts, oldURL, newURL string) {
	t.Helper()
	f.writeJSON(t, filepath.Join(f.dir, NativeWorkStore), map[string]any{
		"v": 1, "profiles": map[string]any{
			accounts.oldKey: map[string]any{"byok-kth": profileRow(oldURL)},
			accounts.newKey: map[string]any{"byok-kth": profileRow(newURL)},
		},
	})
	f.writeJSON(t, filepath.Join(f.dir, NativeWorkConfig), map[string]any{
		"model": map[string]any{
			"providers": map[string]any{
				"byok-kth-kimi": map[string]any{"type": "kimi", "baseUrl": newURL, "credential": "cred-1"},
				"byok-kth-resp": map[string]any{"type": "openai_responses", "baseUrl": newURL, "credential": "cred-1"},
			},
			"models": map[string]any{
				"byok-kth-kimi/kimi-probe-model": map[string]any{"provider": "byok-kth-kimi",
					"model": "kimi-probe-model", "maxContextSize": float64(131072)},
				"byok-kth-resp/gpt-probe-model": map[string]any{"provider": "byok-kth-resp",
					"model": "gpt-probe-model", "maxContextSize": float64(262144)},
			},
		},
		"credentials": map[string]any{"cred-1": map[string]any{"apiKey": "synthetic-not-a-real-key"}},
	})
}

// seedOldIntent records an intent owned by the previous account, pointing at the
// previous account's profile, exactly as a completed import for that account
// would have left it. Both recorded digests live one hash above the store key,
// which is the level `Reconcile` computes from `AccountScope`.
func (f *nativeFixture) seedOldIntent(t *testing.T, accounts switchedAccounts, oldURL string) {
	t.Helper()
	intent := &nativeIntent{
		SchemaVersion: NativeWorkSchema, SourceID: "gateway", OwnerID: "owner-1",
		KimiHome: f.dir, URL: oldURL,
		AccountDigest: accounts.oldDigest, ProfileKeyDigest: accounts.oldDigest,
		CredentialRef: accounts.credential, Aliases: map[string]string{},
		Entries:    ownership{Providers: map[string]ownedEntry{}, Models: map[string]ownedEntry{}},
		Active:     true,
		Generation: 3,
		UpdatedAt:  "2026-11-17T00:00:00Z",
	}
	f.writeJSON(t, filepath.Join(f.state, NativeWorkIntent), intent)
}

// TestNativeScopeBeatsAStaleProfileIntent covers the selection rule: when the
// current account has its own profile, a stale intent naming another account's
// profile must not win.
func TestNativeScopeBeatsAStaleProfileIntent(t *testing.T) {
	fixture := newNativeFixture(t)
	accounts := newSwitchedAccounts(t)
	fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:9999/v1", "http://127.0.0.1:9999/v1")
	fixture.seedOldIntent(t, accounts, "http://127.0.0.1:9999/v1")

	store := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkStore))
	intent, err := fixture.integration(t).readIntent()
	if err != nil || intent == nil {
		t.Fatalf("intent: %v %+v", err, intent)
	}
	// The scope Reconcile computes, i.e. one hash above the store key.
	scopeDigest := sha256Hex(accounts.newScope)
	chosen, ambiguous := selectProfile(kthProfiles(store), scopeDigest, intent)
	if ambiguous {
		t.Fatal("the current account has an exact profile and was called ambiguous")
	}
	if chosen.key != accounts.newKey {
		t.Fatalf("selected %s, want the current account's profile", sha256Hex(chosen.key)[:12])
	}
}

// TestNativeReadbackSelectsTheCurrentAccountsProfile covers the second half of
// the same bug: the readback passed the raw account scope where the selector
// expects the double-hashed one, so it never matched the current profile.
func TestNativeReadbackSelectsTheCurrentAccountsProfile(t *testing.T) {
	fixture := newNativeFixture(t)
	accounts := newSwitchedAccounts(t)
	fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:1/v1", "http://127.0.0.1:9999/v1")
	fixture.seedOldIntent(t, accounts, "http://127.0.0.1:1/v1")

	readback := fixture.integration(t).Readback(accounts.newScope)
	if readback.URL != "http://127.0.0.1:9999/v1" {
		t.Fatalf("readback reported the wrong account's profile: url=%q models=%d", readback.URL, readback.Models)
	}
	if readback.Models != 2 {
		t.Fatalf("readback models: %d", readback.Models)
	}
	if !readback.Holds("http://127.0.0.1:9999/v1") {
		t.Fatalf("the current account's profile was not held: %+v", readback)
	}
}

// TestNativePeriodicReconcileRefusesAnUnconfirmedSwitch is the safety half: a
// reconcile that did not watch an import must still refuse to move the account,
// and must not touch either profile.
func TestNativePeriodicReconcileRefusesAnUnconfirmedSwitch(t *testing.T) {
	fixture := newNativeFixture(t)
	accounts := newSwitchedAccounts(t)
	fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:9999/v1", "http://127.0.0.1:9999/v1")
	fixture.seedOldIntent(t, accounts, "http://127.0.0.1:9999/v1")
	before, err := os.ReadFile(filepath.Join(fixture.dir, NativeWorkStore))
	if err != nil {
		t.Fatal(err)
	}

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		AccountScope: accounts.newScope, URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State != IntegrationNeedsImport || result.Reason != "account_changed" {
		t.Fatalf("got %s/%s", result.State, result.Reason)
	}
	if result.NeedsNativeImport() {
		t.Fatal("an unconfirmed account switch was reported importable")
	}
	after, err := os.ReadFile(filepath.Join(fixture.dir, NativeWorkStore))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused switch wrote to the profile store")
	}
	intent, err := fixture.integration(t).readIntent()
	if err != nil || intent == nil {
		t.Fatalf("intent: %v", err)
	}
	if intent.AccountDigest != accounts.oldDigest {
		t.Fatal("a refused switch moved the recorded account")
	}
}

// TestNativeConfirmedImportRebindsTheCurrentAccount covers the fix: a reconcile
// that follows a successful import moves the recorded account to the current
// profile and reconciles against it.
func TestNativeConfirmedImportRebindsTheCurrentAccount(t *testing.T) {
	fixture := newNativeFixture(t)
	accounts := newSwitchedAccounts(t)
	fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:9999/v1", "http://127.0.0.1:9999/v1")
	fixture.seedOldIntent(t, accounts, "http://127.0.0.1:9999/v1")

	integration := fixture.integration(t)
	result := integration.Reconcile(ReconcileOptions{Enabled: true, AllowAccountRebind: true,
		AccountScope: accounts.newScope, URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State != IntegrationReady && result.State != IntegrationRepaired {
		t.Fatalf("got %s/%s", result.State, result.Reason)
	}
	if !result.Imported {
		t.Fatal("the rebound import was not reported in place")
	}
	if result.AccountDigest != accounts.newDigest {
		t.Fatal("the result did not carry the current account")
	}
	intent, err := integration.readIntent()
	if err != nil || intent == nil {
		t.Fatalf("intent: %v", err)
	}
	if intent.AccountDigest != accounts.newDigest {
		t.Fatal("the recorded account was not rebound to the current profile")
	}
	if intent.ProfileKeyDigest != accounts.newDigest {
		t.Fatal("the recorded profile was not rebound to the current profile")
	}
	if !intent.Active {
		t.Fatal("the rebound intent is not active")
	}

	// The confirmation is consumed: the periodic caller now agrees, because the
	// intent records the account it is looking at.
	again := integration.Reconcile(ReconcileOptions{Enabled: true,
		AccountScope: accounts.newScope, URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if again.State != IntegrationReady || again.Changed {
		t.Fatalf("periodic after rebind: %s/%s changed=%v", again.State, again.Reason, again.Changed)
	}
}

// TestNativeConfirmedImportRefusesAMismatchedScope proves the confirmation is
// not a licence to adopt any profile: a profile whose key is not the caller's
// account is still refused.
func TestNativeConfirmedImportRefusesAMismatchedScope(t *testing.T) {
	fixture := newNativeFixture(t)
	accounts := newSwitchedAccounts(t)
	fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:9999/v1", "http://127.0.0.1:9999/v1")
	fixture.seedOldIntent(t, accounts, "http://127.0.0.1:9999/v1")

	// The confirmed import names an account that matches neither profile: the
	// store has no exact candidate, so nothing may be adopted.
	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true, AllowAccountRebind: true,
		AccountScope: sha256Hex("account-third"), URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State == IntegrationReady || result.State == IntegrationRepaired {
		t.Fatalf("a profile for another account was adopted: %+v", result)
	}
	intent, err := fixture.integration(t).readIntent()
	if err != nil || intent == nil {
		t.Fatalf("intent: %v", err)
	}
	if intent.AccountDigest != accounts.oldDigest {
		t.Fatal("the recorded account moved to an unverified profile")
	}
}

// TestNativeConfirmedImportKeepsTheSafetyBoundaries proves the confirmation does
// not bypass the ordinary gates: a foreign provider, a pending profile and an
// unknown account are all still refused.
func TestNativeConfirmedImportKeepsTheSafetyBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, fixture *nativeFixture, accounts switchedAccounts)
		state   string
		reason  string
	}{
		{name: "foreign provider", state: IntegrationConflict, reason: "foreign_provider",
			prepare: func(t *testing.T, fixture *nativeFixture, accounts switchedAccounts) {
				path := filepath.Join(fixture.dir, NativeWorkConfig)
				config := fixture.readJSON(t, path)
				providers := config["model"].(map[string]any)["providers"].(map[string]any)
				providers["byok-kth-kimi"] = map[string]any{"type": "kimi",
					"baseUrl": "http://elsewhere.test/v1", "credential": "other"}
				fixture.writeJSON(t, path, config)
			}},
		{name: "pending profile", state: IntegrationPending, reason: "native_profile_pending",
			prepare: func(t *testing.T, fixture *nativeFixture, accounts switchedAccounts) {
				path := filepath.Join(fixture.dir, NativeWorkStore)
				store := fixture.readJSON(t, path)
				profile := store["profiles"].(map[string]any)[accounts.newKey].(map[string]any)["byok-kth"].(map[string]any)
				profile["pending"] = map[string]any{"models": []any{}}
				fixture.writeJSON(t, path, store)
			}},
		{name: "no account", state: IntegrationAccountUnknown, reason: "account_ambiguous",
			prepare: func(t *testing.T, fixture *nativeFixture, accounts switchedAccounts) {
				fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:9999/v1", "http://127.0.0.1:9999/v1")
			}},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			fixture := newNativeFixture(t)
			accounts := newSwitchedAccounts(t)
			fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:9999/v1", "http://127.0.0.1:9999/v1")
			if entry.name != "no account" {
				fixture.seedOldIntent(t, accounts, "http://127.0.0.1:9999/v1")
			}
			entry.prepare(t, fixture, accounts)
			scope := accounts.newScope
			if entry.name == "no account" {
				scope = ""
			}
			result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
				AllowAccountRebind: true, AccountScope: scope,
				URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
			if result.State != entry.state || result.Reason != entry.reason {
				t.Fatalf("got %s/%s, want %s/%s", result.State, result.Reason, entry.state, entry.reason)
			}
		})
	}
}

// TestNativeConfirmedImportRefusesADifferentURL proves the confirmation is about
// an account, not about a blank cheque: a confirmed import cannot carry the
// ownership across to a profile pointing somewhere else.
func TestNativeConfirmedImportRefusesADifferentURL(t *testing.T) {
	fixture := newNativeFixture(t)
	accounts := newSwitchedAccounts(t)
	// The current account's profile points at a URL that is not this gateway.
	fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:9999/v1", "http://127.0.0.1:7777/v1")
	fixture.seedOldIntent(t, accounts, "http://127.0.0.1:9999/v1")

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true, AllowAccountRebind: true,
		AccountScope: accounts.newScope, URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State == IntegrationReady || result.State == IntegrationRepaired {
		t.Fatalf("an import into another URL was adopted: %+v", result)
	}
	intent, err := fixture.integration(t).readIntent()
	if err != nil || intent == nil {
		t.Fatalf("intent: %v", err)
	}
	if intent.AccountDigest != accounts.oldDigest || intent.ProfileKeyDigest != accounts.oldDigest {
		t.Fatal("the recorded account moved to a profile pointing elsewhere")
	}
}

// TestControllerImportRebindsTheCurrentAccount drives the whole path: the
// controller's own profile has signed in as a new account, the official page
// accepts an import, and the readback must then report it as applied for that
// account instead of repeating `account_changed`.
func TestControllerImportRebindsTheCurrentAccount(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-new")
	// A completed import for the previous account, on the same files.
	accounts := switchedAccountsFor("user-old", "user-new", "cred-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	// Both accounts have a profile, as they do after a real switch: the previous
	// account still holds the old import, the current one was just imported into.
	store, err := json.Marshal(map[string]any{"v": 1, "profiles": map[string]any{
		accounts.oldKey: map[string]any{"byok-kth": profileRow("http://127.0.0.1:1/v1")},
		accounts.newKey: map[string]any{"byok-kth": profileRow("http://127.0.0.1:9999/v1")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiHome, NativeWorkStore), store, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(kimiHome, NativeWorkConfig))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(config, &document); err != nil {
		t.Fatal(err)
	}
	document["credentials"].(map[string]any)["kimiWeb"] = map[string]any{"userId": "user-new"}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiHome, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	intent := &nativeIntent{SchemaVersion: NativeWorkSchema, SourceID: "gateway", OwnerID: "owner-1",
		KimiHome: kimiHome, URL: "http://127.0.0.1:9999/v1",
		AccountDigest: accounts.oldDigest, ProfileKeyDigest: accounts.oldDigest,
		CredentialRef: "cred-1", Aliases: map[string]string{},
		Entries: ownership{Providers: map[string]ownedEntry{}, Models: map[string]ownedEntry{}},
		Active:  true, Generation: 2, UpdatedAt: "2026-11-17T00:00:00Z"}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(profile), NativeWorkIntent), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, GatewayURL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	// The periodic path refuses the unreconciled switch, as before.
	unconfirmed := controller.ReconcileWithReadback(context.Background())
	if unconfirmed.State != IntegrationNeedsImport || unconfirmed.Reason != "account_changed" {
		t.Fatalf("periodic: %s/%s", unconfirmed.State, unconfirmed.Reason)
	}

	// The official import lands the models in the current account, which is the
	// state the files are already in. The controller's own import call must then
	// report the import as applied for that account.
	actions := &fakeActions{connected: true,
		imported: ImportResult{OK: true, State: ImportStateAccepted, Models: 2}}
	bridge, err := StartBridgeHost(kimiHome, 1, actions)
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
	if result.State != ImportStateApplied || !result.Applied {
		t.Fatalf("import after a switch: %+v", result)
	}
	if result.Readback == nil || result.Readback.URL != "http://127.0.0.1:9999/v1" {
		t.Fatalf("readback: %+v", result.Readback)
	}

	// And the periodic caller now agrees without any further confirmation.
	settled := controller.ReconcileWithReadback(context.Background())
	if settled.State != IntegrationReady {
		t.Fatalf("periodic after the import: %s/%s", settled.State, settled.Reason)
	}
	if !settled.Applied {
		t.Fatalf("the settled import was not applied: %+v", settled)
	}
}

// TestNativeConfirmedImportRebindsAfterTheGatewayMoved covers the port change:
// the confirmed import is what proves the new address works, so requiring the
// old intent to already name it would refuse the very import that repairs it.
// The account gates still apply: only the exact profile may be adopted.
func TestNativeConfirmedImportRebindsAfterTheGatewayMoved(t *testing.T) {
	fixture := newNativeFixture(t)
	accounts := newSwitchedAccounts(t)
	fixture.seedSwitchedStore(t, accounts, "http://127.0.0.1:7777/v1", "http://127.0.0.1:7777/v1")
	fixture.seedOldIntent(t, accounts, "http://127.0.0.1:9999/v1")

	integration := fixture.integration(t)
	result := integration.Reconcile(ReconcileOptions{Enabled: true, AllowAccountRebind: true,
		AccountScope: accounts.newScope, URL: "http://127.0.0.1:7777/v1", Models: testCatalog()})
	if result.State != IntegrationReady && result.State != IntegrationRepaired {
		t.Fatalf("a confirmed import on a new port was refused: %s/%s", result.State, result.Reason)
	}
	intent, err := integration.readIntent()
	if err != nil || intent == nil {
		t.Fatalf("intent: %v", err)
	}
	if intent.AccountDigest != accounts.newDigest {
		t.Fatal("the recorded account did not follow the confirmed import")
	}
	if normalizeNativeURL(intent.URL) != "http://127.0.0.1:7777/v1" {
		t.Fatalf("the recorded address did not move: %s", intent.URL)
	}

	// The confirmation is still about one account: a profile pointing elsewhere
	// is refused, so the relaxed address gate is not a blank cheque.
	foreign := newNativeFixture(t)
	foreign.seedSwitchedStore(t, accounts, "http://127.0.0.1:7777/v1", "http://127.0.0.1:8888/v1")
	foreign.seedOldIntent(t, accounts, "http://127.0.0.1:9999/v1")
	refused := foreign.integration(t).Reconcile(ReconcileOptions{Enabled: true, AllowAccountRebind: true,
		AccountScope: accounts.newScope, URL: "http://127.0.0.1:7777/v1", Models: testCatalog()})
	if refused.State == IntegrationReady || refused.State == IntegrationRepaired {
		t.Fatalf("a profile pointing elsewhere was adopted: %+v", refused)
	}
}
