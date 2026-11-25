package kimi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	actionCallTimeout = 30 * time.Second
	// overlayCloseTimeout bounds the close this call still owes after its own
	// context ended.
	overlayCloseTimeout = 5 * time.Second
	maxActionExpression = 32 * 1024
	// maxDirectExpression is bounded by the pipe's own frame limit: a direct
	// import carries a whole catalog and its keys.
	maxDirectExpression = 2 << 20
	maxDirectCatalog    = 256 << 10
)

var (
	// settingsReadyWindow bounds how long an import that opened the settings
	// overlay waits for its preload API. The page target appears before the API
	// mounts, and a slow start can take seconds; a fixed handful of reads
	// rejected exactly that case.
	settingsReadyWindow = 15 * time.Second
	settingsReadyStep   = 250 * time.Millisecond
	// daimonBootWindow bounds how long a write waits for the official runtime to
	// leave its startup states. Daimon reaches `ready` in 3-5 seconds after a
	// launch; this is the long tail, not the expected wait.
	daimonBootWindow = 15 * time.Second
	// daimonStatusTimeout bounds one status read, so a wedged runtime cannot hold
	// the write for the whole call timeout.
	daimonStatusTimeout = 2 * time.Second
)

// daimonBootStates are the official runtime states a write must not run in:
// the main process answers `busy` and the source save comes back deferred.
var daimonBootStates = map[string]bool{
	"provisioning": true, "spawning": true, "connecting": true,
	"starting": true, "restarting": true,
}

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
		return nil, fmt.Errorf("%w: %s", ErrRendererUnconfirmed, NativeContractChanged)
	}
	return value, nil
}

// targets returns the allowlisted page targets with a session each.
//
// The allowlist is the released build's: the desktop's own pages, by pathname,
// on app://localhost. A target outside it is never attached to, and only a
// `type: "page"` entry is a page: a worker or iframe that happens to carry the
// same URL is not one.
func (a *cdpActions) targets(ctx context.Context) ([]actionTarget, error) {
	result, err := a.pipe.Send(ctx, "Target.getTargets", map[string]any{}, "", actionCallTimeout)
	if err != nil {
		return nil, err
	}
	rows, _ := result.Result["targetInfos"].([]any)
	var out []actionTarget
	present := make(map[string]bool, len(rows))
	for _, row := range rows {
		target, ok := row.(map[string]any)
		if !ok {
			continue
		}
		targetID, _ := target["targetId"].(string)
		kind, _ := target["type"].(string)
		if targetID == "" || kind != "page" {
			continue
		}
		present[targetID] = true
		raw, _ := target["url"].(string)
		path, ok := desktopPath(raw)
		if !ok {
			continue
		}
		sessionID, err := a.session(ctx, targetID)
		if err != nil {
			continue
		}
		out = append(out, actionTarget{targetID: targetID, sessionID: sessionID, path: path, url: raw})
	}
	// A closed target's session id is dead. Keeping it would make every later
	// call to that page answer "session not found" forever.
	a.forgetSessionsExcept(present)
	return out, nil
}

// forgetSessionsExcept drops cached sessions for targets that are gone.
func (a *cdpActions) forgetSessionsExcept(present map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for targetID := range a.sessions {
		if !present[targetID] {
			delete(a.sessions, targetID)
		}
	}
}

func (a *cdpActions) session(ctx context.Context, targetID string) (string, error) {
	a.mu.Lock()
	existing := a.sessions[targetID]
	a.mu.Unlock()
	if existing != "" {
		return existing, nil
	}
	return a.attach(ctx, targetID)
}

// attach issues one Target.attachToTarget and records the session.
func (a *cdpActions) attach(ctx context.Context, targetID string) (string, error) {
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

// dropSession forgets one cached session so the next call re-attaches.
func (a *cdpActions) dropSession(targetID string) {
	a.mu.Lock()
	delete(a.sessions, targetID)
	a.mu.Unlock()
}

// evaluateTarget runs one read-only evaluation, re-attaching once when the
// cached session turns out to be dead. A page that was reloaded takes its
// session with it; that is a page to re-read, not a changed contract. The
// refreshed session is written back into `target`, so the caller's later calls
// use the live one.
//
// Only read-only probes go through here. A write is never replayed: a caller
// that must confirm the effect re-reads the page instead.
func (a *cdpActions) evaluateTarget(ctx context.Context, target *actionTarget, expression string) (any, error) {
	value, err := a.evaluate(ctx, target.sessionID, expression)
	if !errors.Is(err, ErrSessionInvalid) {
		return value, err
	}
	a.dropSession(target.targetID)
	sessionID, attachErr := a.attach(ctx, target.targetID)
	if attachErr != nil {
		return nil, attachErr
	}
	target.sessionID = sessionID
	return a.evaluate(ctx, sessionID, expression)
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
		value, err := a.evaluateTarget(ctx, &target, surfaceProbe)
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
	if request.Token == "" || len(request.Token) > 4096 || (request.RepairProviderID != "" &&
		(!byokUUIDProvider.MatchString(request.RepairProviderID) || request.ExpectedAccountKey == "" || request.DisconnectDirectSourceID != "")) {
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

	settings, opened, failure, err := a.resolveImportTarget(ctx, request.OpenSettings, request.ActivateWork)
	if err != nil {
		return ImportResult{}, err
	}
	if failure != nil {
		return *failure, nil
	}

	// A write sent while Daimon is still provisioning is answered "busy" and
	// deferred, which is exactly the restart loop this wait ends.
	if err := a.waitForDaimonNotBooting(ctx, nil); err != nil {
		state, reason, surfaced := classifyActionError(err)
		result := ImportResult{State: state, Reason: reason, Opened: opened}
		a.closeIfOpenedInto(ctx, opened, &result)
		return result, surfaced
	}
	// The official allocator reuses `byok-kth-kimi` only while the store names it
	// on this source and its route, or names neither anywhere. Repairing that
	// first is what lets the import below keep in-place model switching.
	//
	// The repair is not a guarantee: Electron's own post-restart model refresh
	// can write a preview's source back to a random id between the repair and
	// the save. Such a preview is discarded, never saved, and the repair runs
	// again on the bytes Electron left behind.
	mode, _ := a.integrationRoute()
	attempts := 1
	if mode != IntegrationModeDirect {
		attempts = kthRepairPasses
	}
	expression := importExpression(request.Token, request.URL, request.DisconnectDirectSourceID)
	var answer map[string]any
	var result ImportResult
	if request.RepairProviderID != "" {
		scan := kthStoreState(a.kimiHome, current, request.URL)
		if mode == IntegrationModeDirect || AccountScope(a.kimiHome) != current || !scan.needsDisconnect() || scan.stuck != request.RepairProviderID {
			result := ImportResult{State: ImportStateFailed, Reason: "native_import_store_changed", Opened: opened}
			a.closeIfOpenedInto(ctx, opened, &result)
			return result, nil
		}
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if mode != IntegrationModeDirect {
			stop, err := a.repairKthProvider(ctx, settings, current, request.URL, request.DisconnectDirectSourceID)
			if err != nil {
				state, reason, surfaced := classifyActionError(err)
				result := ImportResult{State: state, Reason: reason, Opened: opened}
				a.closeIfOpenedInto(ctx, opened, &result)
				return result, surfaced
			}
			if stop != nil {
				result := *stop
				result.Opened = opened
				a.closeIfOpenedInto(ctx, opened, &result)
				return result, nil
			}
		}
		// The import itself. The answer reports only what the caller may act on:
		// whether it applied, whether a restart is still owed, and how many models
		// the profile now holds. The profile rows, the token and every other native
		// field stay in the page.
		value, err := a.evaluate(ctx, settings.sessionID, expression)
		if err != nil {
			if errors.Is(err, ErrSessionInvalid) {
				a.dropSession(settings.targetID)
			}
			state, reason, surfaced := classifyActionError(err)
			result := ImportResult{State: state, Reason: reason, Opened: opened}
			a.closeIfOpenedInto(ctx, opened, &result)
			return result, surfaced
		}
		answer, _ = value.(map[string]any)
		result = ImportResult{Opened: opened}
		code := ""
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
			code, _ = answer["code"].(string)
			if code == "" {
				code = "import-failed"
			}
			result.State = ImportStateFailed
			result.Reason = "native_import_" + safeActionCode(code)
			if code == "non-canonical-provider" {
				result.Reason = "native_import_non_canonical_provider"
			}
		}
		if code == "non-canonical-provider" && attempt+1 < attempts {
			if err := a.waitStep(ctx); err != nil {
				state, reason, surfaced := classifyActionError(err)
				result := ImportResult{State: state, Reason: reason, Opened: opened}
				a.closeIfOpenedInto(ctx, opened, &result)
				return result, surfaced
			}
			continue
		}
		break
	}
	// A save that landed inside the post-ready window answered `deferred` with
	// its pending revision still on disk. Applying it now avoids a Kimi restart
	// the user does not need; anything short of "applied" keeps the restart
	// prompt, so a real running turn is never interrupted.
	if result.OK && result.RestartDeferred {
		scopeToken, _ := answer["applyScopeToken"].(string)
		revision, _ := answer["applyRevision"].(string)
		if scopeToken != "" && revision != "" {
			outcome, err := a.retryDeferredApply(ctx, settings, scopeToken, revision)
			if err != nil {
				state, reason, surfaced := classifyActionError(err)
				result := ImportResult{State: state, Reason: reason, Opened: opened}
				a.closeIfOpenedInto(ctx, opened, &result)
				return result, surfaced
			}
			switch outcome {
			case "applied":
				result.RequiresRestart = false
				result.RestartDeferred = false
				if result.RouteCleanupPending && request.DisconnectDirectSourceID != "" {
					outcome := a.disconnectDirectSourceOnce(ctx, settings, request.DisconnectDirectSourceID)
					recordCleanupOutcome(&result, outcome)
				}
			case "apply-failed":
				result.OK = false
				result.State = ImportStateFailed
				result.Reason = "native_import_apply_failed"
				result.RequiresRestart = false
				result.RestartDeferred = false
			case "account-changed":
				result.OK = false
				result.State = ImportStateAccountChanged
				result.Reason = NativeAccountChanged
				result.RequiresRestart = false
				result.RestartDeferred = false
			}
		}
	}
	// A route cleanup answered inside Electron's post-ready window carries the
	// revision its own disconnect left behind; the same retry that finishes a
	// deferred save settles it here.
	if result.OK && !result.RequiresRestart && result.RouteCleanupPending {
		a.settleDeferredCleanup(ctx, settings, answer, &result)
	}
	// Only an overlay this call opened is closed, and the outcome is recorded
	// either way: a close that failed means the window may still be up.
	a.closeIfOpenedInto(ctx, opened, &result)
	return result, nil
}

// settleDeferredCleanup finishes a route cleanup Electron answered with
// `pending` or `deferred`. Only an applied cleanup clears the claim; `deferred`
// and `apply-failed` keep it and ask for the restart that removes the old
// source. The caller can recover a stale cleanup from the returned outcome.
func (a *cdpActions) settleDeferredCleanup(ctx context.Context, settings actionTarget, answer map[string]any, result *ImportResult) string {
	scopeToken, _ := answer["cleanupApplyScopeToken"].(string)
	revision, _ := answer["cleanupApplyRevision"].(string)
	if scopeToken == "" || revision == "" {
		return ""
	}
	outcome, err := a.retryDeferredApply(ctx, settings, scopeToken, revision)
	if err != nil {
		return ""
	}
	recordCleanupOutcome(result, outcome)
	return outcome
}

func recordCleanupOutcome(result *ImportResult, outcome string) {
	switch outcome {
	case "applied":
		result.RouteCleanupPending = false
	case "deferred", "apply-failed":
		result.RequiresRestart = true
		result.RestartDeferred = outcome == "deferred"
	}
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

	settings, opened, failure, err := a.resolveImportTarget(ctx, request.OpenSettings, request.ActivateWork)
	if err != nil {
		return ImportResult{}, err
	}
	if failure != nil {
		return *failure, nil
	}
	if err := a.waitForDaimonNotBooting(ctx, nil); err != nil {
		state, reason, surfaced := classifyActionError(err)
		result := ImportResult{State: state, Reason: reason, Opened: opened}
		a.closeIfOpenedInto(ctx, opened, &result)
		return result, surfaced
	}
	result, retryScopeToken, err := a.importDirectOnce(ctx, settings, request, "")
	if retryScopeToken != "" && request.DisconnectGateway {
		if request.ExistingSourceID == "" {
			request.ExistingSourceID = result.SourceID
		}
		result, _, err = a.importDirectOnce(ctx, settings, request, retryScopeToken)
	}
	result.Opened = opened
	a.closeIfOpenedInto(ctx, opened, &result)
	return result, err
}

// importDirectOnce returns the scope token for a cleanup whose revision became stale.
func (a *cdpActions) importDirectOnce(ctx context.Context, settings actionTarget, request DirectImportRequest, cleanupScopeToken string) (ImportResult, string, error) {
	expression, err := directImportExpression(request, cleanupScopeToken)
	if err != nil {
		result := ImportResult{State: ImportStateFailed, Reason: "native_bridge_invalid_request"}
		return result, "", nil
	}
	value, err := a.evaluateBounded(ctx, settings.sessionID, expression, maxDirectExpression)
	if err != nil {
		if errors.Is(err, ErrSessionInvalid) {
			a.dropSession(settings.targetID)
		}
		state, reason, surfaced := classifyActionError(err)
		result := ImportResult{State: state, Reason: reason}
		if err.Error() == "native_bridge_expression_too_large" || errors.Is(err, ErrMessageTooLarge) {
			result.State, result.Reason = ImportStateFailed, "direct_catalog_too_large"
			surfaced = nil
		}
		return result, "", surfaced
	}
	answer, _ := value.(map[string]any)
	result := ImportResult{}
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
		switch result.Reason {
		case NativeContractChanged, "direct_mode_unsupported":
			result.State = ImportStateContractChanged
		case NativeAccountChanged:
			result.State = ImportStateAccountChanged
		}
	}
	// The apply may have landed inside the post-ready window and answered
	// `deferred`. Applying the pending revision now finishes the import without
	// the restart the deferred answer asked for; every other outcome keeps the
	// restart prompt intact.
	if result.OK && result.RestartDeferred {
		scopeToken, _ := answer["applyScopeToken"].(string)
		revision, _ := answer["applyRevision"].(string)
		if scopeToken != "" && revision != "" {
			outcome, err := a.retryDeferredApply(ctx, settings, scopeToken, revision)
			if err != nil {
				state, reason, surfaced := classifyActionError(err)
				result := ImportResult{State: state, Reason: reason}
				return result, "", surfaced
			}
			switch outcome {
			case "applied":
				result.RequiresRestart = false
				result.RestartDeferred = false
				if result.RouteCleanupPending && request.DisconnectGateway {
					outcome := a.disconnectGatewayOnce(ctx, settings, request.DisconnectGatewayURL)
					recordCleanupOutcome(&result, outcome)
				}
			case "apply-failed":
				result.OK = false
				result.State = ImportStateFailed
				result.Reason = directImportFailureReason("apply-failed")
				result.RequiresRestart = false
				result.RestartDeferred = false
			case "account-changed":
				result.OK = false
				result.State = ImportStateAccountChanged
				result.Reason = NativeAccountChanged
				result.RequiresRestart = false
				result.RestartDeferred = false
			}
		}
	}
	if result.OK && !result.RequiresRestart && result.RouteCleanupPending {
		outcome := a.settleDeferredCleanup(ctx, settings, answer, &result)
		if outcome == "stale" {
			scopeToken, _ := answer["cleanupApplyScopeToken"].(string)
			return result, scopeToken, nil
		}
	}
	return result, "", nil
}

// resolveImportTarget finds the page that carries the import API, opening the
// overlay only when the caller allowed it. A failure result is returned rather
// than an error when the page simply is not there; a transport error is
// returned as an error. `opened` says whether this call opened the overlay, so
// every early return can close it.
//
// A slow start is the normal case this has to survive: initial CDP target
// enumeration may wait for a newly launched Electron process, the Work page may
// still be mounting, and the settings overlay exists as a target before its
// preload API does. Target acquisition and settings readiness each get a bounded
// window after the preceding stage settles, and both stop immediately when the
// outer context or the pipe ends.
func (a *cdpActions) resolveImportTarget(ctx context.Context, openSettings, activateWork bool) (actionTarget, bool, *ImportResult, error) {
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
		workDeadline := time.Now().Add(settingsReadyWindow)
		work, ok := findPath(targets, "/kimi-agent.html")
		if !ok {
			// The Work page itself may still be mounting, or Kimi may be sitting on
			// its chat view. An explicit user action may ask the desktop shell once
			// to switch into Work before waiting on the Work-readiness deadline.
			work, ok = a.waitForWorkTarget(ctx, &targets, workDeadline, activateWork)
			if !ok {
				if err := a.transportEnded(ctx); err != nil {
					return actionTarget{}, false, nil, err
				}
				return actionTarget{}, false, &ImportResult{State: ImportStateWaitingWork, Reason: NativeWaitingWork}, nil
			}
		}
		openDeadline := time.Now().Add(settingsReadyWindow)
		asked, mounted, err := a.openSettingsWhenReady(ctx, work, openDeadline)
		if err != nil {
			return actionTarget{}, false, nil, err
		}
		if !asked {
			reason := NativeWaitingSettings
			if mounted {
				reason = "native_bridge_settings_open_refused"
			}
			return actionTarget{}, false, &ImportResult{State: ImportStateWaitingSettings, Reason: reason}, nil
		}
		opened = true
		settingsDeadline := time.Now().Add(settingsReadyWindow)
		for !found {
			fresh, err := a.targets(ctx)
			if err != nil {
				a.closeIfOpened(ctx, opened)
				return actionTarget{}, opened, nil, err
			}
			targets = fresh
			if candidate, ok := a.findImportTarget(ctx, targets); ok {
				settings, found = candidate, true
				break
			}
			if err := a.transportEnded(ctx); err != nil {
				a.closeIfOpened(ctx, opened)
				return actionTarget{}, opened, nil, err
			}
			if !time.Now().Before(settingsDeadline) {
				break
			}
			if err := a.waitStep(ctx); err != nil {
				a.closeIfOpened(ctx, opened)
				return actionTarget{}, opened, nil, err
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

// waitForWorkTarget polls the allowlist until the Work page appears or the
// deadline passes. When activateWork is true and Work is absent, it asks the
// main desktop view once to open the Work tab as soon as its bridge API mounts.
func (a *cdpActions) waitForWorkTarget(ctx context.Context, targets *[]actionTarget, deadline time.Time, activateWork bool) (actionTarget, bool) {
	activated := false
	for {
		if work, ok := findPath(*targets, "/kimi-agent.html"); ok {
			return work, true
		}
		if activateWork && !activated {
			if a.tryActivateWorkOnce(ctx, *targets) {
				activated = true
			}
		}
		if !time.Now().Before(deadline) {
			return actionTarget{}, false
		}
		if err := a.waitStep(ctx); err != nil {
			return actionTarget{}, false
		}
		fresh, err := a.targets(ctx)
		if err != nil {
			if a.transportEnded(ctx) != nil {
				return actionTarget{}, false
			}
			continue
		}
		*targets = fresh
	}
}

func (a *cdpActions) tryActivateWorkOnce(ctx context.Context, targets []actionTarget) bool {
	if len(targets) == 0 {
		return false
	}
	ordered := make([]actionTarget, 0, len(targets))
	for _, target := range targets {
		if target.path == "/desktopclaw.html" {
			ordered = append(ordered, target)
		}
	}
	for _, target := range targets {
		if target.path != "/desktopclaw.html" {
			ordered = append(ordered, target)
		}
	}
	for _, target := range ordered {
		value, err := a.evaluate(ctx, target.sessionID, activateExpression)
		if err != nil {
			if errors.Is(err, ErrSessionInvalid) {
				a.dropSession(target.targetID)
			}
			continue
		}
		probe, _ := value.(map[string]any)
		asked, _ := probe["asked"].(bool)
		failed, _ := probe["failed"].(bool)
		if asked || failed {
			return true
		}
	}
	return false
}

// openSettingsWhenReady waits for the Work page's preload API to mount, then asks
// it once. Mounting is waited for; a page that answers `false` refused, and a
// refusal is never retried. A read-only presence probe is used for the wait so
// the official call runs exactly once.
func (a *cdpActions) openSettingsWhenReady(ctx context.Context, work actionTarget, deadline time.Time) (asked, mounted bool, err error) {
	for {
		value, err := a.evaluateTarget(ctx, &work, openSettingsProbeExpression)
		if err != nil {
			if errors.Is(err, ErrSessionInvalid) {
				a.dropSession(work.targetID)
				targets, refreshErr := a.targets(ctx)
				if refreshErr != nil {
					return false, false, refreshErr
				}
				replacement, ok := findPath(targets, "/kimi-agent.html")
				if !ok || !time.Now().Before(deadline) {
					return false, false, nil
				}
				work = replacement
				if waitErr := a.waitStep(ctx); waitErr != nil {
					return false, false, waitErr
				}
				continue
			}
			return false, false, err
		}
		if present, _ := value.(bool); present {
			break
		}
		if !time.Now().Before(deadline) {
			// The API never mounted. That is not the page refusing.
			return false, false, nil
		}
		if err := a.waitStep(ctx); err != nil {
			return false, false, err
		}
	}
	answer, err := a.evaluateTarget(ctx, &work, openSettingsExpression)
	if err != nil {
		return false, true, err
	}
	return answer == true, true, nil
}

// waitForDaimonNotBooting holds a write until the official runtime has left its
// startup states. Electron loads the settings page about a second after a
// restart, while Daimon is still provisioning; a source save or disconnect sent
// in that window answers `busy` and comes back deferred, which puts auto-sync
// straight back into "restart required".
//
// It answers only on evidence: a page without the status entry point, an
// unreadable state or any settled state returns immediately, so a build this
// probe does not apply to never pays for it.
func (a *cdpActions) waitForDaimonNotBooting(ctx context.Context, targets []actionTarget) error {
	work, ok := findPath(targets, "/kimi-agent.html")
	if !ok {
		fresh, err := a.targets(ctx)
		if err != nil {
			return nil
		}
		if work, ok = findPath(fresh, "/kimi-agent.html"); !ok {
			return nil
		}
	}
	deadline := time.Now().Add(daimonBootWindow)
	for {
		if !daimonBootStates[a.daimonState(ctx, &work)] {
			return nil
		}
		if !time.Now().Before(deadline) {
			// The window bounds the wait, not the write: the import still runs and
			// its own answer decides.
			return nil
		}
		if err := a.waitStep(ctx); err != nil {
			return err
		}
	}
}

// daimonState reads the official runtime state without keeping the status
// object: it carries transport credentials.
func (a *cdpActions) daimonState(ctx context.Context, work *actionTarget) string {
	probeCtx, cancel := context.WithTimeout(ctx, daimonStatusTimeout)
	defer cancel()
	value, err := a.evaluateTarget(probeCtx, work, daimonStatusExpression)
	if err != nil {
		return ""
	}
	state, _ := value.(string)
	return state
}

// daimonStatusExpression asks the Work shell for the one field this wait needs.
// A page whose preload has not mounted answers an empty string, which is a
// state to leave alone rather than to wait on.
const daimonStatusExpression = `(async()=>{
const api=globalThis.kimiAgentAPI;
if(!api||typeof api.getStatus!=="function")return "";
try{
  const status=await api.getStatus();
  return typeof status?.state==="string"?status.state.slice(0,32):"";
}catch(error){return "";}
})()`

// waitStep sleeps one poll interval, and ends early when the context or the pipe
// is finished. A closed pipe must not be polled until a deadline: nothing will
// ever answer it.
func (a *cdpActions) waitStep(ctx context.Context) error {
	timer := time.NewTimer(settingsReadyStep)
	defer timer.Stop()
	select {
	case <-timer.C:
		return a.transportEnded(ctx)
	case <-ctx.Done():
		return ctx.Err()
	case <-a.pipe.Done():
		return fmt.Errorf("%w: %s", ErrPipeClosed, a.pipe.closeReason())
	}
}

// transportEnded answers the reason no further poll can succeed, or nil.
func (a *cdpActions) transportEnded(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if closed, reason := a.pipe.Closed(); closed {
		return fmt.Errorf("%w: %s", ErrPipeClosed, reason)
	}
	return nil
}

// closeIfOpened closes the overlay this call opened.
func (a *cdpActions) closeIfOpened(ctx context.Context, opened bool) {
	if !opened {
		return
	}
	closeCtx, cancel := closeContext(ctx)
	defer cancel()
	_, _ = a.CloseWorkSettings(closeCtx)
}

// closeContext keeps a close going after the caller's context ended: leaving an
// overlay up because the import was cancelled would strand a window the user did
// not ask for. Cancellation is dropped (values are kept) and the close gets its
// own short budget; it still only touches this call's own overlay.
func closeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), overlayCloseTimeout)
}

// closeIfOpenedInto is closeIfOpened with the answer written into the result, so
// a failed close is reported rather than silently swallowed.
func (a *cdpActions) closeIfOpenedInto(ctx context.Context, opened bool, result *ImportResult) {
	if !opened || result == nil {
		return
	}
	closeCtx, cancel := closeContext(ctx)
	defer cancel()
	closed, err := a.CloseWorkSettings(closeCtx)
	value := err == nil && closed.OK
	result.Closed = &value
}

// classifyActionError turns one failed page call into the answer a caller can
// act on. Only a real protocol/contract mismatch is `contract-changed`: a
// deadline, a cancellation, a reloaded page and a dead transport are different
// facts and are reported as such.
//
// `surfaced` is the error the caller must still see. A transport or budget
// failure is never swallowed into a nil error; a page state that is described
// completely by the result (a reload) is.
func classifyActionError(err error) (state, reason string, surfaced error) {
	switch {
	case err == nil:
		return ImportStateFailed, "native_bridge_failed", nil
	case errors.Is(err, context.Canceled):
		return ImportStateFailed, "native_bridge_cancelled", err
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrPipeTimeout):
		return ImportStateFailed, ErrBridgeTimeout.Error(), err
	case errors.Is(err, ErrPipeClosed):
		return ImportStateFailed, ErrBridgeUnavailable.Error(), err
	case errors.Is(err, ErrSessionInvalid):
		// The page reloaded under the call. The write may or may not have landed,
		// so this is reported, never retried here and never called applied.
		return ImportStateFailed, "native_import_page_reloaded", nil
	case errors.Is(err, ErrProtocolError), errors.Is(err, ErrRendererUnconfirmed):
		return ImportStateContractChanged, NativeContractChanged, nil
	}
	return ImportStateFailed, safeReason(err), err
}

// findImportTarget looks for the page that carries the import API.
//
// The probe is read-only, so a dead session is re-attached once and the same
// question is asked again: a page that reloaded while mounting is a page to
// re-read, not a page that lost its API.
func (a *cdpActions) findImportTarget(ctx context.Context, targets []actionTarget) (actionTarget, bool) {
	for _, target := range targets {
		value, err := a.evaluateTarget(ctx, &target, importProbeExpression)
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

	// openSettingsProbeExpression only reports whether the call exists. It is the
	// wait condition before the real call runs exactly once.
	openSettingsProbeExpression = `(function(){
var api = globalThis.kimiAgentAPI;
return typeof api?.openSettings === "function";
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
const unwrap=result=>result?.ok===true?result.value:null;` + settleDisconnectJS + `
const disconnectDirect=async()=>{
  if(!directSourceID)return {ok:true,settled:true};
  if(typeof sources?.list!=="function"||typeof sources.disconnect!=="function")return {ok:false,settled:false};
  const current=unwrap(await sources.list());
  if(!current||!Array.isArray(current.sources)||typeof current.scopeToken!=="string")return {ok:false,settled:false};
  let direct=current.sources.find(source=>source.id===directSourceID);
  if(!direct)direct=current.sources.find(source=>source.kind==="custom"&&source.displayName==="` + DirectDisplayName + `");
  if(!direct)return {ok:true,settled:true};
  if(direct.kind!=="custom")return {ok:false,settled:false};
  if(direct.quarantined===true||direct.status==="disconnected")return {ok:true,settled:true};
  const cleanupOtherPending=current.sources.some(source=>source.id!==direct.id&&
    (Array.isArray(source.models)?source.models:[]).some(model=>model.status==="pending"));
  const reply=await sources.disconnect({sourceId:direct.id,accountRevision:current.accountRevision,
    sourceRevision:direct.revision??null});
  return await settleDisconnect(reply,current,cleanupOtherPending);
};
if(typeof api?.importByokModels==="function"){
  const result=await api.importByokModels(token,baseURL);
  const imported=result?.ok===true;
  const requiresRestart=result?.requiresRestart===true;
  let cleanup={ok:true,settled:true};
  if(imported&&directSourceID&&!requiresRestart)cleanup=await disconnectDirect();
  return {ok:imported,code:result?.code,
    routeCleanupPending:imported&&!!directSourceID&&(requiresRestart||cleanup?.settled!==true),
    cleanupApplyScopeToken:typeof cleanup?.applyScopeToken==="string"?cleanup.applyScopeToken:"",
    cleanupApplyRevision:typeof cleanup?.applyRevision==="string"?cleanup.applyRevision:"",
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
const hadOtherPending=snapshot.sources.some(source=>{
  if(source.id===existing?.id)return false;
  if(Array.isArray(source.models))return source.models.some(model=>model.status==="pending");
  return snapshot.applyState!==undefined&&!["applied","idle","ready"].includes(snapshot.applyState);
});
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
// Electron's post-restart model refresh can rewrite the store while this
// preview is in flight, putting a random byok-<uuid> back on the kimi route.
// Saving that preview would mint a provider that cannot switch in place, so the
// preview is dropped and the caller repairs the store and previews again.
if(typeof sources.disconnect==="function"&&Array.isArray(preview?.source?.providers)){
  const kimiProviders=preview.source.providers.filter(p=>p?.protocol==="kimi"&&typeof p?.id==="string"&&p.id);
  if(kimiProviders.length&&!kimiProviders.some(p=>p.id==="byok-kth-kimi")){
    if(typeof sources.discard==="function")await sources.discard(preview.previewId);
    return {ok:false,code:"non-canonical-provider"};
  }
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
let applyScopeToken="",applyRevision="";
if(status==="deferred"&&!hadOtherPending&&typeof sources.applyPending==="function"&&
  typeof saved.value?.accountRevision==="string"&&saved.value.accountRevision){
  applyScopeToken=snapshot.scopeToken;applyRevision=saved.value.accountRevision;
}
let cleanupSettled=true,cleanupScopeToken="",cleanupRevision="";
if(directSourceID&&status==="applied"){
  // The route cleanup runs only after the replacement is applied; within
  // Electron's post-ready window it comes back pending or deferred, and the
  // settlement above either finishes it here or hands Go the revision to
  // apply without restarting Kimi.
  const cleanup=await disconnectDirect();
  cleanupSettled=cleanup?.settled===true;
  cleanupScopeToken=typeof cleanup?.applyScopeToken==="string"?cleanup.applyScopeToken:"";
  cleanupRevision=typeof cleanup?.applyRevision==="string"?cleanup.applyRevision:"";
}
return {ok:true,routeCleanupPending:!!directSourceID&&(status!=="applied"||!cleanupSettled),
  cleanupApplyScopeToken:cleanupScopeToken,cleanupApplyRevision:cleanupRevision,
  requiresRestart:status!=="applied",restartDeferred:status==="deferred",models:modelCount,
  applyScopeToken:applyScopeToken,applyRevision:applyRevision};
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
	case "account_changed":
		return NativeAccountChanged
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
