package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

var accountRotationSyncInterval = 30 * time.Second

func (n *Native) initAccounts(gateway *Runtime) error {
	manager, err := newAccountManager(newAccountVault(n.service.StateDir, n.service.Keys), accountHooks{
		Capture:           n.captureAccount,
		RestoresOnFailure: true,
		SyncPending:       n.activateSavedAccount,
		ActivateOffline: func(ctx context.Context, raw json.RawMessage, commit func() error) error {
			var session kimi.AccountSession
			if json.Unmarshal(raw, &session) != nil {
				return errors.New("account_snapshot_invalid")
			}
			return n.controller.ActivateAccountOffline(ctx, session, commit)
		},
		Activate: func(ctx context.Context, raw json.RawMessage) (accountCapture, error) {
			user, err := n.activateSavedAccount(ctx, raw)
			if err != nil {
				return accountCapture{}, err
			}
			captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			fresh, err := n.captureAccount(captureCtx)
			if err == nil && fresh.UserID != user {
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

func (n *Native) activateSavedAccount(ctx context.Context, raw json.RawMessage) (string, error) {
	var session kimi.AccountSession
	if json.Unmarshal(raw, &session) != nil {
		return "", errors.New("account_snapshot_invalid")
	}
	result, err := n.controller.ActivateAccount(ctx, session)
	n.mu.Lock()
	n.accountActivation = core.Document{"state": result.State, "reason": result.Reason, "failureReason": result.FailureReason, "modelsState": result.ModelsState, "modelsReason": result.ModelsReason, "hot": result.Hot, "restarted": result.Restarted, "verified": result.Verified, "rolledBack": result.RolledBack, "requiresLogin": result.RequiresLogin, "accountId": accountID(session.UserID)}
	n.mu.Unlock()
	if result.RequiresLogin {
		return "", errors.New("account_credentials_rejected")
	}
	if err != nil {
		return "", err
	}
	if !result.Verified || result.UserID != session.UserID {
		return "", errors.New("account_identity_mismatch")
	}
	return result.UserID, nil
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

// clearStaleAccountActivation drops an activation record that describes an
// account which has since been removed or re-captured. Only the outcomes that
// ask the user to do something are dropped: an applied, verified activation
// stays visible as the record of a working switch.
func (n *Native) clearStaleAccountActivation() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.accountActivation == nil {
		return
	}
	switch core.String(n.accountActivation["state"]) {
	case "restored", "needs-login", "failed":
		n.accountActivation = nil
		return
	}
	if n.accountActivation["rolledBack"] == true || n.accountActivation["requiresLogin"] == true {
		n.accountActivation = nil
	}
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
	rows, current, pending, err := n.accounts.vault.state()
	if err != nil {
		return
	}
	if pending != "" {
		n.completePendingAccount(ctx, pending)
		return
	}
	identity, err := n.controller.AccountIdentity(deadline)
	if err != nil || !identity.PageVerified || !identity.SignedIn || identity.Source != "page+config" {
		return
	}
	n.accounts.mu.Lock()
	restore, adding := n.accounts.restoreID, n.accounts.adding
	n.accounts.mu.Unlock()
	id := accountID(identity.UserID)
	if adding && id == restore {
		return
	}
	if restore == "" && current == id {
		n.syncCurrentAccountCredentials(deadline, id, rows)
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

func (n *Native) completePendingAccount(ctx context.Context, id string) {
	n.mu.Lock()
	if n.pendingAccountID != id {
		n.resetPendingAccountRetryLocked()
		n.pendingAccountID = id
	}
	waiting := !n.pendingRetryAt.IsZero() && time.Now().Before(n.pendingRetryAt)
	n.mu.Unlock()
	if waiting {
		return
	}
	probe, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
	err := n.accounts.hooks.Busy(probe)
	cancelProbe()
	if err != nil {
		return
	}
	session, err := n.accounts.vault.sessionBackground(id)
	if err != nil {
		return
	}
	n.mu.Lock()
	if n.opWaiter || n.operation["state"] == "running" {
		n.mu.Unlock()
		return
	}
	previous := n.operation
	n.operation = core.Document{"id": RandomKey()[:16], "action": "auto-sync", "state": "running", "startedAt": time.Now().UnixMilli()}
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.operation = previous
		n.notifyOperationIdleLocked()
		n.mu.Unlock()
		n.autoSync.operationFinished("account-page-sync", "", nil, err)
	}()
	deadline, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err = n.accounts.syncPending(deadline, id, session); err != nil {
		n.recordPendingAccountFailure(id)
		return
	}
	n.mu.Lock()
	n.pendingFailures, n.pendingRetryAt = 0, time.Time{}
	n.mu.Unlock()
	n.refreshAccountQuota(deadline)
}

func (n *Native) resetPendingAccountRetryLocked() {
	n.pendingAccountID, n.pendingFailures, n.pendingRetryAt = "", 0, time.Time{}
}

func (n *Native) recordPendingAccountFailure(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pendingAccountID != id {
		n.pendingAccountID, n.pendingFailures = id, 0
	}
	n.pendingFailures++
	if n.pendingFailures < 2 {
		return
	}
	delay := 15 * time.Second
	for attempt := 2; attempt < n.pendingFailures && delay < time.Minute; attempt++ {
		delay *= 2
	}
	if delay > time.Minute {
		delay = time.Minute
	}
	n.pendingRetryAt = time.Now().Add(delay)
}

func (n *Native) syncCurrentAccountCredentials(ctx context.Context, id string, rows []AccountSummary) {
	var currentRow *AccountSummary
	for i := range rows {
		if rows[i].ID == id {
			currentRow = &rows[i]
			break
		}
	}
	if currentRow == nil {
		return
	}
	now := time.Now()
	if accountRotationSyncInterval > 0 {
		if !currentRow.NeedsLogin && now.Sub(time.UnixMilli(currentRow.CapturedAt)) < accountRotationSyncInterval {
			return
		}
		n.mu.Lock()
		throttled := n.accountSyncID == id && !n.accountSyncAt.IsZero() && now.Sub(n.accountSyncAt) < accountRotationSyncInterval
		if !throttled {
			n.accountSyncID = id
			n.accountSyncAt = now
		}
		n.mu.Unlock()
		if throttled {
			return
		}
	}
	storedRaw, err := n.accounts.vault.sessionBackground(id)
	if err != nil {
		return
	}
	captureFn := n.captureAccount
	if n.accounts.hooks.Capture != nil {
		captureFn = n.accounts.hooks.Capture
	}
	fresh, err := captureFn(ctx)
	if err != nil || accountID(fresh.UserID) != id {
		return
	}
	merged, changed := mergeRotatedAccountSession(storedRaw, fresh.Session)
	if len(merged) == 0 {
		return
	}
	if !changed && !currentRow.NeedsLogin {
		lifetime := inspectAccountSessionLifetime(merged, n.accounts.vault.now())
		if currentRow.AccessExpiresAt != lifetime.AccessExpiresAt ||
			currentRow.RefreshExpiresAt != lifetime.RefreshExpiresAt ||
			currentRow.CookieExpiresAt != lifetime.CookieExpiresAt ||
			currentRow.TokenHealth != lifetime.TokenHealth {
			_ = n.accounts.vault.updateTokenLifetime(id, lifetime)
		}
		return
	}
	fresh.Session = merged
	_, _ = n.accounts.vault.captureBackground(fresh)
}

func mergeRotatedAccountSession(storedRaw, freshRaw json.RawMessage) (json.RawMessage, bool) {
	var fresh kimi.AccountSession
	if json.Unmarshal(freshRaw, &fresh) != nil || fresh.UserID == "" {
		return nil, false
	}
	var stored kimi.AccountSession
	if json.Unmarshal(storedRaw, &stored) != nil || stored.UserID != fresh.UserID {
		return freshRaw, true
	}
	storedAT := accountSessionToken(stored, "access_token", "accessToken")
	storedRT := accountSessionToken(stored, "refresh_token", "refreshToken")
	freshAT := accountSessionToken(fresh, "access_token", "accessToken")
	freshRT := accountSessionToken(fresh, "refresh_token", "refreshToken")
	if (storedAT != "" && freshAT == "") || (storedRT != "" && freshRT == "") {
		return nil, false
	}
	if len(fresh.Cookies) == 0 && len(stored.Cookies) > 0 {
		fresh.Cookies = stored.Cookies
	}
	if len(fresh.LocalStorage) == 0 && len(stored.LocalStorage) > 0 {
		fresh.LocalStorage = mergeStoredAccountTokens(stored.LocalStorage, fresh.KimiWeb)
	}
	if len(fresh.CodeKey) == 0 && len(stored.CodeKey) > 0 {
		fresh.CodeKey = stored.CodeKey
	}
	if strings.TrimSpace(fresh.Name) == "" && strings.TrimSpace(stored.Name) != "" {
		fresh.Name = stored.Name
	}
	if strings.TrimSpace(fresh.AvatarURL) == "" && strings.TrimSpace(stored.AvatarURL) != "" {
		fresh.AvatarURL = stored.AvatarURL
	}
	changed := strings.TrimSpace(stored.Name) != strings.TrimSpace(fresh.Name) ||
		strings.TrimSpace(stored.AvatarURL) != strings.TrimSpace(fresh.AvatarURL) ||
		!bytes.Equal(bytes.TrimSpace(stored.CodeKey), bytes.TrimSpace(fresh.CodeKey)) ||
		!accountStorageEntriesEqual(stored.LocalStorage, fresh.LocalStorage) ||
		!accountCookiesEqual(stored.Cookies, fresh.Cookies)
	if !changed {
		for _, key := range []string{"accessToken", "refreshToken"} {
			if accountCredentialString(stored.KimiWeb, key) != accountCredentialString(fresh.KimiWeb, key) ||
				accountCredentialString(stored.KimiCode, key) != accountCredentialString(fresh.KimiCode, key) {
				changed = true
				break
			}
		}
	}
	encoded, err := json.Marshal(fresh)
	if err != nil {
		return nil, false
	}
	return encoded, changed
}

func mergeStoredAccountTokens(stored []kimi.AccountStorageEntry, web map[string]any) []kimi.AccountStorageEntry {
	out := make([]kimi.AccountStorageEntry, len(stored))
	copy(out, stored)
	for key, field := range map[string]string{"access_token": "accessToken", "refresh_token": "refreshToken"} {
		value := accountCredentialString(web, field)
		if value == "" {
			continue
		}
		updated := false
		for i := range out {
			if out[i].Key == key {
				out[i].Value = value
				updated = true
				break
			}
		}
		if !updated {
			out = append(out, kimi.AccountStorageEntry{Key: key, Value: value})
		}
	}
	return out
}

func accountSessionToken(session kimi.AccountSession, storageKey, webField string) string {
	for _, entry := range session.LocalStorage {
		if entry.Key == storageKey && entry.Value != "" {
			return entry.Value
		}
	}
	return accountCredentialString(session.KimiWeb, webField)
}

func accountCredentialString(entry map[string]any, key string) string {
	if entry == nil {
		return ""
	}
	value, _ := entry[key].(string)
	return value
}

func accountStorageEntriesEqual(a, b []kimi.AccountStorageEntry) bool {
	if len(a) != len(b) {
		return false
	}
	items := make(map[string]string, len(a))
	for _, entry := range a {
		items[entry.Key] = entry.Value
	}
	for _, entry := range b {
		if value, ok := items[entry.Key]; !ok || value != entry.Value {
			return false
		}
	}
	return true
}

func accountCookiesEqual(a, b []kimi.AccountCookie) bool {
	if len(a) != len(b) {
		return false
	}
	items := make(map[string]string, len(a))
	for _, cookie := range a {
		items[strings.ToLower(cookie.Name)+"|"+strings.ToLower(cookie.Domain)+"|"+cookie.Path] = cookie.Value
	}
	for _, cookie := range b {
		key := strings.ToLower(cookie.Name) + "|" + strings.ToLower(cookie.Domain) + "|" + cookie.Path
		if value, ok := items[key]; !ok || value != cookie.Value {
			return false
		}
	}
	return true
}
