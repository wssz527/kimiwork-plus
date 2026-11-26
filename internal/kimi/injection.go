package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// The injected renderer is JavaScript and stays JavaScript. This package owns
// only the transport, the world bookkeeping and the surface check; it never
// rewrites the payload and never interprets the page.
//
// Two lifecycles are kept apart on purpose:
//
//	the execution world   dropped only on real evidence that it ended
//	                      (executionContextsCleared, a destroyed context, a new
//	                      main-frame document)
//	the acknowledgement   invalidated by anything that could have replaced the
//	                      document, including a load event or a same-document
//	                      route change
//
// Collapsing them would either lose a valid world (V8 reports an existing
// context once, so re-enabling Runtime never re-reports it) or accept a receipt
// from a document that is gone.

var (
	ErrRendererUnconfirmed = errors.New("translation_renderer_unconfirmed")
	ErrSessionConflict     = errors.New("translation_session_conflict")
)

// SurfaceSession is one attached page and everything the host tracks about it.
//
// Every mutable field is guarded by mu: the host's event loop updates a session
// from its own goroutine while a caller reads its state, and a race here would
// be a race on the very bookkeeping that decides whether a reply is delivered.
type SurfaceSession struct {
	SessionID string
	TargetID  string
	Surface   string

	mu               sync.Mutex
	generation       int
	mainFrameID      string
	mainContextID    int
	runtimeEnabled   bool
	bindingContextID int
	scriptID         string
	revision         string
	injectedURL      string
	// appliedSettings is the renderer config the page is running right now:
	// written by a successful injection or a successful live configure. The next
	// tick compares it with the current settings, so an update that failed is
	// retried instead of being assumed.
	appliedSettings string
	state           string
	structure       string
	blocks          int
	seenBlocks      int
	failed          bool
	pendingContexts map[string]int
	// retired closes when the host lets this session go: the target left the
	// surface or the view was withdrawn. A reply in flight listens to it, so a
	// page that is no longer attached cannot keep a provider call alive.
	retired    chan struct{}
	retireOnce sync.Once
}

// retire tells everything bound to this session that it is no longer attached.
// It is safe to call more than once and on a session built without a channel.
func (s *SurfaceSession) retire() {
	if s.retired == nil {
		return
	}
	s.retireOnce.Do(func() { close(s.retired) })
}

// Generation is the current injection generation. A reply is only delivered to
// the generation that asked for it, so a late answer cannot paint into the page
// that replaced it.
func (s *SurfaceSession) Generation() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// State is the renderer's last reported state, if it has reported one.
func (s *SurfaceSession) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Structure reports whether the page exposed the expected internals:
// `none` is an empty conversation and never counts as verified structure,
// `partial` is the DOM-drift signal.
func (s *SurfaceSession) Structure() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.structure
}

// Counts returns the block counters the renderer observed.
func (s *SurfaceSession) Counts() (blocks, seen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocks, s.seenBlocks
}

// Failed reports whether the last injection was rejected.
func (s *SurfaceSession) Failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// ContextID is the main world this session is pinned to, or 0.
func (s *SurfaceSession) ContextID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mainContextID
}

// ScriptID is the new-document script this session installed, if any.
func (s *SurfaceSession) ScriptID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scriptID
}

// DefaultReplyTimeout bounds one provider call from a renderer request. It is
// not the CDP budget: a custom model provider is allowed 60-120 seconds, and the
// renderer waits 150 seconds for the answer, so the local transport timeout must
// never be what ends a translation.
const DefaultReplyTimeout = 150 * time.Second

// InjectionOptions configures one host.
type InjectionOptions struct {
	// Payload is the versioned renderer source from assets/injection. It is
	// passed through byte for byte.
	Payload string
	// Revision identifies the settings scope. A change re-injects.
	Revision string
	// Settings is the public settings object the renderer reads at boot.
	Settings map[string]any
	// OnReply is called for each binding payload the page sends, with the
	// generation that was live when the request arrived.
	OnReply func(ctx context.Context, session *SurfaceSession, generation int, id string, value any)
	// CallTimeout bounds one CDP call.
	CallTimeout time.Duration
	// ReplyTimeout bounds one OnReply call. It is separate from CallTimeout
	// because a provider answers in seconds while a CDP call answers in
	// milliseconds.
	ReplyTimeout time.Duration
}

// Host owns the CDP sessions for one Kimi process.
type Host struct {
	pipe *Pipe

	// options is read by the event loop and replaced by Configure, so every
	// access goes through optionsMu. A caller that mutated it directly would
	// race Sync.
	optionsMu sync.RWMutex
	options   InjectionOptions

	// syncMu serializes every operation that replaces a page's scripts or
	// settings: Sync, ApplySettings and the withdrawal. Without it a view update
	// and a re-injection interleave, and the page ends up with the scripts of one
	// settings scope and the configure of another.
	//
	// Lock order: syncMu outermost, then h.mu or session.mu, then optionsMu. The
	// event loop never takes syncMu, so a CDP exchange can never wait on a slow
	// caller.
	syncMu sync.Mutex

	mu         sync.Mutex
	sessions   map[string]*SurfaceSession // keyed by target id
	stopped    bool
	replySlots chan struct{}
	stopCh     chan struct{}
	stopOnce   sync.Once
	loopDone   chan struct{}
	// cancelBindings is closed and replaced whenever the attached pages are
	// withdrawn. A reply in flight reads the channel before it starts, so
	// disabling translation or retiring the host cancels an upstream request
	// that would otherwise run to its full provider deadline.
	cancelBindings chan struct{}
	// withdrawing is closed from the moment a withdrawal starts until it
	// completes, and open otherwise. A context wait selects on it: the batch
	// channel alone only interrupts the wait that was current when the withdrawal
	// started, and the next wait in the same sync would read the fresh batch and
	// park for its whole deadline behind the lock the withdrawal needs.
	withdrawing chan struct{}
	// contextWaiters are released when a session's main world is reported.
	contextWaiters map[*SurfaceSession][]chan int
}

// NewHost attaches a host to an existing pipe.
func NewHost(pipe *Pipe, options InjectionOptions) *Host {
	if options.CallTimeout <= 0 {
		options.CallTimeout = DefaultCallTimeout
	}
	if options.ReplyTimeout <= 0 {
		options.ReplyTimeout = DefaultReplyTimeout
	}
	// The settings are copied in and out: a caller that keeps the map it passed
	// (or mutates what Options returned) must not change what an injection reads.
	options.Settings = cloneSettings(options.Settings)
	host := &Host{pipe: pipe, options: options, sessions: map[string]*SurfaceSession{},
		replySlots: make(chan struct{}, 8), stopCh: make(chan struct{}), loopDone: make(chan struct{}),
		cancelBindings: make(chan struct{}), withdrawing: make(chan struct{}),
		contextWaiters: map[*SurfaceSession][]chan int{}}
	// One permanent reader for unsolicited events. Without it, navigation and
	// binding messages would only be seen while a context wait happened to be
	// running, which is exactly the window where they matter least.
	go host.eventLoop()
	return host
}

// Pipe exposes the transport, for callers that need to share it (the native
// import bridge uses the same process and therefore the same pipe).
func (h *Host) Pipe() *Pipe { return h.pipe }

// Options returns a copy of the current options.
func (h *Host) Options() InjectionOptions {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	options := h.options
	options.Settings = cloneSettings(h.options.Settings)
	return options
}

// Configure replaces the settings scope and the renderer settings atomically.
//
// A change to the scope invalidates every session's acknowledgement, so the next
// Sync re-injects under the new revision; a change that keeps the scope (the
// view, for instance) does not, because re-injecting would drop the cache for no
// behavioural gain.
func (h *Host) Configure(settings map[string]any, revision string) {
	h.optionsMu.Lock()
	previous := h.options.Revision
	h.options.Settings = cloneSettings(settings)
	h.options.Revision = revision
	h.optionsMu.Unlock()

	if previous == revision {
		return
	}
	h.mu.Lock()
	sessions := make([]*SurfaceSession, 0, len(h.sessions))
	for _, session := range h.sessions {
		sessions = append(sessions, session)
	}
	h.mu.Unlock()
	for _, session := range sessions {
		session.mu.Lock()
		session.revision = ""
		session.appliedSettings = ""
		session.mu.Unlock()
	}
}

func (h *Host) callTimeout() time.Duration {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options.CallTimeout
}

func (h *Host) replyTimeout() time.Duration {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options.ReplyTimeout
}

// settingsSnapshot is one consistent read of the renderer scope. The settings,
// the revision and the payload are read under a single lock, so a page can never
// be configured from the settings of one Configure call while carrying the
// revision of another: that pair would look applied and never heal.
type settingsSnapshot struct {
	settings map[string]any
	revision string
	payload  string
	enabled  bool
}

func (h *Host) snapshot() settingsSnapshot {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	settings := make(map[string]any, len(h.options.Settings))
	for key, value := range h.options.Settings {
		settings[key] = value
	}
	enabled, _ := settings["enabled"].(bool)
	return settingsSnapshot{settings: settings, revision: h.options.Revision,
		payload: h.options.Payload, enabled: enabled}
}

func (h *Host) onReplyHandler() func(context.Context, *SurfaceSession, int, string, any) {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options.OnReply
}

// eventLoop routes every unsolicited CDP message to the session it belongs to.
//
// A retired host stops here. Without the stop channel its reader would keep
// taking events off the shared pipe after DetachAll, and the host that replaced
// it would only see a fraction of the bindings and navigation events.
func (h *Host) eventLoop() {
	defer close(h.loopDone)
	events := h.pipe.Events()
	done := h.pipe.Done()
	for {
		// A stop is checked before the event is taken, not only in the same
		// select: when both are ready Go picks randomly, and a retired host that
		// steals an event has dropped it for the host that replaced it.
		select {
		case <-h.stopCh:
			return
		default:
		}
		select {
		case <-h.stopCh:
			return
		case <-done:
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			h.mu.Lock()
			var targets []*SurfaceSession
			for _, session := range h.sessions {
				if event.SessionID == "" || event.SessionID == session.SessionID {
					targets = append(targets, session)
				}
			}
			h.mu.Unlock()
			for _, session := range targets {
				h.handleEvent(session, event)
			}
		}
	}
}

// awaitMainContext releases a waiter when the session's main world appears.
// awaitMainContext registers a waiter for the session's main world.
func (h *Host) awaitMainContext(session *SurfaceSession) chan int {
	waiter := make(chan int, 1)
	h.mu.Lock()
	h.contextWaiters[session] = append(h.contextWaiters[session], waiter)
	h.mu.Unlock()
	return waiter
}

// forgetMainContextWaiter drops the registration once the caller has its answer.
func (h *Host) forgetMainContextWaiter(session *SurfaceSession) {
	h.mu.Lock()
	delete(h.contextWaiters, session)
	h.mu.Unlock()
}

func (h *Host) releaseContextWaiters(session *SurfaceSession, contextID int) {
	h.mu.Lock()
	waiters := h.contextWaiters[session]
	delete(h.contextWaiters, session)
	h.mu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter <- contextID:
		default:
		}
	}
}

// Evaluate runs an expression pinned to one execution world. Pinning is not
// optional wherever the answer describes a specific document: without it a call
// after a navigation runs in a newer context and answers a question about a page
// that was never checked.
func (h *Host) Evaluate(ctx context.Context, sessionID, expression string, contextID int) (any, error) {
	params := map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}
	if contextID > 0 {
		params["contextId"] = contextID
	}
	result, err := h.pipe.Send(ctx, "Runtime.evaluate", params, sessionID, h.callTimeout())
	if err != nil {
		return nil, err
	}
	return result.evaluatedValue()
}

// Targets lists the page targets this host may attach to. Everything else —
// settings, membership, plugins, workers, iframes — is filtered out before any
// session exists.
func (h *Host) Targets(ctx context.Context) ([]map[string]any, error) {
	result, err := h.pipe.Send(ctx, "Target.getTargets", map[string]any{}, "", h.callTimeout())
	if err != nil {
		return nil, err
	}
	rows, _ := result.Result["targetInfos"].([]any)
	var out []map[string]any
	for _, row := range rows {
		target, ok := row.(map[string]any)
		if !ok {
			continue
		}
		targetType, _ := target["type"].(string)
		targetURL, _ := target["url"].(string)
		if !IsTranslationTarget(targetType, targetURL) {
			continue
		}
		out = append(out, target)
	}
	return out, nil
}

// Sync attaches to every eligible target, re-injects what changed and retires
// what left the surface. It returns the sessions it now holds.
func (h *Host) Sync(ctx context.Context) ([]*SurfaceSession, error) {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()

	h.mu.Lock()
	stopped := h.stopped
	h.mu.Unlock()
	if stopped {
		return nil, ErrPipeClosed
	}
	snap := h.snapshot()
	targets, err := h.Targets(ctx)
	if err != nil {
		return nil, err
	}
	current := map[string]string{}
	for _, target := range targets {
		targetID, _ := target["targetId"].(string)
		targetURL, _ := target["url"].(string)
		if targetID == "" {
			continue
		}
		current[targetID] = ClassifySurface("page", targetURL)
	}

	h.mu.Lock()
	for targetID, session := range h.sessions {
		surface, still := current[targetID]
		if still && surface == session.Surface {
			continue
		}
		// Leaving the surface (a cross-origin navigation, a SPA route out of
		// /chat, a closed tab) retires the entry and every reply pending for it.
		delete(h.sessions, targetID)
		session.retire()
		h.mu.Unlock()
		_ = h.detach(ctx, session)
		h.mu.Lock()
	}
	h.mu.Unlock()

	var sessions []*SurfaceSession
	for _, target := range targets {
		targetID, _ := target["targetId"].(string)
		targetURL, _ := target["url"].(string)
		surface := current[targetID]

		h.mu.Lock()
		session := h.sessions[targetID]
		h.mu.Unlock()
		if session == nil {
			attached, err := h.attach(ctx, targetID, surface)
			if err != nil {
				continue
			}
			session = attached
			h.mu.Lock()
			if h.stopped {
				h.mu.Unlock()
				_ = h.detach(ctx, attached)
				return nil, ErrPipeClosed
			}
			h.sessions[targetID] = session
			h.mu.Unlock()
		}

		// Compare the full URL so a changed query can trigger a new injection,
		// and treat a session without a main world as needing one: a cleared
		// context must be rebound even when the URL and the scope are unchanged.
		session.mu.Lock()
		moved := targetURL != session.injectedURL
		acknowledged := session.revision == snap.revision
		contextID := session.mainContextID
		applied := session.appliedSettings
		session.mu.Unlock()
		needsInjection := moved || !acknowledged || contextID <= 0

		wantConfig, configErr := json.Marshal(h.rendererConfigFor(snap, session.Surface))
		if needsInjection {
			if err := h.inject(ctx, session, snap, targetURL); err != nil {
				session.mu.Lock()
				session.revision = ""
				session.appliedSettings = ""
				session.state = "failed"
				session.failed = true
				session.mu.Unlock()
				sessions = append(sessions, session)
				continue
			}
		} else if configErr != nil || applied != string(wantConfig) {
			// A live update that failed — the page was mid-navigation, the
			// provider of the CDP call timed out — is retried here instead of
			// leaving the page on the previous view until it navigates.
			_ = h.applySessionSettings(ctx, session, snap)
			sessions = append(sessions, session)
			continue
		} else if status, err := h.status(ctx, session.SessionID, contextID); err != nil || !validStatus(status, surface) {
			session.mu.Lock()
			session.revision = ""
			session.appliedSettings = ""
			session.state = "failed"
			session.failed = true
			session.mu.Unlock()
			sessions = append(sessions, session)
			continue
		} else {
			session.mu.Lock()
			session.state = stringField(status, "state")
			session.structure = stringField(status, "structure")
			session.blocks = intField(status, "blocks")
			session.seenBlocks = intField(status, "seenBlocks")
			session.failed = false
			session.mu.Unlock()
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func validStatus(status map[string]any, surface string) bool {
	if status == nil {
		return false
	}
	version, _ := status["version"].(float64)
	if int(version) != 1 {
		return false
	}
	switch stringField(status, "state") {
	case "active", "inactive", "legacy-running":
	default:
		return false
	}
	return stringField(status, "surface") == surface
}

func (h *Host) attach(ctx context.Context, targetID, surface string) (*SurfaceSession, error) {
	result, err := h.pipe.Send(ctx, "Target.attachToTarget",
		map[string]any{"targetId": targetID, "flatten": true}, "", h.callTimeout())
	if err != nil {
		return nil, err
	}
	sessionID, _ := result.Result["sessionId"].(string)
	if sessionID == "" {
		return nil, ErrRendererUnconfirmed
	}
	return &SurfaceSession{SessionID: sessionID, TargetID: targetID, Surface: surface,
		pendingContexts: map[string]int{}, retired: make(chan struct{})}, nil
}

func (h *Host) detach(ctx context.Context, session *SurfaceSession) error {
	if scriptID := session.ScriptID(); scriptID != "" {
		_, _ = h.pipe.Send(ctx, "Page.removeScriptToEvaluateOnNewDocument",
			map[string]any{"identifier": scriptID}, session.SessionID, h.callTimeout())
	}
	_, err := h.pipe.Send(ctx, "Target.detachFromTarget",
		map[string]any{"sessionId": session.SessionID}, "", h.callTimeout())
	return err
}

// inject installs the binding, the new-document script and the in-page payload.
//
// A new injection is a new generation: anything issued by the previous document
// is retired so it cannot paint into the page that is now attached. The world
// identity is deliberately left alone; only real evidence drops it.
func (h *Host) inject(ctx context.Context, session *SurfaceSession, snap settingsSnapshot, targetURL string) error {
	session.mu.Lock()
	session.generation++
	injection := session.generation
	session.mu.Unlock()

	configJSON, source, err := h.rendererScript(snap, session.Surface, injection)
	if err != nil {
		return err
	}

	if _, err := h.pipe.Send(ctx, "Page.enable", map[string]any{}, session.SessionID, h.callTimeout()); err != nil {
		return err
	}

	session.mu.Lock()
	mainFrameID := session.mainFrameID
	session.mu.Unlock()
	if mainFrameID == "" {
		tree, err := h.pipe.Send(ctx, "Page.getFrameTree", map[string]any{}, session.SessionID, h.callTimeout())
		if err != nil {
			return err
		}
		mainFrameID = nestedString(tree.Result, "frameTree", "frame", "id")
		if mainFrameID == "" {
			return ErrRendererUnconfirmed
		}
		session.mu.Lock()
		session.mainFrameID = mainFrameID
		session.mu.Unlock()
	}

	// Runtime.enable is idempotent in V8: a second call returns success without
	// re-reporting the contexts that already exist. So it is issued once per
	// attach, and a world we already track is never thrown away to refresh it.
	session.mu.Lock()
	enabled := session.runtimeEnabled
	session.mu.Unlock()
	if !enabled {
		if _, err := h.pipe.Send(ctx, "Runtime.enable", map[string]any{}, session.SessionID, h.callTimeout()); err != nil {
			return err
		}
		session.mu.Lock()
		session.runtimeEnabled = true
		session.mu.Unlock()
	}

	contextID, err := h.waitForMainContext(ctx, session)
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.mainContextID = contextID
	bindingStale := session.bindingContextID != contextID
	session.mu.Unlock()
	if bindingStale {
		// The binding lives as long as the world it was registered in.
		if _, err := h.pipe.Send(ctx, "Runtime.addBinding",
			map[string]any{"name": BindingName, "executionContextId": contextID},
			session.SessionID, h.callTimeout()); err != nil {
			return err
		}
		session.mu.Lock()
		session.bindingContextID = contextID
		session.mu.Unlock()
	}

	if previous := session.ScriptID(); previous != "" {
		_, _ = h.pipe.Send(ctx, "Page.removeScriptToEvaluateOnNewDocument",
			map[string]any{"identifier": previous}, session.SessionID, h.callTimeout())
	}
	script, err := h.pipe.Send(ctx, "Page.addScriptToEvaluateOnNewDocument",
		map[string]any{"source": source}, session.SessionID, h.callTimeout())
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.scriptID = stringField(script.Result, "identifier")
	session.mu.Unlock()

	value, err := h.Evaluate(ctx, session.SessionID, source, contextID)
	if err != nil {
		return err
	}
	status, ok := value.(map[string]any)
	if !ok || !validStatus(status, session.Surface) {
		return ErrRendererUnconfirmed
	}
	// The renderer reports the surface it actually observed, plus whether it
	// runs in the top-level document. A document that is not the surface the
	// host classified, or a frame masquerading as the page, must never count as
	// an attached translation.
	if top, ok := status["top"].(bool); !ok || !top {
		return ErrRendererUnconfirmed
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	// A navigation, load or cleared world during the evaluation moved the
	// session to a new generation. Its receipt describes the document that was
	// replaced, so it is not accepted.
	if session.generation != injection {
		return ErrRendererUnconfirmed
	}
	session.state = stringField(status, "state")
	session.structure = stringField(status, "structure")
	session.blocks = intField(status, "blocks")
	session.seenBlocks = intField(status, "seenBlocks")
	session.revision = snap.revision
	session.appliedSettings = string(configJSON)
	session.injectedURL = targetURL
	session.failed = false
	return nil
}

// rendererScript renders the per-injection preamble and returns the config it
// stamps. The payload itself is appended unchanged; only the host-owned globals
// before it are generated here.
func (h *Host) rendererScript(snap settingsSnapshot, surface string, generation int) ([]byte, string, error) {
	// The renderer reads `surface` out of this config to decide which page
	// component family it is looking at, so it is part of the preamble rather
	// than something the payload infers.
	configJSON, err := json.Marshal(h.rendererConfigFor(snap, surface))
	if err != nil {
		return nil, "", err
	}
	scope, _ := json.Marshal(snap.revision)
	source := "if(globalThis.__kwgTranslationScope!==" + string(scope) + "){globalThis.__kwgTranslation?.__cache?.clear();" +
		"globalThis.__kwgTranslationScope=" + string(scope) + ";}\n" +
		"globalThis.__kwgTranslationGeneration=" + fmt.Sprint(generation) + ";\n" +
		"globalThis.__kwgTranslationConfig=" + string(configJSON) + ";\n" + snap.payload
	return configJSON, source, nil
}

// rendererConfigFor is the public settings object plus the surface the session
// was classified as. Both the boot preamble and a live configure carry it, so
// the two can never disagree about which settings a page is running.
func (h *Host) rendererConfigFor(snap settingsSnapshot, surface string) map[string]any {
	config := make(map[string]any, len(snap.settings)+1)
	for key, value := range snap.settings {
		config[key] = value
	}
	config["surface"] = surface
	return config
}

func (h *Host) status(ctx context.Context, sessionID string, contextID int) (map[string]any, error) {
	value, err := h.Evaluate(ctx, sessionID, "globalThis.__kwgTranslation?.status()", contextID)
	if err != nil {
		return nil, err
	}
	status, ok := value.(map[string]any)
	if !ok {
		return nil, ErrRendererUnconfirmed
	}
	return status, nil
}

// waitForMainContext waits for the main frame's default world to be reported.
// It returns an error rather than guessing: a made-up context id would scope the
// binding to a world that does not exist, or silently to none at all.
//
// The waiter is registered before the context is re-checked, not after. Checking
// first and registering second loses the race whenever the event lands in that
// window: the waiter is then never released and the attach fails on the deadline
// even though the world exists.
func (h *Host) waitForMainContext(ctx context.Context, session *SurfaceSession) (int, error) {
	// Read both signals before registering the waiter. A withdrawal that starts
	// after this read closes exactly these channels, and it cannot complete while
	// this call holds syncMu, so the select below always wakes for it. Reading
	// afterwards would let a withdrawal that began a moment earlier hand over the
	// fresh channels and leave this call to run out its five-second deadline.
	cancelled := h.currentCancelBindings()
	withdrawing := h.currentWithdrawal()
	waiter := h.awaitMainContext(session)
	defer h.forgetMainContextWaiter(session)
	session.mu.Lock()
	existing := session.mainContextID
	session.mu.Unlock()
	if existing > 0 {
		return existing, nil
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	select {
	case contextID := <-waiter:
		if contextID <= 0 {
			return 0, ErrRendererUnconfirmed
		}
		return contextID, nil
	case <-deadline.C:
		return 0, ErrRendererUnconfirmed
	case <-h.pipe.Done():
		return 0, ErrPipeClosed
	case <-h.stopCh:
		// The host was retired while this call waited. The event loop that would
		// release the waiter has exited, so waiting out the deadline would only
		// delay the caller's own shutdown.
		return 0, ErrPipeClosed
	case <-cancelled:
		// The view was withdrawn while this call waited. The withdrawal is waiting
		// for the lock this caller holds, so returning now is what lets it run
		// instead of after the deadline.
		return 0, ErrRendererUnconfirmed
	case <-withdrawing:
		// A withdrawal is in progress. This wait belongs to a sync it will undo,
		// so it stops here even when the batch it holds was opened after the
		// withdrawal began.
		return 0, ErrRendererUnconfirmed
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// handleEvent updates one session's world bookkeeping from a CDP event.
func (h *Host) handleEvent(session *SurfaceSession, event Event) {
	if event.SessionID != "" && event.SessionID != session.SessionID {
		return
	}
	switch event.Method {
	case "Runtime.executionContextCreated":
		context, _ := event.Params["context"].(map[string]any)
		h.trackContext(session, context)
	case "Runtime.executionContextDestroyed":
		if id, ok := numberField(event.Params["executionContextId"]); ok {
			h.dropContext(session, int(id))
		}
	case "Runtime.executionContextsCleared":
		h.dropWorld(session)
	case "Page.frameNavigated":
		frame, _ := event.Params["frame"].(map[string]any)
		if frame == nil {
			return
		}
		// A frame that is not the main one never decides whether this page is
		// still the surface: an ad slot or an embedded widget navigating would
		// otherwise retire a translation that is still correct.
		if _, hasParent := frame["parentId"]; hasParent {
			return
		}
		session.mu.Lock()
		mainFrameID := session.mainFrameID
		session.mu.Unlock()
		if mainFrameID != "" && stringField(frame, "id") != mainFrameID {
			return
		}
		h.dropWorld(session)
	case "Page.loadEventFired", "Page.navigatedWithinDocument":
		// The world survives a load or a same-document route change, but the
		// acknowledgement cannot: the next tick re-injects under a new
		// generation, and a configure that was in flight describes the document
		// that was replaced.
		session.mu.Lock()
		session.revision = ""
		session.appliedSettings = ""
		session.state = ""
		session.mu.Unlock()
	case "Runtime.bindingCalled":
		h.deliverBinding(session, event)
	}
}

func (h *Host) trackContext(session *SurfaceSession, context map[string]any) {
	if context == nil {
		return
	}
	id, ok := numberField(context["id"])
	if !ok {
		return
	}
	aux, _ := context["auxData"].(map[string]any)
	isDefault, _ := aux["isDefault"].(bool)
	frameID, _ := aux["frameId"].(string)
	// The page's own world is the default one of the main frame. An isolated
	// world created by an extension or a devtools evaluation is not the page.
	session.mu.Lock()
	mainFrameID := session.mainFrameID
	matched := isDefault && frameID != "" && frameID == mainFrameID
	if matched {
		session.mainContextID = int(id)
	} else if session.mainContextID == int(id) {
		session.mainContextID = 0
		session.bindingContextID = 0
	}
	session.mu.Unlock()
	if matched {
		h.releaseContextWaiters(session, int(id))
	}
}

func (h *Host) dropContext(session *SurfaceSession, id int) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.mainContextID != id {
		return
	}
	// The world that held the injection is gone, so its acknowledgement and the
	// settings it was running are gone with it: a session that kept the revision
	// would look applied and skip the binding the new world needs.
	session.mainContextID = 0
	session.bindingContextID = 0
	session.revision = ""
	session.appliedSettings = ""
	session.state = ""
}

func (h *Host) dropWorld(session *SurfaceSession) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.mainContextID = 0
	session.bindingContextID = 0
	session.revision = ""
	session.appliedSettings = ""
	session.state = ""
}

// deliverBinding forwards one renderer request. Every reply is bound to the
// generation that asked for it, and the evaluation is pinned to the context the
// request came from, so a late answer cannot be delivered into a document that
// replaced it.
func (h *Host) deliverBinding(session *SurfaceSession, event Event) {
	if stringField(event.Params, "name") != BindingName {
		return
	}
	contextID, ok := numberField(event.Params["executionContextId"])
	if !ok {
		return
	}
	session.mu.Lock()
	expected := session.mainContextID
	generation := session.generation
	session.mu.Unlock()
	// A binding call from any other world is stale: answering it would write
	// reasoning text into a document this host never verified.
	if int(contextID) != expected || expected == 0 {
		return
	}
	payload := stringField(event.Params, "payload")
	var raw map[string]any
	if json.Unmarshal([]byte(payload), &raw) != nil || raw == nil {
		return
	}
	// The renderer sends one flat object: {type, id, generation, text,
	// protectedTexts}. A request is only answered when it names itself, and a
	// request stamped with a generation that is no longer live describes a
	// document this host replaced, so it is dropped rather than answered.
	idValue, ok := raw["id"]
	if !ok || idValue == nil {
		return
	}
	if text, isString := idValue.(string); isString && text == "" {
		return
	}
	if rawGeneration, hasGeneration := raw["generation"]; hasGeneration {
		stamped, ok := numberField(rawGeneration)
		if !ok || float64(int(stamped)) != stamped || int(stamped) != generation {
			return
		}
	}
	value, hasValue := raw["value"]
	if !hasValue {
		// The legacy shape wrapped the request in `value`; the live renderer
		// sends it at the top level.
		value = raw
	}
	handler := h.onReplyHandler()
	if handler == nil {
		return
	}
	id := fmt.Sprint(idValue)
	// The liveness check and the batch read happen under one lock: a withdrawal
	// that lands between them would hand this reply the fresh, open batch and let
	// a request for a page nobody shows any more run to its full deadline.
	batch, live := h.acquireBindingBatch(session)
	if !live {
		return
	}
	timeout := h.replyTimeout()
	select {
	case <-h.pipe.Done():
		return
	default:
	}
	select {
	case h.replySlots <- struct{}{}:
		go func() {
			defer func() { <-h.replySlots }()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			// A provider call is allowed to outlive the page that asked for it by
			// minutes. A withdrawal, a session that left the surface, a retired host
			// or the end of the managed child must end it at once instead of holding
			// a reply slot that the next translation needs.
			done := make(chan struct{})
			defer close(done)
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-done:
					return
				case <-batch:
				case <-session.retired:
				case <-h.stopCh:
				case <-h.pipe.Done():
				}
				cancel()
			}()
			handler(ctx, session, generation, id, value)
		}()
	default:
	}
}

// Reply delivers an answer to the renderer. It is a no-op when the session is
// no longer live or the generation moved on.
func (h *Host) Reply(ctx context.Context, session *SurfaceSession, generation int, id string, value any) error {
	h.mu.Lock()
	live := h.sessions[session.TargetID] == session && !h.stopped
	h.mu.Unlock()
	session.mu.Lock()
	current := session.generation
	contextID := session.mainContextID
	session.mu.Unlock()
	if !live || current != generation || contextID == 0 {
		return nil
	}
	encodedID, _ := json.Marshal(id)
	encodedValue, err := json.Marshal(value)
	if err != nil {
		return ErrRendererUnconfirmed
	}
	expression := "globalThis.__kwgTranslationReceive?.(" + string(encodedID) + "," + string(encodedValue) + ")"
	_, err = h.Evaluate(ctx, session.SessionID, expression, contextID)
	return err
}

// HasSessions reports whether any translation target is currently attached.
func (h *Host) HasSessions() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions) > 0
}

// ApplySettings tells every attached page about the current settings without
// starting a new generation and without invalidating the acknowledged scope.
//
// A view change is not a scope change: re-injecting would drop the translations
// the page already paid for, so the live document is asked to switch display
// mode and the new-document script is replaced under the same generation, so a
// later load boots with the settings that are current now.
func (h *Host) ApplySettings(ctx context.Context) {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()

	h.mu.Lock()
	retired := h.stopped
	sessions := make([]*SurfaceSession, 0, len(h.sessions))
	for _, session := range h.sessions {
		sessions = append(sessions, session)
	}
	h.mu.Unlock()
	if retired {
		return
	}
	snap := h.snapshot()
	for _, session := range sessions {
		_ = h.applySessionSettings(ctx, session, snap)
	}
}

// applySessionSettings moves one attached page to the current settings without
// starting a new generation: the live document is reconfigured and its
// new-document script is replaced, so a later load boots with the settings that
// are current now.
//
// Callers hold syncMu. The replacement is registered before the previous script
// is removed: a failure at that point leaves the acknowledged script installed
// rather than a page with no boot script at all. Every failure clears the
// acknowledgement and the applied settings, so the next Sync re-injects and the
// page heals instead of silently keeping the old view.
func (h *Host) applySessionSettings(ctx context.Context, session *SurfaceSession, snap settingsSnapshot) error {
	h.mu.Lock()
	live := h.sessions[session.TargetID] == session && !h.stopped
	h.mu.Unlock()
	if !live {
		return nil
	}
	session.mu.Lock()
	contextID := session.mainContextID
	generation := session.generation
	revision := session.revision
	previous := session.scriptID
	applied := session.appliedSettings
	session.mu.Unlock()
	// A page that is not attached, or one whose scope was never acknowledged,
	// has nothing to configure; the next Sync injects the current settings.
	if contextID <= 0 || revision == "" || revision != snap.revision {
		return nil
	}
	configJSON, source, err := h.rendererScript(snap, session.Surface, generation)
	if err != nil {
		session.markSettingsFailed()
		return err
	}
	if applied == string(configJSON) {
		// The page already runs this configuration, so a tick between a successful
		// update and the next snapshot must not replace the script again.
		return nil
	}
	script, err := h.pipe.Send(ctx, "Page.addScriptToEvaluateOnNewDocument",
		map[string]any{"source": source}, session.SessionID, h.callTimeout())
	if err != nil {
		session.markSettingsFailed()
		return err
	}
	newScriptID := stringField(script.Result, "identifier")
	if previous != "" {
		// Removing the previous script is cleanup, not part of the caller's
		// request: its budget may have ended with the registration above, and an
		// expired context here would leak the script it was meant to replace.
		removeCtx, removeCancel := context.WithTimeout(context.Background(), h.callTimeout())
		_, _ = h.pipe.Send(removeCtx, "Page.removeScriptToEvaluateOnNewDocument",
			map[string]any{"identifier": previous}, session.SessionID, h.callTimeout())
		removeCancel()
	}
	session.mu.Lock()
	session.scriptID = newScriptID
	session.mu.Unlock()

	value, err := h.Evaluate(ctx, session.SessionID,
		"globalThis.__kwgTranslation?.configure("+string(configJSON)+")", contextID)
	status, ok := value.(map[string]any)
	top, isTop := status["top"].(bool)
	// A configure that fails, answers for another surface or frame, or leaves
	// translation inactive while it is enabled did not take effect: the page
	// still shows the previous view, so it is not acknowledged as applied.
	if err != nil || !ok || !validStatus(status, session.Surface) || !isTop || !top ||
		(snap.enabled && stringField(status, "state") != "active") {
		session.markSettingsFailed()
		if err != nil {
			return err
		}
		return ErrRendererUnconfirmed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	// The document may have navigated, cleared its world or been reconfigured
	// while the configure was in flight. Its answer then describes a page this
	// session no longer tracks, and accepting it would overwrite the reset the
	// event loop just recorded.
	if session.generation != generation || session.revision != snap.revision || session.mainContextID != contextID {
		session.appliedSettings = ""
		session.revision = ""
		session.state = "failed"
		session.failed = true
		return ErrRendererUnconfirmed
	}
	session.state = stringField(status, "state")
	session.structure = stringField(status, "structure")
	session.blocks = intField(status, "blocks")
	session.seenBlocks = intField(status, "seenBlocks")
	session.appliedSettings = string(configJSON)
	session.failed = false
	return nil
}

// markSettingsFailed voids the acknowledgement and the applied settings, so the
// next Sync re-injects the current scope. The script the session points at is
// kept: it is what the retry and the detach clean up.
func (s *SurfaceSession) markSettingsFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appliedSettings = ""
	s.revision = ""
	s.state = "failed"
	s.failed = true
}

// withdraw removes translation from every attached page. The caller's context is
// not used for the cleanup: by the time the lock below is available it has
// usually spent its budget waiting, and a cleanup that inherits an expired
// context would fail every call and leave the script and the CDP session behind.
func (h *Host) withdraw(ctx context.Context, stop bool) {
	// Retire the pending replies — and, for a stop, the host — before waiting for
	// syncMu. A Sync parked in waitForMainContext must see one of these closed and
	// release the lock, or this call would wait out the context deadline and then
	// run the cleanup with nothing left.
	h.mu.Lock()
	if stop {
		h.stopped = true
		h.stopOnce.Do(func() { close(h.stopCh) })
	}
	h.beginWithdrawalLocked()
	h.cancelPendingBindingsLocked()
	h.mu.Unlock()

	h.syncMu.Lock()
	defer func() {
		if !stop {
			h.mu.Lock()
			h.completeWithdrawalLocked()
			h.mu.Unlock()
		}
		h.syncMu.Unlock()
	}()

	h.mu.Lock()
	sessions := make([]*SurfaceSession, 0, len(h.sessions))
	for _, session := range h.sessions {
		sessions = append(sessions, session)
	}
	h.mu.Unlock()

	snap := h.snapshot()
	disabled := make(map[string]any, len(snap.settings)+1)
	for key, value := range snap.settings {
		disabled[key] = value
	}
	disabled["enabled"] = false
	encoded, _ := json.Marshal(disabled)
	for _, session := range sessions {
		session.mu.Lock()
		contextID := session.mainContextID
		session.appliedSettings = ""
		session.revision = ""
		session.mu.Unlock()
		cleanCtx, cancel := context.WithTimeout(context.Background(), h.callTimeout())
		_, _ = h.Evaluate(cleanCtx, session.SessionID,
			"globalThis.__kwgTranslation?.configure("+string(encoded)+")", contextID)
		err := h.detach(cleanCtx, session)
		cancel()

		h.mu.Lock()
		if !stop && err != nil && !h.pipeClosed() {
			// The detach failed on a pipe that is still usable, so the page may
			// still hold the script. Keeping the entry makes the next tick retry
			// the withdrawal instead of losing the session.
			h.sessions[session.TargetID] = session
		} else {
			delete(h.sessions, session.TargetID)
			session.retire()
		}
		h.mu.Unlock()
	}
}

// pipeClosed reports whether the transport ended. It is safe to call while
// holding h.mu: it reads only the pipe's own done channel.
func (h *Host) pipeClosed() bool {
	select {
	case <-h.pipe.Done():
		return true
	default:
		return false
	}
}

// cancelPendingBindingsLocked retires the current binding batch: every reply
// already dispatched is cancelled. Callers hold h.mu.
func (h *Host) cancelPendingBindingsLocked() {
	close(h.cancelBindings)
	h.cancelBindings = make(chan struct{})
}

// acquireBindingBatch validates that the session is still attached and returns
// the batch a reply dispatched now belongs to, under a single lock. A withdrawal
// therefore either happens before this call (and the reply is refused) or after
// it (and the reply is cancelled with the batch).
func (h *Host) acquireBindingBatch(session *SurfaceSession) (<-chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped || h.sessions[session.TargetID] != session {
		return nil, false
	}
	return h.cancelBindings, true
}

// currentCancelBindings returns the live batch. It is what a context wait
// listens to so a withdrawal does not have to run the deadline out.
func (h *Host) currentCancelBindings() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancelBindings
}

// currentWithdrawal returns the pending-withdrawal signal, closed while a
// withdrawal runs.
func (h *Host) currentWithdrawal() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.withdrawing
}

// beginWithdrawalLocked marks a withdrawal as pending. Callers hold h.mu. The
// channel stays closed until the withdrawal completes, so every context wait it
// can see stops — not only the one that was current when it started.
func (h *Host) beginWithdrawalLocked() {
	if h.withdrawing == nil {
		return
	}
	select {
	case <-h.withdrawing:
		// A withdrawal is already pending; the close below would panic.
	default:
		close(h.withdrawing)
	}
}

// completeWithdrawalLocked reopens the signal. Callers hold h.mu.
func (h *Host) completeWithdrawalLocked() {
	h.withdrawing = make(chan struct{})
}

// WithdrawAll disables and removes translation scripts from every attached view
// without stopping the host, so re-enabling translation later can re-attach.
func (h *Host) WithdrawAll(ctx context.Context) {
	h.withdraw(ctx, false)
}

// DetachAll withdraws translation from every attached view and marks the host
// stopped so no later replies or syncs run against a retired child.
//
// It returns only after the event loop has exited. A caller that builds the
// replacement host on the same pipe immediately after this call would otherwise
// race a reader that is still parked on the shared event channel.
func (h *Host) DetachAll(ctx context.Context) {
	h.withdraw(ctx, true)
	// A zero-value Host never started a loop, and there is nothing to wait for.
	if h.loopDone != nil {
		<-h.loopDone
	}
}

// ---------------------------------------------------------------------------
// helpers

// cloneSettings copies a settings map shallowly. The values are the primitives
// the renderer reads (strings, numbers, booleans), so the map is the only shared
// mutable state.
func cloneSettings(settings map[string]any) map[string]any {
	if settings == nil {
		return nil
	}
	copied := make(map[string]any, len(settings))
	for key, value := range settings {
		copied[key] = value
	}
	return copied
}

func nestedString(object map[string]any, path ...string) string {
	var current any = object
	for _, key := range path {
		row, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = row[key]
	}
	text, _ := current.(string)
	return text
}

func numberField(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	}
	return 0, false
}

// BindingName is the CDP binding the renderer calls. It is a protocol constant:
// the payload in assets/injection looks it up on globalThis.
const BindingName = "__kwgTranslate"

// ReceiveName is the global the host calls to hand a reply to the renderer.
const ReceiveName = "__kwgTranslationReceive"

// EngineName is the namespace the renderer installs itself under.
const EngineName = "__kwgTranslation"

// SourceHasContract reports whether a payload still declares the protocol this
// host speaks. It is a cheap guard against shipping a mismatched asset.
func SourceHasContract(payload string) bool {
	for _, needle := range []string{EngineName, BindingName, ReceiveName, "__kwgTranslationConfig"} {
		if !strings.Contains(payload, needle) {
			return false
		}
	}
	return true
}
