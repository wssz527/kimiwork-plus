package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

type accountManagerFixture struct {
	vault      *accountVault
	manager    *accountManager
	active     string
	token      string
	failTarget string
	busy       bool
	activates  int
}

func newAccountManagerFixture(t *testing.T) *accountManagerFixture {
	t.Helper()
	f := &accountManagerFixture{vault: newAccountVault(t.TempDir(), &countedKeys{values: map[string]string{}}), active: "first", token: "initial"}
	var err error
	f.manager, err = newAccountManager(f.vault, f.hooks())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *accountManagerFixture) snapshot() accountCapture {
	session, _ := json.Marshal(map[string]string{"userId": f.active, "token": f.token})
	return accountCapture{UserID: f.active, Name: f.active, Session: session}
}

func (f *accountManagerFixture) hooks() accountHooks {
	return accountHooks{
		Capture: func(context.Context) (accountCapture, error) {
			if f.active == "" {
				return accountCapture{}, errors.New("account_not_logged_in")
			}
			return f.snapshot(), nil
		},
		Activate: func(_ context.Context, raw json.RawMessage) (accountCapture, error) {
			var session map[string]string
			if json.Unmarshal(raw, &session) != nil {
				return accountCapture{}, errors.New("account_snapshot_invalid")
			}
			f.activates++
			f.active, f.token = session["userId"], session["token"]
			if f.active == f.failTarget {
				return accountCapture{}, errors.New("account_credentials_rejected")
			}
			return f.snapshot(), nil
		},
		BeginLogin: func(context.Context) error { f.active = ""; return nil },
		Busy: func(context.Context) error {
			if f.busy {
				return errors.New("account_work_busy")
			}
			return nil
		},
	}
}

func (f *accountManagerFixture) action(t *testing.T, action, id string) error {
	t.Helper()
	_, err := f.manager.Action(context.Background(), core.Document{"action": action, "accountId": id})
	return err
}

func TestAccountManagerCapturesLatestTokensBeforeSwitch(t *testing.T) {
	f := newAccountManagerFixture(t)
	f.active, f.token = "second", "second-token"
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	f.active, f.token = "first", "rotated-token"
	if err := f.action(t, "account-switch", accountID("second")); err != nil {
		t.Fatal(err)
	}
	if f.active != "second" || f.token != "second-token" {
		t.Fatal("target was not restored")
	}
	if err := f.action(t, "account-switch", accountID("first")); err != nil {
		t.Fatal(err)
	}
	if f.active != "first" || f.token != "rotated-token" {
		t.Fatal("latest rotated token was not captured")
	}
	_, current, _ := f.vault.summaries()
	if current != accountID("first") || f.manager.State()["restoreAccountId"] != "" {
		t.Fatal("switch was not confirmed")
	}
}

func TestAccountManagerRollsBackRejectedTargetOnly(t *testing.T) {
	f := newAccountManagerFixture(t)
	f.active = "second"
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	f.active, f.failTarget = "first", "second"
	err := f.action(t, "account-switch", accountID("second"))
	if err == nil || err.Error() != "account_credentials_rejected" || f.active != "first" {
		t.Fatal("failed switch did not restore original", err)
	}
	rows, current, _ := f.vault.summaries()
	if current != accountID("first") || f.activates != 2 {
		t.Fatal("rollback was not verified")
	}
	for _, row := range rows {
		if row.NeedsLogin != (row.ID == accountID("second")) {
			t.Fatal("wrong account marked for login")
		}
	}
}

func TestAccountManagerDoesNotRepeatNativeFailureRecovery(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "restored"}[restored], func(t *testing.T) {
			f := newAccountManagerFixture(t)
			f.active = "second"
			if err := f.action(t, "account-capture", ""); err != nil {
				t.Fatal(err)
			}
			f.active = "first"
			f.manager.hooks.RestoresOnFailure = true
			f.manager.hooks.Activate = func(context.Context, json.RawMessage) (accountCapture, error) {
				f.activates++
				if restored {
					f.active = "first"
				} else {
					f.active = ""
				}
				return accountCapture{}, errors.New("account_session_rollback_unverified")
			}
			err := f.action(t, "account-switch", accountID("second"))
			if err == nil || err.Error() != "account_session_rollback_unverified" || f.activates != 1 {
				t.Fatalf("native recovery was repeated: err=%v activates=%d", err, f.activates)
			}
			wantRestore := accountID("first")
			if restored {
				wantRestore = ""
			}
			if f.manager.State()["restoreAccountId"] != wantRestore {
				t.Fatal("recovery state does not match the verified account")
			}
		})
	}
}

func TestAccountManagerPendingLoginSurvivesRestartAndCanCancel(t *testing.T) {
	f := newAccountManagerFixture(t)
	if err := f.action(t, "account-begin-add", ""); err != nil {
		t.Fatal(err)
	}
	if f.active != "" {
		t.Fatal("official login was not opened")
	}
	manager, err := newAccountManager(f.vault, f.hooks())
	if err != nil {
		t.Fatal(err)
	}
	f.manager = manager
	if manager.State()["adding"] != true || manager.State()["restoreAccountId"] != accountID("first") {
		t.Fatal("pending login was lost across restart")
	}
	if err := f.action(t, "account-cancel-add", ""); err != nil || f.active != "first" {
		t.Fatal("original login could not be restored", err)
	}
	if err := f.action(t, "account-begin-add", ""); err != nil {
		t.Fatal(err)
	}
	f.active, f.token = "second", "second-token"
	if err := f.action(t, "account-complete-add", ""); err != nil {
		t.Fatal(err)
	}
	rows, current, err := f.vault.summaries()
	if err != nil || len(rows) != 2 || current != accountID("second") || f.manager.State()["adding"] != false {
		t.Fatal("new official account was not captured", err)
	}
}

func TestAccountManagerFailedLoginShowsRecoveryInsteadOfWaiting(t *testing.T) {
	f := newAccountManagerFixture(t)
	f.manager.hooks.BeginLogin = func(context.Context) error {
		f.active = ""
		return errors.New("account_session_sign_out_incomplete")
	}
	f.failTarget = "first"
	if err := f.action(t, "account-begin-add", ""); err == nil || err.Error() != "account_switch_restore_failed" {
		t.Fatal("failed login did not preserve recovery", err)
	}
	if f.manager.State()["adding"] != false || f.manager.State()["restoreAccountId"] != accountID("first") {
		t.Fatal("failed login still advertised a ready login window")
	}
	f.failTarget = ""
	activations := f.activates
	if err := f.action(t, "account-cancel-add", ""); err != nil || f.activates != activations {
		t.Fatal("already restored identity was rewritten", err)
	}
}

func TestAccountManagerDoesNotSwitchDuringWork(t *testing.T) {
	f := newAccountManagerFixture(t)
	f.busy = true
	if err := f.action(t, "account-begin-add", ""); err == nil || err.Error() != "account_work_busy" {
		t.Fatal(err)
	}
	if f.active != "first" || f.activates != 0 {
		t.Fatal("busy check happened after changing account")
	}
}

func TestAccountManagerHealsStaleRestoreWhenSessionVerified(t *testing.T) {
	f := newAccountManagerFixture(t)
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	f.active, f.token = "second", "second-token"
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	// Simulate an interrupted non-adding switch left in accounts.json across daemon restart.
	f.active, f.token = "first", "first-token"
	if err := f.vault.setPending(accountID("first"), false); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAccountManager(f.vault, f.hooks())
	if err != nil {
		t.Fatal(err)
	}
	f.manager = restarted
	if err := f.action(t, "account-switch", accountID("second")); err != nil {
		t.Fatalf("stale restoreId blocked switch despite verified live session: %v", err)
	}
	if f.active != "second" || f.manager.State()["restoreAccountId"] != "" {
		t.Fatalf("unexpected state after self-healed switch: %+v", f.manager.State())
	}
}

func TestAccountManagerKeepsStaleRestoreWhenSessionUnverified(t *testing.T) {
	f := newAccountManagerFixture(t)
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	f.active, f.token = "second", "second-token"
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	f.active = ""
	if err := f.vault.setPending(accountID("first"), false); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAccountManager(f.vault, f.hooks())
	if err != nil {
		t.Fatal(err)
	}
	f.manager = restarted
	err = f.action(t, "account-switch", accountID("second"))
	if err == nil || err.Error() != "account_login_pending" {
		t.Fatalf("unverified session must keep the recovery marker: %v", err)
	}
	if f.manager.State()["restoreAccountId"] != accountID("first") || f.activates != 0 {
		t.Fatalf("recovery marker changed without verification: %+v", f.manager.State())
	}
}

func TestAccountManagerHealsStaleRestoreBeforeAddKeepsRecovery(t *testing.T) {
	f := newAccountManagerFixture(t)
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	f.active, f.token = "second", "second-token"
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	f.active, f.token = "first", "first-token"
	if err := f.vault.setPending(accountID("first"), false); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAccountManager(f.vault, f.hooks())
	if err != nil {
		t.Fatal(err)
	}
	f.manager = restarted
	if err := f.action(t, "account-begin-add", ""); err != nil {
		t.Fatalf("stale restoreId blocked adding despite verified live session: %v", err)
	}
	state := f.manager.State()
	if state["adding"] != true || state["restoreAccountId"] != accountID("first") || f.active != "" {
		t.Fatalf("adding flow lost its recovery account: %+v", state)
	}
	if err := f.action(t, "account-cancel-add", ""); err != nil || f.active != "first" {
		t.Fatalf("cancelled add did not restore the original account: %v", err)
	}
}

func TestAccountManagerCaptureClearsInterruptedRestoreAfterTargetRemoval(t *testing.T) {
	f := newAccountManagerFixture(t)
	f.active, f.token = "second", "second-token"
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	f.active, f.token = "first", "first-token"
	f.manager.hooks.RestoresOnFailure = true
	f.manager.hooks.Activate = func(context.Context, json.RawMessage) (accountCapture, error) {
		f.active = ""
		return accountCapture{}, errors.New("account_session_rollback_unverified")
	}
	if err := f.action(t, "account-switch", accountID("second")); err == nil || err.Error() != "account_session_rollback_unverified" {
		t.Fatalf("expected rollback unverified error, got: %v", err)
	}
	restarted, err := newAccountManager(f.vault, f.hooks())
	if err != nil {
		t.Fatal(err)
	}
	f.manager = restarted

	if err := f.action(t, "account-remove", accountID("second")); err != nil {
		t.Fatalf("removing failed target account failed: %v", err)
	}
	if err := f.action(t, "account-capture", ""); err == nil {
		t.Fatal("unverified capture cleared recovery state")
	}
	if f.manager.State()["restoreAccountId"] != accountID("first") {
		t.Fatalf("recovery marker cleared while signed out: %+v", f.manager.State())
	}

	f.active, f.token = "first", "reauthenticated-token"
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatalf("verified capture failed to clear interrupted restore: %v", err)
	}
	if f.manager.State()["restoreAccountId"] != "" || f.manager.State()["currentId"] != accountID("first") {
		t.Fatalf("unexpected state after recovery capture: %+v", f.manager.State())
	}
}

func TestMergeRotatedAccountSessionTracksTokenRotationWithoutDroppingState(t *testing.T) {
	stored := json.RawMessage(`{
		"schemaVersion": 1,
		"userId": "user-1",
		"name": "Saved User",
		"cookies": [{"name": "kimi_session", "value": "cookie-1", "domain": ".kimi.com", "path": "/"}],
		"localStorage": [{"key": "access_token", "value": "at-1"}, {"key": "refresh_token", "value": "rt-1"}],
		"kimiWeb": {"userId": "user-1", "accessToken": "at-1", "refreshToken": "rt-1"},
		"codeKey": {"apiKey": "ck-1"}
	}`)
	if _, changed := mergeRotatedAccountSession(stored, stored); changed {
		t.Fatal("identical session reported as rotated")
	}

	cleared := json.RawMessage(`{"schemaVersion": 1, "userId": "user-1", "kimiWeb": {"userId": "user-1"}}`)
	if _, changed := mergeRotatedAccountSession(stored, cleared); changed {
		t.Fatal("signed-out empty tokens overwrote stored credentials")
	}

	rotated := json.RawMessage(`{
		"schemaVersion": 1,
		"userId": "user-1",
		"localStorage": [{"key": "access_token", "value": "at-2"}, {"key": "refresh_token", "value": "rt-2"}],
		"kimiWeb": {"userId": "user-1", "accessToken": "at-2", "refreshToken": "rt-2"}
	}`)
	mergedRaw, changed := mergeRotatedAccountSession(stored, rotated)
	if !changed || len(mergedRaw) == 0 {
		t.Fatal("rotated tokens were not detected")
	}
	var merged map[string]any
	if err := json.Unmarshal(mergedRaw, &merged); err != nil {
		t.Fatal(err)
	}
	if merged["name"] != "Saved User" {
		t.Fatalf("saved name was lost: %+v", merged)
	}
	cookies, _ := merged["cookies"].([]any)
	if len(cookies) != 1 {
		t.Fatalf("saved cookies were lost: %+v", merged)
	}
	if merged["codeKey"] == nil {
		t.Fatalf("saved codeKey was lost: %+v", merged)
	}
}

func TestSyncCurrentAccountCredentialsThrottlesSteadyStateAndProtectsEmptyTokens(t *testing.T) {
	prevInterval := accountRotationSyncInterval
	accountRotationSyncInterval = 30 * time.Second
	t.Cleanup(func() { accountRotationSyncInterval = prevInterval })

	f := newAccountManagerFixture(t)
	f.vault.now = func() time.Time { return time.Now().Add(-2 * time.Minute) }
	sessionJSON := func(at, rt string) json.RawMessage {
		if at == "" && rt == "" {
			return json.RawMessage(`{"schemaVersion":1,"userId":"first","kimiWeb":{"userId":"first"}}`)
		}
		return json.RawMessage(`{"schemaVersion":1,"userId":"first","localStorage":[{"key":"access_token","value":"` + at + `"},{"key":"refresh_token","value":"` + rt + `"}],"kimiWeb":{"userId":"first","accessToken":"` + at + `","refreshToken":"` + rt + `"}}`)
	}
	currentAT, currentRT := "at-1", "rt-1"
	captures := 0
	f.manager.hooks.Capture = func(context.Context) (accountCapture, error) {
		captures++
		return accountCapture{UserID: "first", Name: "First", Session: sessionJSON(currentAT, currentRT)}, nil
	}
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	captures = 0

	native := &Native{accounts: f.manager}
	rows, id, err := f.vault.summaries()
	if err != nil {
		t.Fatal(err)
	}

	// Steady-state unchanged credentials must probe once, then stay throttled.
	native.syncCurrentAccountCredentials(context.Background(), id, rows)
	native.syncCurrentAccountCredentials(context.Background(), id, rows)
	if captures != 1 {
		t.Fatalf("captures within throttle window = %d, want 1", captures)
	}

	// Even when NeedsLogin is set, empty page tokens must never overwrite stored tokens.
	if err := f.vault.mark(id, true); err != nil {
		t.Fatal(err)
	}
	rows, _, err = f.vault.summaries()
	if err != nil {
		t.Fatal(err)
	}
	native.accountSyncAt = time.Now().Add(-time.Minute)
	currentAT, currentRT = "", ""
	native.syncCurrentAccountCredentials(context.Background(), id, rows)

	storedAfter, err := f.vault.sessionBackground(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(storedAfter, []byte("rt-1")) {
		t.Fatalf("stored refresh token overwritten by signed-out snapshot: %s", storedAfter)
	}
	rows, _, _ = f.vault.summaries()
	if !rows[0].NeedsLogin {
		t.Fatal("NeedsLogin cleared despite empty tokens")
	}
}
