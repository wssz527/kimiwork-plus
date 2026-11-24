package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/assets/injection"
)

// The tests here cover the slow-start stability corrections: budgets that fit
// the work they cover, page waits that end on a real signal, and error
// vocabulary that never turns a deadline into a contract change.

// ---------------------------------------------------------------------------
// A: budgets

func TestBridgeCallBudgetSeparatesImportsFromActions(t *testing.T) {
	if bridgeCallBudget(OperationImportModels) <= settingsReadyWindow {
		t.Fatalf("an import budget of %s cannot outlast the %s page wait",
			bridgeCallBudget(OperationImportModels), settingsReadyWindow)
	}
	// An import that spends the whole page wait must still have room for the
	// official call, so the two are not near-equal.
	remaining := bridgeCallBudget(OperationImportModels) - settingsReadyWindow
	if remaining < 20*time.Second {
		t.Fatalf("an import has only %s left after the page wait", remaining)
	}
	if bridgeCallBudget(OperationCloseWorkSettings) >= bridgeCallBudget(OperationImportModels) {
		t.Fatal("a click was given an import's budget")
	}
}

// TestBridgeClientReportsATimeoutNotAnAbsentBridge proves a call that ran out of
// time is not folded into "unavailable": the two facts call for different
// reactions, and only one of them means the child is gone.
func TestBridgeClientReportsATimeoutNotAnAbsentBridge(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	client := NewBridgeClient(BridgeDescriptor{URL: server.URL, Token: strings.Repeat("a", 64)})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := client.call(ctx, OperationStatus, nil, &BridgeStatus{})
	// Whichever side ran out first, the answer is a timeout. It must never be the
	// "no bridge" fact, which says the child is gone.
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrBridgeTimeout) {
		t.Fatalf("a slow endpoint was reported as %v", err)
	}
	if errors.Is(err, ErrBridgeUnavailable) {
		t.Fatal("a slow endpoint was reported as an absent bridge")
	}
}

// TestBridgeClientKeepsTheHostReason proves the host's own reason survives the
// round trip instead of being collapsed into one word.
func TestBridgeClientKeepsTheHostReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": false, "reason": "native_bridge_waiting_settings"})
	}))
	defer server.Close()
	client := NewBridgeClient(BridgeDescriptor{URL: server.URL, Token: strings.Repeat("a", 64)})
	err := client.call(context.Background(), OperationImportModels, ImportRequest{}, &ImportResult{})
	if err == nil || err.Error() != "native_bridge_waiting_settings" {
		t.Fatalf("the host reason was lost: %v", err)
	}
}

// ---------------------------------------------------------------------------
// C: stale sessions

func TestTargetsIgnoreNonPageTargets(t *testing.T) {
	page := &fakePage{surface: "work", actions: true}
	page.workerWorkURL = true
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	targets, err := NewCDPActions(harness.pipe, t.TempDir(), t.TempDir()).(*cdpActions).targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.targetID == "worker-work" {
			t.Fatal("a non-page target carrying the Work URL was attached to")
		}
	}
}

func TestForgetSessionsDropsVanishedTargets(t *testing.T) {
	actions := &cdpActions{sessions: map[string]string{"gone": "session-gone", "kept": "session-kept"}}
	actions.forgetSessionsExcept(map[string]bool{"kept": true})
	if _, ok := actions.sessions["gone"]; ok {
		t.Fatal("a vanished target kept its session")
	}
	if actions.sessions["kept"] != "session-kept" {
		t.Fatal("a live target lost its session")
	}
}

// TestImportProbeReattachesOnceAfterASessionDies proves a reloaded page is
// re-read rather than reported as a changed contract, and that the probe itself
// is repeated while nothing is written.
func TestImportProbeReattachesOnceAfterASessionDies(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	page := &fakePage{surface: "work", actions: true, importAPI: true, expireFirstProbe: true}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := NewCDPActions(harness.pipe, dir, dir).ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token",
		ExpectedAccountKey: sha256Hex("user-1")})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("a reloaded page was not recovered: %+v", result)
	}
	if page.importCalls != 1 {
		t.Fatalf("the import ran %d times, want 1", page.importCalls)
	}
	if page.attachCalls < 2 {
		t.Fatalf("the dead session was not re-attached: %d attaches", page.attachCalls)
	}
	// The page was already there: recovering the session must not be mistaken for
	// a missing overlay and open a second one.
	if page.openCalls != 0 {
		t.Fatalf("a reload was repaired by opening settings %d times", page.openCalls)
	}
}

// ---------------------------------------------------------------------------
// D: bounded waits

func TestImportWaitsForSluggishWorkPage(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	// The Work page target only appears after several reads, and its preload API
	// only after a few more; the overlay and its import API appear last.
	page := &fakePage{surface: "work", actions: true, importAPI: true, hideSettings: true,
		workAvailableAfter: 3, openAPIAvailableAfter: 4, settingsAvailableAfter: 3}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := NewCDPActions(harness.pipe, dir, dir).ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token",
		ExpectedAccountKey: sha256Hex("user-1"), OpenSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("a slow Work page was rejected instead of awaited: %+v", result)
	}
	// The official call still ran exactly once, no matter how long the wait.
	if page.openCalls != 1 {
		t.Fatalf("openSettings ran %d times, want 1", page.openCalls)
	}
}

// TestImportStopsImmediatelyWhenThePipeEnds proves a dead transport is not
// polled until a deadline: it answers as soon as the pipe closes.
func TestImportStopsImmediatelyWhenThePipeEnds(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	// The overlay opens but its API never mounts, so the call is inside the wait
	// when the pipe goes away.
	page := &fakePage{surface: "work", actions: true, importAPI: true, hideSettings: true, settingsAvailableAfter: 10_000}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		time.Sleep(400 * time.Millisecond)
		harness.pipe.Dispose()
	}()
	started := time.Now()
	_, err := NewCDPActions(harness.pipe, dir, dir).ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token",
		ExpectedAccountKey: sha256Hex("user-1"), OpenSettings: true})
	if err == nil || !errors.Is(err, ErrPipeClosed) {
		t.Fatalf("a dead pipe was not reported: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a dead pipe was polled for %s", elapsed)
	}
}

// TestCloseAfterCancellationStillClosesTheOpenedOverlay proves a cancelled
// import does not strand the overlay it opened.
func TestCloseAfterCancellationStillClosesTheOpenedOverlay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closeCtx, closeCancel := closeContext(ctx)
	defer closeCancel()
	if err := closeCtx.Err(); err != nil {
		t.Fatalf("the close context inherited the cancellation: %v", err)
	}
	select {
	case <-closeCtx.Done():
		t.Fatal("the close context was already done")
	default:
	}
}

// ---------------------------------------------------------------------------
// E: error vocabulary

func TestActionErrorsDistinguishContractFromTransport(t *testing.T) {
	cases := []struct {
		err    error
		state  string
		reason string
	}{
		{context.DeadlineExceeded, ImportStateFailed, ErrBridgeTimeout.Error()},
		{ErrPipeTimeout, ImportStateFailed, ErrBridgeTimeout.Error()},
		{context.Canceled, ImportStateFailed, "native_bridge_cancelled"},
		{ErrPipeClosed, ImportStateFailed, ErrBridgeUnavailable.Error()},
		{ErrSessionInvalid, ImportStateFailed, "native_import_page_reloaded"},
		{ErrProtocolError, ImportStateContractChanged, NativeContractChanged},
		{ErrRendererUnconfirmed, ImportStateContractChanged, NativeContractChanged},
	}
	for _, entry := range cases {
		state, reason, _ := classifyActionError(entry.err)
		if state != entry.state || reason != entry.reason {
			t.Fatalf("%v -> %s/%s, want %s/%s", entry.err, state, reason, entry.state, entry.reason)
		}
	}
	// A transport failure is surfaced, a contract mismatch is described fully by
	// its result, and neither is ever swallowed into nil by itself.
	if _, _, surfaced := classifyActionError(context.DeadlineExceeded); surfaced == nil {
		t.Fatal("a timeout was swallowed")
	}
	if _, _, surfaced := classifyActionError(ErrPipeClosed); surfaced == nil {
		t.Fatal("a dead transport was swallowed")
	}
}

// TestImportReportsATimeoutAsAFailureNotAContractChange drives the real path: a
// page that never answers the import call must not be reported as a changed
// contract.
func TestImportReportsATimeoutAsAFailureNotAContractChange(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	page := &fakePage{surface: "work", actions: true, importAPI: true, importBlockFor: 10 * time.Second}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := NewCDPActions(harness.pipe, dir, dir).ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token",
		ExpectedAccountKey: sha256Hex("user-1")})
	if err == nil {
		t.Fatal("a timeout was swallowed into a nil error")
	}
	if result.State == ImportStateContractChanged {
		t.Fatalf("a timeout was reported as a changed contract: %+v", result)
	}
	if result.State != ImportStateFailed {
		t.Fatalf("state: %+v", result)
	}
}

// ---------------------------------------------------------------------------
// B: live probe readiness

func TestLiveAPICheckIgnoresAnUnprobedPage(t *testing.T) {
	static := AccountAPICompatCheck{Status: CheckStatusPass, Source: "preload",
		GetUserProfile: true, GetByokProfile: true, ByokSourcesList: true, ModelImportAPI: true}
	unprobed := AccountAPICompatCheck{Status: CheckStatusUnavailable, Source: "live", Reason: "page_not_ready"}
	merged := mergeLiveAPICheck(static, unprobed)
	if merged != static {
		t.Fatalf("an unprobed page overrode the static check: %+v", merged)
	}
}

func TestLiveAPICheckKeepsTheClawMarkerWhenThePageIsNotMounted(t *testing.T) {
	static := AccountAPICompatCheck{Status: CheckStatusPass, Source: "preload",
		GetUserProfile: true, GetByokProfile: true, ByokSourcesList: true, ModelImportAPI: true}
	// The claw page exists but its preload has not mounted: it reports no API at
	// all, which must not be read as "this build has no import API".
	live := AccountAPICompatCheck{Status: CheckStatusPass, Source: "live",
		GetUserProfile: true, GetByokProfile: true, ByokSourcesList: true,
		ModelImportAPI: false, Reason: "claw_page_not_open"}
	merged := mergeLiveAPICheck(static, live)
	if !merged.ModelImportAPI {
		t.Fatal("a claw page without a mounted preload cleared the import API")
	}
	if merged.Status != CheckStatusPass {
		t.Fatalf("status: %+v", merged)
	}
}

func TestLiveAPIProbeReportsNotMountedWithoutAnAPIObject(t *testing.T) {
	page := &fakePage{surface: "work", actions: true, noShellAPI: true}
	harness := newHarness(t, page)
	controller := &Controller{actions: NewCDPActions(harness.pipe, t.TempDir(), t.TempDir())}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if check, ok := controller.ProbeAccountAPIsLive(ctx); ok {
		t.Fatalf("an unmounted preload was reported as evidence: %+v", check)
	}
}

// ---------------------------------------------------------------------------
// F: repair suppression

func TestRepairSignatureSurvivesUncertainWrites(t *testing.T) {
	preWrite := []ImportResult{
		{State: ImportStateWaitingWork},
		{State: ImportStateWaitingSettings},
	}
	for _, result := range preWrite {
		if !repairFailedTransiently(result) {
			t.Fatalf("%+v would suppress a repair before any write", result)
		}
	}
	uncertain := []ImportResult{
		{State: ImportStateFailed, Reason: ErrBridgeTimeout.Error()},
		{State: ImportStateFailed, Reason: ErrBridgeUnavailable.Error()},
		{State: ImportStateFailed, Reason: "native_import_page_reloaded"},
		{State: ImportStateFailed, Reason: "native_bridge_cancelled"},
		{OK: true, State: ImportStateAccepted},
		{State: ImportStateAccountChanged, Reason: NativeAccountChanged},
		{State: ImportStateContractChanged, Reason: NativeContractChanged},
		{State: ImportStateFailed, Reason: "native_import_import-failed"},
	}
	for _, result := range uncertain {
		if repairFailedTransiently(result) {
			t.Fatalf("%+v would allow replaying an uncertain write", result)
		}
	}
}

func TestChildExitForgetsTheRepairSuppression(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile), UserDataDir: profile})
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.repairSignature = "account\x00http://127.0.0.1:1/v1\x00http://127.0.0.1:2/v1"
	controller.mu.Unlock()
	controller.forgetRepairSignature()
	controller.mu.Lock()
	signature := controller.repairSignature
	controller.mu.Unlock()
	if signature != "" {
		t.Fatalf("the suppression outlived the process it described: %q", signature)
	}
}

// ---------------------------------------------------------------------------
// G: account sync

func TestAccountStillTheirsSeparatesUnreadableFromChanged(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	_ = profile

	// A page that reports the account, with a config that holds it: confirmed.
	good := newFakeAccountPage()
	good.user[good.shellPage] = "user-1"
	good.officialIdentity = "user-1"
	if ok, retry := accountStillTheirs(context.Background(), good, kimiHome, "user-1"); !ok || retry {
		t.Fatalf("the fixture did not confirm its own account: ok=%v retry=%v", ok, retry)
	}

	// A page that reports another account: changed, and never pending.
	other := newFakeAccountPage()
	other.user[other.shellPage] = "user-2"
	other.officialIdentity = "user-2"
	if ok, retry := accountStillTheirs(context.Background(), other, kimiHome, "user-1"); ok || retry {
		t.Fatalf("a different account was not reported as changed: ok=%v retry=%v", ok, retry)
	}

	// A page whose official read fails: nothing was disproved.
	unreadable := newFakeAccountPage()
	unreadable.officialMissing = true
	if ok, retry := accountStillTheirs(context.Background(), unreadable, kimiHome, "user-1"); ok || !retry {
		t.Fatalf("an unreadable page was reported as a switched account: ok=%v retry=%v", ok, retry)
	}
}

func TestAccountConfigStateSeparatesUnreadableFromChanged(t *testing.T) {
	_, kimiHome := nativeProfile(t, "user-1")
	if matches, readable := accountConfigState(kimiHome, "user-1"); !matches || !readable {
		t.Fatalf("the fixture was not readable: matches=%v readable=%v", matches, readable)
	}
	// The generator is rewriting the file: it is absent for a moment.
	if err := os.Remove(filepath.Join(kimiHome, NativeWorkConfig)); err != nil {
		t.Fatal(err)
	}
	if matches, readable := accountConfigState(kimiHome, "user-1"); matches || readable {
		t.Fatalf("a missing config was read as an account: matches=%v readable=%v", matches, readable)
	}
}

func TestConfirmRunningCatalogStopsRuntimeWatchAfterCancellation(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	page := newFakeAccountPage()
	page.user[page.shellPage] = "user-1"
	page.officialIdentity = "user-1"
	page.runtimeWatch = true
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, GatewayURL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.actions = &accountActions{fakeAccountPage: page, fakeActions: &fakeActions{connected: true}}
	controller.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, reason := controller.syncAccountModels(ctx, page, kimiHome, "user-1")
	if reason == NativeAccountChanged {
		t.Fatal("a cancelled sync was reported as an account change")
	}
}

// ---------------------------------------------------------------------------
// the fake's own counters

func TestBridgeErrorsUseStableCodesWithoutEndpointDetails(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&url.Error{Op: "Post", URL: "http://127.0.0.1:49999/import-models", Err: context.DeadlineExceeded}, ErrBridgeTimeout.Error()},
		{context.Canceled, "native_bridge_cancelled"},
		{fmt.Errorf("%w: target closed", ErrSessionInvalid), "native_import_page_reloaded"},
		{fmt.Errorf("%w: write failed", ErrPipeClosed), ErrBridgeUnavailable.Error()},
		{errors.New("native_bridge_settings_open_refused"), "native_bridge_settings_open_refused"},
		{errors.New(`Post "http://127.0.0.1:49999/import-models": secret`), "native_bridge_failed"},
	}
	for _, test := range cases {
		if got := safeReason(test.err); got != test.want {
			t.Fatalf("safeReason(%v) = %q, want %q", test.err, got, test.want)
		}
		if strings.Contains(safeReason(test.err), "127.0.0.1") || strings.Contains(safeReason(test.err), "secret") {
			t.Fatalf("safeReason(%v) leaked endpoint details", test.err)
		}
	}
}

func TestOpenSettingsRefreshesAReplacedWorkTarget(t *testing.T) {
	page := &fakePage{surface: "work", actions: true, importAPI: true, hideSettings: true, replaceWorkTarget: true}
	harness := newHarness(t, page)
	actions := NewCDPActions(harness.pipe, t.TempDir(), t.TempDir()).(*cdpActions)
	target, opened, failure, err := actions.resolveImportTarget(context.Background(), true, false)
	if err != nil || failure != nil || !opened || target.targetID != "settings" {
		t.Fatalf("replaced Work target: target=%+v opened=%v failure=%+v err=%v", target, opened, failure, err)
	}
	if page.openCalls != 1 {
		t.Fatalf("openSettings calls = %d, want 1", page.openCalls)
	}
}

func TestSettingsProbeIsReadOnly(t *testing.T) {
	page := &fakePage{surface: "work", actions: true, importAPI: true}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	actions := NewCDPActions(harness.pipe, t.TempDir(), t.TempDir()).(*cdpActions)
	if _, err := actions.evaluate(ctx, "session-work-1", openSettingsProbeExpression); err != nil {
		t.Fatal(err)
	}
	var opens int64
	if value := atomic.LoadInt64(&opens); value != 0 {
		t.Fatalf("opens: %d", value)
	}
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.openCalls != 0 || page.settingsOpen {
		t.Fatalf("the probe opened the overlay: %d/%v", page.openCalls, page.settingsOpen)
	}
}

// TestSyncAccountModelsStillReportsARealSwitch proves the retry path did not
// swallow the one answer it must keep: a page that names a different account is
// still `account_changed`, not a reason to wait.
func TestSyncAccountModelsStillReportsARealSwitch(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	page := newFakeAccountPage()
	// The page is signed in as someone else entirely.
	page.user[page.shellPage] = "user-2"
	page.officialIdentity = "user-2"
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, GatewayURL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	// The sync only runs against an active intent, which the first reconcile
	// writes for the same gateway.
	if result := controller.ReconcileWithReadback(context.Background()); !result.Applied {
		t.Fatalf("the fixture was not imported: %+v", result)
	}
	state, reason := controller.syncAccountModels(context.Background(), page, kimiHome, "user-1")
	if state != "failed" || reason != NativeAccountChanged {
		t.Fatalf("a real switch was not reported: %s/%s", state, reason)
	}
}

// TestSyncAccountModelsRetriesAnUnreadablePage proves the other half: a page
// whose official identity read fails is retried, never called a switch.
func TestSyncAccountModelsRetriesAnUnreadablePage(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")
	page := newFakeAccountPage()
	page.user[page.shellPage] = "user-1"
	page.officialMissing = true
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, GatewayURL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if result := controller.ReconcileWithReadback(context.Background()); !result.Applied {
		t.Fatalf("the fixture was not imported: %+v", result)
	}
	previousStep, previousRetries := accountVerifyStep, accountRuntimeRetries
	accountVerifyStep, accountRuntimeRetries = time.Millisecond, 3
	t.Cleanup(func() { accountVerifyStep, accountRuntimeRetries = previousStep, previousRetries })
	state, reason := controller.syncAccountModels(context.Background(), page, kimiHome, "user-1")
	if reason == NativeAccountChanged {
		t.Fatalf("an unreadable page was reported as a switch: %s/%s", state, reason)
	}
	if state != "pending" {
		t.Fatalf("state: %s/%s", state, reason)
	}
}

func TestResolveImportTargetActivatesWorkOnceWhenAllowed(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	previousStep := settingsReadyStep
	settingsReadyStep = 10 * time.Millisecond
	t.Cleanup(func() { settingsReadyStep = previousStep })

	page := &fakePage{
		surface:                "work",
		actions:                true,
		importAPI:              true,
		hideSettings:           true,
		mainDesktopTarget:      true,
		workRequiresActivate:   true,
		activateAvailableAfter: 2,
	}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := NewCDPActions(harness.pipe, dir, dir).ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token",
		ExpectedAccountKey: sha256Hex("user-1"), OpenSettings: true, ActivateWork: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("expected import after Work activation, got %+v", result)
	}
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.activateCalls != 1 {
		t.Fatalf("openKimiWorkTab calls = %d, want 1", page.activateCalls)
	}
	if page.openCalls != 1 {
		t.Fatalf("openSettings calls = %d, want 1", page.openCalls)
	}
}

func TestResolveImportTargetDoesNotActivateWorkWhenDisallowed(t *testing.T) {
	previousWindow, previousStep := settingsReadyWindow, settingsReadyStep
	settingsReadyWindow, settingsReadyStep = 60*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		settingsReadyWindow, settingsReadyStep = previousWindow, previousStep
	})

	page := &fakePage{
		surface:              "work",
		actions:              true,
		importAPI:            true,
		hideSettings:         true,
		mainDesktopTarget:    true,
		workRequiresActivate: true,
	}
	harness := newHarness(t, page)
	actions := NewCDPActions(harness.pipe, t.TempDir(), t.TempDir()).(*cdpActions)
	_, opened, failure, err := actions.resolveImportTarget(context.Background(), true, false)
	if err != nil || opened || failure == nil {
		t.Fatalf("want waiting-work failure, got opened=%v failure=%+v err=%v", opened, failure, err)
	}
	if failure.State != ImportStateWaitingWork || failure.Reason != NativeWaitingWork {
		t.Fatalf("failure = %+v, want waiting-work", failure)
	}
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.activateCalls != 0 {
		t.Fatalf("background path activated Work %d times", page.activateCalls)
	}
}

func TestDisconnectModelsActivatesWorkWhenMissing(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	previousStep := settingsReadyStep
	settingsReadyStep = 10 * time.Millisecond
	t.Cleanup(func() { settingsReadyStep = previousStep })

	page := &fakePage{
		surface:              "work",
		actions:              true,
		importAPI:            true,
		hideSettings:         true,
		mainDesktopTarget:    true,
		workRequiresActivate: true,
	}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := NewCDPActions(harness.pipe, dir, dir).DisconnectModels(ctx, DisconnectRequest{
		ExpectedAccountKey: sha256Hex("user-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != "disconnected" {
		t.Fatalf("expected disconnect after Work activation, got %+v", result)
	}
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.activateCalls != 1 {
		t.Fatalf("openKimiWorkTab calls = %d, want 1", page.activateCalls)
	}
}

func TestImportActivateWorkFlagMatchesExplicitVsBackground(t *testing.T) {
	profile, kimiHome := nativeProfile(t, "user-1")
	controller, actions := controllerFor(t, profile, nil)
	seedNativeProfile(t, kimiHome, "http://127.0.0.1:9999/v1", "cred-1")

	if _, err := controller.ImportModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.importModelsAuthorized(context.Background(), sha256Hex("user-1"), "repair-sig"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ImportModelsAuthorized(context.Background()); err != nil {
		t.Fatal(err)
	}

	actions.mu.Lock()
	imports := append([]ImportRequest(nil), actions.imports...)
	actions.mu.Unlock()
	if len(imports) != 3 {
		t.Fatalf("imports = %d, want 3", len(imports))
	}
	if imports[0].ActivateWork || imports[0].OpenSettings {
		t.Fatalf("passive ImportModels set ActivateWork/OpenSettings: %+v", imports[0])
	}
	if imports[1].ActivateWork || !imports[1].OpenSettings {
		t.Fatalf("background repair set unexpected flags: %+v", imports[1])
	}
	if !imports[2].ActivateWork || !imports[2].OpenSettings {
		t.Fatalf("explicit ImportModelsAuthorized did not set ActivateWork/OpenSettings: %+v", imports[2])
	}

	settings := controller.Settings()
	settings.IntegrationMode = IntegrationModeDirect
	if err := controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	actions.mu.Lock()
	actions.direct = ImportResult{OK: true, State: ImportStateAccepted, SourceID: "src-1", Models: 1}
	actions.mu.Unlock()
	if _, err := controller.ImportDirectAuthorized(context.Background(), DirectImportRequest{
		Providers:    []DirectProvider{{ID: "p1", DisplayName: "P1", Protocol: "openai", BaseURL: "https://api.example.com/v1", APIKey: "sk-test"}},
		Models:       []DirectModel{{ID: "p1/m1", ProviderID: "p1", Model: "m1", DisplayName: "M1", MaxContextSize: 128000, Capabilities: []string{"tools"}}},
		OpenSettings: true,
	}); err != nil {
		t.Fatal(err)
	}
	actions.mu.Lock()
	directs := append([]DirectImportRequest(nil), actions.directs...)
	actions.mu.Unlock()
	if len(directs) != 1 || !directs[0].ActivateWork {
		t.Fatalf("explicit ImportDirectAuthorized did not set ActivateWork: %+v", directs)
	}
}

func TestControllerHasManagedBridgeReflectsChildLifecycle(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, _ := controllerFor(t, profile, nil)
	if !controller.HasManagedBridge() {
		t.Fatal("live bridge without child reported false")
	}

	alive := &Child{exitWait: make(chan struct{})}
	controller.mu.Lock()
	controller.child = alive
	controller.mu.Unlock()
	if !controller.HasManagedBridge() {
		t.Fatal("live child with bridge reported false")
	}

	close(alive.exitWait)
	if controller.HasManagedBridge() {
		t.Fatal("exited child still reported managed bridge")
	}
}

func TestResolveImportTargetColdStartSlowTargetsGetsFullWorkAndSettingsWindows(t *testing.T) {
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	previousWindow, previousStep := settingsReadyWindow, settingsReadyStep
	settingsReadyWindow, settingsReadyStep = 80*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		settingsReadyWindow, settingsReadyStep = previousWindow, previousStep
	})

	page := &fakePage{
		surface:                "work",
		actions:                true,
		importAPI:              true,
		hideSettings:           true,
		mainDesktopTarget:      true,
		workRequiresActivate:   true,
		activateAvailableAfter: 2,
		firstTargetDelay:       120 * time.Millisecond,
	}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := NewCDPActions(harness.pipe, dir, dir).ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token",
		ExpectedAccountKey: sha256Hex("user-1"), OpenSettings: true, ActivateWork: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.State != ImportStateAccepted {
		t.Fatalf("slow cold-start target list exhausted readiness window: %+v", result)
	}
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.activateCalls != 1 || page.openCalls != 1 {
		t.Fatalf("activateCalls=%d openCalls=%d, want 1/1", page.activateCalls, page.openCalls)
	}
}

func TestResolveImportTargetActivationFailureDoesNotLoop(t *testing.T) {
	previousWindow, previousStep := settingsReadyWindow, settingsReadyStep
	settingsReadyWindow, settingsReadyStep = 60*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		settingsReadyWindow, settingsReadyStep = previousWindow, previousStep
	})

	page := &fakePage{
		surface:              "work",
		actions:              true,
		importAPI:            true,
		hideSettings:         true,
		mainDesktopTarget:    true,
		workRequiresActivate: true,
		activateFails:        true,
	}
	harness := newHarness(t, page)
	actions := NewCDPActions(harness.pipe, t.TempDir(), t.TempDir()).(*cdpActions)
	_, opened, failure, err := actions.resolveImportTarget(context.Background(), true, true)
	if err != nil || opened || failure == nil {
		t.Fatalf("want waiting-work failure, got opened=%v failure=%+v err=%v", opened, failure, err)
	}
	if failure.State != ImportStateWaitingWork || failure.Reason != NativeWaitingWork {
		t.Fatalf("failure = %+v, want waiting-work", failure)
	}
	page.mu.Lock()
	defer page.mu.Unlock()
	if page.activateCalls != 1 {
		t.Fatalf("failed openKimiWorkTab was retried: calls = %d, want 1", page.activateCalls)
	}
}

func TestControllerStartReturnsNormalizedErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("kimi_start_failed: %w", os.ErrNotExist), "kimi_start_failed"},
		{fmt.Errorf("kimi_start_cancelled: %w", context.Canceled), "kimi_start_cancelled"},
		{fmt.Errorf("kimi_user_data_dir_unavailable: %w", os.ErrPermission), "kimi_user_data_dir_unavailable"},
		{fmt.Errorf("%w: handle value does not fit", errManagedChildUnsupported), "platform_cannot_manage_child"},
		{errors.New("kimi_application_missing"), "kimi_application_missing"},
		{errors.New("kimi_user_data_dir_invalid"), "kimi_user_data_dir_invalid"},
		{errors.New("unexpected os error: broken pipe"), "kimi_start_failed"},
	} {
		if got := startErrorCode(tc.err); got != tc.want {
			t.Fatalf("startErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}

	t.Run("cancelled context returns kimi_start_cancelled", func(t *testing.T) {
		controller, err := NewController(ControllerSettings{StateDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := controller.Start(ctx); err == nil || err.Error() != "kimi_start_cancelled" {
			t.Fatalf("Start(cancelled) = %v, want kimi_start_cancelled", err)
		}
	})

	t.Run("failed translation payload keeps child ready and records translation reason", func(t *testing.T) {
		child, profile := startFakeChild(t)
		controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile), UserDataDir: profile})
		if err != nil {
			t.Fatal(err)
		}
		original := translationPayload
		translationPayload = func() ([]byte, injection.Asset, error) {
			return nil, injection.Asset{}, os.ErrNotExist
		}
		t.Cleanup(func() { translationPayload = original })

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := controller.attachChild(ctx, child, TranslationSettings{Enabled: false}); err != nil {
			t.Fatalf("attachChild() = %v, want nil", err)
		}
		t.Cleanup(func() { _ = controller.Stop(context.Background()) })
		status := controller.Status(context.Background())
		if status.State != StateReady {
			t.Fatalf("state = %s, want ready", status.State)
		}
		if status.Translation == nil || status.Translation.State != "failed" || status.Translation.Reason != "injection_payload_unavailable" {
			t.Fatalf("translation = %+v", status.Translation)
		}
	})

	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Run("StartOfficial missing binary returns stable code", func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("AppData", dir)
			controller, err := NewController(ControllerSettings{
				StateDir:     filepath.Join(dir, "state"),
				Installation: Installation{Application: filepath.Join(dir, "missing-kimi")},
			})
			if err != nil {
				t.Fatal(err)
			}
			err = controller.StartOfficial(context.Background())
			if err == nil {
				t.Fatal("StartOfficial unexpectedly succeeded for missing binary")
			}
			if errors.Is(err, ErrUnmanagedKimiRunning) {
				if err.Error() != "unmanaged_kimi_running" {
					t.Fatalf("unmanaged err = %q", err.Error())
				}
				return
			}
			if err.Error() != "kimi_start_failed" {
				t.Fatalf("StartOfficial missing binary err = %q, want kimi_start_failed", err.Error())
			}
		})
	}
}
