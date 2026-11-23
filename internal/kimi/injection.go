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
	state            string
	structure        string
	blocks           int
	seenBlocks       int
	failed           bool
	pendingContexts  map[string]int
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
}

// Host owns the CDP sessions for one Kimi process.
type Host struct {
	pipe *Pipe

	// options is read by the event loop and replaced by Configure, so every
	// access goes through optionsMu. A caller that mutated it directly would
	// race Sync.
	optionsMu sync.RWMutex
	options   InjectionOptions

	mu         sync.Mutex
	sessions   map[string]*SurfaceSession // keyed by target id
	stopped    bool
	replySlots chan struct{}

	// contextWaiters are released when a session's main world is reported.
	contextWaiters map[*SurfaceSession][]chan int
}

// NewHost attaches a host to an existing pipe.
func NewHost(pipe *Pipe, options InjectionOptions) *Host {
	if options.CallTimeout <= 0 {
		options.CallTimeout = DefaultCallTimeout
	}
	host := &Host{pipe: pipe, options: options, sessions: map[string]*SurfaceSession{},
		replySlots: make(chan struct{}, 8), contextWaiters: map[*SurfaceSession][]chan int{}}
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
	return h.options
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
	h.options.Settings = settings
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
		session.mu.Unlock()
	}
}

func (h *Host) callTimeout() time.Duration {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options.CallTimeout
}

func (h *Host) revision() string {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options.Revision
}

func (h *Host) settings() map[string]any {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options.Settings
}

func (h *Host) payload() string {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options.Payload
}

func (h *Host) onReplyHandler() func(context.Context, *SurfaceSession, int, string, any) {
	h.optionsMu.RLock()
	defer h.optionsMu.RUnlock()
	return h.options.OnReply
}

// eventLoop routes every unsolicited CDP message to the session it belongs to.
func (h *Host) eventLoop() {
	for event := range h.pipe.Events() {
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
	h.mu.Lock()
	stopped := h.stopped
	h.mu.Unlock()
	if stopped {
		return nil, ErrPipeClosed
	}
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

		// The document changed under a session that stays attached: a reload, a
		// cross-site navigation back into the surface, or anything else that
		// replaced the page.
		session.mu.Lock()
		moved := targetURL != session.injectedURL
		needsInjection := moved || session.revision != h.revision()
		contextID := session.mainContextID
		session.mu.Unlock()

		if needsInjection {
			if err := h.inject(ctx, session, targetURL); err != nil {
				session.mu.Lock()
				session.revision = ""
				session.state = "failed"
				session.failed = true
				session.mu.Unlock()
				sessions = append(sessions, session)
				continue
			}
		} else if status, err := h.status(ctx, session.SessionID, contextID); err != nil || !validStatus(status, surface) {
			session.mu.Lock()
			session.revision = ""
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
		pendingContexts: map[string]int{}}, nil
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
func (h *Host) inject(ctx context.Context, session *SurfaceSession, targetURL string) error {
	session.mu.Lock()
	session.generation++
	injection := session.generation
	session.mu.Unlock()

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
	source := h.source(session, injection)
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
	session.revision = h.revision()
	session.injectedURL = targetURL
	session.failed = false
	return nil
}

// source renders the per-injection preamble. The payload itself is appended
// unchanged; only the host-owned globals before it are generated here.
func (h *Host) source(session *SurfaceSession, generation int) string {
	// The renderer reads `surface` out of this config to decide which page
	// component family it is looking at, so it is part of the preamble rather
	// than something the payload infers.
	config := map[string]any{}
	for key, value := range h.settings() {
		config[key] = value
	}
	config["surface"] = session.Surface
	settings, _ := json.Marshal(config)
	scope, _ := json.Marshal(h.revision())
	return "if(globalThis.__kwgTranslationScope!==" + string(scope) + "){globalThis.__kwgTranslation?.__cache?.clear();" +
		"globalThis.__kwgTranslationScope=" + string(scope) + ";}\n" +
		"globalThis.__kwgTranslationGeneration=" + fmt.Sprint(generation) + ";\n" +
		"globalThis.__kwgTranslationConfig=" + string(settings) + ";\n" + h.payload()
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
		// generation.
		session.mu.Lock()
		session.revision = ""
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
	if session.mainContextID == id {
		session.mainContextID = 0
		session.bindingContextID = 0
	}
}

func (h *Host) dropWorld(session *SurfaceSession) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.mainContextID = 0
	session.bindingContextID = 0
	session.revision = ""
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
	var message struct {
		ID    any    `json:"id"`
		Value any    `json:"value"`
		Kind  string `json:"kind"`
	}
	if json.Unmarshal([]byte(payload), &message) != nil {
		return
	}
	handler := h.onReplyHandler()
	if handler == nil {
		return
	}
	id := fmt.Sprint(message.ID)
	value := message.Value
	timeout := h.callTimeout()
	select {
	case h.replySlots <- struct{}{}:
		go func() {
			defer func() { <-h.replySlots }()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
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

func (h *Host) withdraw(ctx context.Context, stop bool) {
	h.mu.Lock()
	sessions := make([]*SurfaceSession, 0, len(h.sessions))
	for _, session := range h.sessions {
		sessions = append(sessions, session)
	}
	h.sessions = map[string]*SurfaceSession{}
	if stop {
		h.stopped = true
	}
	h.mu.Unlock()

	disabled := map[string]any{"enabled": false}
	if settings := h.settings(); settings != nil {
		disabled = map[string]any{}
		for key, value := range settings {
			disabled[key] = value
		}
		disabled["enabled"] = false
	}
	encoded, _ := json.Marshal(disabled)
	for _, session := range sessions {
		session.mu.Lock()
		contextID := session.mainContextID
		session.mu.Unlock()
		_, _ = h.Evaluate(ctx, session.SessionID,
			"globalThis.__kwgTranslation?.configure("+string(encoded)+")", contextID)
		_ = h.detach(ctx, session)
	}
}

// WithdrawAll disables and removes translation scripts from every attached view
// without stopping the host, so re-enabling translation later can re-attach.
func (h *Host) WithdrawAll(ctx context.Context) {
	h.withdraw(ctx, false)
}

// DetachAll withdraws translation from every attached view and marks the host
// stopped so no later replies or syncs run against a retired child.
func (h *Host) DetachAll(ctx context.Context) {
	h.withdraw(ctx, true)
}

// ---------------------------------------------------------------------------
// helpers

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
