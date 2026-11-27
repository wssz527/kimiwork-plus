package kimi

import (
	"context"
	"encoding/json"
	"errors"
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
	workURL       string
	statusReply   map[string]any
	attachFailure bool
	navigateOn    bool

	// actions models the official preload API the CDP actions call. It is off by
	// default so every existing test keeps the behaviour it was written for.
	actions                bool
	evaluateAction         func(string) (any, bool)
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
	importBlockFor time.Duration
	importReply    map[string]any
	// importReplies answers consecutive import calls; the last entry repeats.
	// onImport runs before the answer, so a test can model the store being
	// rewritten by another process between the repair and the preview.
	importReplies   []map[string]any
	onImport        func()
	disconnectReply map[string]any
	// applyPendingReplies answers consecutive applyPending probes the same way
	// the official page does: the last entry repeats.
	applyPendingReplies []map[string]any
	applyPendingCalls   int
	// directImportReplies answers consecutive direct imports; the last repeats.
	directImportReplies []map[string]any
	directCleanupReply  map[string]any
	directCleanupCalls  int
	gatewayCleanupReply map[string]any
	gatewayCleanupCalls int
	closeFails          bool
	// daimonStates answers the official runtime status probe in order, so a
	// page can model the startup window; an exhausted list reports an unreadable
	// state. daimonStuck holds a single startup state forever.
	daimonStates []string
	daimonStuck  bool
	daimonReads  int
	// kthDisconnectReply answers the canonical-provider repair; nil answers an
	// applied disconnect. onKthDisconnect runs when that probe asks, so a test
	// can write the store the way the official apply would.
	kthDisconnectReply map[string]any
	kthDisconnectCalls int
	onKthDisconnect    func()

	// observations
	methods       []string
	injections    int
	removals      int
	detachCalls   int
	evaluations   []string
	onEvaluate    func(string)
	lastSource    string
	contextID     int
	generation    int
	bindingCalls  int
	openCalls     int
	closeCalls    int
	importCalls   int
	directImports int
	activateCalls int
	// installedScripts is the set of new-document scripts the page is holding,
	// keyed by CDP session. Identifiers are unique like a real browser's, so a
	// leaked replacement shows up as a script nobody removed.
	installedScripts map[string]map[string]bool
	scriptSeq        int
	// configureSession restricts the configure knobs to one CDP session; empty
	// means every session. configureFailures answers that many leading configure
	// calls with a protocol error, and configureInactiveFor answers them with
	// state "inactive", so an update that did not take effect can be modelled.
	configureSession     string
	configureCalls       int
	configureFailures    int
	configureInactiveFor int
	// onConfigure runs before the configure answer, so a test can deliver a page
	// event while the call is still in flight.
	onConfigure func(sessionID string)
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
			workURL := p.workURL
			if workURL == "" {
				workURL = "app://localhost/kimi-agent.html"
			}
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
					"url": workURL}}, targets...)
			}
			if workerWork {
				targets = append(targets, map[string]any{"targetId": "worker-work", "type": "worker",
					"url": "app://localhost/kimi-agent.html"})
			}
			// The settings overlay only exists as a target once it is open, which
			// is what a real desktop does: the page is created by openSettings.
			if !p.hideSettings || settingsOpen {
				targets = append(targets, map[string]any{"targetId": "settings", "type": "page",
					"url": "app://localhost/work-settings.html?theme=dark"})
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
			p.scriptSeq++
			identifier := "script-" + strconv.Itoa(p.scriptSeq)
			if p.installedScripts == nil {
				p.installedScripts = map[string]map[string]bool{}
			}
			if p.installedScripts[request.SessionID] == nil {
				p.installedScripts[request.SessionID] = map[string]bool{}
			}
			p.installedScripts[request.SessionID][identifier] = true
			p.lastSource, _ = request.Params["source"].(string)
			p.mu.Unlock()
			result["identifier"] = identifier
		case "Page.removeScriptToEvaluateOnNewDocument":
			identifier, _ := request.Params["identifier"].(string)
			p.mu.Lock()
			p.removals++
			delete(p.installedScripts[request.SessionID], identifier)
			p.mu.Unlock()
		case "Runtime.evaluate":
			expression, _ := request.Params["expression"].(string)
			p.mu.Lock()
			p.evaluations = append(p.evaluations, expression)
			p.mu.Unlock()
			if p.onEvaluate != nil {
				p.onEvaluate(expression)
			}
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
			case strings.Contains(expression, "__kwgTranslation?.configure("):
				if hook := p.configureHook(); hook != nil {
					hook(request.SessionID)
				}
				reply := p.configureReply(request.SessionID, expression)
				if reply == nil {
					// A page that replaced its document under the call answers with
					// a protocol error, not with a status.
					response, _ := json.Marshal(map[string]any{"id": request.ID,
						"error":     map[string]any{"code": -32001, "message": "Session with given id not found."},
						"sessionId": request.SessionID})
					write(string(response))
					return
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

// configureReply answers a live configure the way the payload's configure does:
// a status report for this page, in the state the settings ask for. A scoped
// failure lets a test model a page that replaced its document under the call.
func (p *fakePage) configureReply(sessionID, expression string) map[string]any {
	p.mu.Lock()
	scoped := p.configureSession == "" || p.configureSession == sessionID
	if scoped {
		p.configureCalls++
	}
	calls := p.configureCalls
	state := "active"
	if strings.Contains(expression, `"enabled":false`) {
		state = "inactive"
	}
	if scoped && calls <= p.configureInactiveFor {
		state = "inactive"
	}
	fail := scoped && calls <= p.configureFailures
	surface := p.surface
	p.mu.Unlock()
	if fail {
		return nil
	}
	return map[string]any{"version": 1, "state": state, "surface": surface, "top": true,
		"structure": "matched", "blocks": 3, "seenBlocks": 3}
}

func (p *fakePage) installed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var identifiers []string
	for _, scripts := range p.installedScripts {
		for identifier := range scripts {
			identifiers = append(identifiers, identifier)
		}
	}
	return identifiers
}

func (p *fakePage) installedFor(sessionID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var identifiers []string
	for identifier := range p.installedScripts[sessionID] {
		identifiers = append(identifiers, identifier)
	}
	return identifiers
}

func (p *fakePage) configureHook() func(string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.onConfigure
}

func (p *fakePage) methodCount(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, recorded := range p.methods {
		if recorded == method {
			count++
		}
	}
	return count
}

// actionReply models the official preload API for the CDP actions. It is the
// same surface main_test.go's fake child speaks, so the actions are exercised
// through their real expressions rather than through a stub.
func (p *fakePage) actionReply(sessionID, expression string) any {
	if p.evaluateAction != nil {
		if value, handled := p.evaluateAction(expression); handled {
			return value
		}
	}
	switch {
	case expression == daimonStatusExpression:
		p.mu.Lock()
		defer p.mu.Unlock()
		p.daimonReads++
		if p.daimonStuck {
			return "provisioning"
		}
		if len(p.daimonStates) == 0 {
			return ""
		}
		state := p.daimonStates[0]
		p.daimonStates = p.daimonStates[1:]
		return state
	case expression == kthDisconnectExpression:
		p.mu.Lock()
		p.kthDisconnectCalls++
		reply := p.kthDisconnectReply
		hook := p.onKthDisconnect
		p.mu.Unlock()
		if hook != nil {
			hook()
		}
		if reply == nil {
			reply = map[string]any{"ok": true, "status": "applied"}
		}
		return reply
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
		if next := nextFakeReply(&p.importReplies); next != nil {
			reply = next
		}
		hook := p.onImport
		hold := p.importBlockFor
		p.mu.Unlock()
		if hook != nil {
			hook()
		}
		if hold > 0 {
			time.Sleep(hold)
		}
		if reply == nil {
			reply = map[string]any{"ok": true, "models": float64(2), "requiresRestart": false}
		}
		return reply
	case strings.Contains(expression, "applyPending(expectedRevision)"):
		p.mu.Lock()
		p.applyPendingCalls++
		reply := nextFakeReply(&p.applyPendingReplies)
		p.mu.Unlock()
		if reply == nil {
			reply = map[string]any{"ok": true, "status": "applied"}
		}
		return reply
	case strings.Contains(expression, "const request=JSON.parse(raw);"):
		p.mu.Lock()
		p.directImports++
		reply := nextFakeReply(&p.directImportReplies)
		p.mu.Unlock()
		if reply == nil {
			reply = map[string]any{"ok": true, "sourceId": "byok-src-1", "models": float64(1), "requiresRestart": false}
		}
		return reply
	case strings.Contains(expression, "sourceRevision:direct.revision??null"):
		p.mu.Lock()
		p.directCleanupCalls++
		reply := p.directCleanupReply
		p.mu.Unlock()
		if reply == nil {
			reply = map[string]any{"ok": true}
		}
		return reply
	case strings.Contains(expression, "sourceRevision:gateway.revision??null"):
		p.mu.Lock()
		p.gatewayCleanupCalls++
		reply := p.gatewayCleanupReply
		p.mu.Unlock()
		if reply == nil {
			reply = map[string]any{"ok": true}
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

// nextFakeReply takes the next scripted answer from a queue and keeps the last
// one for every later call. Callers hold the page lock.
func nextFakeReply(replies *[]map[string]any) map[string]any {
	if len(*replies) == 0 {
		return nil
	}
	reply := (*replies)[0]
	if len(*replies) > 1 {
		*replies = (*replies)[1:]
	}
	return reply
}

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

func TestHostSyncTracksWorkURLParameters(t *testing.T) {
	page := &fakePage{surface: "work", workURL: "app://localhost/kimi-agent.html?theme=dark&locale=zh-CN&platform=darwin"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer host.DetachAll(ctx)

	syncWork := func() *SurfaceSession {
		t.Helper()
		sessions, err := host.Sync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, session := range sessions {
			if session.TargetID == "work-1" && session.State() == "active" && !session.Failed() {
				return session
			}
		}
		t.Fatal("Work page did not accept injection")
		return nil
	}
	work := syncWork()
	generation := work.Generation()
	if same := syncWork(); same != work || same.Generation() != generation {
		t.Fatal("unchanged URL triggered a new injection")
	}
	page.mu.Lock()
	page.workURL = "app://localhost/kimi-agent.html?theme=light&locale=zh-CN&platform=darwin"
	page.mu.Unlock()
	if same := syncWork(); same != work || same.Generation() != generation+1 {
		t.Fatal("changed query did not trigger a new injection in the attached session")
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

// TestBindingDeliversTheLiveRendererPayload pins the contract the real renderer
// sends: one flat object carrying text and protectedTexts at the top level,
// stamped with the generation that asked for the translation. A request stamped
// with another generation describes a document this host already replaced, so it
// must be dropped instead of answered.
func TestBindingDeliversTheLiveRendererPayload(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	type delivery struct {
		id    string
		value any
	}
	delivered := make(chan delivery, 4)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		OnReply: func(_ context.Context, _ *SurfaceSession, _ int, id string, value any) {
			delivered <- delivery{id: id, value: value}
		}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("sync: %v", err)
	}
	session := sessions[0]
	dispatch := func(payload string) {
		harness.pipe.dispatch(`{"method":"Runtime.bindingCalled","sessionId":"` + session.SessionID +
			`","params":{"name":"__kwgTranslate","payload":` + strconv.Quote(payload) +
			`,"executionContextId":` + itoa(session.ContextID()) + `}}`)
	}
	dispatch(`{"type":"translate","id":"kwg-1","generation":` + itoa(session.Generation()) +
		`,"text":"Check code","protectedTexts":["code"]}`)
	select {
	case got := <-delivered:
		value, ok := got.value.(map[string]any)
		if !ok {
			t.Fatalf("value = %#v, want the flat request object", got.value)
		}
		if got.id != "kwg-1" || value["text"] != "Check code" {
			t.Fatalf("delivery = %+v value = %#v", got, value)
		}
		protected, ok := value["protectedTexts"].([]any)
		if !ok || len(protected) != 1 || protected[0] != "code" {
			t.Fatalf("protectedTexts = %#v", value["protectedTexts"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the live payload never reached the provider")
	}
	// A stale generation, a request that names no id and one with an empty id
	// cannot be answered in this document, so none of them is delivered.
	dispatch(`{"type":"translate","id":"kwg-2","generation":` + itoa(session.Generation()+1) +
		`,"text":"stale","protectedTexts":[]}`)
	dispatch(`{"type":"translate","generation":` + itoa(session.Generation()) + `,"text":"nameless","protectedTexts":[]}`)
	dispatch(`{"id":"","value":"hi"}`)
	select {
	case got := <-delivered:
		t.Fatalf("a request that cannot be answered was delivered: %+v", got)
	case <-time.After(150 * time.Millisecond):
	}
	// The wrapped shape an older renderer sent keeps working.
	dispatch(`{"id":"legacy","value":"hi"}`)
	select {
	case got := <-delivered:
		if got.id != "legacy" || got.value != "hi" {
			t.Fatalf("legacy delivery = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wrapped payload was refused")
	}
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

// workSession returns the attached session for the Work page.
func workSession(t *testing.T, sessions []*SurfaceSession) *SurfaceSession {
	t.Helper()
	for _, session := range sessions {
		if session.TargetID == "work-1" {
			return session
		}
	}
	t.Fatal("the Work page was not attached")
	return nil
}

// TestHostSettingsSyncAndWithdrawalLeaveNoScriptBehind runs the three operations
// that replace a page's scripts against each other. They are serialized, so this
// proves neither a data race nor a deadlock, and the final accounting proves the
// page is left with one boot script per attached view and none after a
// withdrawal.
func TestHostSettingsSyncAndWithdrawalLeaveNoScriptBehind(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	settings := func(view string) map[string]any {
		return map[string]any{"enabled": true, "provider": "bing", "targetLanguage": "zh", "view": view}
	}
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		Settings: settings("translated")})
	t.Cleanup(func() { host.DetachAll(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const rounds = 20
	var wait sync.WaitGroup
	failures := make(chan string, 8)
	fail := func(message string) {
		select {
		case failures <- message:
		default:
		}
	}
	wait.Add(3)
	go func() {
		defer wait.Done()
		views := []string{"translated", "original"}
		for round := 0; round < rounds; round++ {
			host.Configure(settings(views[round%len(views)]), "rev-1")
			host.ApplySettings(ctx)
		}
	}()
	go func() {
		defer wait.Done()
		for round := 0; round < rounds; round++ {
			if _, err := host.Sync(ctx); err != nil {
				fail("sync: " + err.Error())
				return
			}
		}
	}()
	go func() {
		defer wait.Done()
		for round := 0; round < rounds; round++ {
			host.WithdrawAll(ctx)
		}
	}()

	done := make(chan struct{})
	go func() {
		wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the host deadlocked between settings, sync and withdrawal")
	}
	close(failures)
	for message := range failures {
		t.Fatal(message)
	}

	sessions, err := host.Sync(ctx)
	if err != nil {
		t.Fatalf("final sync: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("attached %d sessions, want 2", len(sessions))
	}
	if installed := page.installed(); len(installed) != len(sessions) {
		t.Fatalf("the page holds %d new-document scripts for %d sessions: %v", len(installed), len(sessions), installed)
	}
	for _, session := range sessions {
		if held := page.installedFor(session.SessionID); len(held) != 1 {
			t.Fatalf("session %s holds %d boot scripts: %v", session.TargetID, len(held), held)
		}
	}
	host.WithdrawAll(ctx)
	if host.HasSessions() {
		t.Fatal("the withdrawal left sessions attached")
	}
	if installed := page.installed(); len(installed) != 0 {
		t.Fatalf("the withdrawal left new-document scripts behind: %v", installed)
	}
}

// TestSyncHealsAFailedSettingsUpdate pins the self-healing contract: a view that
// never reached the page — the document was replaced under the call, or the page
// answered that translation stayed inactive — is not acknowledged. The next tick
// re-injects the scope, so the page ends on the newest view instead of keeping
// the previous one until it navigates.
func TestSyncHealsAFailedSettingsUpdate(t *testing.T) {
	for _, testCase := range []struct {
		name string
		page *fakePage
	}{
		{"page answers inactive", &fakePage{surface: "work", configureSession: "session-work-1",
			configureInactiveFor: 1}},
		{"page errors under the call", &fakePage{surface: "work", configureSession: "session-work-1",
			configureFailures: 1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			page := testCase.page
			harness := newHarness(t, page)
			settings := func(view string) map[string]any {
				return map[string]any{"enabled": true, "provider": "bing", "targetLanguage": "zh", "view": view}
			}
			host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
				Settings: settings("translated")})
			t.Cleanup(func() { host.DetachAll(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			sessions, err := host.Sync(ctx)
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			session := workSession(t, sessions)
			if session.State() != "active" {
				t.Fatalf("work state = %q", session.State())
			}
			generation := session.Generation()

			// Only the view moves, so this is a live configure and no re-injection.
			host.Configure(settings("original"), "rev-1")
			host.ApplySettings(ctx)
			if !session.Failed() || session.State() != "failed" {
				t.Fatalf("a failed update was acknowledged: state=%q failed=%v", session.State(), session.Failed())
			}
			if session.Generation() != generation {
				t.Fatal("the settings update started a new generation")
			}

			sessions, err = host.Sync(ctx)
			if err != nil {
				t.Fatalf("healing sync: %v", err)
			}
			session = workSession(t, sessions)
			if session.State() != "active" || session.Failed() {
				t.Fatalf("the next sync did not heal the page: state=%q failed=%v", session.State(), session.Failed())
			}
			if session.Generation() != generation+1 {
				t.Fatalf("generation = %d, want %d (the scope was not re-injected)", session.Generation(), generation+1)
			}
			page.mu.Lock()
			source := page.lastSource
			page.mu.Unlock()
			if !strings.Contains(source, `"view":"original"`) {
				t.Fatal("the healed page boot script still carries the previous view")
			}

			// Once healed, the acknowledgement holds and a further tick does not
			// inject again.
			sessions, err = host.Sync(ctx)
			if err != nil {
				t.Fatalf("settled sync: %v", err)
			}
			if healed := workSession(t, sessions); healed.Generation() != generation+1 {
				t.Fatalf("a healed session was injected again: generation=%d", healed.Generation())
			}
		})
	}
}

// TestWithdrawalCancelsAnInFlightTranslation proves a provider call is not left
// running for its full reply deadline when the page it answers is withdrawn: the
// subscription ends inside the same call, so a later translation is not queued
// behind a request nobody will use.
func TestWithdrawalCancelsAnInFlightTranslation(t *testing.T) {
	for _, testCase := range []struct {
		name string
		stop bool
	}{
		{"DetachAll", true},
		{"WithdrawAll", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			page := &fakePage{surface: "work"}
			harness := newHarness(t, page)
			started := make(chan struct{}, 1)
			waited := make(chan time.Duration, 4)
			host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
				ReplyTimeout: 10 * time.Second,
				OnReply: func(ctx context.Context, _ *SurfaceSession, _ int, _ string, _ any) {
					select {
					case started <- struct{}{}:
					default:
					}
					began := time.Now()
					<-ctx.Done()
					waited <- time.Since(began)
				}})
			t.Cleanup(func() { host.DetachAll(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			sessions, err := host.Sync(ctx)
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			session := workSession(t, sessions)

			harness.pipe.dispatch(bindingPayload(session, "slow-1", "hello"))
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("the provider call did not start")
			}

			if testCase.stop {
				host.DetachAll(ctx)
			} else {
				host.WithdrawAll(ctx)
			}
			select {
			case elapsed := <-waited:
				if elapsed > 200*time.Millisecond {
					t.Fatalf("the provider context took %v to end", elapsed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the provider context was not cancelled; it would wait out the reply timeout")
			}

			if testCase.stop {
				if _, err := host.Sync(ctx); err == nil {
					t.Fatal("a retired host accepted a sync")
				}
				return
			}
			// The host survives a withdrawal: re-enabling translation can attach
			// again without restarting the process.
			sessions, err = host.Sync(ctx)
			if err != nil {
				t.Fatalf("sync after withdrawal: %v", err)
			}
			if len(sessions) == 0 {
				t.Fatal("the host did not re-attach after a withdrawal")
			}
		})
	}
}

// TestWithdrawAllWakesABlockedSyncAndCleansUpAnExpiredCaller covers the window a
// controller with a three-second budget hits: a sync is waiting for a world with
// the host lock held, and the withdrawal is called with a context that is already
// spent. The withdrawal must interrupt the wait, and the cleanup must not inherit
// the expired context — a failed cleanup with an emptied session map would leak
// the script and the CDP session for good.
func TestWithdrawAllWakesABlockedSyncAndCleansUpAnExpiredCaller(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		Settings: map[string]any{"enabled": true, "provider": "bing", "targetLanguage": "zh"}})
	t.Cleanup(func() { host.DetachAll(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sync: %v, sessions=%d", err, len(sessions))
	}
	if installed := page.installed(); len(installed) != len(sessions) {
		t.Fatalf("boot scripts before the withdrawal = %v", installed)
	}

	// The worlds are cleared and the fake reports no new one until Runtime.enable
	// is issued again, so the next injection parks in waitForMainContext with
	// syncMu held.
	for _, session := range sessions {
		harness.pipe.dispatch(`{"method":"Runtime.executionContextsCleared","sessionId":"` + session.SessionID + `"}`)
	}
	eventually(t, "the worlds to be dropped", func() bool {
		return sessions[0].ContextID() == 0 && sessions[1].ContextID() == 0
	})
	enables := page.methodCount("Page.enable")

	blocked := make(chan error, 1)
	go func() {
		_, err := host.Sync(context.Background())
		blocked <- err
	}()
	eventually(t, "the injection to reach the world wait", func() bool {
		if page.methodCount("Page.enable") < enables+1 {
			return false
		}
		// The waiter is registered after the cancel batch is read, so seeing it
		// means a withdrawal now is guaranteed to close the channel the sync holds.
		host.mu.Lock()
		defer host.mu.Unlock()
		return len(host.contextWaiters) > 0
	})

	caller, cancelCaller := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelCaller()
	<-caller.Done()

	start := time.Now()
	host.WithdrawAll(caller)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("the withdrawal waited %v for the blocked sync", elapsed)
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("the withdrawal did not release the blocked sync")
	}
	if installed := page.installed(); len(installed) != 0 {
		t.Fatalf("the withdrawal left new-document scripts behind: %v", installed)
	}
	if host.HasSessions() {
		t.Fatal("the withdrawal left sessions attached")
	}
}

// TestWithdrawnSessionIgnoresALateBinding pins the atomic liveness check: the
// event loop takes an event under the lock, and the withdrawal may retire the
// session before the binding is dispatched. The late request must be dropped, not
// handed the fresh batch and left to run for the whole provider deadline.
func TestWithdrawnSessionIgnoresALateBinding(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	var mu sync.Mutex
	calls := 0
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		OnReply: func(context.Context, *SurfaceSession, int, string, any) {
			mu.Lock()
			calls++
			mu.Unlock()
		}})
	t.Cleanup(func() { host.DetachAll(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	session := workSession(t, sessions)
	host.WithdrawAll(ctx)
	if host.HasSessions() {
		t.Fatal("the withdrawal left the session attached")
	}

	// The event loop already holds this session reference from an event taken
	// before the withdrawal.
	host.deliverBinding(session, Event{Method: "Runtime.bindingCalled", SessionID: session.SessionID,
		Params: map[string]any{"name": BindingName, "executionContextId": float64(session.ContextID()),
			"payload": `{"type":"translate","id":"late-1","generation":` + itoa(session.Generation()) +
				`,"text":"hello","protectedTexts":[]}`}})
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("a withdrawn session delivered %d late requests", calls)
	}
}

// TestLeavingTheSurfaceCancelsThePendingReply proves a session that leaves the
// surface retires on its own: the provider call it started is cancelled instead
// of holding a reply slot until the deadline.
func TestLeavingTheSurfaceCancelsThePendingReply(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	started := make(chan struct{}, 1)
	waited := make(chan time.Duration, 4)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		ReplyTimeout: 10 * time.Second,
		OnReply: func(ctx context.Context, _ *SurfaceSession, _ int, _ string, _ any) {
			select {
			case started <- struct{}{}:
			default:
			}
			began := time.Now()
			<-ctx.Done()
			waited <- time.Since(began)
		}})
	t.Cleanup(func() { host.DetachAll(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	session := workSession(t, sessions)

	harness.pipe.dispatch(bindingPayload(session, "slow-1", "hello"))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the provider call did not start")
	}

	page.mu.Lock()
	page.workURL = "https://example.com/away"
	page.mu.Unlock()
	sessions, err = host.Sync(ctx)
	if err != nil {
		t.Fatalf("sync after the page left: %v", err)
	}
	for _, live := range sessions {
		if live.TargetID == session.TargetID {
			t.Fatalf("the page that left the surface is still attached: %s", live.SessionID)
		}
	}
	select {
	case elapsed := <-waited:
		if elapsed > 200*time.Millisecond {
			t.Fatalf("the pending reply took %v to end after the page left", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reply for a page that left the surface was not cancelled")
	}
	if installed := page.installedFor(session.SessionID); len(installed) != 0 {
		t.Fatalf("the retired session kept its boot script: %v", installed)
	}
}

// TestStaleConfigureIsNotAcknowledged covers a configure that was in flight while
// the document was replaced: accepting its answer would overwrite the reset the
// event loop just recorded and leave the page on the previous view with no retry.
func TestStaleConfigureIsNotAcknowledged(t *testing.T) {
	settings := func(view string) map[string]any {
		return map[string]any{"enabled": true, "provider": "bing", "targetLanguage": "zh", "view": view}
	}
	t.Run("load event during the configure", func(t *testing.T) {
		page := &fakePage{surface: "work"}
		harness := newHarness(t, page)
		host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
			Settings: settings("translated")})
		t.Cleanup(func() { host.DetachAll(context.Background()) })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sessions, err := host.Sync(ctx)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		session := workSession(t, sessions)
		generation := session.Generation()

		var once sync.Once
		page.mu.Lock()
		page.onConfigure = func(sessionID string) {
			if sessionID != session.SessionID {
				return
			}
			once.Do(func() {
				harness.pipe.dispatch(`{"method":"Page.loadEventFired","sessionId":"` + session.SessionID + `"}`)
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) && session.State() != "" {
					time.Sleep(time.Millisecond)
				}
			})
		}
		page.mu.Unlock()

		host.Configure(settings("original"), "rev-1")
		host.ApplySettings(ctx)
		if !session.Failed() || session.State() == "active" {
			t.Fatalf("a configure for a replaced document was acknowledged: state=%q failed=%v",
				session.State(), session.Failed())
		}

		sessions, err = host.Sync(ctx)
		if err != nil {
			t.Fatalf("healing sync: %v", err)
		}
		session = workSession(t, sessions)
		if session.State() != "active" || session.Failed() {
			t.Fatalf("the next sync did not restore the page: state=%q failed=%v", session.State(), session.Failed())
		}
		if session.Generation() != generation+1 {
			t.Fatalf("generation = %d, want %d", session.Generation(), generation+1)
		}
	})

	t.Run("destroyed world", func(t *testing.T) {
		page := &fakePage{surface: "work"}
		harness := newHarness(t, page)
		host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
			Settings: settings("translated")})
		t.Cleanup(func() { host.DetachAll(context.Background()) })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sessions, err := host.Sync(ctx)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		session := workSession(t, sessions)
		generation := session.Generation()
		bindings := page.methodCount("Runtime.addBinding")

		// The world that carried the injection is destroyed. A session that kept
		// its acknowledgement would skip the re-injection, so the new world would
		// never get its binding.
		harness.pipe.dispatch(`{"method":"Runtime.executionContextDestroyed","sessionId":"` + session.SessionID +
			`","params":{"executionContextId":` + itoa(session.ContextID()) + `}}`)
		eventually(t, "the acknowledgement to be voided with the world", func() bool {
			return session.ContextID() == 0 && session.State() == ""
		})
		harness.pipe.dispatch(`{"method":"Runtime.executionContextCreated","sessionId":"` + session.SessionID +
			`","params":{"context":{"id":9,"auxData":{"isDefault":true,"frameId":"main-frame"}}}}`)
		eventually(t, "the new world to be tracked", func() bool { return session.ContextID() == 9 })

		sessions, err = host.Sync(ctx)
		if err != nil {
			t.Fatalf("sync after the world was replaced: %v", err)
		}
		session = workSession(t, sessions)
		if session.State() != "active" || session.Failed() {
			t.Fatalf("the new world did not recover: state=%q failed=%v", session.State(), session.Failed())
		}
		if session.Generation() != generation+1 {
			t.Fatalf("generation = %d, want %d", session.Generation(), generation+1)
		}
		if now := page.methodCount("Runtime.addBinding"); now <= bindings {
			t.Fatal("the new world did not get the binding again")
		}
	})
}

// TestSyncAppliesAViewChangeInPlace proves a view-only switch does not wait for a
// controller push: the next tick sees the settings the page is not running and
// updates the live document and its boot script without a new generation.
func TestSyncAppliesAViewChangeInPlace(t *testing.T) {
	settings := func(view string) map[string]any {
		return map[string]any{"enabled": true, "provider": "bing", "targetLanguage": "zh", "view": view}
	}
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		Settings: settings("translated")})
	t.Cleanup(func() { host.DetachAll(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	session := workSession(t, sessions)
	generation := session.Generation()
	configures := func() int {
		count := 0
		for _, expression := range page.snapshotEvaluations() {
			if strings.Contains(expression, "__kwgTranslation?.configure(") {
				count++
			}
		}
		return count
	}
	before := configures()

	host.Configure(settings("original"), "rev-1")
	sessions, err = host.Sync(ctx)
	if err != nil {
		t.Fatalf("sync after the view change: %v", err)
	}
	session = workSession(t, sessions)
	if session.Generation() != generation {
		t.Fatalf("the view change re-injected the page: generation=%d", session.Generation())
	}
	if session.State() != "active" || session.Failed() {
		t.Fatalf("the in-place update failed: state=%q failed=%v", session.State(), session.Failed())
	}
	page.mu.Lock()
	source := page.lastSource
	page.mu.Unlock()
	if !strings.Contains(source, `"view":"original"`) {
		t.Fatal("the boot script still carries the previous view")
	}
	found := false
	for _, expression := range page.snapshotEvaluations() {
		if strings.Contains(expression, "__kwgTranslation?.configure(") &&
			strings.Contains(expression, `"view":"original"`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the live document was not told about the new view")
	}

	// The update is acknowledged now, so another tick does not repeat it.
	applied := configures()
	if applied <= before {
		t.Fatalf("the view change was not configured (%d -> %d)", before, applied)
	}
	if _, err := host.Sync(ctx); err != nil {
		t.Fatalf("settled sync: %v", err)
	}
	if again := configures(); again != applied {
		t.Fatalf("an acknowledged view change was configured again (%d -> %d)", applied, again)
	}
}

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

func TestBindingReceivesBusyReplyWhenTranslationSlotsAreFull(t *testing.T) {
	evaluated := make(chan string, 1)
	page := &fakePage{surface: "work", onEvaluate: func(expression string) {
		if strings.Contains(expression, "__kwgTranslationReceive?.(") {
			evaluated <- expression
		}
	}}
	harness := newHarness(t, page)
	providerCalled := make(chan struct{}, 1)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		OnReply: func(context.Context, *SurfaceSession, int, string, any) { providerCalled <- struct{}{} }})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("sync: %v", err)
	}
	for range cap(host.replySlots) {
		host.replySlots <- struct{}{}
	}
	session := sessions[0]
	payload := `{"type":"translate","id":"kwg-busy","generation":` + itoa(session.Generation()) + `,"text":"Translate this","protectedTexts":[]}`
	harness.pipe.dispatch(`{"method":"Runtime.bindingCalled","sessionId":"` + session.SessionID +
		`","params":{"name":"__kwgTranslate","payload":` + strconv.Quote(payload) +
		`,"executionContextId":` + itoa(session.ContextID()) + `}}`)

	select {
	case got := <-evaluated:
		if want := `globalThis.__kwgTranslationReceive?.("kwg-busy",{"error":"translation_busy","id":"kwg-busy"})`; got != want {
			t.Fatalf("renderer reply = %s, want %s", got, want)
		}
	case <-providerCalled:
		t.Fatal("a saturated queue submitted a provider request")
	case <-time.After(2 * time.Second):
		t.Fatal("the renderer never received a busy reply")
	}
}

func TestFailedInjectionPreflightPreservesGeneration(t *testing.T) {
	harness := newHarness(t, &fakePage{surface: "work"})
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("sync: %v", err)
	}
	session := sessions[0]
	generation, script := session.Generation(), session.ScriptID()
	harness.pipe.Dispose()
	if err := host.inject(ctx, session, host.snapshot(), "app://localhost/kimi-agent.html"); !errors.Is(err, ErrPipeClosed) {
		t.Fatalf("expected a preflight transport failure, got %v", err)
	}
	if session.Generation() != generation || session.ScriptID() != script {
		t.Fatalf("failed preflight changed generation or script: %d %q", session.Generation(), session.ScriptID())
	}
}
