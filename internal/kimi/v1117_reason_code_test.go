package kimi

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dop251/goja"
)

// sourcesMock is a page whose byokSources API answers list with `list` and
// refuses every write: the expressions below must stop before one.
func sourcesMock(list string) string {
	return `
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){return ` + list + `;},
  preview(){throw new Error("unexpected preview");},
  save(){throw new Error("unexpected save");},
  disconnect(){throw new Error("unexpected disconnect");},
  applyPending(){throw new Error("unexpected applyPending");}
}};
`
}

// runPageExpression evaluates one page expression against a `byokSources` mock
// whose list answer is the one under test.
func runPageExpression(t *testing.T, setup, expression string) *goja.Object {
	t.Helper()
	vm := goja.New()
	if _, err := vm.RunString(setup); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`(` + expression + `).then(result=>globalThis.result=result,` +
		`error=>globalThis.failure=String(error&&error.stack||error));`); err != nil {
		t.Fatal(err)
	}
	if failure := vm.Get("failure"); failure != nil {
		t.Fatalf("page expression rejected: %v", failure)
	}
	result := vm.Get("result")
	if result == nil {
		t.Fatal("page expression returned no result")
	}
	return result.ToObject(vm)
}

// The official 3.2.29 build answers every byokSources call with a reasonCode
// while its gate is closed. The panel code has to name that gate; folding it
// into `contract-changed` hides the only cause the user can act on.
func TestSourcesListFailureKeepsTheOfficialReasonCode(t *testing.T) {
	directExpression, err := directImportExpression(DirectImportRequest{DisplayName: DirectDisplayName}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"feature-disabled", "no-account", "byok-not-ready"} {
		t.Run(code, func(t *testing.T) {
			setup := sourcesMock(`{ok:false,reasonCode:"` + code + `"}`)
			expressions := map[string]string{
				"import":         importExpression("fixture", "http://127.0.0.1:9100/v1/", ""),
				"direct import":  directExpression,
				"disconnect":     disconnectModelsExpression("direct-1"),
				"kth disconnect": kthDisconnectExpression,
				"kth settle":     kthSettlePendingExpression("", "revision"),
				"apply pending":  applyPendingExpression("scope", "revision"),
			}
			for name, expression := range expressions {
				result := runPageExpression(t, setup, expression)
				if result.Get("ok").ToBoolean() {
					t.Fatalf("%s accepted a refused list", name)
				}
				if got := result.Get("code").String(); got != code {
					t.Fatalf("%s code = %q, want %q", name, got, code)
				}
			}
		})
	}
}

// A failed list without a usable reason keeps the contract answer, and a
// structurally wrong value is still a contract change rather than a gate.
func TestSourcesListStructuralFailuresStayContractChanges(t *testing.T) {
	for name, answer := range map[string]string{
		"missing reply":       `undefined`,
		"null reply":          `null`,
		"null value":          `{ok:true,value:null}`,
		"missing scope token": `{ok:true,value:{sources:[]}}`,
		"no reason code":      `{ok:false}`,
		"empty reason code":   `{ok:false,reasonCode:""}`,
		"numeric reason code": `{ok:false,reasonCode:7}`,
	} {
		t.Run(name, func(t *testing.T) {
			setup := sourcesMock(answer)
			for _, expression := range []string{
				importExpression("fixture", "http://127.0.0.1:9100/v1/", ""),
				disconnectModelsExpression("direct-1"),
				kthDisconnectExpression,
				kthSettlePendingExpression("", "revision"),
				applyPendingExpression("scope", "revision"),
			} {
				result := runPageExpression(t, setup, expression)
				if result.Get("ok").ToBoolean() {
					t.Fatal("a broken list answer was accepted")
				}
				want := "contract-changed"
				if name == "no reason code" || name == "empty reason code" || name == "numeric reason code" {
					want = "failed"
				}
				if got := result.Get("code").String(); got != want {
					t.Fatalf("code = %q, want %q", got, want)
				}
			}
		})
	}
}

// The official gate has to reach the vocabulary the panel and the auto-sync
// publisher render.
func TestFeatureDisabledMapsToThePanelCodes(t *testing.T) {
	removed := decodeDisconnectAnswer(map[string]any{"ok": false, "code": "feature-disabled"}, false)
	if removed.State != ImportStateFailed || removed.Reason != "model_disconnect_feature_disabled" {
		t.Fatalf("disconnect answer = %+v", removed)
	}
	for code, want := range map[string]string{
		"no-account":     "model_disconnect_no_account",
		"byok-not-ready": "model_disconnect_byok_not_ready",
	} {
		if got := decodeDisconnectAnswer(map[string]any{"ok": false, "code": code}, false); got.Reason != want {
			t.Fatalf("%s reason = %q, want %q", code, got.Reason, want)
		}
	}
	if got := directImportFailureReason("feature-disabled"); got != "native_import_feature_disabled" {
		t.Fatalf("direct import reason = %q", got)
	}
	for code, want := range map[string]string{
		"feature-disabled": "native_import_feature_disabled",
		"no-account":       "native_import_no_account",
		"byok-not-ready":   "native_import_byok_not_ready",
	} {
		if got := kthResetFailureReason(code); got != want {
			t.Fatalf("%s reset reason = %q, want %q", code, got, want)
		}
	}
}

// orphanPendingStore is an account whose gateway source is already gone while
// its pending removal still names the models it used to hold.
func orphanPendingStore(accountKey string) map[string]any {
	store := kthTestStore(accountKey, kthTestAccount("http://127.0.0.1:9100/v1", nil))
	row := store["accounts"].(map[string]any)[accountKey].(map[string]any)
	row["sources"] = []any{}
	return store
}

// HasActiveManagedStoreSources is the evidence a feature-disabled disconnect
// needs before it may clear local state without an official call.
func TestHasActiveManagedStoreSourcesReadsOnlyTheSignedInAccount(t *testing.T) {
	const user = "test-current-account"
	account := sha256Hex(user)
	for _, tc := range []struct {
		name   string
		store  map[string]any
		want   bool
		verify bool
	}{
		{name: "absent store", want: false, verify: true},
		{name: "gateway source", store: kthTestStore(account, kthTestAccount("http://127.0.0.1:9100/v1", nil)), want: true, verify: true},
		{name: "direct source", store: directStoreFixture(account), want: true, verify: true},
		{name: "other account", store: kthTestStore("other-account", kthTestAccount("http://127.0.0.1:9100/v1", nil)), want: false, verify: true},
		{name: "other account direct", store: directStoreFixture("other-account"), want: false, verify: true},
		{name: "orphan pending removal", store: orphanPendingStore(account), want: true, verify: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newNativeFixture(t)
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
			writeIdentity(t, fixture, user)
			controller := &Controller{settings: ControllerSettings{
				StateDir: fixture.state, UserDataDir: filepath.Dir(fixture.dir), KimiHome: fixture.dir,
				GatewayURL: "http://127.0.0.1:9200/v1",
			}}
			if tc.store != nil {
				fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), tc.store)
			}
			hasActive, verified := controller.HasActiveManagedStoreSources()
			if verified != tc.verify || hasActive != tc.want {
				t.Fatalf("hasActive=%v verified=%v, want %v/%v", hasActive, verified, tc.want, tc.verify)
			}
		})
	}
	t.Run("unreadable store proves nothing", func(t *testing.T) {
		fixture := newNativeFixture(t)
		fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
		writeIdentity(t, fixture, user)
		if err := os.WriteFile(filepath.Join(fixture.dir, NativeWorkStore), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		controller := &Controller{settings: ControllerSettings{
			StateDir: fixture.state, UserDataDir: filepath.Dir(fixture.dir), KimiHome: fixture.dir,
		}}
		if hasActive, verified := controller.HasActiveManagedStoreSources(); hasActive || verified {
			t.Fatalf("a corrupt store was trusted: hasActive=%v verified=%v", hasActive, verified)
		}
	})
	t.Run("a profile without an identity proves nothing", func(t *testing.T) {
		fixture := newNativeFixture(t)
		fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
		controller := &Controller{settings: ControllerSettings{
			StateDir: fixture.state, UserDataDir: filepath.Dir(fixture.dir), KimiHome: fixture.dir,
		}}
		if _, verified := controller.HasActiveManagedStoreSources(); verified {
			t.Fatal("an unidentified profile reported a verified store")
		}
	})
}

// DisconnectLocalStateAuthorized is the local-only half of a disconnect: it
// clears this product's own record and leaves the official store byte for byte.
func TestDisconnectLocalStateAuthorizedLeavesTheOfficialStoreAlone(t *testing.T) {
	profile, home := nativeProfile(t, "user-1")
	seedNativeProfile(t, home, "http://127.0.0.1:9999/v1", "cred-1")
	controller, _ := controllerFor(t, profile, nil)
	controller.reconcile(false, "")
	storePath := filepath.Join(home, NativeWorkStore)
	before, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	catalog := controller.Settings().Models
	result, err := controller.DisconnectLocalStateAuthorized(context.Background())
	if err != nil || !result.OK || result.State != "disconnected" {
		t.Fatalf("local disconnect = %+v %v", result, err)
	}
	after, err := os.ReadFile(storePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the official store was modified: %v", err)
	}
	intent, err := readIntentFile(filepath.Dir(profile))
	if err != nil || intent == nil || intent.Active {
		t.Fatalf("intent after local disconnect: %+v %v", intent, err)
	}
	if len(controller.Settings().Models) != len(catalog) || len(catalog) == 0 {
		t.Fatal("the local catalog was dropped")
	}
	if controller.integration == nil || controller.integration.State != IntegrationDisabled {
		t.Fatalf("controller integration = %+v", controller.integration)
	}
}

func TestDisconnectLocalStateAuthorizedRefusesAnUnknownProfile(t *testing.T) {
	fixture := newNativeFixture(t)
	controller := &Controller{settings: ControllerSettings{
		StateDir: fixture.state, UserDataDir: filepath.Dir(fixture.dir), KimiHome: fixture.dir,
	}}
	result, err := controller.DisconnectLocalStateAuthorized(context.Background())
	if err != nil || result.OK || result.State != ImportStateWaitingLogin || result.Reason != NativeWaitingLogin {
		t.Fatalf("local disconnect without an identity = %+v %v", result, err)
	}
}

// seedDirectIntent records the active Direct Mode intent this product writes
// after a verified import. The store key is the account scope, so the recorded
// digest is one hash above it.
func seedDirectIntent(t *testing.T, fixture *nativeFixture, account, sourceID string) {
	t.Helper()
	fixture.writeJSON(t, filepath.Join(fixture.state, NativeWorkIntent), &nativeIntent{
		SchemaVersion: NativeWorkSchema, SourceID: "gateway", OwnerID: "owner-1",
		KimiHome: fixture.dir, Mode: IntegrationModeDirect, DirectSourceID: sourceID,
		AccountDigest: sha256Hex(account), Aliases: map[string]string{},
		Entries: ownership{Providers: map[string]ownedEntry{}, Models: map[string]ownedEntry{}},
		Active:  true, Generation: 3, UpdatedAt: "2026-11-17T00:00:00Z",
	})
}

// A gateway import that the official gate refuses must name the gate in the
// underscore vocabulary every other layer renders.
func TestGatewayImportModelsDecodesTheGateCodes(t *testing.T) {
	for code, want := range map[string]string{
		"feature-disabled": "native_import_feature_disabled",
		"no-account":       "native_import_no_account",
		"byok-not-ready":   "native_import_byok_not_ready",
	} {
		t.Run(code, func(t *testing.T) {
			home := kthTestHome(t)
			vm := goja.New()
			if _, err := vm.RunString(sourcesMock(`{ok:false,reasonCode:"` + code + `"}`)); err != nil {
				t.Fatal(err)
			}
			page := &fakePage{surface: "work", actions: true, importAPI: true, evaluateAction: executeSourceActions(t, vm)}
			result, err := kthImportActions(t, home, page).ImportModels(kthImportContext(t), ImportRequest{
				URL: "http://127.0.0.1:9999/v1", Token: "fixture", ExpectedAccountKey: AccountScope(home)})
			if err != nil {
				t.Fatal(err)
			}
			if result.OK || result.State != ImportStateFailed || result.Reason != want {
				t.Fatalf("result = %+v, want reason %q", result, want)
			}
		})
	}
}

// A Direct Mode source whose display name was changed is still matched through
// the id its intent recorded, and only for the account that intent names.
func TestHasActiveManagedStoreSourcesMatchesADirectSourceByIntentID(t *testing.T) {
	const user = "test-current-account"
	account := sha256Hex(user)
	for _, tc := range []struct {
		name   string
		intent string
		want   bool
	}{
		{name: "the current account's intent", intent: account, want: true},
		{name: "another account's intent", intent: "other-account", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newNativeFixture(t)
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
			writeIdentity(t, fixture, user)
			store := directStoreFixture(account)
			source := store["accounts"].(map[string]any)[account].(map[string]any)["sources"].([]any)[0].(map[string]any)
			source["displayName"] = "Renamed provider"
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
			seedDirectIntent(t, fixture, tc.intent, directSourceID)
			controller := &Controller{settings: ControllerSettings{
				StateDir: fixture.state, UserDataDir: filepath.Dir(fixture.dir), KimiHome: fixture.dir,
			}}
			hasActive, verified := controller.HasActiveManagedStoreSources()
			if !verified || hasActive != tc.want {
				t.Fatalf("hasActive=%v verified=%v, want %v/true", hasActive, verified, tc.want)
			}
		})
	}
}

// A disconnected direct source whose removal is still pending must keep the
// official disconnect in play; a settled one must not.
func TestHasActiveManagedStoreSourcesSeesAPendingDirectRemoval(t *testing.T) {
	const user = "test-current-account"
	account := sha256Hex(user)
	for _, tc := range []struct {
		name    string
		pending map[string]any
		want    bool
	}{
		{
			name:    "removal still pending",
			pending: map[string]any{"providers": []any{}, "models": []any{}},
			want:    true,
		},
		{
			name: "removal already applied",
			pending: map[string]any{"providers": []any{map[string]any{
				"id": "prov-1", "sourceId": directSourceID, "protocol": "openai",
				"baseUrl": "https://api.example.com/v1", "credentialRef": "cred-1",
			}}, "models": []any{map[string]any{
				"alias": "byok-uuid/model-a", "providerId": "prov-1", "sourceId": directSourceID,
			}}},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newNativeFixture(t)
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
			writeIdentity(t, fixture, user)
			store := directStoreFixture(account)
			row := store["accounts"].(map[string]any)[account].(map[string]any)
			source := row["sources"].([]any)[0].(map[string]any)
			source["status"] = "disconnected"
			row["pending"] = tc.pending
			fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)
			controller := &Controller{settings: ControllerSettings{
				StateDir: fixture.state, UserDataDir: filepath.Dir(fixture.dir), KimiHome: fixture.dir,
			}}
			hasActive, verified := controller.HasActiveManagedStoreSources()
			if !verified || hasActive != tc.want {
				t.Fatalf("hasActive=%v verified=%v, want %v/true", hasActive, verified, tc.want)
			}
		})
	}
}

// A local-only disconnect is still a write: it must not run beside an import
// that already holds the controller's grant.
func TestDisconnectLocalStateAuthorizedRefusesAConcurrentImport(t *testing.T) {
	profile, home := nativeProfile(t, "user-1")
	seedNativeProfile(t, home, "http://127.0.0.1:9999/v1", "cred-1")
	controller, _ := controllerFor(t, profile, nil)
	controller.mu.Lock()
	controller.importGranted = true
	controller.mu.Unlock()
	result, err := controller.DisconnectLocalStateAuthorized(context.Background())
	if !errors.Is(err, ErrImportBusy) || result.OK || result.Reason != ErrImportBusy.Error() {
		t.Fatalf("a concurrent local disconnect = %+v %v", result, err)
	}
	controller.mu.Lock()
	granted := controller.importGranted
	controller.mu.Unlock()
	if !granted {
		t.Fatal("the refused call released another caller's grant")
	}
}

// An unreadable intent could hide the id of a renamed direct source, so the
// store answer is not verified enough for a local-only clear.
func TestHasActiveManagedStoreSourcesRefusesAnUnreadableIntent(t *testing.T) {
	const user = "test-current-account"
	fixture := newNativeFixture(t)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), directConfigFixture())
	writeIdentity(t, fixture, user)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), directStoreFixture(sha256Hex(user)))
	if err := os.MkdirAll(filepath.Join(fixture.state, NativeWorkIntent), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := &Controller{settings: ControllerSettings{
		StateDir: fixture.state, UserDataDir: filepath.Dir(fixture.dir), KimiHome: fixture.dir,
	}}
	if hasActive, verified := controller.HasActiveManagedStoreSources(); hasActive || verified {
		t.Fatalf("an unreadable intent was trusted: hasActive=%v verified=%v", hasActive, verified)
	}
}
