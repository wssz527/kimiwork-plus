package kimi

import (
	"context"
	"errors"
)

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
	home := c.kimiHome()
	integration, integrationErr := NewNativeIntegration(settings.StateDir, home, settings.integrationMode())
	if integrationErr != nil {
		result.OK = false
		result.State = ImportStateFailed
		result.Reason = "model_disconnect_local_state_failed"
		return result, errors.New(result.Reason)
	}
	// The whole write is inside reconcileMu, and the generation moves before it:
	// a reconcile that read the previous generation must not restore the models
	// this call removes, and the page has already been disconnected, so the old
	// generation is no longer valid whatever the local write answers.
	// Lock order stays reconcileMu -> mu, the same order a reconcile uses.
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	c.mu.Lock()
	c.settingsGen++
	c.mu.Unlock()
	disconnected := integration.DisconnectForAccount(account)
	if disconnected.State != IntegrationDisabled {
		result.OK = false
		result.State = ImportStateFailed
		result.Reason = "model_disconnect_local_state_failed"
		return result, errors.New(result.Reason)
	}
	disconnected.Readback = c.readback(account)
	c.mu.Lock()
	c.integration = &disconnected
	c.mu.Unlock()
	return result, nil
}
