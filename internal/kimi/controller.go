package kimi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Controller owns the managed child, native integration, CDP host and translation
// surface state. Start never attaches to or signals an official Kimi it did not
// start; it reports needs_user_close. RestartOfficial is the explicit action that
// requests normal application exit before starting a managed child. It never
// force-kills an unmanaged process.

// ControllerState is the coarse state the panel renders.
type ControllerState string

const (
	// StateStopped means no managed child is running.
	StateStopped ControllerState = "stopped"
	// StateStarting means a managed child is launching.
	StateStarting ControllerState = "starting"
	// StateAttaching means the child is up and CDP is being established.
	StateAttaching ControllerState = "attaching"
	// StateReady means the child is up and the contract was confirmed.
	StateReady ControllerState = "ready"
	// StateFailed means the last attempt failed; Reason explains it.
	StateFailed ControllerState = "failed"
	// StateNeedsUserClose means an unmanaged Kimi is running. The user must
	// close it; this package will not.
	StateNeedsUserClose ControllerState = "needs_user_close"
	// StateUnsupported means this host cannot manage a child at all.
	StateUnsupported ControllerState = "unsupported"
)

// ControllerSettings is what the caller configures.
type ControllerSettings struct {
	// Installation selects the Kimi to manage. Required for Start.
	Installation Installation
	// StateDir is this product's private state directory.
	StateDir string
	// UserDataDir identifies the Work data root and, except in existing-authorized
	// mode, the Electron profile. It must resolve inside StateDir unless ProfileMode
	// is official.
	UserDataDir string
	// LoginDataDir is the Electron profile in existing-authorized mode. Official
	// mode sets it to the same official profile as UserDataDir.
	LoginDataDir string
	// KimiHome optionally names the Work data directory. It is honoured only
	// when it resolves inside UserDataDir.
	KimiHome string
	// GatewayURL and GatewayToken are what Kimi's import is pointed at. The
	// token is the gateway's own local token.
	GatewayURL   string
	GatewayToken string
	// Models is the catalog to import.
	Models []Model
	// AccountScope identifies the Kimi account, when known. It is only ever
	// compared against the account read from the selected profile; it never
	// supplies one.
	AccountScope string
	// AllowNativeImport is retained for callers that still set it. Overlay opening
	// is limited to one authorized import call or a verified own-loopback repair.
	// A permanently-on flag cannot authorize repeated overlays.
	AllowNativeImport bool
	// ProfileMode selects the profile the managed child runs against. Empty means
	// official. Existing-authorized retains login while isolating Work storage.
	ProfileMode string
	// Translation configures the renderer injection.
	Translation TranslationSettings
	// Executor bounds the contract probe.
	Executor ExecutorConfig
	// IntegrationMode selects how models reach Kimi. Empty means the gateway,
	// which is the released behaviour; `direct` imports the user's own
	// providers through the official API with no local gateway in between.
	IntegrationMode string
	// DirectSpec is the non-secret Direct Mode catalog: providers and models
	// without any key. Reconciliation and readback use it; the import itself
	// receives keys in its own DirectImportRequest.
	DirectSpec *DirectSpec
}

// TranslationSettings is the public renderer configuration.
type TranslationSettings struct {
	Enabled        bool   `json:"enabled"`
	Provider       string `json:"provider"`
	TargetLanguage string `json:"targetLanguage"`
	View           string `json:"view"`
}

// Revision is the settings scope digest. A change re-injects or withdraws the renderer.
func (t TranslationSettings) Revision() string {
	return sha256Hex(stableJSON(map[string]any{
		"enabled":        t.Enabled,
		"provider":       t.Provider,
		"targetLanguage": t.TargetLanguage,
	}))
}

// TranslationReply is one renderer request handed to the caller's provider.
type TranslationReply struct {
	SessionID  string
	Surface    string
	Generation int
	ID         string
	Value      any
}

// TranslationHandler is the provider the caller implements. The controller owns
// the host, the binding and the reply plumbing; the caller only answers.
type TranslationHandler func(ctx context.Context, reply TranslationReply) (any, error)

// ControllerStatus is the read-only snapshot the bridge exposes.
type ControllerStatus struct {
	State          ControllerState    `json:"state"`
	Reason         string             `json:"reason,omitempty"`
	ManagedPID     int                `json:"managedPid,omitempty"`
	ApplicationPID int                `json:"applicationPid,omitempty"`
	Contract       int                `json:"contract"`
	Native         *NativeState       `json:"native,omitempty"`
	Integration    *IntegrationResult `json:"integration,omitempty"`
	Translation    *TranslationStatus `json:"translation,omitempty"`
	Bridge         *BridgeDescriptor  `json:"bridge,omitempty"`
	Profile        string             `json:"profile,omitempty"`
}

// TranslationStatus reports what the renderer is doing, per surface.
type TranslationStatus struct {
	State      string                    `json:"state"`
	Reason     string                    `json:"reason,omitempty"`
	Surfaces   map[string]SurfaceSummary `json:"surfaces,omitempty"`
	Generation int                       `json:"generation,omitempty"`
}

// SurfaceSummary is one page's renderer report.
type SurfaceSummary struct {
	State     string `json:"state"`
	Structure string `json:"structure,omitempty"`
	Blocks    int    `json:"blocks,omitempty"`
	Seen      int    `json:"seenBlocks,omitempty"`
}

// Controller manages one Kimi install.
//
// Lock order is accountSessionMu -> lifecycle -> reconcileMu -> mu. Nothing
// takes mu and then reconcileMu: a reconcile reads its generation and settings
// under mu, releases it, and only then enters the critical section. reconcileMu
// covers every write this package makes to the official profile and to the page
// route, so a Configure that changes the mode cannot interleave with a reconcile
// that is writing the previous one.
type Controller struct {
	lifecycle   sync.Mutex
	reconcileMu sync.Mutex
	mu          sync.Mutex
	settings    ControllerSettings
	settingsGen uint64
	state       ControllerState
	reason      string
	child       *Child
	isolation   *workIsolation
	host        *Host
	bridge      *BridgeHost
	actions     PageActions
	handler     TranslationHandler
	native      *NativeState
	integration *IntegrationResult
	translation *TranslationStatus
	startedAt   time.Time
	// beforeReconcileHook runs between reading the generation and entering the
	// reconcile critical section. It is a test seam for the window a Configure
	// must be able to win; production leaves it nil.
	beforeReconcileHook func()
	// importGranted is held only while the one authorised import runs. It is
	// what makes the authorisation one-shot: a second call waits rather than
	// opening a second overlay.
	importGranted bool
	// repairSignature suppresses repeated automatic imports for the same observed
	// transient failure (a page that was not ready before any import was sent),
	// a stopped child or a new launch may retry. An uncertain dispatched write
	// remains suppressed until reconciliation or a process boundary.
	repairSignature string
	// loadedGeneration is the intent generation the running child was started
	// against. A child started before an import cannot have loaded it, so
	// adoption is decided by this number rather than by "a child is running".
	loadedGeneration int
	loadedKnown      bool
	// A matching catalog cannot prove a deferred credential change took effect.
	deferredChild *Child
}

// ErrUnmanagedKimiRunning is returned by Start when an official Kimi this
// controller did not start is already running.
var ErrUnmanagedKimiRunning = errors.New("unmanaged_kimi_running")

// NewController builds a controller. No process is started and no file is
// touched until Start or Reconcile is called.
func NewController(settings ControllerSettings) (*Controller, error) {
	if settings.StateDir == "" || !filepath.IsAbs(settings.StateDir) {
		return nil, errors.New("controller_state_dir_invalid")
	}
	return &Controller{settings: settings, state: StateStopped, startedAt: time.Now()}, nil
}

// SetTranslationHandler installs the caller's provider. The controller calls it
// for every renderer request and writes the answer back through the binding.
func (c *Controller) SetTranslationHandler(handler TranslationHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = handler
}

// Settings returns the current configuration.
func (c *Controller) Settings() ControllerSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings
}

// Configure replaces the settings. A change to the gateway target or the model
// catalog is applied on the next Reconcile; a change to the translation scope
// re-injects on the next Tick.
func (c *Controller) Configure(settings ControllerSettings) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if settings.StateDir == "" || !filepath.IsAbs(settings.StateDir) {
		return errors.New("controller_state_dir_invalid")
	}
	c.mu.Lock()
	if c.importGranted && settings.integrationMode() != c.settings.integrationMode() {
		c.mu.Unlock()
		return ErrImportBusy
	}
	if (c.child != nil || c.isolation != nil) && (settings.profileMode() != c.settings.profileMode() || !equalPath(settings.LoginDataDir, c.settings.LoginDataDir) || !equalPath(settings.UserDataDir, c.settings.UserDataDir) || !equalPath(settings.StateDir, c.settings.StateDir)) {
		c.mu.Unlock()
		return errors.New("profile_mode_change_requires_exit")
	}
	c.mu.Unlock()
	sourceID := c.directSourceID(settings)
	c.reconcileMu.Lock()
	c.mu.Lock()
	previousEnabled := c.settings.Translation.Enabled
	c.settings = settings
	c.settingsGen++
	actions := c.actions
	host := c.host
	c.mu.Unlock()
	updateActionsIntegrationRoute(actions, settings.integrationMode(), sourceID)
	c.reconcileMu.Unlock()
	if host != nil {
		// Configure takes the host's own lock and invalidates the sessions whose
		// scope changed, so this cannot race a concurrent Sync.
		host.Configure(translationPublic(settings.Translation), settings.Translation.Revision())
		if !settings.Translation.Enabled && (previousEnabled || host.HasSessions()) {
			withdrawCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			host.WithdrawAll(withdrawCtx)
			cancel()
			status := TranslationStatus{State: "inactive", Reason: "translation_disabled"}
			c.mu.Lock()
			c.translation = &status
			c.mu.Unlock()
		}
	}
	return nil
}

// beginConfigWrite ends the previous configuration generation and waits for any
// reconcile that is already inside its critical section to finish. A caller that
// is about to change the mode, the catalog or the intent uses it so that an
// older reconcile cannot write the previous configuration back over the new
// one: the reconcile aborts on the generation it no longer owns.
//
// It takes reconcileMu for the bump only. The caller does not hold it across its
// own work, because the reconcile that follows must be able to take it again.
func (c *Controller) beginConfigWrite() {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	c.mu.Lock()
	c.settingsGen++
	c.mu.Unlock()
}

// Status is a read-only snapshot. It never starts or stops anything.
func (c *Controller) Status(ctx context.Context) ControllerStatus {
	c.mu.Lock()
	status := ControllerStatus{
		State: c.state, Reason: c.reason, Contract: BridgeContractGeneration,
		Native: c.native, Integration: c.integration, Translation: c.translation,
	}
	if c.child != nil {
		status.ManagedPID = c.child.PID()
		status.ApplicationPID = c.child.PID()
	}
	c.mu.Unlock()
	return status
}

// PublicState is the snapshot the UI and the bridge may see. It carries no token,
// no private path and no descriptor: the endpoint is for this process's own
// client, not for anything that renders or logs.
func (c *Controller) PublicState(ctx context.Context) PublicState {
	status := c.Status(ctx)
	public := PublicState{
		State: status.State, Reason: status.Reason, Contract: status.Contract,
		ManagedPID: status.ManagedPID, ApplicationPID: status.ApplicationPID,
		Native: status.Native, Integration: status.Integration, Translation: status.Translation,
	}
	c.mu.Lock()
	published := c.bridge != nil
	c.mu.Unlock()
	if published {
		// Only the fact that a bridge is published, never where or with what.
		public.Bridge = &BridgePublication{Published: true, Contract: BridgeContractGeneration}
	}
	profile := c.ProfileStatus()
	public.Profile = &profile
	return public
}

// PublicState is the renderable view of a controller.
type PublicState struct {
	State          ControllerState    `json:"state"`
	Reason         string             `json:"reason,omitempty"`
	ManagedPID     int                `json:"managedPid,omitempty"`
	ApplicationPID int                `json:"applicationPid,omitempty"`
	Contract       int                `json:"contract"`
	Native         *NativeState       `json:"native,omitempty"`
	Integration    *IntegrationResult `json:"integration,omitempty"`
	Translation    *TranslationStatus `json:"translation,omitempty"`
	Bridge         *BridgePublication `json:"bridge,omitempty"`
	// Profile reports the profile mode and its digests, never the path.
	Profile *ProfileStatus `json:"profileStatus,omitempty"`
}

// BridgePublication says a bridge exists without naming its endpoint.
type BridgePublication struct {
	Published bool `json:"published"`
	Contract  int  `json:"contract"`
}

// NativeState re-reads the official application state. It is always read-only.
func (c *Controller) NativeState(ctx context.Context) NativeState {
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()
	state := DetectState(ctx, ProbeOptions{Archive: settings.Installation.Archive,
		Executor: settings.Executor})
	c.mu.Lock()
	c.native = &state
	c.mu.Unlock()
	return state
}

// Start launches a managed Kimi in the selected profile.
//
// It is idempotent for the child this controller already owns: when that child is
// still running, Start checks it (the published bridge answers, or the host
// re-syncs) and returns without starting a second application. A child that has
// exited is recorded as such, never revived.
//
// It refuses to proceed when an official Kimi is already running that this
// controller did not start, because attaching to it would mean taking over an
// application the user started. It never sends a signal to that process: the
// answer is `needs_user_close` and the user decides.
func (c *Controller) Start(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	return c.start(ctx)
}

func (c *Controller) start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.New("kimi_start_cancelled")
	}
	c.reapChildLocked()
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()

	if !ManagedChildConfigured() {
		c.setState(StateUnsupported, "platform_cannot_manage_child")
		return errors.New("platform_cannot_manage_child")
	}
	// The managed child, if this controller still holds a live one, is checked
	// rather than duplicated. A second launch would leave two applications on one
	// profile, which is the failure this branch exists to prevent. It comes first
	// because a running child is already the answer: there is nothing to launch
	// and no installation left to resolve.
	if c.managedChildAlive() {
		return c.checkManagedChild(ctx)
	}
	if settings.Installation.Application == "" {
		c.setState(StateFailed, "kimi_application_missing")
		return errors.New("kimi_application_missing")
	}
	if err := c.requireSupportedProfileMode(); err != nil {
		c.setState(StateFailed, err.Error())
		return err
	}
	var profile string
	var err error
	if settings.profileMode() == ProfileModeOfficial {
		profile, err = c.validateOfficialProfile(settings.UserDataDir)
	} else {
		profile, err = c.validateProfile(settings.UserDataDir)
	}
	if err != nil {
		c.setState(StateFailed, err.Error())
		return err
	}
	settings.UserDataDir = profile

	native := c.NativeState(ctx)
	if native.Application == Running {
		// Only a child this controller started may be attached to. An official
		// process the user launched is reported, never signalled.
		c.setState(StateNeedsUserClose, "official_kimi_already_running")
		return ErrUnmanagedKimiRunning
	}
	if native.Application == RunningStateUnknown {
		c.setState(StateFailed, "kimi_process_state_unknown")
		return errors.New("kimi_process_state_unknown")
	}

	if err := c.restoreWorkIsolation(); err != nil {
		reason := stableErrorCode(err, "work_storage_restore_failed")
		c.setState(StateFailed, reason)
		return errors.New(reason)
	}
	launchProfile := settings.UserDataDir
	if settings.profileMode() == ProfileModeExistingAuthorized {
		home := c.kimiHome()
		if home == "" {
			return ErrKimiHomeUnresolved
		}
		lease, err := beginWorkIsolation(settings.LoginDataDir, settings.UserDataDir, filepath.Dir(home))
		if err != nil {
			reason := stableErrorCode(err, "work_storage_isolation_failed")
			c.setState(StateFailed, reason)
			return errors.New(reason)
		}
		c.mu.Lock()
		c.isolation = lease
		c.mu.Unlock()
		if err := verifyExistingLoginStorage(settings.Installation); err != nil {
			if restoreErr := c.restoreWorkIsolation(); restoreErr != nil {
				reason := stableErrorCode(restoreErr, "work_storage_restore_failed")
				c.setState(StateFailed, reason)
				return errors.New(reason)
			}
			reason := stableErrorCode(err, "work_storage_contract_unverified")
			c.setState(StateFailed, reason)
			return errors.New(reason)
		}
		launchProfile = settings.LoginDataDir
	}
	c.setState(StateStarting, "")
	child, err := StartKimi(ctx, LaunchOptions{Installation: settings.Installation,
		UserDataDir: launchProfile})
	if err != nil {
		if restoreErr := c.restoreWorkIsolation(); restoreErr != nil {
			reason := stableErrorCode(restoreErr, "work_storage_restore_failed")
			c.setState(StateFailed, reason)
			return errors.New(reason)
		}
		reason := startErrorCode(err)
		c.setState(StateFailed, reason)
		return errors.New(reason)
	}
	// The child is about to read the profile as it is now, so it can load the
	// current intent generation. That is what later adoption compares against.
	// It is read before the lock: currentGeneration takes c.mu itself.
	generation := c.currentGeneration()
	c.mu.Lock()
	c.child = child
	c.deferredChild = nil
	c.loadedGeneration = generation
	c.loadedKnown = true
	c.mu.Unlock()

	c.setState(StateAttaching, "")
	return c.attachChild(ctx, child, settings.Translation)
}

// currentGeneration reads the intent generation for this controller's profile
// without writing anything. It answers 0 when there is no intent yet.
func (c *Controller) currentGeneration() int {
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()
	home := c.kimiHome()
	if home == "" {
		return 0
	}
	integration, err := NewNativeIntegration(settings.StateDir, home, settings.integrationMode())
	if err != nil {
		return 0
	}
	return integration.ReadbackMode("", settings.integrationMode(), "").Generation
}

// HasManagedBridge reports whether this controller currently holds a live
// managed Kimi process with an active native bridge.
func (c *Controller) HasManagedBridge() bool {
	c.reapChild()
	c.mu.Lock()
	child := c.child
	bridge := c.bridge
	c.mu.Unlock()
	if bridge == nil {
		return false
	}
	if child == nil {
		return true
	}
	select {
	case <-child.Exit():
		return false
	default:
		return true
	}
}

// managedChildAlive reports whether this controller still holds a child that has
// not exited. It is the whole idempotency test: a live child is this
// controller's own, so it is checked rather than duplicated.
func (c *Controller) managedChildAlive() bool {
	c.mu.Lock()
	child := c.child
	c.mu.Unlock()
	if child == nil {
		return false
	}
	select {
	case <-child.Exit():
		return false
	default:
		return true
	}
}

// checkManagedChild re-confirms a child that is already running. It answers the
// same contract the launch would have established, without starting anything:
// the bridge must answer, or the host must re-sync when there is no bridge.
func (c *Controller) checkManagedChild(ctx context.Context) error {
	c.mu.Lock()
	bridge := c.bridge
	host := c.host
	enabled := c.settings.Translation.Enabled
	c.mu.Unlock()

	if bridge != nil {
		status, err := bridge.Client().Status(ctx)
		if err == nil && status.OK {
			c.setState(StateReady, "")
			return nil
		}
	}
	if host != nil && enabled {
		if _, err := host.Sync(ctx); err == nil {
			c.setState(StateReady, "")
			return nil
		}
	}
	if host != nil {
		// The pipe is still this controller's own; only the bridge is missing.
		c.setState(StateReady, "native_bridge_unavailable")
		return nil
	}
	// A live child with no host at all cannot be checked. It is not a reason to
	// start a second application, so the state says what is missing.
	c.setState(StateFailed, "managed_child_unattached")
	return errors.New("managed_child_unattached")
}

// reapChild notices a managed child that exited on its own and records it.
//
// It never restarts and never pretends the exit did not happen: the child this
// controller started is gone, so the state becomes `stopped` with the exit kind
// as its reason. Nothing is signalled — the process is already gone.
func (c *Controller) reapChild() bool {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	return c.reapChildLocked()
}

func (c *Controller) reapChildLocked() bool {
	c.mu.Lock()
	child := c.child
	c.mu.Unlock()
	if child == nil {
		return false
	}
	select {
	case <-child.Exit():
	default:
		return false
	}
	exit := child.ExitState()

	c.mu.Lock()
	host := c.host
	bridge := c.bridge
	c.child = nil
	c.deferredChild = nil
	c.host = nil
	c.bridge = nil
	c.actions = nil
	// The suppression belongs to the process that failed to answer; that process
	// is gone, so a later launch must be free to repair again.
	c.repairSignature = ""
	c.mu.Unlock()

	if host != nil {
		host.DetachAll(context.Background())
	}
	if bridge != nil {
		_ = bridge.Close()
	}
	reason := "managed_child_exited"
	if exit.Kind != "" {
		reason = "managed_child_exited_" + exit.Kind
	}
	status := TranslationStatus{State: "inactive", Reason: "no_managed_child"}
	c.mu.Lock()
	c.translation = &status
	c.mu.Unlock()
	if err := c.restoreWorkIsolation(); err != nil {
		c.setState(StateFailed, stableErrorCode(err, "work_storage_restore_failed"))
		return true
	}
	c.setState(StateStopped, reason)
	return true
}

// attachChild builds the host for a started child and publishes the native
// bridge. It is separate from Start so the branch that runs with translation off
// — the one that must still publish the import bridge — can be exercised without
// launching an application.
func (c *Controller) attachChild(ctx context.Context, child *Child, translation TranslationSettings) error {
	// The host is built even when translation is off: it owns the pipe the
	// native bridge shares, and enabling translation later must not require a
	// restart. Page-enhancement failures stay isolated from launch and bridge.
	payload, asset, err := translationPayload()
	if err != nil {
		status := TranslationStatus{State: "failed", Reason: "injection_payload_unavailable"}
		c.mu.Lock()
		c.translation = &status
		c.mu.Unlock()
		c.setState(StateReady, c.startNativeBridge(child))
		return nil
	}
	_ = asset

	host := NewHost(child.Pipe(), InjectionOptions{
		Payload:  string(payload),
		Revision: translation.Revision(),
		Settings: translationPublic(translation),
		OnReply:  c.onReply,
	})
	c.mu.Lock()
	c.host = host
	c.mu.Unlock()

	if !translation.Enabled {
		// Control-only launch keeps the host alive (not stopped) so enabling
		// translation at runtime can attach and deliver replies without restart.
		status := TranslationStatus{State: "inactive", Reason: "translation_disabled"}
		c.mu.Lock()
		c.translation = &status
		c.mu.Unlock()
		c.setState(StateReady, c.startNativeBridge(child))
		return nil
	}
	if _, err := host.Sync(ctx); err != nil {
		status := TranslationStatus{State: "failed", Reason: firstNonEmpty(safeReason(err), "translation_attach_failed")}
		c.mu.Lock()
		c.translation = &status
		c.mu.Unlock()
		c.setState(StateReady, c.startNativeBridge(child))
		return nil
	}
	c.setState(StateReady, c.startNativeBridge(child))
	return nil
}

// abortStart unwinds a launch whose initialisation failed after the child was
// started. Only the child this call created is stopped, so a failed launch
// cannot leave an application running with no handle, and no other process is
// ever signalled.
func (c *Controller) abortStart(child *Child, reason string, cause error) error {
	c.mu.Lock()
	host := c.host
	bridge := c.bridge
	c.child = nil
	c.host = nil
	c.bridge = nil
	c.actions = nil
	c.mu.Unlock()
	if host != nil {
		host.DetachAll(context.Background())
	}
	if bridge != nil {
		_ = bridge.Close()
	}
	// The child is this call's own; it never started successfully, so it is
	// stopped here rather than reported as a running managed Kimi.
	if err := child.Stop(5 * time.Second); err != nil {
		c.mu.Lock()
		c.child = child
		c.mu.Unlock()
		c.setState(StateFailed, "kimi_stop_failed")
		return errors.New("kimi_stop_failed")
	}
	if err := c.restoreWorkIsolation(); err != nil {
		reason := stableErrorCode(err, "work_storage_restore_failed")
		c.setState(StateFailed, reason)
		return errors.New(reason)
	}
	c.setState(StateFailed, reason)
	if reason != "" {
		return errors.New(reason)
	}
	return cause
}

func isStableErrorCode(value string) bool {
	if len(value) < 2 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		ch := value[i]
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_') {
			return false
		}
	}
	return true
}

func stableErrorCode(err error, fallback string) string {
	if err == nil {
		return ""
	}
	if text := err.Error(); isStableErrorCode(text) {
		return text
	}
	return fallback
}

func startErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errManagedChildUnsupported) {
		return "platform_cannot_manage_child"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "kimi_start_cancelled"
	}
	s := err.Error()
	for _, code := range []string{
		"kimi_application_missing",
		"kimi_user_data_dir_invalid",
		"kimi_user_data_dir_unavailable",
		"kimi_start_cancelled",
		"kimi_start_failed",
	} {
		if s == code || strings.HasPrefix(s, code+": ") {
			return code
		}
	}
	return "kimi_start_failed"
}

// startNativeBridge publishes the import bridge for this child and answers the
// reason the launch is degraded, or "". The child and translation are already
// up, so a bridge that cannot start is reported rather than turned into a failed
// launch.
func (c *Controller) startNativeBridge(child *Child) string {
	// The native bridge shares this child's pipe, so it is published as part of
	// the launch rather than as a separate step a caller could forget. The
	// actions run over CDP against the pages this child serves.
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()
	home := c.kimiHome()
	actions := NewCDPActionsForMode(child.Pipe(), home, home,
		settings.integrationMode(), c.directSourceID(settings))
	bridge, err := StartBridgeHost(home, child.PID(), actions)
	if err != nil {
		return "native_bridge_unavailable"
	}
	c.mu.Lock()
	c.bridge = bridge
	c.actions = actions
	c.mu.Unlock()
	return ""
}

// directSourceID reads the managed custom source from this tool's own intent.
// It is only used to aim a runtime read at the right source; the display-name
// fallback still covers a source whose id changed.
func (c *Controller) directSourceID(settings ControllerSettings) string {
	if settings.integrationMode() != IntegrationModeDirect {
		return ""
	}
	return c.previousDirectSourceID(settings)
}

func updateActionsIntegrationRoute(actions PageActions, mode, sourceID string) {
	if configurable, ok := actions.(interface{ setIntegrationMode(string, string) }); ok {
		configurable.setIntegrationMode(mode, sourceID)
	}
}

func (c *Controller) syncActionsIntegrationRoute(settings ControllerSettings, sourceID string) {
	if settings.integrationMode() == IntegrationModeDirect && sourceID == "" {
		sourceID = c.directSourceID(settings)
	}
	c.mu.Lock()
	actions := c.actions
	c.mu.Unlock()
	updateActionsIntegrationRoute(actions, settings.integrationMode(), sourceID)
}

// previousDirectSourceID answers the active Direct Mode intent first. Once a
// gateway reconcile has replaced that intent, the official store still names a
// connected managed source whose disconnect failed or was deferred.
func (c *Controller) previousDirectSourceID(settings ControllerSettings) string {
	home := c.kimiHome()
	account := AccountScope(home)
	scopeDigest := ""
	if account != "" {
		scopeDigest = sha256Hex(account)
	}
	native, err := NewNativeIntegration(settings.StateDir, home, IntegrationModeGateway)
	if err == nil {
		intent, intentErr := native.readIntent()
		if intentErr == nil && intent != nil && intent.Active && intent.Mode == IntegrationModeDirect && intent.DirectSourceID != "" &&
			(scopeDigest == "" || intent.AccountDigest == "" || intent.AccountDigest == scopeDigest) {
			return intent.DirectSourceID
		}
	}
	if settings.integrationMode() != IntegrationModeGateway {
		return ""
	}
	return DirectSourceRemains(home, account, "")
}

// Stop ends the managed child. It never touches any other process.
func (c *Controller) Stop(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.mu.Lock()
	child := c.child
	host := c.host
	bridge := c.bridge
	c.child = nil
	c.host = nil
	c.bridge = nil
	c.actions = nil
	// Stopping the child ends the process a repair suppression was recorded
	// against; a later start must be able to repair again.
	c.repairSignature = ""
	c.mu.Unlock()

	if host != nil {
		host.DetachAll(ctx)
	}
	if bridge != nil {
		_ = bridge.Close()
	}
	if child != nil {
		if err := child.Stop(5 * time.Second); err != nil {
			c.mu.Lock()
			c.child = child
			c.mu.Unlock()
			c.setState(StateFailed, "kimi_stop_failed")
			return err
		}
	}
	if err := c.restoreWorkIsolation(); err != nil {
		reason := stableErrorCode(err, "work_storage_restore_failed")
		c.setState(StateFailed, reason)
		return errors.New(reason)
	}
	c.setState(StateStopped, "")
	return nil
}

// Tick re-syncs the injection and the surface report. It is what a periodic
// caller runs; it does nothing when no child is running.
func (c *Controller) Tick(ctx context.Context) TranslationStatus {
	c.mu.Lock()
	host := c.host
	child := c.child
	translation := c.settings.Translation
	c.mu.Unlock()

	if child == nil {
		return TranslationStatus{State: "inactive", Reason: "no_managed_child"}
	}
	if !translation.Enabled {
		if host != nil && host.HasSessions() {
			host.WithdrawAll(ctx)
		}
		// Nothing is injected while translation is off, and the surface report
		// must not claim a translation that was never applied.
		status := TranslationStatus{State: "inactive", Reason: "translation_disabled"}
		c.mu.Lock()
		if c.child == child && !c.settings.Translation.Enabled {
			c.translation = &status
		}
		c.mu.Unlock()
		return status
	}
	if host == nil {
		payload, _, err := translationPayload()
		if err != nil {
			status := TranslationStatus{State: "failed", Reason: "injection_payload_unavailable"}
			c.mu.Lock()
			if c.child == child && c.settings.Translation.Enabled {
				c.translation = &status
			}
			c.mu.Unlock()
			return status
		}
		c.mu.Lock()
		if c.child == child && c.host == nil && c.settings.Translation.Enabled {
			liveTranslation := c.settings.Translation
			// NewHost must be built under the same lock that publishes it: two
			// concurrent Ticks would otherwise both see host == nil and the loser
			// would leave a detached host whose event loop still reads the pipe.
			c.host = NewHost(child.Pipe(), InjectionOptions{
				Payload:  string(payload),
				Revision: liveTranslation.Revision(),
				Settings: translationPublic(liveTranslation),
				OnReply:  c.onReply,
			})
		}
		host = c.host
		stillSameChild := c.child == child
		stillEnabled := stillSameChild && c.settings.Translation.Enabled
		c.mu.Unlock()
		if !stillSameChild || host == nil {
			return TranslationStatus{State: "inactive", Reason: "no_managed_child"}
		}
		if !stillEnabled {
			return TranslationStatus{State: "inactive", Reason: "translation_disabled"}
		}
	}
	sessions, err := host.Sync(ctx)
	if err != nil {
		status := TranslationStatus{State: "failed", Reason: safeReason(err)}
		c.mu.Lock()
		stillActive := c.child == child && c.host == host && c.settings.Translation.Enabled
		if stillActive {
			c.translation = &status
		}
		c.mu.Unlock()
		if !stillActive {
			return TranslationStatus{State: "inactive", Reason: "translation_disabled"}
		}
		return status
	}
	status := TranslationStatus{Surfaces: map[string]SurfaceSummary{}}
	active := 0
	failed := 0
	for _, session := range sessions {
		blocks, seen := session.Counts()
		summary := SurfaceSummary{State: session.State(), Structure: session.Structure(),
			Blocks: blocks, Seen: seen}
		status.Surfaces[session.Surface] = summary
		if session.State() == "active" && session.Structure() == "matched" {
			active++
		}
		if session.Failed() {
			failed++
		}
		status.Generation = session.Generation()
	}
	switch {
	case failed > 0:
		status.State = "failed"
	case active > 0:
		status.State = "active"
	case len(sessions) > 0:
		status.State = "attaching"
		status.Reason = "translation_waiting_view"
	default:
		status.State = "inactive"
		status.Reason = "translation_waiting_view"
	}
	c.mu.Lock()
	childMatch := c.child == child && c.host == host
	enabledNow := c.settings.Translation.Enabled
	if childMatch && enabledNow {
		c.translation = &status
	}
	c.mu.Unlock()
	if !childMatch {
		return TranslationStatus{State: "inactive", Reason: "no_managed_child"}
	}
	if !enabledNow {
		host.WithdrawAll(ctx)
		disabled := TranslationStatus{State: "inactive", Reason: "translation_disabled"}
		c.mu.Lock()
		if c.child == child && c.host == host && !c.settings.Translation.Enabled {
			c.translation = &disabled
		}
		c.mu.Unlock()
		return disabled
	}
	return status
}

// Reconcile runs the native integration against the configured catalog.
//
// It reads the account from the selected profile first. A profile that has not
// signed in is reported as `account-unverified` rather than reconciled against
// whatever account happens to be there, because an import into the wrong account
// is the one mistake that cannot be undone from here.
func (c *Controller) Reconcile() IntegrationResult {
	return c.reconcile(false, "")
}

// ReconcileWithReadback is the call the production loop uses: reconcile, then
// read back the directory, the account, the URL and the config generation, and
// grade the result from that evidence.
//
// The account is always taken from this controller's selected profile, never
// from the caller's configuration and never from a directory outside it.
func (c *Controller) ReconcileWithReadback(ctx context.Context) IntegrationResult {
	result, gen, stale := c.reconcileChecked(true, "")
	if stale {
		return result
	}
	if result.Applied && !result.Adopted && !result.Changed && accountSessionMu.TryLock() {
		defer accountSessionMu.Unlock()
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		result = c.confirmRunningCatalog(ctx, result, gen)
	}
	return result
}

func (c *Controller) reconcile(withReadback bool, confirmedAccount string) IntegrationResult {
	result, _, _ := c.reconcileChecked(withReadback, confirmedAccount)
	return result
}

func (c *Controller) staleReconcileResult(gen uint64) (bool, IntegrationResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.settingsGen == gen {
		return false, IntegrationResult{}
	}
	if c.integration != nil {
		return true, *c.integration
	}
	return true, IntegrationResult{State: IntegrationNeedsImport, Mode: c.settings.integrationMode()}
}

func (c *Controller) commitReconcileResult(gen uint64, result IntegrationResult, withReadback bool) (IntegrationResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.settingsGen != gen {
		if c.integration != nil {
			return *c.integration, true
		}
		return IntegrationResult{State: IntegrationNeedsImport, Mode: c.settings.integrationMode()}, true
	}
	if withReadback && result.Applied && result.Readback != nil {
		result.Adopted = c.runtimeLoadedLocked(result.Readback.Generation)
	}
	c.integration = &result
	return result, false
}

func (c *Controller) reconcileChecked(withReadback bool, confirmedAccount string) (IntegrationResult, uint64, bool) {
	c.mu.Lock()
	settings := c.settings
	gen := c.settingsGen
	hook := c.beforeReconcileHook
	c.mu.Unlock()

	// A child that exited is recorded before anything is written for it.
	c.reapChild()

	if hook != nil {
		hook()
	}

	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()

	if stale, current := c.staleReconcileResult(gen); stale {
		return current, gen, true
	}

	account, verified := c.profileAccount()
	scope := account
	if confirmedAccount != "" && (!verified || confirmedAccount != account) {
		result := IntegrationResult{State: IntegrationNeedsImport, Reason: "account_changed",
			AccountDigest: sha256If(account), Readback: c.readback(account)}
		committed, stale := c.commitReconcileResult(gen, result, false)
		return committed, gen, stale
	}
	if !verified {
		// Nothing is read from a profile without an identity, and no reconcile
		// writes into it either: the answer is waiting for the user's login.
		if withReadback {
			result := IntegrationResult{State: IntegrationAccountUnknown, Reason: "waiting-login",
				Readback: c.readback("")}
			committed, stale := c.commitReconcileResult(gen, result, false)
			return committed, gen, stale
		}
		scope = settings.AccountScope
	}

	kimiDir := c.kimiHome()
	mode := settings.integrationMode()
	integration, err := NewNativeIntegration(settings.StateDir, kimiDir, mode)
	if err != nil {
		result := IntegrationResult{State: IntegrationConflict, Reason: "invalid_integration_path", Mode: mode}
		committed, stale := c.commitReconcileResult(gen, result, false)
		return committed, gen, stale
	}
	options := ReconcileOptions{AccountScope: scope, AllowAccountRebind: confirmedAccount != "", Mode: mode}
	if mode == IntegrationModeDirect {
		options.Enabled = settings.DirectSpec != nil && len(settings.DirectSpec.Models) > 0
		options.DirectSpec = settings.DirectSpec
	} else {
		options.Enabled = settings.GatewayURL != ""
		options.URL = settings.GatewayURL
		options.Models = settings.Models
	}
	if stale, current := c.staleReconcileResult(gen); stale {
		return current, gen, true
	}
	result := integration.Reconcile(options)
	if stale, current := c.staleReconcileResult(gen); stale {
		return current, gen, true
	}
	c.syncActionsIntegrationRoute(settings, result.DirectSourceID)
	if withReadback {
		readback := integration.ReadbackMode(account, mode, result.DirectSourceID)
		result.Readback = &readback
		if readback.AccountDigest != "" {
			// IntegrationResult.AccountDigest uses the intent-level scope digest
			// sha256(store key), whereas ImportReadback.AccountDigest carries the
			// store key sha256(uid) itself.
			result.AccountDigest = sha256Hex(readback.AccountDigest)
		}
		// `Applied` is the readback's answer, not the reconcile's: the profile and
		// the generated config must both hold the models. In Direct Mode the
		// evidence is the managed custom source, never the gateway URL.
		if mode == IntegrationModeDirect {
			result.Applied = readback.HoldsDirect(settings.DirectSpec)
		} else {
			result.Applied = readback.Holds(settings.GatewayURL)
		}
		result.Applied = result.Applied && (result.State == IntegrationReady || result.State == IntegrationRepaired)
	}
	committed, stale := c.commitReconcileResult(gen, result, withReadback)
	return committed, gen, stale
}

func (c *Controller) recordImportReload(child *Child, deferred bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if child == nil || c.child != child {
		return
	}
	if deferred {
		c.deferredChild = child
		c.loadedKnown = false
		if c.integration != nil && c.integration.Adopted {
			current := *c.integration
			current.Adopted = false
			c.integration = &current
		}
	} else if c.deferredChild == child {
		c.deferredChild = nil
	}
}

func (c *Controller) runtimeLoadedLocked(generation int) bool {
	child := c.child
	if c.bridge == nil || child == nil || c.deferredChild == child || !c.loadedKnown || generation <= 0 {
		return false
	}
	select {
	case <-child.Exit():
		return false
	default:
		return c.loadedGeneration >= generation
	}
}

// WorkSurface reports what is on screen, through the managed child's bridge.
func (c *Controller) WorkSurface(ctx context.Context) (WorkSurface, error) {
	c.mu.Lock()
	bridge := c.bridge
	c.mu.Unlock()
	if bridge == nil {
		return WorkSurface{}, errors.New("native_bridge_unavailable")
	}
	return bridge.Client().WorkSurface(ctx)
}

// StartBridge publishes the native bridge endpoint over the managed pipe.
func (c *Controller) StartBridge(ctx context.Context, actions PageActions) error {
	c.mu.Lock()
	child := c.child
	c.mu.Unlock()
	if child == nil {
		return errors.New("no_managed_child")
	}
	host, err := StartBridgeHost(c.kimiHome(), child.PID(), actions)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.bridge = host
	c.actions = actions
	c.mu.Unlock()
	return nil
}

// Close releases everything this controller owns.
func (c *Controller) Close(ctx context.Context) error {
	return c.Stop(ctx)
}

// ---------------------------------------------------------------------------
// internals

func (c *Controller) setState(state ControllerState, reason string) {
	c.mu.Lock()
	c.state = state
	c.reason = reason
	c.mu.Unlock()
}

// WorkHome is the Work data directory the managed child uses, derived the same
// way Kimi derives it: `daimon-storage.json` shareDir, else
// <profile>/daimon-share/daimon. Isolated modes keep the result inside the
// selected profile; official mode also accepts a local relocated shareDir.
func (c *Controller) WorkHome() (string, error) {
	c.mu.Lock()
	profile := c.settings.UserDataDir
	override := c.settings.KimiHome
	mode := c.settings.profileMode()
	c.mu.Unlock()
	if profile == "" {
		return "", ErrKimiHomeUnresolved
	}
	if mode == ProfileModeOfficial {
		return ResolveOfficialKimiHome(profile, override)
	}
	return ResolveKimiHome(profile, override)
}

func (c *Controller) kimiHome() string {
	home, err := c.WorkHome()
	if err != nil {
		return ""
	}
	return home
}

func (c *Controller) validateOfficialProfile(candidate string) (string, error) {
	expected, err := OfficialProfileDirectory()
	if err != nil {
		return "", err
	}
	resolved, err := resolveAllowMissing(candidate)
	if err != nil {
		return "", errors.New("user_data_dir_invalid")
	}
	official, err := resolveAllowMissing(expected)
	if err != nil || !equalPath(resolved, official) {
		return "", errors.New("official_profile_mismatch")
	}
	if _, err := os.Lstat(expected); !errors.Is(err, os.ErrNotExist) {
		if err := validateExistingProfileDirectory(expected); err != nil {
			return "", err
		}
	}
	return filepath.Clean(candidate), nil
}

// validateProfile requires the managed profile to live inside this product's own
// state directory, with no symlink escape.
//
// A single string comparison against the macOS profile path was not enough: it
// said nothing on Windows, and it accepted a child directory of the real profile
// — which is still the user's signed-in tree. Containment is the property that
// actually matters, and it is platform independent.
func (c *Controller) validateProfile(candidate string) (string, error) {
	if candidate == "" || !filepath.IsAbs(candidate) {
		return "", errors.New("user_data_dir_invalid")
	}
	stateDir, err := filepath.Abs(c.settings.StateDir)
	if err != nil {
		return "", errors.New("controller_state_dir_invalid")
	}
	realState, err := resolveAllowMissing(stateDir)
	if err != nil {
		return "", errors.New("controller_state_dir_invalid")
	}
	resolved, err := resolveAllowMissing(candidate)
	if err != nil {
		return "", errors.New("user_data_dir_invalid")
	}
	if !pathWithinDir(resolved, realState) {
		return "", errors.New("user_data_dir_outside_state")
	}
	// Return the caller's own path shape: only the containment test needs the
	// resolved form, and returning /private/var/... for a /var/... input would
	// surprise every later comparison.
	resolved = filepath.Clean(candidate)
	// A profile that already exists must not be a symlink: the containment check
	// resolved it, but a link here would let a later rename move it.
	if info, err := os.Lstat(resolved); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("user_data_dir_symlinked")
	}
	return resolved, nil
}

func translationPublic(settings TranslationSettings) map[string]any {
	return map[string]any{
		"enabled":        settings.Enabled,
		"provider":       settings.Provider,
		"targetLanguage": settings.TargetLanguage,
		"view":           settings.View,
	}
}

// onReply is the one place a renderer request becomes a caller callback and a
// reply. The answer is written back only if the generation still matches, which
// Reply enforces.
func (c *Controller) onReply(ctx context.Context, session *SurfaceSession, generation int, id string, value any) {
	c.mu.Lock()
	handler := c.handler
	host := c.host
	c.mu.Unlock()
	if handler == nil || host == nil {
		return
	}
	answer, err := handler(ctx, TranslationReply{SessionID: session.SessionID, Surface: session.Surface,
		Generation: generation, ID: id, Value: value})
	if err != nil {
		// A provider failure is reported to the renderer as a failure, never as
		// an empty translation that would replace the original text.
		answer = map[string]any{"error": "translation_provider_failed"}
	}
	_ = host.Reply(ctx, session, generation, id, answer)
}

// String renders a one-line status for logs.
func (s ControllerStatus) String() string {
	parts := []string{string(s.State)}
	if s.Reason != "" {
		parts = append(parts, s.Reason)
	}
	if s.ManagedPID > 0 {
		parts = append(parts, fmt.Sprintf("pid=%d", s.ManagedPID))
	}
	if s.Translation != nil {
		parts = append(parts, "translation="+s.Translation.State)
	}
	return strings.Join(parts, " ")
}
