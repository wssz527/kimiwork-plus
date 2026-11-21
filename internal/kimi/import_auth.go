package kimi

import (
	"context"
	"errors"
)

// An import is two separate facts and this file keeps them apart:
//
//	the request   the official page accepted the model import
//	the evidence  the profile and the generated config now hold those models
//
// A page that answers `ok` has only done the first. The second is read back from
// the files Kimi itself owns, so `Applied` is never set from the page's word.
//
// Authorisation is one shot. The public action consumes its grant whether the
// import succeeds, fails or never reaches the page. The automatic repair path uses
// the same grant only after proving the account, current gateway credential and
// previous loopback URL; it also suppresses repeated attempts for the same drift.

// The reason codes the panel already knows. They stay stable; the coarse state
// below is what a renderer switches on.
const (
	NativeWaitingLogin    = "native_import_waiting_login"
	NativeWaitingWork     = "native_bridge_waiting_work"
	NativeWaitingSettings = "native_bridge_waiting_settings"
	NativeContractChanged = "native_bridge_contract_changed"
	NativeAccountChanged  = "native_import_account_changed"
	NativeRequiresRestart = "native_import_requires_restart"
)

// ImportState is the coarse answer a panel renders. Several reason codes may map
// to one state, and a state never claims more than the evidence supports.
const (
	// ImportStateWaitingLogin means the selected profile has no account identity
	// yet. Only the user signing in to the official window produces one.
	ImportStateWaitingLogin = "waiting-login"
	// ImportStateWaitingWork means the Work view is not there to ask.
	ImportStateWaitingWork = "waiting-work"
	// ImportStateWaitingSettings means the import page is not open and this call
	// was not authorised to open it.
	ImportStateWaitingSettings = "waiting-settings"
	// ImportStateContractChanged means the official page answered something this
	// package does not understand; the build is not the one it was written for.
	ImportStateContractChanged = "contract-changed"
	// ImportStateAccountChanged means the profile belongs to another account than
	// the configured one. It is never repaired by importing anyway.
	ImportStateAccountChanged = "account-changed"
	// ImportStateRequiresRestart means the import needs a Work kernel reload.
	// The official importer schedules it; this is not a desktop restart request.
	ImportStateRequiresRestart = "requires-restart"
	// ImportStateApplied means the readback found the models in the profile and
	// the generated config. It is the only state that claims an effect.
	ImportStateApplied = "applied"
	// ImportStateAccepted means the page accepted the request and the readback
	// does not (yet) show the result. It is not an applied import.
	ImportStateAccepted = "accepted"
	// ImportStateFailed means the page or the transport refused.
	ImportStateFailed = "failed"
)

// ErrImportBusy means an authorised import is already in flight. Two overlays
// opened at once would fight over the same settings page.
var ErrImportBusy = errors.New("native_import_busy")

// ImportModelsAuthorized runs the import as an explicit, one-shot user action:
// this call, and only this call, may open Kimi's settings page and owns the
// overlay it opened.
//
// The authorisation is consumed before the work starts and cleared when it ends,
// so a second call — from a retry, a periodic reconcile or a stale UI event —
// can never reopen the overlay on its own.
func (c *Controller) ImportModelsAuthorized(ctx context.Context) (ImportResult, error) {
	return c.importModelsAuthorized(ctx, "", "")
}

func (c *Controller) importModelsAuthorized(ctx context.Context, expectedAccount, repairSignature string) (ImportResult, error) {
	if repairSignature != "" {
		c.reapChild()
	}
	c.mu.Lock()
	if c.importGranted || (repairSignature != "" && c.repairSignature == repairSignature) {
		c.mu.Unlock()
		return ImportResult{State: ImportStateFailed, Reason: ErrImportBusy.Error()}, ErrImportBusy
	}
	if repairSignature != "" && c.bridge == nil {
		c.mu.Unlock()
		return ImportResult{State: ImportStateWaitingWork, Reason: "native_bridge_unavailable"}, errors.New("native_bridge_unavailable")
	}
	if repairSignature != "" {
		c.repairSignature = repairSignature
	}
	c.importGranted = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.importGranted = false
		c.mu.Unlock()
	}()
	result, err := c.importModels(ctx, true, expectedAccount)
	if repairSignature != "" && (result.State == ImportStateWaitingWork || result.State == ImportStateWaitingSettings) {
		c.mu.Lock()
		if c.repairSignature == repairSignature {
			c.repairSignature = ""
		}
		c.mu.Unlock()
	}
	return result, err
}

// RepairIntegration restores a verified existing import after its loopback port
// changes. It never repairs a foreign URL, an inactive intent or another account.
//
// It is a no-op in Direct Mode: that mode has no loopback gateway to repair, and
// rewriting its config back to a gateway would undo the user's explicit choice.
func (c *Controller) RepairIntegration(ctx context.Context, observed IntegrationResult) {
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()
	if settings.integrationMode() == IntegrationModeDirect {
		return
	}
	readback := observed.Readback
	if observed.State != IntegrationNeedsImport || observed.Reason != "base_url_changed" ||
		readback == nil || !readback.Active || !readback.AccountVerified || readback.AccountDigest == "" ||
		!isLoopbackGateway(readback.URL) {
		return
	}
	account, verified := c.profileAccount()
	if !verified || account != readback.AccountDigest {
		return
	}
	native, err := NewNativeIntegration(settings.StateDir, c.kimiHome(), "gateway")
	if err != nil {
		return
	}
	_, reason, needsImport := native.accountCatalog(settings, account)
	if !needsImport || reason != "base_url_changed" {
		return
	}
	previous := normalizeNativeURL(native.Readback(account).URL)
	target := normalizeNativeURL(settings.GatewayURL)
	if target == "" || previous == target {
		return
	}
	signature := account + "\x00" + previous + "\x00" + target
	_, _ = c.importModelsAuthorized(ctx, account, signature)
}

// ImportDirectAuthorized runs one Direct Mode import as an explicit, one-shot
// user action: this call, and only this call, may open Kimi's settings page and
// owns the overlay it opened.
//
// The keys in `request` exist only for the duration of this call. The intent
// that a later reconcile reads records the source id and aliases, never a key.
func (c *Controller) ImportDirectAuthorized(ctx context.Context, request DirectImportRequest) (ImportResult, error) {
	c.reapChild()
	c.mu.Lock()
	if c.importGranted {
		c.mu.Unlock()
		return ImportResult{State: ImportStateFailed, Reason: ErrImportBusy.Error()}, ErrImportBusy
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
		return ImportResult{State: ImportStateWaitingWork, Reason: "native_bridge_unavailable"},
			errors.New("native_bridge_unavailable")
	}
	if settings.integrationMode() != IntegrationModeDirect {
		return ImportResult{State: ImportStateFailed, Reason: "native_import_gateway_mode"},
			errors.New("native_import_gateway_mode")
	}
	if request.DisplayName == "" {
		request.DisplayName = DirectDisplayName
	}
	account, verified := c.profileAccount()
	if !verified {
		return ImportResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	if request.ExpectedAccountKey == "" {
		request.ExpectedAccountKey = settings.AccountScope
	}
	if request.ExpectedAccountKey != "" && request.ExpectedAccountKey != account {
		return ImportResult{State: ImportStateAccountChanged, Reason: NativeAccountChanged}, nil
	}
	request.ExpectedAccountKey = account
	// Updating the source this tool already manages keeps Kimi's provider ids and
	// aliases; creating a second source would split the catalog.
	if request.ExistingSourceID == "" {
		request.ExistingSourceID = c.directSourceID(settings)
	}
	if request.DisconnectGateway && request.DisconnectGatewayURL == "" {
		request.DisconnectGatewayURL = settings.GatewayURL
	}
	result, err := bridge.Client().ImportDirect(ctx, request)
	if err != nil {
		return ImportResult{State: ImportStateFailed, Reason: safeReason(err)}, err
	}
	return c.gradeDirectImport(result, account, settings), nil
}

// ImportModels runs the import without opening anything. It keeps the released
// build's default: the settings page has to be open already, or the answer is
// waiting-settings for the caller to act on.
func (c *Controller) ImportModels(ctx context.Context) (ImportResult, error) {
	return c.importModels(ctx, false, "")
}

// importModels is the one import path. `authorized` permits opening the overlay
// for an explicit action or a verified own-loopback repair.
func (c *Controller) importModels(ctx context.Context, authorized bool, expectedAccount string) (ImportResult, error) {
	// A child that already exited must not be imported into, and its state must
	// not stay `ready`.
	c.reapChild()

	c.mu.Lock()
	settings := c.settings
	bridge := c.bridge
	c.mu.Unlock()

	if bridge == nil {
		// There is no managed child to import into. That is the "work view is not
		// there" case, not a settings problem: opening an overlay would have
		// nothing to open it in.
		return ImportResult{State: ImportStateWaitingWork, Reason: "native_bridge_unavailable"},
			errors.New("native_bridge_unavailable")
	}
	if settings.integrationMode() == IntegrationModeDirect {
		// A gateway import would overwrite the direct configuration the user
		// chose. Switching modes is a settings change followed by an explicit
		// import, never a periodic side effect.
		return ImportResult{State: ImportStateFailed, Reason: "native_import_direct_mode"},
			errors.New("native_import_direct_mode")
	}
	// The account is read from the selected profile's own files. A profile that
	// has not signed in has no identity, and an import into "whatever account
	// happens to be there" is exactly what this product must not do.
	account, verified := c.profileAccount()
	if !verified {
		return ImportResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	if expectedAccount == "" {
		expectedAccount = settings.AccountScope
	}
	if expectedAccount != "" && expectedAccount != account {
		return ImportResult{State: ImportStateAccountChanged, Reason: NativeAccountChanged}, nil
	}

	// A Direct Mode intent that is still active names the custom source this
	// gateway import replaces. It is disconnected only after the new gateway
	// source has been applied; only a source recorded by this tool is ever named.
	disconnectDirect := ""
	if settings.integrationMode() == IntegrationModeGateway {
		disconnectDirect = c.previousDirectSourceID(settings)
	}
	result, err := bridge.Client().ImportModels(ctx, ImportRequest{
		URL: settings.GatewayURL, Token: settings.GatewayToken,
		ExpectedAccountKey: account, OpenSettings: authorized,
		DisconnectDirectSourceID: disconnectDirect,
	})
	if err != nil {
		// A bridge that cannot be reached is not a page state: it is a failure the
		// caller has to report, and it says nothing about what Kimi would have done.
		return ImportResult{State: ImportStateFailed, Reason: safeReason(err)}, err
	}
	return c.gradeImport(result, account), nil
}

// gradeImport turns the page's answer into a state backed by readback evidence.
//
// The page only reports that the official API accepted the request. Whether the
// models are in place is answered by reconciling the profile and the generated
// config again, which is the same readback the periodic caller uses.
// Confirmation remains bound to the account named before the page request.
func (c *Controller) gradeImport(result ImportResult, expectedAccount string) ImportResult {
	if result.State == "" {
		result.State = importState(result)
	}
	if !result.OK {
		return result
	}
	integration := c.reconcile(true, expectedAccount)
	result.Readback = integration.Readback
	result.Adopted = integration.Adopted
	if integration.Reason == "account_changed" {
		result.State, result.Reason = ImportStateAccountChanged, NativeAccountChanged
		result.Applied, result.Adopted = false, false
		return result
	}
	if result.RequiresRestart {
		// The files hold the import but Kimi has not loaded it. Calling that
		// applied would tell the user to look for models the application has not
		// read yet.
		result.State = ImportStateRequiresRestart
		result.Reason = firstNonEmpty(result.Reason, NativeRequiresRestart)
		result.Applied = integration.Applied
		return result
	}
	if result.RouteCleanupPending && !result.RequiresRestart {
		result.State = ImportStateAccepted
		result.Reason = "route_cleanup_pending"
		result.Applied = integration.Applied
		return result
	}
	if integration.Applied {
		result.State = ImportStateApplied
		result.Applied = true
		return result
	}
	// Accepted, not applied: the readback disagrees with the page, and the
	// readback is the evidence that counts.
	result.State = ImportStateAccepted
	result.Applied = false
	result.Reason = firstNonEmpty(integration.Reason, result.Reason, "native_import_not_verified")
	return result
}

// importState maps a page answer onto the coarse vocabulary. It is used by the
// actions themselves so a result carries a state even when the controller is not
// in the loop.
func importState(result ImportResult) string {
	switch result.Reason {
	case NativeWaitingLogin:
		return ImportStateWaitingLogin
	case "native_bridge_unavailable", NativeWaitingWork:
		return ImportStateWaitingWork
	case NativeWaitingSettings, "native_bridge_settings_open_refused":
		return ImportStateWaitingSettings
	case NativeContractChanged:
		return ImportStateContractChanged
	case NativeAccountChanged, "native_import_account_unverified":
		return ImportStateAccountChanged
	}
	if result.RequiresRestart {
		return ImportStateRequiresRestart
	}
	if result.OK {
		return ImportStateAccepted
	}
	return ImportStateFailed
}

// profileAccount reads the account identity from the selected profile only. The
// path is the one this controller resolved for its managed child, so an account
// is never read from a directory outside that profile.
func (c *Controller) profileAccount() (string, bool) {
	home := c.kimiHome()
	if home == "" {
		return "", false
	}
	account := AccountScope(home)
	if account == "" {
		return "", false
	}
	return account, true
}

// readback reads what the profile and the generated config hold right now. It is
// the evidence half of an import; the caller's answer is the request half.
func (c *Controller) readback(account string) *ImportReadback {
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()
	mode := settings.integrationMode()
	home := c.kimiHome()
	if home == "" {
		return &ImportReadback{AccountDigest: account, AccountVerified: account != "", Mode: mode}
	}
	integration, err := NewNativeIntegration(settings.StateDir, home, mode)
	if err != nil {
		return &ImportReadback{AccountDigest: account, AccountVerified: account != "", Mode: mode}
	}
	result := integration.ReadbackMode(account, mode, "")
	return &result
}
