package kimi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AccountScope derives the Kimi account identity from the managed child's own
// files, using the same two sources the released build reads:
//
//	config.json credentials.kimiWeb.userId           a plain native id
//	config.json credentials.kimiCode.apiKey          a JWT whose payload carries
//	                                                 user_id or sub
//
// An identity that cannot be read answers empty, never a guess. The caller uses
// it to prove that an import is going into the account it was configured for.
func AccountScope(kimiDir string) string {
	config, kind := readJSONFile(filepath.Join(kimiDir, NativeWorkConfig), maxConfigBytes)
	if kind != "" {
		return ""
	}
	credentials, ok := config["credentials"].(map[string]any)
	if !ok {
		return ""
	}
	if web, ok := credentials["kimiWeb"].(map[string]any); ok {
		if id := stringField(web, "userId"); validAccountID(id) {
			return sha256Hex(id)
		}
	}
	code, ok := credentials["kimiCode"].(map[string]any)
	if !ok {
		return ""
	}
	token := stringField(code, "apiKey")
	pieces := strings.Split(token, ".")
	if len(pieces) != 3 || len(pieces[1]) > 32768 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(pieces[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	for _, key := range []string{"user_id", "sub"} {
		if id, ok := claims[key].(string); ok && validAccountID(id) {
			return sha256Hex(id)
		}
	}
	return ""
}

func validAccountID(value string) bool {
	if value == "" || len(value) > 1024 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// real PageActions over CDP

// cdpActions is the production PageActions. Every step is one CDP evaluation
// against an allowlisted page; nothing drives coordinates, reads page content
// for the caller, or navigates.
//
// The expressions are the released build's own, kept byte for byte where they
// touch the page API. They are the contract with the preload scripts:
//
//	clawBridgeAPI.importByokModels(token, baseUrl)   the official import
//	clawBridgeAPI.revealByok()                       feature availability
//	clawBridgeAPI.closeSettingsPage()                overlay close
//	__KIMI_DESKTOP_BRIDGE.closeKimiWorkSettingPage() main-view close
//	__KIMI_DESKTOP_BRIDGE.openKimiWorkTab()          show Work
//	kimiAgentAPI.openSettings(pane)                  open the overlay
type cdpActions struct {
	pipe     *Pipe
	kimiDir  string
	kimiHome string
	// mode is the integration mode this controller runs in, and directSourceID
	// is the managed custom source it verifies. They only change which source a
	// runtime model check reads; the page allowlist is the same either way.
	mode           string
	directSourceID string

	mu       sync.Mutex
	sessions map[string]string // target id -> CDP session id
}

// NewCDPActions builds the production PageActions for a managed child.
func NewCDPActions(pipe *Pipe, kimiDir, kimiHome string) PageActions {
	return NewCDPActionsForMode(pipe, kimiDir, kimiHome, IntegrationModeGateway, "")
}

// NewCDPActionsForMode builds the same actions for a controller that may be in
// Direct Mode.
func NewCDPActionsForMode(pipe *Pipe, kimiDir, kimiHome, mode, directSourceID string) PageActions {
	if mode != IntegrationModeDirect {
		directSourceID = ""
	}
	return &cdpActions{pipe: pipe, kimiDir: kimiDir, kimiHome: kimiHome,
		mode: mode, directSourceID: directSourceID, sessions: map[string]string{}}
}

func (a *cdpActions) setIntegrationMode(mode, sourceID string) {
	if mode != IntegrationModeDirect {
		sourceID = ""
	}
	a.mu.Lock()
	a.mode, a.directSourceID = mode, sourceID
	a.mu.Unlock()
}

func (a *cdpActions) integrationRoute() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode, a.directSourceID
}

const (
	actionCallTimeout   = 30 * time.Second
	settingsOpenReads   = 6
	settingsOpenStep    = 250 * time.Millisecond
	maxActionExpression = 32 * 1024
	// maxDirectExpression is bounded by the pipe's own frame limit: a direct
	// import carries a whole catalog and its keys.
	maxDirectExpression = 2 << 20
	maxDirectCatalog    = 256 << 10
)

func (a *cdpActions) Connected() bool {
	closed, _ := a.pipe.Closed()
	return !closed
}

func (a *cdpActions) evaluate(ctx context.Context, sessionID, expression string) (any, error) {
	return a.evaluateBounded(ctx, sessionID, expression, maxActionExpression)
}

func (a *cdpActions) evaluateBounded(ctx context.Context, sessionID, expression string, limit int) (any, error) {
	if len(expression) > limit {
		return nil, errors.New("native_bridge_expression_too_large")
	}
	params := map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}
	encoded, _ := json.Marshal(params)
	if len(encoded)+1 > MaxFrameBytes {
		return nil, errors.New("native_bridge_expression_too_large")
	}
	result, err := a.pipe.Send(ctx, "Runtime.evaluate", params, sessionID, actionCallTimeout)
	if err != nil {
		return nil, err
	}
	value, err := result.evaluatedValue()
	if err != nil {
		return nil, errors.New("native_bridge_contract_changed")
	}
	return value, nil
}

// targets returns the allowlisted page targets with a session each.
//
// The allowlist is the released build's: the desktop's own pages, by pathname,
// on app://localhost. A target outside it is never attached to.
func (a *cdpActions) targets(ctx context.Context) ([]actionTarget, error) {
	result, err := a.pipe.Send(ctx, "Target.getTargets", map[string]any{}, "", actionCallTimeout)
	if err != nil {
		return nil, err
	}
	rows, _ := result.Result["targetInfos"].([]any)
	var out []actionTarget
	for _, row := range rows {
		target, ok := row.(map[string]any)
		if !ok {
			continue
		}
		targetID, _ := target["targetId"].(string)
		raw, _ := target["url"].(string)
		path, ok := desktopPath(raw)
		if targetID == "" || !ok {
			continue
		}
		sessionID, err := a.session(ctx, targetID)
		if err != nil {
			continue
		}
		out = append(out, actionTarget{targetID: targetID, sessionID: sessionID, path: path, url: raw})
	}
	return out, nil
}

type actionTarget struct {
	targetID  string
	sessionID string
	path      string
	url       string
}

func desktopPath(raw string) (string, bool) {
	parsed, err := parseURL(raw)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return "", false
	}
	if parsed.Scheme != "app" || parsed.Host != "localhost" {
		return "", false
	}
	switch parsed.Path {
	case "/kimi-agent.html", "/desktopclaw.html", "/work-settings.html":
		return parsed.Path, true
	}
	return "", false
}

func (a *cdpActions) session(ctx context.Context, targetID string) (string, error) {
	a.mu.Lock()
	existing := a.sessions[targetID]
	a.mu.Unlock()
	if existing != "" {
		return existing, nil
	}
	result, err := a.pipe.Send(ctx, "Target.attachToTarget",
		map[string]any{"targetId": targetID, "flatten": true}, "", actionCallTimeout)
	if err != nil {
		return "", err
	}
	sessionID, _ := result.Result["sessionId"].(string)
	if sessionID == "" {
		return "", ErrRendererUnconfirmed
	}
	a.mu.Lock()
	a.sessions[targetID] = sessionID
	a.mu.Unlock()
	return sessionID, nil
}

// WorkSurface reads what each allowlisted page reports about itself.
func (a *cdpActions) WorkSurface(ctx context.Context) (WorkSurface, error) {
	targets, err := a.targets(ctx)
	if err != nil {
		return WorkSurface{}, err
	}
	surface := WorkSurface{OK: true}
	settingsVisible := false
	for _, target := range targets {
		surface.Pages++
		value, err := a.evaluate(ctx, target.sessionID, surfaceProbe)
		if err != nil {
			// A page that cannot be read is unknown, not a failure and not a
			// proof of absence.
			surface.Shown = append(surface.Shown, map[string]any{"path": target.path, "visible": nil, "painted": nil})
			continue
		}
		state, _ := value.(map[string]any)
		visible, _ := state["visible"].(bool)
		painted, _ := state["painted"].(bool)
		if target.path == "/work-settings.html" {
			if visible {
				settingsVisible = true
			}
			surface.Shown = append(surface.Shown, map[string]any{"path": target.path, "visible": visible, "painted": painted})
			continue
		}
		// A Work surface is one target that is both visible and painted. Only
		// that target may set both flags.
		if visible && painted {
			surface.Work = true
			surface.Rendered = true
		}
		if open, ok := state["open"].(bool); ok && open {
			surface.Settings = true
		}
		surface.Shown = append(surface.Shown, map[string]any{"path": target.path, "visible": visible, "painted": painted})
	}
	if settingsVisible {
		surface.Settings = true
	}
	return surface, nil
}

// surfaceProbe is the released build's read. It reads only visibility and a
// painted flag, never conversation text.
const surfaceProbe = `(function(){
var doc = globalThis.document;
if (!doc) return { visible: null, painted: null, text: 0, open: null };
var root = doc.getElementById && doc.getElementById("root");
var body = doc.body;
var text = body && typeof body.textContent === "string" ? body.textContent.replace(/\s+/g, "") : "";
var painted = Boolean((root && root.childElementCount > 0) || text.length > 0);
var bridge = globalThis.__KIMI_DESKTOP_BRIDGE;
var open = typeof bridge?.isSettingsPageOpen === "function" ? bridge.isSettingsPageOpen() : null;
return { visible: doc.visibilityState === "visible", painted: painted, text: text.length, open: open };
})()`

// ImportModels performs the official import.
//
// The order matters and is the released build's:
//
//  1. the expected account must still be the account this Kimi is signed into,
//     or the import is refused before anything is sent;
//  2. the page carrying importByokModels is looked for on every allowlisted
//     target;
//  3. when it is absent and the caller allowed it, the Work view's own
//     openSettings("general") opens the overlay, and the target is looked for
//     again, bounded;
//  4. revealByok() is consulted when present, so a build with the feature
//     switched off is reported rather than attempted;
//  5. the import runs, and the overlay is closed again through the official
//     control if this call opened it.
func (a *cdpActions) ImportModels(ctx context.Context, request ImportRequest) (ImportResult, error) {
	if !isLoopbackGateway(request.URL) {
		return ImportResult{State: ImportStateFailed, Reason: "native_bridge_invalid_request"}, nil
	}
	if request.Token == "" || len(request.Token) > 4096 {
		return ImportResult{State: ImportStateFailed, Reason: "native_bridge_invalid_request"}, nil
	}
	// The account is read from this managed profile's own files before any page
	// is touched. An empty expected key is not "any account will do": a profile
	// with no identity has not signed in, and importing into it would either fail
	// or land in an account nobody named.
	//
	// An empty kimiHome is refused rather than read: a relative path here would
	// make the identity lookup read whatever config.json happens to sit in the
	// working directory.
	if a.kimiHome == "" || !filepath.IsAbs(a.kimiHome) {
		return ImportResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	current := AccountScope(a.kimiHome)
	if current == "" {
		return ImportResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	// A configured expectation that disagrees with the profile is refused before
	// the page sees anything.
	if request.ExpectedAccountKey != "" && request.ExpectedAccountKey != current {
		return ImportResult{State: ImportStateAccountChanged, Reason: NativeAccountChanged}, nil
	}

	settings, opened, failure, err := a.resolveImportTarget(ctx, request.OpenSettings)
	if err != nil {
		return ImportResult{}, err
	}
	if failure != nil {
		return *failure, nil
	}

	// The import itself. The answer reports only what the caller may act on:
	// whether it applied, whether a restart is still owed, and how many models
	// the profile now holds. The profile rows, the token and every other native
	// field stay in the page.
	expression := importExpression(request.Token, request.URL, request.DisconnectDirectSourceID)
	value, err := a.evaluate(ctx, settings.sessionID, expression)
	if err != nil {
		result := ImportResult{State: ImportStateContractChanged, Reason: NativeContractChanged, Opened: opened}
		a.closeIfOpenedInto(ctx, opened, &result)
		return result, nil
	}
	answer, _ := value.(map[string]any)
	result := ImportResult{Opened: opened}
	if ok, _ := answer["ok"].(bool); ok {
		result.OK = true
		if count, ok := answer["models"].(float64); ok {
			result.Models = int(count)
		}
		if restart, ok := answer["requiresRestart"].(bool); ok {
			result.RequiresRestart = restart
		}
		if deferred, ok := answer["restartDeferred"].(bool); ok {
			result.RestartDeferred = deferred
		}
		if pending, ok := answer["routeCleanupPending"].(bool); ok {
			result.RouteCleanupPending = pending
		}
		// The page's own word: accepted. The controller grades this against a
		// readback before anything is called applied.
		result.State = ImportStateAccepted
	} else {
		code, _ := answer["code"].(string)
		if code == "" {
			code = "import-failed"
		}
		result.State = ImportStateFailed
		result.Reason = "native_import_" + safeActionCode(code)
	}
	// Only an overlay this call opened is closed, and the outcome is recorded
	// either way: a close that failed means the window may still be up.
	a.closeIfOpenedInto(ctx, opened, &result)
	return result, nil
}

// ImportDirect writes the caller's providers and models into a managed custom
// source through the official byokSources API.
//
// Everything the page schema would reject is refused here first: a non-loopback
// http endpoint, a key with a line break, a model without tool use. The account,
// target and overlay rules are the same ones ImportModels follows, because both
// import into the same signed-in page.
func (a *cdpActions) ImportDirect(ctx context.Context, request DirectImportRequest) (ImportResult, error) {
	if reason := validateDirectImportRequest(request); reason != "" {
		return ImportResult{State: ImportStateFailed, Reason: reason}, nil
	}
	if a.kimiHome == "" || !filepath.IsAbs(a.kimiHome) {
		return ImportResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	current := AccountScope(a.kimiHome)
	if current == "" {
		return ImportResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	if request.ExpectedAccountKey != "" && request.ExpectedAccountKey != current {
		return ImportResult{State: ImportStateAccountChanged, Reason: NativeAccountChanged}, nil
	}
	if request.DisplayName == "" {
		request.DisplayName = DirectDisplayName
	}

	settings, opened, failure, err := a.resolveImportTarget(ctx, request.OpenSettings)
	if err != nil {
		return ImportResult{}, err
	}
	if failure != nil {
		return *failure, nil
	}
	expression, err := directImportExpression(request)
	if err != nil {
		result := ImportResult{State: ImportStateFailed, Reason: "native_bridge_invalid_request", Opened: opened}
		a.closeIfOpenedInto(ctx, opened, &result)
		return result, nil
	}
	value, err := a.evaluateBounded(ctx, settings.sessionID, expression, maxDirectExpression)
	if err != nil {
		result := ImportResult{State: ImportStateContractChanged, Reason: NativeContractChanged, Opened: opened}
		if err.Error() == "native_bridge_expression_too_large" || errors.Is(err, ErrMessageTooLarge) {
			result.State, result.Reason = ImportStateFailed, "direct_catalog_too_large"
		}
		a.closeIfOpenedInto(ctx, opened, &result)
		return result, nil
	}
	answer, _ := value.(map[string]any)
	result := ImportResult{Opened: opened}
	if ok, _ := answer["ok"].(bool); ok {
		result.SourceID, _ = answer["sourceId"].(string)
		if result.SourceID == "" {
			// A save without a source id cannot be reconciled later, so it is not
			// reported as accepted.
			result.State, result.Reason = ImportStateContractChanged, NativeContractChanged
		} else {
			result.OK = true
			if count, ok := answer["models"].(float64); ok {
				result.Models = int(count)
			}
			if restart, ok := answer["requiresRestart"].(bool); ok {
				result.RequiresRestart = restart
			}
			if deferred, ok := answer["restartDeferred"].(bool); ok {
				result.RestartDeferred = deferred
			}
			if pending, ok := answer["routeCleanupPending"].(bool); ok {
				result.RouteCleanupPending = pending
			}
			result.State = ImportStateAccepted
		}
	} else {
		code, _ := answer["code"].(string)
		result.State = ImportStateFailed
		result.Reason = directImportFailureReason(code)
		if result.Reason == NativeContractChanged || result.Reason == "direct_mode_unsupported" {
			result.State = ImportStateContractChanged
		}
	}
	a.closeIfOpenedInto(ctx, opened, &result)
	return result, nil
}

// resolveImportTarget finds the page that carries the import API, opening the
// overlay only when the caller allowed it. A failure result is returned rather
// than an error when the page simply is not there; a transport error is
// returned as an error. `opened` says whether this call opened the overlay, so
// every early return can close it.
func (a *cdpActions) resolveImportTarget(ctx context.Context, openSettings bool) (actionTarget, bool, *ImportResult, error) {
	targets, err := a.targets(ctx)
	if err != nil {
		return actionTarget{}, false, nil, err
	}
	settings, found := a.findImportTarget(ctx, targets)
	opened := false
	if !found {
		if !openSettings {
			return actionTarget{}, false, &ImportResult{State: ImportStateWaitingSettings, Reason: NativeWaitingSettings}, nil
		}
		work, ok := findPath(targets, "/kimi-agent.html")
		if !ok {
			return actionTarget{}, false, &ImportResult{State: ImportStateWaitingWork, Reason: NativeWaitingWork}, nil
		}
		asked, err := a.evaluate(ctx, work.sessionID, openSettingsExpression)
		if err != nil || asked != true {
			return actionTarget{}, false, &ImportResult{State: ImportStateWaitingSettings, Reason: "native_bridge_settings_open_refused"}, nil
		}
		opened = true
		for attempt := 0; attempt < settingsOpenReads && !found; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(settingsOpenStep):
				case <-ctx.Done():
					a.closeIfOpened(ctx, opened)
					return actionTarget{}, opened, nil, ctx.Err()
				}
			}
			if fresh, err := a.targets(ctx); err == nil {
				targets = fresh
			}
			if candidate, ok := a.findImportTarget(ctx, targets); ok {
				settings, found = candidate, true
			}
		}
	}
	if !found {
		// The overlay this call opened is closed again: leaving it up would strand
		// a window the user did not ask for.
		result := ImportResult{State: ImportStateWaitingSettings, Reason: NativeWaitingSettings, Opened: opened}
		a.closeIfOpenedInto(ctx, opened, &result)
		return actionTarget{}, opened, &result, nil
	}
	return settings, opened, nil, nil
}

// closeIfOpened closes the overlay this call opened and records the outcome.
func (a *cdpActions) closeIfOpened(ctx context.Context, opened bool) {
	if !opened {
		return
	}
	_, _ = a.CloseWorkSettings(ctx)
}

// closeIfOpenedInto is closeIfOpened with the answer written into the result, so
// a failed close is reported rather than silently swallowed.
func (a *cdpActions) closeIfOpenedInto(ctx context.Context, opened bool, result *ImportResult) {
	if !opened || result == nil {
		return
	}
	closed, err := a.CloseWorkSettings(ctx)
	value := err == nil && closed.OK
	result.Closed = &value
}

// findImportTarget looks for the page that carries the import API.
func (a *cdpActions) findImportTarget(ctx context.Context, targets []actionTarget) (actionTarget, bool) {
	for _, target := range targets {
		value, err := a.evaluate(ctx, target.sessionID, importProbeExpression)
		if err != nil {
			// A page still mounting answers nothing yet; it is not evidence of
			// absence.
			continue
		}
		if present, _ := value.(bool); present {
			return target, true
		}
	}
	return actionTarget{}, false
}

// CloseWorkSettings asks both official close controls, on the overlay first.
func (a *cdpActions) CloseWorkSettings(ctx context.Context) (ActionResult, error) {
	targets, err := a.targets(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	if len(targets) == 0 {
		return ActionResult{Reason: "native_bridge_waiting_work"}, nil
	}
	// The overlay owns the close control and is the page that exists while
	// settings is open; the main view carries the other control. Both are asked
	// because which one a build wires up is not decidable here.
	ordered := make([]actionTarget, 0, len(targets))
	for _, target := range targets {
		if target.path == "/work-settings.html" {
			ordered = append(ordered, target)
		}
	}
	for _, target := range targets {
		if target.path != "/work-settings.html" {
			ordered = append(ordered, target)
		}
	}
	sawAPI := false
	var accepted, refused []string
	for _, target := range ordered {
		value, err := a.evaluate(ctx, target.sessionID, closeExpression)
		if err != nil {
			continue
		}
		probe, _ := value.(map[string]any)
		asked, _ := probe["asked"].([]any)
		failed, _ := probe["failed"].([]any)
		if len(asked) > 0 || len(failed) > 0 {
			sawAPI = true
		}
		for _, item := range asked {
			if surface, ok := item.(string); ok {
				accepted = append(accepted, surface)
			}
		}
		for _, item := range failed {
			if surface, ok := item.(string); ok {
				refused = append(refused, surface)
			}
		}
	}
	if len(accepted) > 0 {
		return ActionResult{OK: true, Via: accepted}, nil
	}
	if sawAPI {
		return ActionResult{Reason: "native_bridge_close_refused", Via: refused}, nil
	}
	return ActionResult{Reason: "native_bridge_contract_changed"}, nil
}

// ActivateWork asks the desktop's own show-Work action on the main Kimi view
// first, then on the Work view.
func (a *cdpActions) ActivateWork(ctx context.Context) (ActionResult, error) {
	targets, err := a.targets(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	if len(targets) == 0 {
		return ActionResult{Reason: "native_bridge_waiting_work"}, nil
	}
	sawAPI := false
	var accepted, refused []string
	for _, target := range targets {
		value, err := a.evaluate(ctx, target.sessionID, activateExpression)
		if err != nil {
			continue
		}
		probe, _ := value.(map[string]any)
		asked, _ := probe["asked"].(bool)
		failed, _ := probe["failed"].(bool)
		if asked || failed {
			sawAPI = true
		}
		name := "work"
		if target.path == "/desktopclaw.html" {
			name = "main"
		}
		if asked {
			accepted = append(accepted, name)
		}
		if failed {
			refused = append(refused, name)
		}
	}
	if len(accepted) > 0 {
		return ActionResult{OK: true, Via: accepted}, nil
	}
	if sawAPI {
		return ActionResult{Reason: "native_bridge_activation_refused", Via: refused}, nil
	}
	return ActionResult{Reason: "native_bridge_contract_changed"}, nil
}

// OpenWorkSettings opens the Work view's own settings page through the official
// API on that view only.
func (a *cdpActions) OpenWorkSettings(ctx context.Context) (ActionResult, error) {
	targets, err := a.targets(ctx)
	if err != nil {
		return ActionResult{}, err
	}
	work, ok := findPath(targets, "/kimi-agent.html")
	if !ok {
		return ActionResult{Reason: "native_bridge_waiting_work"}, nil
	}
	value, err := a.evaluate(ctx, work.sessionID, openSettingsExpression)
	if err != nil {
		return ActionResult{Reason: "native_bridge_contract_changed"}, nil
	}
	if asked, _ := value.(bool); asked {
		return ActionResult{OK: true, Via: []string{"work"}}, nil
	}
	return ActionResult{Reason: "native_bridge_settings_open_refused"}, nil
}

func findPath(targets []actionTarget, path string) (actionTarget, bool) {
	for _, target := range targets {
		if target.path == path {
			return target, true
		}
	}
	return actionTarget{}, false
}

// The page expressions. They are the released build's own calls.
const (
	importProbeExpression = `(function(){
const api = globalThis.clawBridgeAPI;
const sources = api?.byokSources;
return typeof api?.importByokModels === "function" || (typeof sources?.list === "function" && typeof sources.preview === "function" && typeof sources.save === "function");
})()`

	openSettingsExpression = `(function(){
var api = globalThis.kimiAgentAPI;
if (!api || typeof api.openSettings !== "function") return false;
try { api.openSettings("general"); return true; } catch (error) { return false; }
})()`

	closeExpression = `(function(){
var asked = [], failed = [];
var overlay = globalThis.clawBridgeAPI;
if (overlay && typeof overlay.closeSettingsPage === "function") {
  try { overlay.closeSettingsPage(); asked.push("overlay"); } catch (error) { failed.push("overlay"); }
}
var bridge = globalThis.__KIMI_DESKTOP_BRIDGE;
if (bridge && typeof bridge.closeKimiWorkSettingPage === "function") {
  try { bridge.closeKimiWorkSettingPage(); asked.push("bridge"); } catch (error) { failed.push("bridge"); }
}
return { asked: asked, failed: failed };
})()`

	activateExpression = `(function(){
var bridge = globalThis.__KIMI_DESKTOP_BRIDGE;
if (!bridge || typeof bridge.openKimiWorkTab !== "function") return { asked: false, failed: false };
try { bridge.openKimiWorkTab(); return { asked: true, failed: false }; }
catch (error) { return { asked: false, failed: true }; }
})()`
)

// importExpression renders the official import call. `disconnectDirectSourceID`
// is set when the controller switches from Direct Mode to the gateway: the old
// custom source is disconnected only after the replacement has been applied.
func importExpression(token, baseURL, disconnectDirectSourceID string) string {
	return `(async(token, baseURL, directSourceID)=>{
const api=globalThis.clawBridgeAPI;
if(typeof api?.revealByok==="function"){
  const state=await api.revealByok();
  if(state?.available===false)return {ok:false,code:"feature-disabled"};
}
const sources=api?.byokSources;
const unwrap=result=>result?.ok===true?result.value:null;
const disconnectDirect=async()=>{
  if(!directSourceID)return true;
  if(typeof sources?.list!=="function"||typeof sources.disconnect!=="function")return false;
  const current=unwrap(await sources.list());
  if(!current||!Array.isArray(current.sources)||typeof current.scopeToken!=="string")return false;
  let direct=current.sources.find(source=>source.id===directSourceID);
  if(!direct)direct=current.sources.find(source=>source.kind==="custom"&&source.displayName==="` + DirectDisplayName + `");
  if(!direct)return true;
  if(direct.kind!=="custom")return false;
  const reply=await sources.disconnect({sourceId:direct.id,accountRevision:current.accountRevision,
    sourceRevision:direct.revision??null});
  return reply?.ok===true;
};
if(typeof api?.importByokModels==="function"){
  const result=await api.importByokModels(token,baseURL);
  const imported=result?.ok===true;
  const requiresRestart=result?.requiresRestart===true;
  return {ok:imported,code:result?.code,routeCleanupPending:imported&&!!directSourceID&&
    (requiresRestart||!(await disconnectDirect())),
    requiresRestart:requiresRestart,restartDeferred:result?.restartDeferred===true,
    models:Array.isArray(result?.profile?.models)?result.profile.models.length:0};
}
if(!sources||typeof sources?.list!=="function"||typeof sources.preview!=="function"||typeof sources.save!=="function"){
  return {ok:false,code:"contract-changed"};
}
baseURL=baseURL.replace(/\/$/,"");
const catalogURL=baseURL+"/models/api.json";
const snapshot=unwrap(await sources.list());
if(!snapshot||!Array.isArray(snapshot.sources)||typeof snapshot.scopeToken!=="string"){
  return {ok:false,code:"contract-changed"};
}
const existing=snapshot.sources.find(source=>source.id==="byok-kth");
const ownPreviousLoopback=existing&&existing.kind==="kth"&&
  /^http:\/\/(?:127\.0\.0\.1|\[::1\]):\d+\/v1(?:\/models\/api\.json)?\/?$/.test(existing.registryUrl||"");
if(existing&&(existing.kind!=="kth"||(existing.registryUrl!==catalogURL&&existing.registryUrl!==baseURL&&!ownPreviousLoopback))){
  return {ok:false,code:"foreign-url"};
}
const hadOtherPending=snapshot.applyState!==undefined&&snapshot.applyState!=="applied"&&
  snapshot.sources.some(source=>source.id!==existing?.id);
const displayName=existing?.displayName;
const draft={kind:"kth",displayName:(!displayName||displayName==="KTH"||displayName==="Kimi Work+")?"KimiWork +":displayName,registryUrl:catalogURL,
  registryKey:{mode:"set",value:token},reuseRegistryCredential:true};
const runPreview=async body=>{
  const result=await sources.preview({sourceId:existing?.id,sourceRevision:existing?.revision??null,
    accountRevision:snapshot.accountRevision,draft:body});
  const preview=unwrap(result);
  if(!preview?.canSave||typeof preview.previewId!=="string"||!Array.isArray(preview.candidates)||!preview.candidates.length||
    preview.scopeToken!==snapshot.scopeToken||preview.accountRevision!==snapshot.accountRevision||
    preview.sourceRevision!==(existing?.revision??null)){
    if(typeof preview?.previewId==="string"&&typeof sources.discard==="function")await sources.discard(preview.previewId);
    return {ok:false,code:result?.reasonCode||"catalog-unconfirmed"};
  }
  return {ok:true,preview};
};
let step=await runPreview(draft);
if(!step.ok)return step;
let preview=step.preview;
const hasRouteSelection=p=>Array.isArray(p?.source?.notices)&&p.source.notices.some(n=>n?.reason==="route-selection-required");
let modelCount=preview.candidates.length;
if(hasRouteSelection(preview)){
  const selected=[];
  const seen=new Set();
  for(const candidate of preview.candidates){
    if(typeof candidate?.id!=="string"||!candidate.id)continue;
    const key=typeof candidate.modelId==="string"&&candidate.modelId?candidate.modelId:candidate.id;
    if(seen.has(key))continue;
    seen.add(key);
    selected.push(candidate.id);
  }
  if(typeof sources.discard==="function")await sources.discard(preview.previewId);
  if(!selected.length)return {ok:false,code:"route-selection-required"};
  step=await runPreview({...draft,selectedCandidates:selected});
  if(!step.ok)return step;
  preview=step.preview;
  if(hasRouteSelection(preview)){
    if(typeof sources.discard==="function")await sources.discard(preview.previewId);
    return {ok:false,code:"route-selection-required"};
  }
  modelCount=selected.length;
}
const saved=await sources.save({previewId:preview.previewId,accountRevision:preview.accountRevision,sourceRevision:preview.sourceRevision});
if(!saved?.ok||saved.value?.sourceId!==preview.sourceId)return {ok:false,code:saved?.reasonCode||"import-failed"};
let status=saved.value.status;
if(status==="pending"&&!hadOtherPending&&typeof sources.applyPending==="function"){
  const current=unwrap(await sources.list());
  if(current?.scopeToken!==snapshot.scopeToken||current.accountRevision!==saved.value.accountRevision){
    return {ok:false,code:"byok-source-stale"};
  }
  const applied=await sources.applyPending(saved.value.accountRevision);
  if(!applied?.ok)return {ok:false,code:applied?.reasonCode||"import-failed"};
  status=applied.value?.status;
}
if(!["applied","pending","deferred"].includes(status))return {ok:false,code:"import-failed"};
return {ok:true,routeCleanupPending:!!directSourceID&&(status!=="applied"||!(await disconnectDirect())),
  requiresRestart:status!=="applied",restartDeferred:status==="deferred",models:modelCount};
})(` + quoteJS(token) + `,` + quoteJS(baseURL) + `,` + quoteJS(disconnectDirectSourceID) + `)`
}

// isLoopbackGateway accepts only a loopback http endpoint with a /v1 path, which
// is what the gateway serves. Anything else is refused before the page sees it.
func isLoopbackGateway(value string) bool {
	parsed, err := parseURL(value)
	if err != nil || parsed.Scheme != "http" {
		return false
	}
	if parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "[::1]" && parsed.Hostname() != "::1" {
		return false
	}
	if parsed.Port() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Path == "/v1" || parsed.Path == "/v1/"
}

func directImportFailureReason(code string) string {
	code = strings.ReplaceAll(safeActionCode(code), "-", "_")
	code = strings.TrimPrefix(code, "native_import_")
	switch code {
	case "", "contract_changed":
		return NativeContractChanged
	case "direct_mode_unsupported", "native_direct_mode_unsupported":
		return "direct_mode_unsupported"
	case "direct_preview_refused", "direct_import_failed", "direct_foreign_source",
		"direct_disconnect_failed", "direct_disconnect_unavailable", "direct_catalog_too_large":
		return code
	case "foreign_source":
		return "direct_foreign_source"
	case "gateway_disconnect_failed":
		return "direct_gateway_disconnect_failed"
	case "import_failed":
		return "direct_import_failed"
	}
	return "native_import_" + code
}

// safeActionCode keeps an error code from the page inside the vocabulary the
// panel renders.
func safeActionCode(value string) string {
	out := strings.Builder{}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
			out.WriteRune(character)
		}
		if out.Len() >= 80 {
			break
		}
	}
	if out.Len() == 0 {
		return "failed"
	}
	return out.String()
}
