package kimi

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePage answers CDP calls the way the official renderer would, so the host's
// world bookkeeping can be exercised without a browser.
type fakePage struct {
	mu sync.Mutex

	// behaviour
	surface       string
	statusReply   map[string]any
	attachFailure bool
	navigateOn    bool

	// actions models the official preload API the CDP actions call. It is off by
	// default so every existing test keeps the behaviour it was written for.
	actions                bool
	importAPI              bool
	hideSettings           bool
	settingsOpen           bool
	settingsAvailableAfter int
	settingsProbeReads     int
	// workAvailableAfter hides every desktop target for the first N target
	// reads, so the Work page can be modelled as still mounting.
	workAvailableAfter     int
	targetReads            int
	firstTargetDelay       time.Duration
	mainDesktopTarget      bool
	workRequiresActivate   bool
	activateAvailableAfter int
	activateProbes         int
	activateFails          bool
	workActivated          bool
	// openAPIAvailableAfter withholds kimiAgentAPI.openSettings for the first N
	// probes, so the Work page's preload can be modelled as still mounting.
	openAPIAvailableAfter int
	openAPIProbes         int
	// workerWorkURL adds a non-page target that carries the Work URL.
	workerWorkURL bool
	// replaceWorkTarget models WebContents being recreated while the page mounts.
	replaceWorkTarget bool
	// expireFirstProbe answers the settings page's first import probe with a
	// session-invalid protocol error, as a page that reloaded would.
	expireFirstProbe bool
	probeExpired     bool
	attachCalls      int
	// noShellAPI makes the Work page report no kimiAgentAPI at all.
	noShellAPI bool
	// importBlockFor holds the import call for that long before answering.
	importBlockFor  time.Duration
	importReply     map[string]any
	disconnectReply map[string]any
	closeFails      bool

	// observations
	methods       []string
	injections    int
	removals      int
	detachCalls   int
	evaluations   []string
	lastSource    string
	contextID     int
	generation    int
	bindingCalls  int
	openCalls     int
	closeCalls    int
	importCalls   int
	activateCalls int
}

func (p *fakePage) record(method string) {
	p.methods = append(p.methods, method)
}

func (p *fakePage) handle(write func(string)) func(string) {
	return func(frame string) {
		var request struct {
			ID        int            `json:"id"`
			Method    string         `json:"method"`
			Params    map[string]any `json:"params"`
			SessionID string         `json:"sessionId"`
		}
		if json.Unmarshal([]byte(frame), &request) != nil {
			return
		}
		p.mu.Lock()
		p.record(request.Method)
		p.mu.Unlock()

		result := map[string]any{}
		switch request.Method {
		case "Target.getTargets":
			p.mu.Lock()
			settingsOpen := p.settingsOpen
			p.targetReads++
			reads := p.targetReads
			firstDelay := time.Duration(0)
			if reads == 1 {
				firstDelay = p.firstTargetDelay
			}
			hideWork := (p.workAvailableAfter > 0 && reads <= p.workAvailableAfter) ||
				(p.workRequiresActivate && !p.workActivated)
			mainDesktop := p.mainDesktopTarget
			workerWork := p.workerWorkURL
			workTarget := "work-1"
			if p.replaceWorkTarget {
				workTarget = "work-2"
			}
			p.mu.Unlock()
			if firstDelay > 0 {
				time.Sleep(firstDelay)
			}
			targets := []any{
				map[string]any{"targetId": "chat-1", "type": "page", "url": "https://www.kimi.com/chat/abc-123"},
				map[string]any{"targetId": "evil", "type": "page", "url": "https://www.kimi.com.evil.test/chat/abc"},
				map[string]any{"targetId": "worker", "type": "worker", "url": "https://www.kimi.com/chat/abc"},
			}
			if mainDesktop {
				targets = append([]any{map[string]any{"targetId": "main-1", "type": "page",
					"url": "app://localhost/desktopclaw.html"}}, targets...)
			}
			if !hideWork {
				targets = append([]any{map[string]any{"targetId": workTarget, "type": "page",
					"url": "app://localhost/kimi-agent.html"}}, targets...)
			}
			if workerWork {
				targets = append(targets, map[string]any{"targetId": "worker-work", "type": "worker",
					"url": "app://localhost/kimi-agent.html"})
			}
			// The settings overlay only exists as a target once it is open, which
			// is what a real desktop does: the page is created by openSettings.
			if !p.hideSettings || settingsOpen {
				targets = append(targets, map[string]any{"targetId": "settings", "type": "page",
					"url": "app://localhost/work-settings.html"})
			}
			result["targetInfos"] = targets
		case "Target.attachToTarget":
			p.mu.Lock()
			p.attachCalls++
			failure := p.attachFailure
			p.mu.Unlock()
			if failure {
				result = nil
			} else {
				result["sessionId"] = "session-" + request.Params["targetId"].(string)
			}
		case "Target.detachFromTarget":
			p.mu.Lock()
			p.detachCalls++
			p.mu.Unlock()
		case "Page.enable":
		case "Page.getFrameTree":
			result["frameTree"] = map[string]any{"frame": map[string]any{"id": "main-frame"}}
		case "Runtime.enable":
			// The browser reports an existing context exactly once, on enable.
			p.mu.Lock()
			p.contextID = 7
			p.mu.Unlock()
			p.emit(write, map[string]any{
				"method": "Runtime.executionContextCreated", "sessionId": request.SessionID,
				"params": map[string]any{"context": map[string]any{"id": 7,
					"auxData": map[string]any{"isDefault": true, "frameId": "main-frame"}}},
			})
		case "Runtime.addBinding":
			p.mu.Lock()
			p.bindingCalls++
			p.mu.Unlock()
		case "Page.addScriptToEvaluateOnNewDocument":
			p.mu.Lock()
			p.injections++
			p.lastSource, _ = request.Params["source"].(string)
			p.mu.Unlock()
			result["identifier"] = "script-1"
		case "Page.removeScriptToEvaluateOnNewDocument":
			p.mu.Lock()
			p.removals++
			p.mu.Unlock()
		case "Runtime.evaluate":
			expression, _ := request.Params["expression"].(string)
			p.mu.Lock()
			p.evaluations = append(p.evaluations, expression)
			p.mu.Unlock()
			switch {
			case strings.Contains(expression, "status()"),
				strings.Contains(expression, "__kwgTranslationGeneration"):
				// The status call and the injection receipt are answered the
				// same way, because the host validates both against the surface.
				reply := p.statusReply
				if reply == nil {
					reply = map[string]any{"version": 1, "state": "active", "surface": p.surface,
						"top": true, "structure": "matched", "blocks": 3, "seenBlocks": 3}
				}
				result["result"] = reply
			case p.actions:
				value := p.actionReply(request.SessionID, expression)
				if _, invalid := value.(sessionInvalidReply); invalid {
					// A CDP error object, not a value: the session is gone. The
					// caller must classify this as a dead session, not as a page
					// that answered something unexpected.
					response, _ := json.Marshal(map[string]any{"id": request.ID,
						"error":     map[string]any{"code": -32001, "message": "Session with given id not found."},
						"sessionId": request.SessionID})
					write(string(response))
					return
				}
				result["result"] = value
			default:
				result["result"] = nil
			}
		}
		if request.Method == "Runtime.evaluate" {
			result["result"] = map[string]any{"type": "object", "value": result["result"]}
		}
		response, _ := json.Marshal(map[string]any{"id": request.ID, "result": result, "sessionId": request.SessionID})
		write(string(response))
	}
}

func (p *fakePage) emit(write func(string), message map[string]any) {
	encoded, _ := json.Marshal(message)
	write(string(encoded))
}

// actionReply models the official preload API for the CDP actions. It is the
// same surface main_test.go's fake child speaks, so the actions are exercised
// through their real expressions rather than through a stub.
func (p *fakePage) actionReply(sessionID, expression string) any {
	switch {
	case expression == importProbeExpression:
		// Only the settings overlay carries the import API. The Work view has the
		// settings *open* call; a fake that answered both from every page would
		// hide the ordering the actions depend on.
		p.mu.Lock()
		expire := p.expireFirstProbe && !p.probeExpired && sessionID == "session-settings"
		if expire {
			p.probeExpired = true
		}
		p.mu.Unlock()
		if expire {
			// A page that reloaded under the probe: the protocol answers that the
			// session is gone, which the actions must recover from by re-attaching.
			return sessionInvalidReply{}
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if sessionID == "session-settings" {
			p.settingsProbeReads++
		}
		return p.importAPI && sessionID == "session-settings" &&
			(p.settingsAvailableAfter == 0 || p.settingsProbeReads >= p.settingsAvailableAfter)
	case strings.Contains(expression, "importByokModels"):
		p.mu.Lock()
		p.importCalls++
		reply := p.importReply
		hold := p.importBlockFor
		p.mu.Unlock()
		if hold > 0 {
			time.Sleep(hold)
		}
		if reply == nil {
			reply = map[string]any{"ok": true, "models": float64(2), "requiresRestart": false}
		}
		return reply
	case strings.Contains(expression, "revealByok"):
		return map[string]any{"available": true}
	case strings.Contains(expression, "closeSettingsPage") || strings.Contains(expression, "closeKimiWorkSettingPage"):
		p.mu.Lock()
		p.closeCalls++
		fails := p.closeFails
		p.settingsOpen = false
		p.mu.Unlock()
		if fails {
			return map[string]any{"asked": []any{}, "failed": []any{"overlay"}}
		}
		return map[string]any{"asked": []any{"overlay"}, "failed": []any{}}
	case strings.Contains(expression, "sources.disconnect") && strings.Contains(expression, "managedGateway"):
		p.mu.Lock()
		reply := p.disconnectReply
		p.mu.Unlock()
		if reply == nil {
			reply = map[string]any{"ok": true, "sources": float64(1)}
		}
		return reply
	case strings.Contains(expression, "openKimiWorkTab"):
		p.mu.Lock()
		p.activateProbes++
		probes := p.activateProbes
		ready := p.activateAvailableAfter == 0 || probes >= p.activateAvailableAfter
		fails := p.activateFails
		if ready {
			p.activateCalls++
			if !fails {
				p.workActivated = true
			}
		}
		p.mu.Unlock()
		if !ready {
			return map[string]any{"asked": false, "failed": false}
		}
		if fails {
			return map[string]any{"asked": false, "failed": true}
		}
		return map[string]any{"asked": true, "failed": false}
	case expression == openSettingsProbeExpression:
		// The read-only presence probe opens nothing and must not be counted as
		// an open call.
		if p.replaceWorkTarget && sessionID == "session-work-1" {
			return sessionInvalidReply{}
		}
		p.mu.Lock()
		p.openAPIProbes++
		probes := p.openAPIProbes
		noShell := p.noShellAPI
		p.mu.Unlock()
		if noShell {
			return false
		}
		return p.openAPIAvailableAfter == 0 || probes >= p.openAPIAvailableAfter
	case strings.Contains(expression, "openSettings"):
		p.mu.Lock()
		p.openCalls++
		p.settingsOpen = true
		p.mu.Unlock()
		return true
	case strings.Contains(expression, "visibilityState"):
		return map[string]any{"visible": true, "painted": true, "text": 10, "open": false}
	case expression == liveAccountAPIProbeExpression || expression == liveClawAPIProbeExpression:
		p.mu.Lock()
		noShell := p.noShellAPI
		importAPI := p.importAPI
		p.mu.Unlock()
		if expression == liveAccountAPIProbeExpression {
			// The Work shell answers identity. `mounted` is what separates a page
			// whose preload has not loaded yet from a build without the API.
			return map[string]any{"mounted": !noShell,
				"getUserProfile": !noShell, "getByokProfile": !noShell,
				"byokSourcesList": !noShell, "modelImportApi": !noShell && importAPI}
		}
		return map[string]any{"mounted": !noShell, "byokSourcesList": !noShell,
			"modelImportApi": !noShell && importAPI}
	}
	return nil
}

// sessionInvalidReply is returned by the fake's page when the expression must be
// answered with a CDP protocol error rather than a value.
type sessionInvalidReply struct{}

// harness wires a Pipe to a fake page through two pipes.
type harness struct {
	pipe *Pipe
	page *fakePage
}

func newHarness(t *testing.T, page *fakePage) *harness {
	t.Helper()
	// host -> page commands, page -> host responses and events.
	toHostReader, toHostWriter := io.Pipe()
	toPageReader, toPageWriter := io.Pipe()

	pipe := NewPipe(toHostReader, toPageWriter)
	handle := page.handle(func(frame string) { _, _ = toHostWriter.Write(append([]byte(frame), 0)) })

	go func() {
		buffer := make([]byte, 0, 64<<10)
		chunk := make([]byte, 4096)
		for {
			n, err := toPageReader.Read(chunk)
			buffer = append(buffer, chunk[:n]...)
			for {
				at := strings.IndexByte(string(buffer), 0)
				if at < 0 {
					break
				}
				frame := string(buffer[:at])
				buffer = buffer[at+1:]
				handle(frame)
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { pipe.Dispose() })
	return &harness{pipe: pipe, page: page}
}

const testPayload = `
globalThis.__kwgTranslation = {
  status: () => ({version: 1, state: 'active', surface: globalThis.__kwgTranslationConfig?.surface,
    top: true, structure: 'matched', blocks: 3, seenBlocks: 3}),
  configure: () => ({version: 1, state: 'inactive', surface: 'work', top: true}),
  __cache: new Map(),
};
globalThis.__kwgTranslate = () => {};
globalThis.__kwgTranslationReceive = () => {};
`

func TestSurfaceClassificationIsAnAllowlist(t *testing.T) {
	cases := map[string]string{
		"app://localhost/kimi-agent.html":               "work",
		"file:///Applications/Kimi.app/kimi-agent.html": "work",
		"https://www.kimi.com/chat/abc-123":             "chat",
		"https://www.kimi.com/chat/history":             "chat",
		"https://www.kimi.com/":                         "chat",
		// Everything below must never receive a script or reasoning text.
		"app://localhost/work-settings.html":       "other",
		"https://www.kimi.com/settings":            "other",
		"https://www.kimi.com.evil.test/chat/abc":  "other",
		"https://evil.test/chat/abc":               "other",
		"https://www.kimi.com/chat/abc?token=leak": "other",
		"https://www.kimi.com/chat/abc/extra":      "other",
	}
	for raw, want := range cases {
		if got := ClassifySurface("page", raw); got != want {
			t.Fatalf("%s -> %s, want %s", raw, got, want)
		}
	}
	if ClassifySurface("worker", "https://www.kimi.com/chat/abc") != "other" {
		t.Fatal("a worker must never be a surface")
	}
	if ClassifySurface("iframe", "app://localhost/kimi-agent.html") != "other" {
		t.Fatal("a non-page target must never be a surface")
	}
}

func TestHostAttachesOnlyToAllowedTargets(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		Settings: map[string]any{"enabled": true, "provider": "bing", "targetLanguage": "zh", "view": "translated"}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("attached %d sessions, want 2 (work + chat)", len(sessions))
	}
	for _, session := range sessions {
		if session.TargetID == "settings" || session.TargetID == "evil" || session.TargetID == "worker" {
			t.Fatalf("attached to a forbidden target: %s", session.TargetID)
		}
	}
	if page.injections != 2 {
		t.Fatalf("injections=%d, want 2", page.injections)
	}
	if page.bindingCalls != 2 {
		t.Fatalf("binding registrations=%d, want 2", page.bindingCalls)
	}
}

func TestInjectionPreamblePrecedesThePayload(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		Settings: map[string]any{"enabled": true}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := host.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	source := page.lastSource
	if !strings.Contains(source, `globalThis.__kwgTranslationScope="rev-1"`) {
		t.Fatalf("scope preamble missing: %s", source[:min(200, len(source))])
	}
	if !strings.Contains(source, testPayload) {
		t.Fatal("payload was modified or dropped")
	}
	// The payload must be the tail: the host prepends, it never rewrites.
	if !strings.HasSuffix(strings.TrimSpace(source), strings.TrimSpace(testPayload)) {
		t.Fatal("payload is not the tail of the injected source")
	}
}

func TestRejectedReceiptIsFailureNotSuccess(t *testing.T) {
	// The renderer claims a surface that is not the one the host classified for
	// either target, so no session may accept the receipt.
	page := &fakePage{surface: "work",
		statusReply: map[string]any{"version": 1, "state": "active", "surface": "settings", "top": true}}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.State() == "active" {
			t.Fatalf("a receipt naming the wrong surface was accepted for %s", session.TargetID)
		}
	}
}

func TestTopLevelRequirementRejectsFrames(t *testing.T) {
	page := &fakePage{surface: "work",
		statusReply: map[string]any{"version": 1, "state": "active", "surface": "work", "top": false}}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, _ := host.Sync(ctx)
	for _, session := range sessions {
		if session.State() == "active" {
			t.Fatal("a non-top document was accepted")
		}
	}
}

func TestRevisionChangeReinjectsAndRetiresTheOldScript(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := host.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	first := page.injections
	host.options.Revision = "rev-2"
	if _, err := host.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if page.injections <= first {
		t.Fatalf("revision change did not re-inject (%d -> %d)", first, page.injections)
	}
	if page.removals == 0 {
		t.Fatal("the previous new-document script was not withdrawn")
	}
}

func TestNavigationInvalidatesTheAcknowledgement(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("initial sync: %v", err)
	}
	session := sessions[0]
	if session.State() != "active" {
		t.Fatalf("initial state %q", session.State())
	}

	// A same-document route change keeps the world but voids the receipt.
	harness.pipe.dispatch(`{"method":"Page.navigatedWithinDocument","sessionId":"` + session.SessionID + `"}`)
	eventually(t, "the acknowledgement to be invalidated", func() bool { return session.State() == "" })
	if session.ContextID() == 0 {
		t.Fatal("the execution world was dropped on a route change")
	}

	// A cleared world drops it.
	harness.pipe.dispatch(`{"method":"Runtime.executionContextsCleared","sessionId":"` + session.SessionID + `"}`)
	eventually(t, "the execution world to be dropped", func() bool { return session.ContextID() == 0 })
}

func TestSubframeNavigationDoesNotRetireTheSurface(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, _ := host.Sync(ctx)
	session := sessions[0]

	harness.pipe.dispatch(`{"method":"Page.frameNavigated","sessionId":"` + session.SessionID +
		`","params":{"frame":{"id":"ad-slot","parentId":"main-frame"}}}`)
	time.Sleep(50 * time.Millisecond)
	if session.State() != "active" {
		t.Fatal("a subframe navigation retired a page that was still the surface")
	}
	harness.pipe.dispatch(`{"method":"Page.frameNavigated","sessionId":"` + session.SessionID +
		`","params":{"frame":{"id":"main-frame"}}}`)
	eventually(t, "a main-frame navigation to retire the acknowledgement", func() bool { return session.State() == "" })
}

func TestBindingFromAnotherContextIsIgnored(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	var mu sync.Mutex
	var deliveries int
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		OnReply: func(context.Context, *SurfaceSession, int, string, any) {
			mu.Lock()
			deliveries++
			mu.Unlock()
		}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, _ := host.Sync(ctx)
	session := sessions[0]

	harness.pipe.dispatch(`{"method":"Runtime.bindingCalled","sessionId":"` + session.SessionID +
		`","params":{"name":"__kwgTranslate","payload":"{\"id\":\"1\",\"value\":\"hi\"}","executionContextId":99}}`)
	harness.pipe.dispatch(`{"method":"Runtime.bindingCalled","sessionId":"` + session.SessionID +
		`","params":{"name":"__kwgTranslate","payload":"{\"id\":\"2\",\"value\":\"hi\"}","executionContextId":` +
		itoa(session.ContextID()) + `}}`)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if deliveries != 1 {
		t.Fatalf("deliveries=%d, want 1 (the foreign context must be dropped)", deliveries)
	}
}

func itoa(value int) string { return strconv.Itoa(value) }

// eventually waits for an asynchronous host update. Events are delivered on the
// host's own goroutine, so asserting immediately after dispatch would race it.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
