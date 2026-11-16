//go:build darwin && cgo

package platform

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// The OSStatus names this store uses must match the installed SDK. The previous
// version had -25308 and -25315 the wrong way round, which is a diagnosis bug no
// amount of retrying can fix.
func TestStatusCodesMatchTheInstalledSDK(t *testing.T) {
	expected := map[string]int32{
		"errSecInteractionNotAllowed": -25308,
		"errSecInteractionRequired":   -25315,
		"errSecItemNotFound":          -25300,
		"errSecDuplicateItem":         -25299,
		"errSecAuthFailed":            -25293,
		"errSecUserCanceled":          -128,
		"errSecNotAvailable":          -25291,
		"errSecNoSuchKeychain":        -25294,
		"errSecDecode":                -26275,
		"errSecMissingEntitlement":    -34018,
	}
	sdk := sdkStatusCodes()
	for name, want := range expected {
		if got := sdk[name]; got != want {
			t.Errorf("%s: headers say %d, this build assumes %d", name, got, want)
		}
	}
	for name, value := range sdk {
		var ours int32
		switch name {
		case "errSecInteractionNotAllowed":
			ours = errSecInteractionNotAllowed
		case "errSecInteractionRequired":
			ours = errSecInteractionRequired
		case "errSecItemNotFound":
			ours = errSecItemNotFound
		case "errSecDuplicateItem":
			ours = errSecDuplicateItem
		case "errSecAuthFailed":
			ours = errSecAuthFailed
		case "errSecUserCanceled":
			ours = errSecUserCanceled
		case "errSecNotAvailable":
			ours = errSecNotAvailable
		case "errSecNoSuchKeychain":
			ours = errSecNoSuchKeychain
		case "errSecDecode":
			ours = errSecDecode
		case "errSecMissingEntitlement":
			ours = errSecMissingEntitlement
		default:
			t.Fatalf("unmapped SDK status %q", name)
		}
		if ours != value {
			t.Errorf("%s: headers say %d, constant in the store is %d", name, value, ours)
		}
	}
	if got := sdkBooleanSize(); got != 1 {
		t.Fatalf("Boolean is %d bytes; SecKeychainGetUserInteractionAllowed writes one", got)
	}
}

// fakeSDK records the calls the store makes and returns canned results, so the
// classification logic is exercised without creating or deleting a real item.
type fakeSDK struct {
	mu             sync.Mutex
	stateValue     sdkState
	readResult     keychainRead
	writeResult    keychainWrite
	removeStatus   int32
	removeRestored bool
	calls          []string
	interactive    []bool
}

func newFakeSDK() *fakeSDK {
	return &fakeSDK{
		stateValue:     sdkState{available: 1, locked: 0, interactionAllowed: 1, onConsole: 1, loginComplete: 1},
		removeRestored: true,
	}
}

func (f *fakeSDK) state() sdkState {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "state")
	return f.stateValue
}

func (f *fakeSDK) read(service, account string, interactive bool) keychainRead {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "read:"+account)
	f.interactive = append(f.interactive, interactive)
	return f.readResult
}

func (f *fakeSDK) write(service, account, secret string, interactive bool) keychainWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "write:"+account)
	f.interactive = append(f.interactive, interactive)
	return f.writeResult
}

func (f *fakeSDK) remove(service, account string, interactive bool) (int32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "remove:"+account)
	f.interactive = append(f.interactive, interactive)
	return f.removeStatus, f.removeRestored
}

func (f *fakeSDK) interactiveCalls() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.interactive...)
}

func fakeStore(sdk *fakeSDK) *keychain {
	return &keychain{service: "dev.kimi-work-plus.preview.test", sdk: sdk}
}

func TestVisibleReadRequiresAVisibleSession(t *testing.T) {
	sdk := newFakeSDK()
	sdk.readResult = keychainRead{status: errSecSuccess, value: "saved-session", restored: true}
	value, err := fakeStore(sdk).GetVisible("account-session")
	if err != nil || value != "saved-session" || len(sdk.interactiveCalls()) != 1 || !sdk.interactiveCalls()[0] {
		t.Fatal("visible read did not permit its authorization", err)
	}
	sdk.stateValue.onConsole = 0
	if _, err := fakeStore(sdk).GetVisible("account-session"); err == nil {
		t.Fatal("headless session attempted authorization")
	}
	if len(sdk.interactiveCalls()) != 1 {
		t.Fatal("headless read reached the framework")
	}
}

// A background read must never ask the framework for a dialog.
func TestBackgroundReadNeverPermitsInteraction(t *testing.T) {
	sdk := newFakeSDK()
	sdk.readResult = keychainRead{status: errSecInteractionNotAllowed, restored: true}
	_, err := fakeStore(sdk).Get("connection-fixture")
	if !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatalf("want ErrCredentialAccessDenied, got %v", err)
	}
	for _, interactive := range sdk.interactiveCalls() {
		if interactive {
			t.Fatal("a read permitted interaction")
		}
	}
	var keychainError *KeychainError
	if !errors.As(err, &keychainError) {
		t.Fatalf("the denial lost its detail: %#v", err)
	}
	if keychainError.Code != CodeCredentialAuthorizationRequired {
		t.Fatalf("code: %s", keychainError.Code)
	}
	if keychainError.Status != errSecInteractionNotAllowed || keychainError.Existed != -1 {
		t.Fatalf("status/existed: %d/%d", keychainError.Status, keychainError.Existed)
	}
	if keychainError.Session != "console" {
		t.Fatalf("session: %s", keychainError.Session)
	}
	if strings.Contains(keychainError.Detail, "fixture") {
		t.Fatal("the diagnostic repeats the secret or the reference")
	}
}

// A locked keychain must be named as locked, not as an ACL problem: the user's
// remedy is to unlock it, not to re-enter the key.
func TestLockedKeychainIsReportedAsLocked(t *testing.T) {
	sdk := newFakeSDK()
	sdk.stateValue.locked = 1
	sdk.readResult = keychainRead{status: errSecInteractionNotAllowed, restored: true}
	_, err := fakeStore(sdk).Get("connection-fixture")
	var keychainError *KeychainError
	if !errors.As(err, &keychainError) {
		t.Fatalf("want a KeychainError, got %v", err)
	}
	if keychainError.Code != CodeCredentialLocked {
		t.Fatalf("code: %s", keychainError.Code)
	}
}

// A brand-new item refused with -25308 is not an "old ACL" item: it did not exist
// before this write. The error must carry that fact, so the panel can offer the
// visible re-save instead of telling the user their old key is unreadable.
func TestNewItemRefusedAtCreateIsNotAnOldACLItem(t *testing.T) {
	sdk := newFakeSDK()
	sdk.writeResult = keychainWrite{status: errSecInteractionNotAllowed, existed: 0, stage: stageAdd, restored: true}
	err := fakeStore(sdk).Set("connection-fixture", "synthetic-value")
	var keychainError *KeychainError
	if !errors.As(err, &keychainError) {
		t.Fatalf("want a KeychainError, got %v", err)
	}
	if keychainError.Existed != 0 || keychainError.Stage != "add" {
		t.Fatalf("existed/stage: %d/%s", keychainError.Existed, keychainError.Stage)
	}
	if keychainError.Code != CodeCredentialAuthorizationRequired {
		t.Fatalf("code: %s", keychainError.Code)
	}
	if errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatal("a create that never happened must not read as an access denial on an existing key")
	}
}

// The same refusal in a process with no console session is a calling-context
// problem: the system had nowhere to show the dialog. Naming the session lets the
// panel point at the app window instead of blaming the keychain.
func TestNewItemRefusedWithoutAConsoleSessionNamesTheSession(t *testing.T) {
	sdk := newFakeSDK()
	sdk.stateValue.onConsole = 0
	sdk.writeResult = keychainWrite{status: errSecInteractionNotAllowed, existed: 0, stage: stageAdd, restored: true}
	err := fakeStore(sdk).Set("connection-fixture", "synthetic-value")
	var keychainError *KeychainError
	if !errors.As(err, &keychainError) {
		t.Fatalf("want a KeychainError, got %v", err)
	}
	if keychainError.Code != CodeCredentialSessionUnavailable {
		t.Fatalf("code: %s", keychainError.Code)
	}
	if keychainError.Session != "background" {
		t.Fatalf("session: %s", keychainError.Session)
	}
}

// An existing item refused with -25308 is the state the panel turns into
// "re-enter the key", so it keeps the sentinel callers already match on.
func TestExistingItemRefusedOnUpdateKeepsAccessDenied(t *testing.T) {
	sdk := newFakeSDK()
	sdk.writeResult = keychainWrite{status: errSecInteractionRequired, existed: 1, stage: stageUpdate, restored: true}
	err := fakeStore(sdk).Set("connection-fixture", "synthetic-value")
	if !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatalf("want ErrCredentialAccessDenied, got %v", err)
	}
	var keychainError *KeychainError
	if !errors.As(err, &keychainError) {
		t.Fatalf("want a KeychainError, got %v", err)
	}
	if keychainError.Code != CodeCredentialAuthorizationRequired || keychainError.Existed != 1 {
		t.Fatalf("code/existed: %s/%d", keychainError.Code, keychainError.Existed)
	}
}

// A dismissed or rejected authorization keeps its own code so the panel can say
// what the user did instead of showing a generic failure.
func TestAuthorizationOutcomesKeepDistinctCodes(t *testing.T) {
	cases := []struct {
		status int32
		code   string
	}{
		{errSecUserCanceled, CodeCredentialAuthorizationCancelled},
		{errSecAuthFailed, CodeCredentialAuthorizationFailed},
		{errSecDecode, CodeCredentialStoreCorrupt},
		{errSecNotAvailable, CodeCredentialUnavailable},
		{errSecMissingEntitlement, CodeCredentialUnavailable},
		{-1, CodeCredentialStoreFailed},
	}
	for _, item := range cases {
		sdk := newFakeSDK()
		sdk.readResult = keychainRead{status: item.status, restored: true}
		_, err := fakeStore(sdk).Get("connection-fixture")
		var keychainError *KeychainError
		if !errors.As(err, &keychainError) {
			t.Fatalf("%d: want a KeychainError, got %v", item.status, err)
		}
		if keychainError.Code != item.code {
			t.Errorf("%d: code %s, want %s", item.status, keychainError.Code, item.code)
		}
	}
}

// A store that could not restore the process interaction flag must say so: the
// next operation in this process may run with the wrong setting.
func TestFailedInteractionRestoreIsReported(t *testing.T) {
	sdk := newFakeSDK()
	sdk.readResult = keychainRead{status: errSecSuccess, value: "synthetic", restored: false}
	if _, err := fakeStore(sdk).Get("connection-fixture"); err == nil {
		t.Fatal("a read with an unrestored interaction flag returned success")
	} else if !strings.Contains(err.Error(), CodeCredentialInteractionNotRestored) {
		t.Fatalf("error: %v", err)
	}
}

// The visible path is the only one that may ask for a dialog, and it refuses
// before calling the framework when no session could show the dialog.
func TestVisibleSaveRequiresAVisibleSession(t *testing.T) {
	sdk := newFakeSDK()
	sdk.stateValue.onConsole = 0
	store := fakeStore(sdk)
	if store.SessionVisible() {
		t.Fatal("a background session reported itself as visible")
	}
	err := store.SetVisible("connection-fixture", "synthetic-value")
	if !strings.Contains(err.Error(), CodeCredentialSessionUnavailable) {
		t.Fatalf("error: %v", err)
	}
	for _, call := range sdk.calls {
		if strings.HasPrefix(call, "write:") {
			t.Fatal("the store entered the framework without a session that could show a dialog")
		}
	}
}

func TestVisibleSavePermitsExactlyOneInteraction(t *testing.T) {
	sdk := newFakeSDK()
	sdk.writeResult = keychainWrite{status: errSecSuccess, existed: 1, stage: stageUpdate, restored: true}
	store := fakeStore(sdk)
	if !store.SessionVisible() {
		t.Fatal("a console session did not report itself as visible")
	}
	if err := store.SetVisible("connection-fixture", "synthetic-value"); err != nil {
		t.Fatalf("visible save: %v", err)
	}
	interactive := sdk.interactiveCalls()
	if len(interactive) != 1 || !interactive[0] {
		t.Fatalf("interaction flags: %v", interactive)
	}
}

// The default write must stay non-interactive even when a console session exists;
// only the explicit user gesture may ask for authorization.
func TestBackgroundSaveStaysNonInteractiveInAConsoleSession(t *testing.T) {
	sdk := newFakeSDK()
	sdk.writeResult = keychainWrite{status: errSecSuccess, existed: 0, stage: stageAdd, restored: true}
	if err := fakeStore(sdk).Set("connection-fixture", "synthetic-value"); err != nil {
		t.Fatalf("background save: %v", err)
	}
	interactive := sdk.interactiveCalls()
	if len(interactive) != 1 || interactive[0] {
		t.Fatalf("interaction flags: %v", interactive)
	}
}

func TestDeleteOfAMissingItemSucceeds(t *testing.T) {
	sdk := newFakeSDK()
	sdk.removeStatus = errSecItemNotFound
	if err := fakeStore(sdk).Delete("connection-fixture"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestDeleteNeverPermitsInteraction(t *testing.T) {
	sdk := newFakeSDK()
	sdk.removeStatus = errSecSuccess
	if err := fakeStore(sdk).Delete("connection-fixture"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	interactive := sdk.interactiveCalls()
	if len(interactive) != 1 || interactive[0] {
		t.Fatalf("interaction flags: %v", interactive)
	}
}

// Read returns the stored bytes unchanged, including a value with a NUL, and
// refuses a value beyond the documented bound instead of returning a truncated
// secret that would silently fail authentication later.
func TestReadReturnsTheExactStoredValue(t *testing.T) {
	sdk := newFakeSDK()
	const value = "synthetic\x00value"
	sdk.readResult = keychainRead{status: errSecSuccess, value: value, restored: true}
	got, err := fakeStore(sdk).Get("connection-fixture")
	if err != nil || got != value {
		t.Fatalf("get: %q %v", got, err)
	}

	sdk.readResult = keychainRead{status: errSecSuccess, value: strings.Repeat("x", maxSecretBytes+1), restored: true}
	if _, err := fakeStore(sdk).Get("connection-fixture"); !strings.Contains(err.Error(), CodeCredentialStoreCorrupt) {
		t.Fatalf("oversized value: %v", err)
	}
}

func TestAMissingItemIsReportedAsMissing(t *testing.T) {
	sdk := newFakeSDK()
	sdk.readResult = keychainRead{status: errSecItemNotFound, restored: true}
	if _, err := fakeStore(sdk).Get("connection-fixture"); !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("want ErrCredentialMissing, got %v", err)
	}
}

// Probe must map the new states without changing the panel's existing vocabulary
// for the states it already renders.
func TestProbeMapsTheStoreStates(t *testing.T) {
	sdk := newFakeSDK()
	sdk.readResult = keychainRead{status: errSecInteractionNotAllowed, restored: true}
	probe := Probe(fakeStore(sdk), "connection-fixture")
	if probe.State != CredentialStateAccessDenied || probe.Reason != "credential_access_denied" {
		t.Fatalf("probe: %#v", probe)
	}

	sdk.readResult = keychainRead{status: errSecItemNotFound, restored: true}
	probe = Probe(fakeStore(sdk), "connection-fixture")
	if probe.State != CredentialStateMissing {
		t.Fatalf("probe: %#v", probe)
	}
}

// State is a read-only observation: it must not change the interaction flag or
// touch an item.
func TestStateDoesNotTouchTheStore(t *testing.T) {
	sdk := newFakeSDK()
	sdk.stateValue.locked = 1
	state := fakeStore(sdk).State()
	if !state.Available || !state.Locked || !state.VisibleSession || !state.LoginComplete {
		t.Fatalf("state: %#v", state)
	}
	if state.ScreenState() != CodeCredentialLocked {
		t.Fatalf("screen state: %s", state.ScreenState())
	}
	for _, call := range sdk.calls {
		if call != "state" {
			t.Fatalf("State performed %q", call)
		}
	}
}

func TestCredentialsRemainExclusiveStates(t *testing.T) {
	if errors.Is(ErrCredentialAccessDenied, ErrCredentialMissing) {
		t.Fatal("an access denial must not read as a missing credential")
	}
	if ErrCredentialAccessDenied.Error() != "credential_access_denied" {
		t.Fatalf("reason code: %q", ErrCredentialAccessDenied.Error())
	}
	if CodeCredentialLocked == CodeCredentialAuthorizationRequired {
		t.Fatal("a locked keychain and an authorization need share one code")
	}
}

// Each cross-cutting error code must survive the panel's error-code pattern, or
// the UI receives operation_failed and cannot tell the states apart. The pattern
// is repeated here because it is the bridge's contract, not this package's.
func TestErrorCodesSurviveThePanelPattern(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-z][a-z0-9_]{0,100}$`)
	for _, code := range []string{
		CodeCredentialLocked,
		CodeCredentialUnavailable,
		CodeCredentialAuthorizationRequired,
		CodeCredentialAuthorizationCancelled,
		CodeCredentialAuthorizationFailed,
		CodeCredentialStoreCorrupt,
		CodeCredentialStoreFailed,
		CodeCredentialSessionUnavailable,
		CodeCredentialInteractionNotRestored,
	} {
		if !pattern.MatchString(code) {
			t.Errorf("%s would reach the panel as operation_failed", code)
		}
	}
}

// A failed add that reports a duplicate proves the item exists, even if the
// retry update is refused. Reporting existed=-1 there would let the panel call an
// existing, protected item "new".
func TestDuplicateAddReportsAnExistingItem(t *testing.T) {
	sdk := newFakeSDK()
	sdk.writeResult = keychainWrite{status: errSecInteractionNotAllowed, existed: 1, stage: stageUpdate, restored: true}
	err := fakeStore(sdk).Set("connection-fixture", "synthetic-value")
	var keychainError *KeychainError
	if !errors.As(err, &keychainError) {
		t.Fatalf("want a KeychainError, got %v", err)
	}
	if keychainError.Existed != 1 {
		t.Fatalf("existed: %d", keychainError.Existed)
	}
	if keychainError.Code != CodeCredentialAuthorizationRequired {
		t.Fatalf("code: %s", keychainError.Code)
	}
	if !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatal("an existing item that needs authorization must keep the access-denied sentinel")
	}
}

// A locked keychain outranks the write stage: the user's remedy is to unlock it,
// whether or not an item already existed.
func TestLockedKeychainOutranksTheWriteStage(t *testing.T) {
	sdk := newFakeSDK()
	sdk.stateValue.locked = 1
	sdk.writeResult = keychainWrite{status: errSecInteractionRequired, existed: 1, stage: stageUpdate, restored: true}
	err := fakeStore(sdk).Set("connection-fixture", "synthetic-value")
	var keychainError *KeychainError
	if !errors.As(err, &keychainError) {
		t.Fatalf("want a KeychainError, got %v", err)
	}
	if keychainError.Code != CodeCredentialLocked {
		t.Fatalf("code: %s", keychainError.Code)
	}
}
