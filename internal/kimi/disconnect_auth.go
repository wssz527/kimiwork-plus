package kimi

import (
	"context"
	"errors"
	"path/filepath"
)

func (c *Controller) HasManagedModelSources() bool {
	settings := c.Settings()
	home := c.kimiHome()
	account := ""
	if home != "" {
		account = AccountScope(home)
	}
	intent, err := readIntentFile(settings.StateDir)
	if err == nil && intent != nil && intent.Active &&
		(home == "" || intent.KimiHome == "" || filepath.Clean(intent.KimiHome) == filepath.Clean(home)) &&
		(account == "" || intent.AccountDigest == "" || intent.AccountDigest == sha256Hex(account)) {
		return true
	}
	if account == "" {
		return false
	}
	gateway := kthStoreState(home, account, settings.GatewayURL)
	return gateway.owned ||
		gateway.needsPendingSettle() && kthSourceOwned(gateway.source, gateway.account) ||
		DirectSourceRemains(home, account, "") != ""
}

func (c *Controller) DisconnectModelsAuthorized(ctx context.Context) (DisconnectResult, error) {
	if err := c.waitForImport(ctx); err != nil {
		return DisconnectResult{State: ImportStateFailed, Reason: safeReason(err)}, err
	}
	c.reapChild()
	c.mu.Lock()
	if c.importGranted {
		c.mu.Unlock()
		return DisconnectResult{State: ImportStateFailed, Reason: ErrImportBusy.Error()}, ErrImportBusy
	}
	bridge := c.bridge
	settings := c.settings
	c.importGranted = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.importGranted = false
		c.mu.Unlock()
	}()
	if bridge == nil {
		return DisconnectResult{State: ImportStateWaitingWork, Reason: "native_bridge_unavailable"},
			errors.New("native_bridge_unavailable")
	}
	account, verified := c.profileAccount()
	if !verified {
		return DisconnectResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	if settings.AccountScope != "" && settings.AccountScope != account {
		return DisconnectResult{State: ImportStateAccountChanged, Reason: NativeAccountChanged}, nil
	}
	result, err := bridge.Client().DisconnectModels(ctx, DisconnectRequest{
		ExpectedAccountKey: account,
		DirectSourceID:     c.previousDirectSourceID(settings),
	})
	if err != nil {
		return DisconnectResult{State: ImportStateFailed, Reason: safeReason(err)}, err
	}
	if !result.OK {
		return result, nil
	}
	if _, err := c.disconnectLocalState(account); err != nil {
		result.OK = false
		result.State = ImportStateFailed
		result.Reason = "model_disconnect_local_state_failed"
		return result, err
	}
	return result, nil
}

// DisconnectLocalStateAuthorized clears this product's own record of the import
// without calling the official source API. It is the local-only half of a
// disconnect: the official store is left exactly as Kimi has it, and no other
// account is read or written.
func (c *Controller) DisconnectLocalStateAuthorized(ctx context.Context) (DisconnectResult, error) {
	if err := c.waitForImport(ctx); err != nil {
		return DisconnectResult{State: ImportStateFailed, Reason: safeReason(err)}, err
	}
	c.reapChild()
	c.mu.Lock()
	if c.importGranted {
		c.mu.Unlock()
		return DisconnectResult{State: ImportStateFailed, Reason: ErrImportBusy.Error()}, ErrImportBusy
	}
	c.importGranted = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.importGranted = false
		c.mu.Unlock()
	}()
	account, verified := c.profileAccount()
	if !verified {
		return DisconnectResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	settings := c.Settings()
	if settings.AccountScope != "" && settings.AccountScope != account {
		return DisconnectResult{State: ImportStateAccountChanged, Reason: NativeAccountChanged}, nil
	}
	if _, err := c.disconnectLocalState(account); err != nil {
		return DisconnectResult{State: ImportStateFailed, Reason: err.Error()}, err
	}
	return DisconnectResult{OK: true, State: "disconnected"}, nil
}

// disconnectLocalState records the removal this product's own intent describes.
// The whole write is inside reconcileMu, and the generation moves before it: a
// reconcile that read the previous generation must not restore the models this
// call removes, and the official side is already gone, so the old generation is
// no longer valid whatever the local write answers. Lock order stays
// reconcileMu -> mu, the same order a reconcile uses.
func (c *Controller) disconnectLocalState(account string) (IntegrationResult, error) {
	settings := c.Settings()
	integration, err := NewNativeIntegration(settings.StateDir, c.kimiHome(), settings.integrationMode())
	if err != nil {
		return IntegrationResult{}, errors.New("model_disconnect_local_state_failed")
	}
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	c.mu.Lock()
	c.settingsGen++
	c.mu.Unlock()
	disconnected := integration.DisconnectForAccount(account)
	if disconnected.State != IntegrationDisabled {
		return IntegrationResult{}, errors.New("model_disconnect_local_state_failed")
	}
	disconnected.Readback = c.readback(account)
	c.mu.Lock()
	c.integration = &disconnected
	c.mu.Unlock()
	return disconnected, nil
}

// HasActiveManagedStoreSources reports whether the profile's own store still
// names a managed source for the signed-in account, and whether the store could
// be read well enough to answer. It is what makes a local-only disconnect safe:
// with no managed source left in the store, nothing in Kimi can still hold the
// catalog this product imported, so the official source API is not needed.
func (c *Controller) HasActiveManagedStoreSources() (hasActive bool, verified bool) {
	settings := c.Settings()
	home := c.kimiHome()
	if home == "" {
		return false, false
	}
	account, ok := c.profileAccount()
	if !ok {
		return false, false
	}
	// One snapshot decides the answer. Reading the store again for each scan
	// could fail between the reads and turn a live source into "nothing left to
	// disconnect", which is exactly the case a local-only clear must not happen.
	store, kind := readJSONFile(filepath.Join(home, NativeWorkStore), maxStoreBytes)
	if kind == "absent" {
		return false, true
	}
	if kind != "" {
		return false, false
	}
	accounts, ok := store["accounts"].(map[string]any)
	if !ok {
		return false, false
	}
	gateway := scanKthStore(store, account, settings.GatewayURL)
	if gateway.owned || gateway.needsPendingSettle() {
		return true, true
	}
	row, _ := accounts[account].(map[string]any)
	if row == nil {
		return false, true
	}
	sourceID, known := managedDirectSourceID(settings.StateDir, account)
	if !known {
		return false, false
	}
	if directStoreActive(row, sourceID) {
		return true, true
	}
	return false, true
}

// managedDirectSourceID is the custom source an active Direct Mode intent
// recorded for this account. It is read from the intent file only: the store
// snapshot above stays the single one this call reads. `known` is false when
// the intent cannot be read at all, because then an id-only source cannot be
// ruled out.
func managedDirectSourceID(stateDir, account string) (sourceID string, known bool) {
	intent, err := readIntentFile(stateDir)
	if err != nil {
		return "", false
	}
	if intent == nil || !intent.Active || intent.Mode != IntegrationModeDirect || intent.DirectSourceID == "" {
		return "", true
	}
	scopeDigest := sha256If(account)
	if scopeDigest != "" && intent.AccountDigest != "" && intent.AccountDigest != scopeDigest {
		return "", true
	}
	return intent.DirectSourceID, true
}
