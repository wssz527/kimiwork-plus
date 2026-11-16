package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"kimiworkplus.local/migration/internal/core"
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
