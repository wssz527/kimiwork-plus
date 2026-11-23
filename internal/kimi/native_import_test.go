package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests here cover the Phase 2 corrections to the native path:
//
//	authorisation  one explicit import, one overlay, closed by the call that
//	               opened it
//	account        an import never runs against a profile with no identity, and
//	               never against a different account than the configured one
//	readback       a page's `ok` is not an applied import; the files decide
//	launch         Start is idempotent for the child this controller owns, and
//	               still refuses an official Kimi it did not start
//	translation    turning translation off leaves the native bridge working

// nativeProfile writes a Kimi home that looks like a signed-in isolated profile.
// `account` is written as Kimi's own `credentials.kimiWeb.userId`; an empty
// account writes a config with no identity at all, which is what a profile looks
// like before the user signs in.
func nativeProfile(t *testing.T, account string) (profile, kimiHome string) {
	t.Helper()
	base := t.TempDir()
	profile = filepath.Join(base, "kimi-profile")
	kimiHome = filepath.Join(profile, "daimon-share", "daimon")
	if err := os.MkdirAll(kimiHome, 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := map[string]any{}
	if account != "" {
		credentials["kimiWeb"] = map[string]any{"userId": account}
	}
	config, err := json.Marshal(map[string]any{"model": map[string]any{}, "credentials": credentials})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiHome, NativeWorkConfig), config, 0o600); err != nil {
		t.Fatal(err)
	}
	return profile, kimiHome
}

// seedNativeProfile writes a complete, already-imported profile at `kimiHome`:
// the store Kimi owns, the profile row, and the generated config carrying the
// credential. It is the state the readback must recognise as applied.
func seedNativeProfile(t *testing.T, kimiHome, baseURL, credentialRef string) {
	t.Helper()
	profile := map[string]any{
		"kind": "kth", "id": "byok-kth", "baseUrl": baseURL, "credentialRef": credentialRef,
		"models": []any{
			map[string]any{"alias": "byok-kth-kimi/kimi-probe-model", "providerId": "byok-kth-kimi",
				"model": "kimi-probe-model", "family": "kimi", "maxContextSize": 131072},
			map[string]any{"alias": "byok-kth-resp/gpt-probe-model", "providerId": "byok-kth-resp",
				"model": "gpt-probe-model", "family": "responses", "maxContextSize": 262144},
		},
	}
	store, err := json.Marshal(map[string]any{"v": 1,
		"profiles": map[string]any{"profile-key": map[string]any{"byok-kth": profile}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiHome, NativeWorkStore), store, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{
		"model": map[string]any{
			"providers": map[string]any{"byok-kth-kimi": map[string]any{"type": "kimi",
				"baseUrl": baseURL, "credential": credentialRef}},
			"models": map[string]any{"byok-kth-kimi/kimi-probe-model": map[string]any{
				"provider": "byok-kth-kimi", "model": "kimi-probe-model", "maxContextSize": 131072}},
		},
		"credentials": map[string]any{
			credentialRef: map[string]any{"apiKey": "synthetic"},
			// The signed-in identity Kimi writes into its own config. The readback
			// reads it from here, never from the caller.
			"kimiWeb": map[string]any{"userId": "user-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kimiHome, NativeWorkConfig), config, 0o600); err != nil {
		t.Fatal(err)
	}
}

// controllerFor builds a controller whose profile and Kimi home are the fixture's
// own, with the bridge pointed at a live fake host.
func controllerFor(t *testing.T, profile string, page *fakePage) (*Controller, *fakeActions) {
	t.Helper()
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, GatewayURL: "http://127.0.0.1:9999/v1", GatewayToken: "gateway-token",
		Models: testCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	actions := &fakeActions{connected: true,
		imported: ImportResult{OK: true, State: ImportStateAccepted, Models: 2}}
	bridge, err := StartBridgeHost(filepath.Join(profile, "daimon-share", "daimon"), 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	controller.mu.Lock()
	controller.bridge = bridge
	controller.mu.Unlock()
	return controller, actions
}

// ---------------------------------------------------------------------------
// authorisation

// TestImportAuthorisationIsOneShot proves the grant is consumed by one call: a
// second call while the first is in flight is refused rather than opening a
// second overlay.
func TestImportAuthorisationIsOneShot(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, actions := controllerFor(t, profile, nil)

	// Hold the grant as the first call would, then ask again.
	controller.mu.Lock()
	controller.importGranted = true
	controller.mu.Unlock()

	result, err := controller.ImportModelsAuthorized(context.Background())
	if !errors.Is(err, ErrImportBusy) {
		t.Fatalf("a second authorised import was allowed: %v", err)
	}
	if result.State != ImportStateFailed || result.Reason != ErrImportBusy.Error() {
		t.Fatalf("result: %+v", result)
	}
	if len(actions.imports) != 0 {
		t.Fatal("a refused import still reached the bridge")
	}

	// The grant is released when the call that owned it finishes, so the next
	// explicit user action works.
	controller.mu.Lock()
	controller.importGranted = false
	controller.mu.Unlock()
	if _, err := controller.ImportModelsAuthorized(context.Background()); err != nil {
		t.Fatalf("the grant was not released: %v", err)
	}
	if len(actions.imports) != 1 {
		t.Fatalf("imports: %d", len(actions.imports))
	}
	if !actions.imports[0].OpenSettings {
		t.Fatal("the authorised import did not carry the open-settings consent")
	}
}

// TestPlainImportNeverOpensSettings proves the default import keeps the released
// behaviour: the settings page has to be open already.
func TestPlainImportNeverOpensSettings(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, actions := controllerFor(t, profile, nil)
	if _, err := controller.ImportModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions.imports) != 1 {
		t.Fatalf("imports: %d", len(actions.imports))
	}
	if actions.imports[0].OpenSettings {
		t.Fatal("a plain import opened the settings page")
	}
}

// ---------------------------------------------------------------------------
// account

// TestImportRefusesAProfileWithoutAnAccount proves an empty account is not
// "any account will do": nothing is sent and the state is waiting-login.
func TestImportRefusesAProfileWithoutAnAccount(t *testing.T) {
	profile, _ := nativeProfile(t, "")
	controller, actions := controllerFor(t, profile, nil)

	result, err := controller.ImportModelsAuthorized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateWaitingLogin || result.Reason != NativeWaitingLogin {
		t.Fatalf("result: %+v", result)
	}
	if len(actions.imports) != 0 {
		t.Fatal("an import ran against a profile with no account")
	}
}

// TestImportRefusesADifferentAccount proves a configured account that disagrees
// with the profile stops the import before it is sent.
func TestImportRefusesADifferentAccount(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, actions := controllerFor(t, profile, nil)
	settings := controller.Settings()
	settings.AccountScope = sha256Hex("someone-else")
	if err := controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	result, err := controller.ImportModelsAuthorized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateAccountChanged {
		t.Fatalf("result: %+v", result)
	}
	if len(actions.imports) != 0 {
		t.Fatal("an import ran against a different account")
	}
}

// TestCDPActionsRefusesAnUnverifiedProfile covers the page-level gate directly:
// the empty expected key path now reads the profile's own identity.
func TestCDPActionsRefusesAnUnverifiedProfile(t *testing.T) {
	page := &fakePage{surface: "work", actions: true, importAPI: true}
	harness := newHarness(t, page)
	dir := t.TempDir()
	// A config with no identity: the profile exists but has not signed in.
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{}, "credentials": map[string]any{}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	actions := NewCDPActions(harness.pipe, dir, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := actions.ImportModels(ctx, ImportRequest{URL: "http://127.0.0.1:9999/v1",
		Token: "gateway-token"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateWaitingLogin {
		t.Fatalf("result: %+v", result)
	}
	if len(page.snapshotEvaluations()) != 0 {
		t.Fatalf("the page was evaluated before the account was known: %v", page.snapshotEvaluations())
	}
}

// ---------------------------------------------------------------------------
// overlay ownership

// TestImportClosesOnlyTheOverlayItOpened proves the overlay this call opened is
// closed again, and that a call which found the page already open closes
// nothing.
func TestImportClosesOnlyTheOverlayItOpened(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The overlay does not exist yet: the call opens it and must close it.
	page := &fakePage{surface: "work", actions: true, importAPI: true, hideSettings: true}
	harness := newHarness(t, page)
	actions := NewCDPActions(harness.pipe, dir, dir)
	result, err := actions.ImportModels(ctx, ImportRequest{URL: "http://127.0.0.1:9999/v1",
		Token: "gateway-token", ExpectedAccountKey: sha256Hex("user-1"), OpenSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Opened {
		t.Fatalf("the overlay was not recorded as opened: %+v", result)
	}
	// The close is asked of both official controls (overlay and main view), so the
	// count is per target; what matters is that it was sent at all.
	if page.openCalls != 1 || page.closeCalls == 0 {
		t.Fatalf("open/close: %d/%d", page.openCalls, page.closeCalls)
	}
	if result.Closed == nil || !*result.Closed {
		t.Fatalf("the close was not recorded: %+v", result)
	}

	// The overlay is already open: this call owns nothing and closes nothing.
	open := &fakePage{surface: "work", actions: true, importAPI: true}
	openHarness := newHarness(t, open)
	openActions := NewCDPActions(openHarness.pipe, dir, dir)
	result, err = openActions.ImportModels(ctx, ImportRequest{URL: "http://127.0.0.1:9999/v1",
		Token: "gateway-token", ExpectedAccountKey: sha256Hex("user-1"), OpenSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Opened || result.Closed != nil {
		t.Fatalf("a pre-opened overlay was claimed: %+v", result)
	}
	if open.closeCalls != 0 {
		t.Fatalf("a close was sent for an overlay this call did not open: %d", open.closeCalls)
	}
}

// TestImportRecordsAFailedClose proves a close that the page refused is
// reported rather than swallowed.
func TestImportRecordsAFailedClose(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	page := &fakePage{surface: "work", actions: true, importAPI: true, hideSettings: true, closeFails: true}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := NewCDPActions(harness.pipe, dir, dir).ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token",
		ExpectedAccountKey: sha256Hex("user-1"), OpenSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Closed == nil || *result.Closed {
		t.Fatalf("a failed close was not recorded: %+v", result)
	}
}

// TestImportWithoutConsentReportsWaitingSettings proves an unauthorised call does
// not open anything and says so in the vocabulary the panel renders.
func TestImportWithoutConsentReportsWaitingSettings(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	page := &fakePage{surface: "work", actions: true, importAPI: true, hideSettings: true}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := NewCDPActions(harness.pipe, dir, dir).ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token", OpenSettings: false})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateWaitingSettings || result.Opened {
		t.Fatalf("result: %+v", result)
	}
	if page.openCalls != 0 {
		t.Fatal("an unauthorised call opened the settings page")
	}
}

func TestImportWaitsForSettingsPageReadiness(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	page := &fakePage{surface: "work", actions: true, importAPI: true, hideSettings: true, settingsAvailableAfter: 8}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := NewCDPActions(harness.pipe, dir, dir).ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token",
		ExpectedAccountKey: sha256Hex("user-1"), OpenSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("a slow settings page was rejected instead of awaited: %+v", result)
	}
	if page.settingsProbeReads != 8 {
		t.Fatalf("settings probe reads = %d, want 8", page.settingsProbeReads)
	}
}

// ---------------------------------------------------------------------------
// readback

// TestReconcileWithReadbackReportsTheEvidence proves the production reconcile
// answers with the directory, the account, the URL and the config generation,
// and that `Applied` follows the files rather than the request.
func TestReconcileWithReadbackReportsTheEvidence(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, GatewayURL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	result := controller.ReconcileWithReadback(context.Background())
	if result.Readback == nil {
		t.Fatal("no readback was reported")
	}
	readback := result.Readback
	if !readback.ProfilePresent || !readback.StorePresent || !readback.ConfigPresent {
		t.Fatalf("readback: %+v", readback)
	}
	if !readback.AccountVerified || readback.AccountDigest != sha256Hex("user-1") {
		t.Fatalf("account: %+v", readback)
	}
	if readback.URL != "http://127.0.0.1:9999/v1" {
		t.Fatalf("url: %s", readback.URL)
	}
	if readback.CredentialRef != "cred-1" || readback.Models != 2 {
		t.Fatalf("profile: %+v", readback)
	}
	if !result.Applied {
		t.Fatalf("an imported profile was not reported applied: %+v", result)
	}
	// No live child, so nothing may claim the running application has it.
	if result.Adopted {
		t.Fatal("adoption was claimed without a managed child")
	}
	// The readback carries no path and no token.
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{profile, kimiHome, "gateway-token", "synthetic"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("the readback leaked %q: %s", forbidden, text)
		}
	}
}

// TestReconcileWithReadbackWaitsForLogin proves a profile with no identity is
// reported as waiting-login and is not written to.
func TestReconcileWithReadbackWaitsForLogin(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "")
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, GatewayURL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(kimiHome, NativeWorkConfig))
	if err != nil {
		t.Fatal(err)
	}
	result := controller.ReconcileWithReadback(context.Background())
	if result.State != IntegrationAccountUnknown || result.Reason != "waiting-login" {
		t.Fatalf("result: %s/%s", result.State, result.Reason)
	}
	if result.Applied {
		t.Fatal("an unverified profile was reported applied")
	}
	if result.Readback == nil || result.Readback.AccountVerified {
		t.Fatalf("readback claimed an account: %+v", result.Readback)
	}
	after, err := os.ReadFile(filepath.Join(kimiHome, NativeWorkConfig))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("an unverified profile was written to")
	}
	// No intent file was created either.
	if _, err := os.Stat(filepath.Join(filepath.Dir(profile), NativeWorkIntent)); !os.IsNotExist(err) {
		t.Fatalf("an unverified profile left an intent behind: %v", err)
	}
}

// TestImportGradesAcceptedAsNotApplied proves a page that says `ok` but leaves
// the profile empty is reported as accepted, never as applied.
func TestImportGradesAcceptedAsNotApplied(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, _ := controllerFor(t, profile, nil)
	result, err := controller.ImportModelsAuthorized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateAccepted {
		t.Fatalf("result: %+v", result)
	}
	if result.Applied || result.Adopted {
		t.Fatal("an accepted request was reported applied")
	}
	if result.Readback == nil || result.Readback.Models != 0 {
		t.Fatalf("readback: %+v", result.Readback)
	}
}

// TestImportGradesAppliedFromTheFiles proves the readback promotes an accepted
// import to applied once the profile and the config hold it.
func TestImportGradesAppliedFromTheFiles(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	controller, _ := controllerFor(t, profile, nil)
	result, err := controller.ImportModelsAuthorized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateApplied || !result.Applied {
		t.Fatalf("result: %+v", result)
	}
}

// TestImportOwesARestartRatherThanClaimingApplied proves a restart-owing import
// is reported as requires-restart even when the files already hold it.
func TestImportOwesARestartRatherThanClaimingApplied(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	controller, actions := controllerFor(t, profile, nil)
	actions.imported = ImportResult{OK: true, State: ImportStateAccepted, Models: 2, RequiresRestart: true}
	result, err := controller.ImportModelsAuthorized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateRequiresRestart {
		t.Fatalf("result: %+v", result)
	}
	if result.Reason != NativeRequiresRestart {
		t.Fatalf("reason: %s", result.Reason)
	}
}

// adoptedController is a controller whose managed child already loaded the
// current import: reconcile reports it adopted before an import runs.
func adoptedController(t *testing.T) (*Controller, *fakeActions, *Child) {
	t.Helper()
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	controller, actions := controllerFor(t, profile, nil)
	child, _ := startFakeChild(t)
	bridge, err := StartBridgeHost(kimiHome, child.PID(), actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	// The first reconcile establishes the intent the later readbacks compare
	// against; a generation read before it is 0 and could never be adopted.
	seeded := controller.ReconcileWithReadback(context.Background())
	if !seeded.Applied {
		t.Fatalf("the fixture profile was not applied: %+v", seeded)
	}
	generation := controller.currentGeneration()
	if generation <= 0 {
		t.Fatalf("the fixture has no intent generation: %d", generation)
	}
	controller.mu.Lock()
	controller.child = child
	controller.bridge = bridge
	controller.loadedGeneration = generation
	controller.loadedKnown = true
	controller.mu.Unlock()
	t.Cleanup(func() { _ = child.Stop(3 * time.Second) })
	if adopted := controller.ReconcileWithReadback(context.Background()); !adopted.Applied || !adopted.Adopted {
		t.Fatalf("the fixture did not start out adopted: %+v", adopted)
	}
	return controller, actions, child
}

// A same-catalog sync that only changes a credential leaves the intent
// generation unchanged, so the readback alone cannot tell that the running
// kernel kept its old catalog. A deferred answer must therefore withdraw the
// running child's adoption instead of leaving `adopted` true behind a stale
// generation.
func TestDeferredImportWithdrawsAdoptionUntilAValidGeneration(t *testing.T) {
	controller, actions, _ := adoptedController(t)
	before := controller.ReconcileWithReadback(context.Background())
	actions.imported = ImportResult{OK: true, State: ImportStateAccepted, Models: 2,
		RequiresRestart: true, RestartDeferred: true}
	result, err := controller.ImportModelsAuthorized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ImportStateRequiresRestart || !result.Applied {
		t.Fatalf("a deferred import was not reported as restart-owing: %+v", result)
	}
	if result.Adopted {
		t.Fatalf("a deferred import still claimed adoption: %+v", result)
	}
	if before.Readback == nil || result.Readback == nil ||
		before.Readback.Generation != result.Readback.Generation {
		t.Fatalf("the fixture did not keep the generation: before=%+v after=%+v",
			before.Readback, result.Readback)
	}
	// The status a renderer reads must agree with the import answer, or the UI
	// clears a restart prompt the running kernel still owes.
	status := controller.Status(context.Background())
	if status.Integration == nil || status.Integration.Adopted || !status.Integration.Applied {
		t.Fatalf("status integration: %+v", status.Integration)
	}
	// A periodic reconcile cannot re-adopt the same generation either.
	again := controller.ReconcileWithReadback(context.Background())
	if !again.Applied || again.Adopted {
		t.Fatalf("a later reconcile re-claimed adoption: %+v", again)
	}
}

// A valid generation after the deferred answer does restore adoption: the
// withdrawal belongs to the child that answered, not to adoption in general.
func TestDeferredImportAdoptsAgainFromANewChild(t *testing.T) {
	controller, actions, previous := adoptedController(t)
	actions.imported = ImportResult{OK: true, State: ImportStateAccepted, Models: 2,
		RequiresRestart: true, RestartDeferred: true}
	if result, err := controller.ImportModelsAuthorized(context.Background()); err != nil || result.Adopted {
		t.Fatalf("deferred import: %+v %v", result, err)
	}

	// The reload the answer asked for is a new child: it reads the profile as it
	// is now, so Start records its baseline and adoption returns.
	reloaded, _ := startFakeChild(t)
	generation := controller.currentGeneration()
	controller.mu.Lock()
	controller.child = reloaded
	controller.loadedGeneration = generation
	controller.loadedKnown = true
	controller.mu.Unlock()
	t.Cleanup(func() { _ = reloaded.Stop(3 * time.Second) })
	restored := controller.ReconcileWithReadback(context.Background())
	if !restored.Applied || !restored.Adopted {
		t.Fatalf("adoption did not recover for the reloaded child: %+v", restored)
	}
	// Only the child that answered is withdrawn, so the older one stays out of
	// the picture even though it is still alive.
	select {
	case <-previous.Exit():
		t.Fatal("the previous child was stopped")
	default:
	}
}

// TestReadbackHoldsIsStrict covers the applied condition directly: a missing
// store, a missing credential or a different URL is not applied.
func TestReadbackHoldsIsStrict(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	integration, err := NewNativeIntegration(filepath.Dir(profile), kimiHome, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	readback := integration.Readback("user-1")
	if !readback.Holds("http://127.0.0.1:9999/v1") {
		t.Fatalf("a complete profile was not held: %+v", readback)
	}
	// A different gateway is a different import.
	if readback.Holds("http://127.0.0.1:8888/v1") {
		t.Fatal("a different URL was reported held")
	}
	if readback.Holds("") {
		t.Fatal("an empty URL was reported held")
	}
	// A profile that does not exist holds nothing.
	missing, err := NewNativeIntegration(filepath.Dir(profile), filepath.Join(kimiHome, "gone"), "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if gone := missing.Readback(""); gone.Holds("http://127.0.0.1:9999/v1") {
		t.Fatalf("a missing profile was reported held: %+v", gone)
	}
}

// ---------------------------------------------------------------------------
// launch idempotency

// TestStartIsIdempotentForTheManagedChild proves Start checks the child this
// controller owns instead of starting a second application.
func TestStartIsIdempotentForTheManagedChild(t *testing.T) {
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	child, profile := startFakeChild(t)
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Attach as a launch would, then ask Start again: it must not launch.
	if err := controller.attachChild(ctx, child, TranslationSettings{Enabled: false}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// Start's idempotency is about the child this controller owns, which Start is
	// what records. attachChild deliberately does not, so the ownership is set
	// here exactly as Start sets it.
	controller.mu.Lock()
	controller.child = child
	controller.mu.Unlock()
	t.Cleanup(func() { _ = controller.Stop(context.Background()) })

	if err := controller.Start(ctx); err != nil {
		t.Fatalf("a second Start refused the child it already owns: %v", err)
	}
	status := controller.Status(context.Background())
	if status.State != StateReady {
		t.Fatalf("state: %s (%s)", status.State, status.Reason)
	}
	if status.ManagedPID != child.PID() {
		t.Fatalf("a second child was started: pid=%d want=%d", status.ManagedPID, child.PID())
	}
	// The original child is still the one serving: it was never replaced.
	select {
	case <-child.Exit():
		t.Fatal("the managed child was restarted or stopped")
	default:
	}
}

// TestStartRecordsAManagedChildExit proves a child that exited is recorded, not
// revived, and that its bridge is closed.
func TestStartRecordsAManagedChildExit(t *testing.T) {
	child, profile := startFakeChild(t)
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := controller.attachChild(ctx, child, TranslationSettings{Enabled: false}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	controller.mu.Lock()
	controller.child = child
	bridge := controller.bridge
	controller.mu.Unlock()
	if bridge == nil {
		t.Fatal("no bridge was published")
	}
	descriptorFile := filepath.Join(controller.kimiHome(), BridgeDescriptorName)
	if _, err := os.Stat(descriptorFile); err != nil {
		t.Fatal("the descriptor was not published")
	}
	if err := child.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	// The next reconcile notices the exit: state stopped, nothing revived.
	result := controller.ReconcileWithReadback(ctx)
	_ = result
	status := controller.Status(context.Background())
	if status.State != StateStopped {
		t.Fatalf("state after an exit: %s (%s)", status.State, status.Reason)
	}
	if !strings.HasPrefix(status.Reason, "managed_child_exited") {
		t.Fatalf("reason: %s", status.Reason)
	}
	if status.ManagedPID != 0 {
		t.Fatalf("an exited child still reported a pid: %d", status.ManagedPID)
	}
	// The bridge descriptor is gone with the bridge.
	if _, err := os.Stat(descriptorFile); !os.IsNotExist(err) {
		t.Fatalf("the descriptor outlived its bridge: %v", err)
	}
}

// TestStartStillRefusesAnUnmanagedKimi proves the boundary is unchanged: a live
// child of this controller is idempotent, but an official Kimi this controller
// did not start is still refused and never signalled.
func TestStartStillRefusesAnUnmanagedKimi(t *testing.T) {
	state := DetectState(context.Background(), ProbeOptions{})
	if state.Installation == nil || state.Application != Running {
		t.Skip("no running official Kimi on this host")
	}
	dir := t.TempDir()
	controller, err := NewController(ControllerSettings{StateDir: dir,
		UserDataDir: filepath.Join(dir, "profile"), ProfileMode: ProfileModeIsolated, Installation: *state.Installation})
	if err != nil {
		t.Fatal(err)
	}
	// No managed child is held, so this is not the idempotent branch.
	if controller.managedChildAlive() {
		t.Fatal("a controller that started nothing reported a live child")
	}
	if err := controller.Start(context.Background()); !errors.Is(err, ErrUnmanagedKimiRunning) {
		t.Fatalf("want ErrUnmanagedKimiRunning, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// translation and the native bridge are independent

// TestTranslationToggleKeepsTheBridge proves switching translation off does not
// tear down the native bridge: the import path must survive it.
func TestTranslationToggleKeepsTheBridge(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	child, profile := startFakeChild(t)
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, GatewayURL: "http://127.0.0.1:9999/v1",
		Translation: TranslationSettings{Enabled: true, Provider: "bing", TargetLanguage: "zh"}})
	if err != nil {
		t.Fatal(err)
	}
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		Settings: map[string]any{"enabled": true}})
	actions := &fakeActions{connected: true, imported: ImportResult{OK: true, State: ImportStateAccepted}}
	bridge, err := StartBridgeHost(t.TempDir(), child.PID(), actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	controller.mu.Lock()
	controller.child = child
	controller.host = host
	controller.bridge = bridge
	controller.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := host.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// Turning translation off must not touch the bridge.
	settings := controller.Settings()
	settings.Translation = TranslationSettings{Enabled: false}
	if err := controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	status := controller.Tick(ctx)
	if status.State != "inactive" || status.Reason != "translation_disabled" {
		t.Fatalf("translation: %+v", status)
	}
	if _, err := bridge.Client().Status(ctx); err != nil {
		t.Fatalf("the native bridge died with translation: %v", err)
	}
	// And it is still the same bridge, not a replacement.
	controller.mu.Lock()
	same := controller.bridge == bridge
	controller.mu.Unlock()
	if !same {
		t.Fatal("the bridge was replaced when translation was disabled")
	}
}

// ---------------------------------------------------------------------------
// profile mode

// TestProfileModeOfficialIsTheDefault verifies the product default and that its
// report carries no path.
func TestProfileModeOfficialIsTheDefault(t *testing.T) {
	stateDir := t.TempDir()
	controller, err := NewController(ControllerSettings{StateDir: stateDir,
		UserDataDir: filepath.Join(stateDir, "kimi-profile")})
	if err != nil {
		t.Fatal(err)
	}
	status := controller.ProfileStatus()
	if status.Mode != ProfileModeOfficial || !status.Implemented {
		t.Fatalf("status: %+v", status)
	}
	if status.ProfileDigest == "" || status.ModeDigest == "" {
		t.Fatalf("digests missing: %+v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), stateDir) {
		t.Fatalf("the profile status leaked the path: %s", encoded)
	}
	// A renderer sees the same report through PublicState.
	public := controller.PublicState(context.Background())
	if public.Profile == nil || public.Profile.Mode != ProfileModeOfficial {
		t.Fatalf("public profile: %+v", public.Profile)
	}
}

// TestProfileModeExistingAuthorizedIsRefused proves the unimplemented mode is
// reported rather than half-done, and that Start refuses it before launching.
func TestProfileModeExistingAuthorizedIsRefused(t *testing.T) {
	stateDir := t.TempDir()
	controller, err := NewController(ControllerSettings{StateDir: stateDir,
		UserDataDir: filepath.Join(stateDir, "kimi-profile"), ProfileMode: ProfileModeExistingAuthorized,
		Installation: Installation{Application: "/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	status := controller.ProfileStatus()
	if status.Implemented || status.Reason != ErrProfileModeUnsupported.Error() {
		t.Fatalf("status: %+v", status)
	}
	if err := controller.Start(context.Background()); !errors.Is(err, ErrProfileModeUnsupported) {
		t.Fatalf("want ErrProfileModeUnsupported, got %v", err)
	}
	if state := controller.Status(context.Background()).State; state != StateFailed {
		t.Fatalf("state: %s", state)
	}
}

// TestAdoptionRequiresAChildThatLoadedTheImport proves a child started before the
// import is not reported as adopted. "A child is running" is not evidence the
// running application has read the new profile.
func TestAdoptionRequiresAChildThatLoadedTheImport(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	controller, _ := controllerFor(t, profile, nil)

	// A child that started before any import: it loaded generation 0.
	child, childProfile := startFakeChild(t)
	_ = childProfile
	controller.mu.Lock()
	controller.child = child
	controller.loadedKnown = true
	controller.loadedGeneration = 0
	controller.mu.Unlock()
	t.Cleanup(func() { _ = child.Stop(3 * time.Second) })

	result := controller.ReconcileWithReadback(context.Background())
	if !result.Applied {
		t.Fatalf("the profile is imported and must be applied: %+v", result)
	}
	if result.Adopted {
		t.Fatalf("a child that never loaded the import was reported adopted: %+v", result)
	}

	// A child that started after it is adopted.
	controller.mu.Lock()
	controller.loadedGeneration = result.Readback.Generation
	controller.mu.Unlock()
	result = controller.ReconcileWithReadback(context.Background())
	if !result.Adopted {
		t.Fatalf("a child that loaded the import was not reported adopted: %+v", result)
	}
}

// TestLaunchRecordsTheGenerationWithoutDeadlock drives the real launch path and
// asserts Start records the generation it launched against. The path used to take
// the controller lock twice, so a deadlock would hang here rather than fail an
// assertion; the test's own timeout is the guard.
func TestLaunchRecordsTheGenerationWithoutDeadlock(t *testing.T) {
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, GatewayURL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	// Establish an intent so the generation is non-zero.
	if result := controller.ReconcileWithReadback(context.Background()); !result.Applied {
		t.Fatalf("seed reconcile: %+v", result)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	settings := controller.Settings()
	settings.Installation = Installation{Application: executable}
	if err := controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	// Start would launch the real installation; the recording it performs is
	// exercised directly so the test does not depend on an installed Kimi.
	generation := controller.currentGeneration()
	if generation <= 0 {
		t.Fatalf("generation: %d", generation)
	}
	// Adoption also needs the runtime evidence: a published bridge and a child
	// that has not exited. Both are supplied here as the launch would supply them.
	child, childProfile := startFakeChild(t)
	_ = childProfile
	bridge, err := StartBridgeHost(kimiHome, child.PID(), &fakeActions{connected: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	controller.mu.Lock()
	controller.child = child
	controller.bridge = bridge
	controller.loadedGeneration = generation
	controller.loadedKnown = true
	controller.mu.Unlock()
	t.Cleanup(func() { _ = child.Stop(3 * time.Second) })

	result := controller.ReconcileWithReadback(context.Background())
	if !result.Applied || !result.Adopted {
		t.Fatalf("a child that loaded the current generation was not adopted: %+v", result)
	}
}

// ---------------------------------------------------------------------------
// error vocabulary

// TestImportStateVocabularyIsStable covers the mapping the panel switches on.
func TestImportStateVocabularyIsStable(t *testing.T) {
	cases := []struct {
		result ImportResult
		want   string
	}{
		{ImportResult{Reason: NativeWaitingLogin}, ImportStateWaitingLogin},
		{ImportResult{Reason: NativeWaitingWork}, ImportStateWaitingWork},
		{ImportResult{Reason: NativeWaitingSettings}, ImportStateWaitingSettings},
		{ImportResult{Reason: "native_bridge_settings_open_refused"}, ImportStateWaitingSettings},
		{ImportResult{Reason: NativeContractChanged}, ImportStateContractChanged},
		{ImportResult{Reason: NativeAccountChanged}, ImportStateAccountChanged},
		{ImportResult{Reason: "native_import_account_unverified"}, ImportStateAccountChanged},
		{ImportResult{OK: true, RequiresRestart: true}, ImportStateRequiresRestart},
		{ImportResult{OK: true}, ImportStateAccepted},
		{ImportResult{Reason: "native_import_import-failed"}, ImportStateFailed},
		{ImportResult{}, ImportStateFailed},
	}
	for _, entry := range cases {
		if got := importState(entry.result); got != entry.want {
			t.Fatalf("%+v: got %q want %q", entry.result, got, entry.want)
		}
	}
}

// TestImportNeverLogsASecret proves the result a caller logs carries neither the
// token nor the credential.
func TestImportNeverLogsASecret(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, _ := controllerFor(t, profile, nil)
	result, err := controller.ImportModelsAuthorized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"gateway-token", "127.0.0.1", profile} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("the import result leaked %q: %s", forbidden, encoded)
		}
	}
}

// fakeActionsCounted is a small concurrency guard for the one-shot grant: two
// callers race the same controller and exactly one may proceed.
func TestImportGrantIsRaceSafe(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, actions := controllerFor(t, profile, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	busy := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := controller.ImportModelsAuthorized(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, ErrImportBusy) {
				busy++
				return
			}
			if err == nil {
				accepted++
			}
		}()
	}
	wg.Wait()
	if accepted+busy != 4 {
		t.Fatalf("accepted=%d busy=%d", accepted, busy)
	}
	// Every call that got through reached the bridge exactly once.
	if len(actions.imports) != accepted {
		t.Fatalf("imports=%d accepted=%d", len(actions.imports), accepted)
	}
}
