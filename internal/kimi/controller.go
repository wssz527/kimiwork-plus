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

// Controller is the one API the bridge calls. It owns the managed child, the
// native integration, the CDP host and the translation surface state, and it
// never touches an official Kimi it did not start.
//
// The hard rule this type enforces: an official Kimi that is already running and
// was not started here is never attached to, never signalled and never killed.
// `Start` reports `needs_user_close` instead, so the panel can ask the user to
// quit their own application and try again.

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
	// UserDataDir is the isolated profile for the managed child. It must live
	// inside StateDir; anything else is refused.
	UserDataDir string
	// KimiHome optionally names the Work data directory. It is honoured only
	// when it resolves inside UserDataDir.
	KimiHome string
	// GatewayURL and GatewayToken are what Kimi's import is pointed at. The
	// token is the gateway's own local token.
	GatewayURL   string
	GatewayToken string
	// Models is the catalog to import.
	Models []Model
	// AccountScope identifies the Kimi account, when known.
	AccountScope string
	// AllowNativeImport authorises the visible settings-page import.
	AllowNativeImport bool
	// Translation configures the renderer injection.
	Translation TranslationSettings
	// Executor bounds the contract probe.
	Executor ExecutorConfig
}

// TranslationSettings is the public renderer configuration.
type TranslationSettings struct {
	Enabled        bool   `json:"enabled"`
	Provider       string `json:"provider"`
	TargetLanguage string `json:"targetLanguage"`
	View           string `json:"view"`
}

// Revision is the settings scope digest. A change re-injects the renderer.
func (t TranslationSettings) Revision() string {
	return sha256Hex(stableJSON(map[string]any{"provider": t.Provider, "targetLanguage": t.TargetLanguage}))
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
type Controller struct {
	mu          sync.Mutex
	settings    ControllerSettings
	state       ControllerState
	reason      string
	child       *Child
	host        *Host
	bridge      *BridgeHost
	actions     PageActions
	handler     TranslationHandler
	native      *NativeState
	integration *IntegrationResult
	translation *TranslationStatus
	startedAt   time.Time
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
	if settings.StateDir == "" || !filepath.IsAbs(settings.StateDir) {
		return errors.New("controller_state_dir_invalid")
	}
	c.mu.Lock()
	c.settings = settings
	host := c.host
	c.mu.Unlock()
	if host != nil {
		// Configure takes the host's own lock and invalidates the sessions whose
		// scope changed, so this cannot race a concurrent Sync.
		host.Configure(translationPublic(settings.Translation), settings.Translation.Revision())
	}
	return nil
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

// Start launches a managed Kimi in an isolated profile.
//
// It refuses to proceed when an official Kimi is already running, because
// attaching to it would mean taking over an application the user started. It
// never sends a signal to that process: the answer is `needs_user_close` and the
// user decides.
func (c *Controller) Start(ctx context.Context) error {
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()

	if !ManagedChildConfigured() {
		c.setState(StateUnsupported, "platform_cannot_manage_child")
		return errors.New("platform_cannot_manage_child")
	}
	if settings.Installation.Application == "" {
		c.setState(StateFailed, "kimi_application_missing")
		return errors.New("kimi_application_missing")
	}
	profile, err := c.validateProfile(settings.UserDataDir)
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

	c.setState(StateStarting, "")
	child, err := StartKimi(ctx, LaunchOptions{Installation: settings.Installation,
		UserDataDir: settings.UserDataDir})
	if err != nil {
		c.setState(StateFailed, "kimi_start_failed")
		return err
	}
	c.mu.Lock()
	c.child = child
	c.mu.Unlock()

	c.setState(StateAttaching, "")
	return c.attachChild(ctx, child, settings.Translation)
}

// attachChild builds the host for a started child and publishes the native
// bridge. It is separate from Start so the branch that runs with translation off
// — the one that must still publish the import bridge — can be exercised without
// launching an application.
func (c *Controller) attachChild(ctx context.Context, child *Child, translation TranslationSettings) error {
	// The host is built even when translation is off: it owns the pipe the
	// native bridge shares, and enabling translation later must not require a
	// restart. What changes is that nothing is injected and no surface is
	// attached.
	payload, asset, err := translationPayload()
	if err != nil {
		return c.abortStart(child, "injection_payload_unavailable", err)
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
		// The released build's control-only behaviour: with translation off the
		// host keeps the pipe and injects nothing, and reports inactive rather
		// than attaching to a surface it would not translate.
		host.DetachAll(ctx)
		status := TranslationStatus{State: "inactive", Reason: "translation_disabled"}
		c.mu.Lock()
		c.translation = &status
		c.mu.Unlock()
		// The native bridge is not part of translation: it is how the panel
		// imports models into this child. Leaving it out whenever translation is
		// off would ship a managed Kimi with no import bridge at all, so it is
		// started on both paths.
		c.setState(StateReady, c.startNativeBridge(child))
		return nil
	}
	if _, err := host.Sync(ctx); err != nil {
		return c.abortStart(child, "translation_attach_failed", err)
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
		c.setState(StateFailed, "kimi_stop_failed")
		return err
	}
	c.setState(StateFailed, reason)
	return cause
}

// startNativeBridge publishes the import bridge for this child and answers the
// reason the launch is degraded, or "". The child and translation are already
// up, so a bridge that cannot start is reported rather than turned into a failed
// launch.
func (c *Controller) startNativeBridge(child *Child) string {
	// The native bridge shares this child's pipe, so it is published as part of
	// the launch rather than as a separate step a caller could forget. The
	// actions run over CDP against the pages this child serves.
	actions := NewCDPActions(child.Pipe(), c.kimiHome(), c.kimiHome())
	bridge, err := StartBridgeHost(c.kimiHome(), child.PID(), actions)
	if err != nil {
		return "native_bridge_unavailable"
	}
	c.mu.Lock()
	c.bridge = bridge
	c.actions = actions
	c.mu.Unlock()
	return ""
}

// Stop ends the managed child. It never touches any other process.
func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	child := c.child
	host := c.host
	bridge := c.bridge
	c.child = nil
	c.host = nil
	c.bridge = nil
	c.mu.Unlock()

	if host != nil {
		host.DetachAll(ctx)
	}
	if bridge != nil {
		_ = bridge.Close()
	}
	if child == nil {
		c.setState(StateStopped, "")
		return nil
	}
	if err := child.Stop(5 * time.Second); err != nil {
		c.setState(StateFailed, "kimi_stop_failed")
		return err
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
	enabled := c.settings.Translation.Enabled
	c.mu.Unlock()

	if host == nil || child == nil {
		return TranslationStatus{State: "inactive", Reason: "no_managed_child"}
	}
	if !enabled {
		// Nothing is injected while translation is off, and the surface report
		// must not claim a translation that was never applied.
		status := TranslationStatus{State: "inactive", Reason: "translation_disabled"}
		c.mu.Lock()
		c.translation = &status
		c.mu.Unlock()
		return status
	}
	sessions, err := host.Sync(ctx)
	if err != nil {
		status := TranslationStatus{State: "failed", Reason: safeReason(err)}
		c.mu.Lock()
		c.translation = &status
		c.mu.Unlock()
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
	c.translation = &status
	c.mu.Unlock()
	return status
}

// Reconcile runs the native integration against the configured catalog.
func (c *Controller) Reconcile() IntegrationResult {
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()

	kimiDir := c.kimiHome()
	integration, err := NewNativeIntegration(settings.StateDir, kimiDir, "gateway")
	if err != nil {
		result := IntegrationResult{State: IntegrationConflict, Reason: "invalid_integration_path"}
		c.mu.Lock()
		c.integration = &result
		c.mu.Unlock()
		return result
	}
	result := integration.Reconcile(ReconcileOptions{
		Enabled: settings.GatewayURL != "", URL: settings.GatewayURL, Models: settings.Models,
		AccountScope: settings.AccountScope, AllowNativeImport: settings.AllowNativeImport,
	})
	c.mu.Lock()
	c.integration = &result
	c.mu.Unlock()
	return result
}

// ImportModels performs the native import through the managed child's bridge.
//
// It is a separate call from Reconcile because it is a visible action: the
// caller decides when Kimi's own settings page may be opened. A result that
// still owes a restart says so instead of claiming the import applied.
func (c *Controller) ImportModels(ctx context.Context) (ImportResult, error) {
	c.mu.Lock()
	settings := c.settings
	bridge := c.bridge
	c.mu.Unlock()

	if bridge == nil {
		return ImportResult{}, errors.New("native_bridge_unavailable")
	}
	result, err := bridge.Client().ImportModels(ctx, ImportRequest{
		URL: settings.GatewayURL, Token: settings.GatewayToken,
		ExpectedAccountKey: settings.AccountScope, OpenSettings: settings.AllowNativeImport,
	})
	if err != nil {
		return ImportResult{}, err
	}
	return result, nil
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

// kimiHome is the Work data directory the managed child uses, derived the same
// way Kimi derives it: `daimon-storage.json` shareDir, else
// <profile>/daimon-share/daimon. A resolution that leaves the managed profile
// answers empty, so the integration refuses rather than writing the user's real
// installation.
func (c *Controller) kimiHome() string {
	c.mu.Lock()
	profile := c.settings.UserDataDir
	override := c.settings.KimiHome
	c.mu.Unlock()
	if profile == "" {
		return ""
	}
	home, err := ResolveKimiHome(profile, override)
	if err != nil {
		return ""
	}
	return home
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
	if resolved != realState && !strings.HasPrefix(resolved, realState+string(filepath.Separator)) {
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
