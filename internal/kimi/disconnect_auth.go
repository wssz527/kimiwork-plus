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
	disconnected := integration.DisconnectForAccount(account)
	if disconnected.State != IntegrationDisabled {
		result.OK = false
		result.State = ImportStateFailed
		result.Reason = "model_disconnect_local_state_failed"
		return result, errors.New(result.Reason)
	}
	readback := c.readback(account)
	disconnected.Readback = readback
	c.mu.Lock()
	c.integration = &disconnected
	c.mu.Unlock()
	return result, nil
}
