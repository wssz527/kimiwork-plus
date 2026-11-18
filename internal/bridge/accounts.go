package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"kimiworkplus.local/migration/internal/core"
)

type accountHooks struct {
	Capture           func(context.Context) (accountCapture, error)
	Activate          func(context.Context, json.RawMessage) (accountCapture, error)
	BeginLogin        func(context.Context) error
	Busy              func(context.Context) error
	RestoresOnFailure bool
}

type accountManager struct {
	opMu      sync.Mutex
	mu        sync.Mutex
	vault     *accountVault
	hooks     accountHooks
	restoreID string
	adding    bool
}

func newAccountManager(vault *accountVault, hooks accountHooks) (*accountManager, error) {
	restore, adding, err := vault.pending()
	if err != nil {
		return nil, err
	}
	return &accountManager{vault: vault, hooks: hooks, restoreID: restore, adding: adding}, nil
}

func (a *accountManager) State() core.Document {
	rows, current, err := a.vault.summaries()
	a.mu.Lock()
	adding, restore := a.adding, a.restoreID
	a.mu.Unlock()
	if err != nil {
		return core.Document{"state": "unavailable", "reason": core.ErrorCode(err), "accounts": []AccountSummary{}, "adding": adding, "restoreAccountId": restore}
	}
	return core.Document{"state": "ready", "accounts": rows, "currentId": current, "adding": adding, "restoreAccountId": restore}
}

func (a *accountManager) setRestore(id string, adding bool) error {
	if err := a.vault.setPending(id, adding); err != nil {
		return err
	}
	a.mu.Lock()
	a.restoreID, a.adding = id, adding
	a.mu.Unlock()
	return nil
}

func (a *accountManager) capture(ctx context.Context) (accountCapture, AccountSummary, error) {
	snapshot, err := a.hooks.Capture(ctx)
	if err != nil {
		return accountCapture{}, AccountSummary{}, err
	}
	row, err := a.vault.capture(snapshot)
	return snapshot, row, err
}

// captureBackground stores a session that a periodic observation happened to
// see. It is not a user gesture, so its credential write may neither open a
// system authorization dialog nor queue behind one that is already open.
func (a *accountManager) captureBackground(ctx context.Context) (accountCapture, AccountSummary, error) {
	snapshot, err := a.hooks.Capture(ctx)
	if err != nil {
		return accountCapture{}, AccountSummary{}, err
	}
	row, err := a.vault.captureBackground(snapshot)
	return snapshot, row, err
}

func (a *accountManager) confirmRestore(ctx context.Context, id string) (bool, error) {
	fresh, err := a.hooks.Capture(ctx)
	if err != nil || accountID(fresh.UserID) != id {
		return false, nil
	}
	if _, err := a.vault.capture(fresh); err != nil {
		return false, err
	}
	return true, a.setRestore("", false)
}

func (a *accountManager) restore(ctx context.Context, id string) error {
	if restored, err := a.confirmRestore(ctx, id); restored || err != nil {
		return err
	}
	session, err := a.vault.session(id)
	if err != nil {
		return err
	}
	fresh, err := a.hooks.Activate(ctx, session)
	if err != nil {
		return err
	}
	if accountID(fresh.UserID) != id {
		return errors.New("account_identity_mismatch")
	}
	if _, err := a.vault.capture(fresh); err != nil {
		return err
	}
	return a.setRestore("", false)
}

func (a *accountManager) Action(ctx context.Context, in core.Document) (any, error) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	action := core.String(in["action"])
	if action == "account-remove" {
		if err := a.vault.remove(core.String(in["accountId"])); err != nil {
			return nil, err
		}
		return a.State(), nil
	}
	if a.hooks.Capture == nil || a.hooks.Activate == nil || a.hooks.BeginLogin == nil || a.hooks.Busy == nil {
		return nil, errors.New("account_switch_unavailable")
	}
	if action == "account-begin-add" || action == "account-switch" || action == "account-cancel-add" {
		if err := a.hooks.Busy(ctx); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	restore, adding := a.restoreID, a.adding
	a.mu.Unlock()
	switch action {
	case "account-capture":
		if _, _, err := a.capture(ctx); err != nil {
			return nil, err
		}
	case "account-begin-add":
		if restore != "" {
			return nil, errors.New("account_login_pending")
		}
		_, row, err := a.capture(ctx)
		if err != nil {
			return nil, err
		}
		if err := a.setRestore(row.ID, true); err != nil {
			return nil, err
		}
		if err := a.hooks.BeginLogin(ctx); err != nil {
			if pendingErr := a.setRestore(row.ID, false); pendingErr != nil {
				return nil, pendingErr
			}
			if a.restore(ctx, row.ID) != nil {
				return nil, errors.New("account_switch_restore_failed")
			}
			return nil, err
		}
	case "account-complete-add":
		if !adding {
			return nil, errors.New("account_login_not_pending")
		}
		snapshot, err := a.hooks.Capture(ctx)
		if err != nil {
			return nil, err
		}
		if accountID(snapshot.UserID) == restore {
			return nil, errors.New("account_login_unchanged")
		}
		if _, err := a.vault.capture(snapshot); err != nil {
			return nil, err
		}
		if err := a.setRestore("", false); err != nil {
			return nil, err
		}
	case "account-cancel-add":
		if restore == "" {
			return nil, errors.New("account_login_not_pending")
		}
		if err := a.restore(ctx, restore); err != nil {
			return nil, err
		}
	case "account-switch":
		if restore != "" {
			return nil, errors.New("account_login_pending")
		}
		id := core.String(in["accountId"])
		session, err := a.vault.session(id)
		if err != nil {
			return nil, err
		}
		_, original, err := a.capture(ctx)
		if err != nil {
			return nil, err
		}
		if id == original.ID {
			return a.State(), nil
		}
		if err := a.setRestore(original.ID, false); err != nil {
			return nil, err
		}
		fresh, switchErr := a.hooks.Activate(ctx, session)
		if switchErr == nil && accountID(fresh.UserID) != id {
			switchErr = errors.New("account_identity_mismatch")
		}
		if switchErr == nil {
			_, switchErr = a.vault.capture(fresh)
		}
		if switchErr != nil {
			if code := core.ErrorCode(switchErr); code == "account_credentials_rejected" || code == "account_session_expired" {
				_ = a.vault.mark(id, true)
			}
			if a.hooks.RestoresOnFailure {
				if _, err := a.confirmRestore(ctx, original.ID); err != nil {
					return nil, err
				}
				return nil, switchErr
			}
			if err := a.restore(ctx, original.ID); err != nil {
				return nil, errors.New("account_switch_restore_failed")
			}
			return nil, switchErr
		}
		if err := a.setRestore("", false); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("invalid_account_action")
	}
	return a.State(), nil
}
