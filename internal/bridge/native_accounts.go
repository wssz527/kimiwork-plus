package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func (n *Native) initAccounts(gateway *Runtime) error {
	manager, err := newAccountManager(newAccountVault(n.service.StateDir, n.service.Keys), accountHooks{
		Capture:           n.captureAccount,
		RestoresOnFailure: true,
		Activate: func(ctx context.Context, raw json.RawMessage) (accountCapture, error) {
			var session kimi.AccountSession
			if json.Unmarshal(raw, &session) != nil {
				return accountCapture{}, errors.New("account_snapshot_invalid")
			}
			result, err := n.controller.ActivateAccount(ctx, session)
			n.mu.Lock()
			n.accountActivation = core.Document{"state": result.State, "reason": result.Reason, "failureReason": result.FailureReason, "modelsState": result.ModelsState, "modelsReason": result.ModelsReason, "hot": result.Hot, "restarted": result.Restarted, "verified": result.Verified, "rolledBack": result.RolledBack, "requiresLogin": result.RequiresLogin}
			n.mu.Unlock()
			if result.RequiresLogin {
				return accountCapture{}, errors.New("account_credentials_rejected")
			}
			if err != nil {
				return accountCapture{}, err
			}
			if !result.Verified || result.UserID != session.UserID {
				return accountCapture{}, errors.New("account_identity_mismatch")
			}
			fresh, err := n.captureAccount(ctx)
			if err == nil && fresh.UserID != session.UserID {
				err = errors.New("account_identity_mismatch")
			}
			return fresh, err
		},
		BeginLogin: n.controller.BeginAccountLogin,
		Busy: func(ctx context.Context) error {
			gateway.mu.RLock()
			active := gateway.gateway
			gateway.mu.RUnlock()
			if active.Inflight() > 0 {
				return errors.New("account_work_busy")
			}
			busy, err := n.controller.AccountWorkBusy(ctx)
			if err != nil {
				return err
			}
			if busy {
				return errors.New("account_work_busy")
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	n.accounts = manager
	return nil
}

func (n *Native) captureAccount(ctx context.Context) (accountCapture, error) {
	identity, err := n.controller.AccountIdentity(ctx)
	if err != nil {
		return accountCapture{}, err
	}
	if !identity.PageVerified || !identity.SignedIn || identity.Source != "page+config" {
		return accountCapture{}, errors.New("account_identity_unverified")
	}
	session, err := n.controller.CaptureAccount(ctx)
	if err != nil {
		return accountCapture{}, err
	}
	if session.UserID != identity.UserID {
		return accountCapture{}, errors.New("account_identity_mismatch")
	}
	raw, err := json.Marshal(session)
	if err != nil {
		return accountCapture{}, errors.New("account_snapshot_invalid")
	}
	return accountCapture{UserID: session.UserID, Name: session.Name, AvatarURL: session.AvatarURL, Session: raw}, nil
}

func (n *Native) refreshAccountQuota(ctx context.Context) {
	n.mu.Lock()
	n.swarm = kimi.SwarmSnapshot{State: kimi.SwarmStateUnknown, Reason: "work_swarm_not_checked"}
	n.mu.Unlock()
	_, _ = n.quota.code.Action(ctx, "detect", nil)
	state := n.quota.code.State()
	if state.SelectedSource != nil && *state.SelectedSource == "work" {
		n.quota.code.Refresh(ctx, true)
	}
}

func (n *Native) accountsState() core.Document {
	if n.accounts == nil {
		return core.Document{"state": "unavailable", "reason": n.accountReason, "accounts": []AccountSummary{}}
	}
	state := n.accounts.State()
	n.mu.Lock()
	activation := core.Document{}
	for key, value := range n.accountActivation {
		activation[key] = value
	}
	state["activation"] = activation
	n.mu.Unlock()
	return state
}

func (n *Native) observeAccounts(ctx context.Context) {
	n.mu.Lock()
	operating := n.operation != nil && n.operation["state"] == "running"
	n.mu.Unlock()
	if operating || n.accounts == nil || !n.accounts.opMu.TryLock() {
		return
	}
	defer n.accounts.opMu.Unlock()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	identity, err := n.controller.AccountIdentity(deadline)
	if err != nil || !identity.PageVerified || !identity.SignedIn || identity.Source != "page+config" {
		return
	}
	_, current, err := n.accounts.vault.summaries()
	if err != nil {
		return
	}
	n.accounts.mu.Lock()
	restore, adding := n.accounts.restoreID, n.accounts.adding
	n.accounts.mu.Unlock()
	id := accountID(identity.UserID)
	if (adding && id == restore) || (restore == "" && current == id) {
		return
	}
	if _, _, err := n.accounts.captureBackground(deadline); err != nil {
		return
	}
	if restore != "" {
		if err := n.accounts.setRestore("", false); err != nil {
			return
		}
	}
	n.refreshAccountQuota(deadline)
}
