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

	// observations
	methods      []string
	injections   int
	removals     int
	detachCalls  int
	evaluations  []string
	lastSource   string
	contextID    int
	generation   int
	bindingCalls int
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
			result["targetInfos"] = []any{
				map[string]any{"targetId": "work-1", "type": "page", "url": "app://localhost/kimi-agent.html"},
				map[string]any{"targetId": "chat-1", "type": "page", "url": "https://www.kimi.com/chat/abc-123"},
				map[string]any{"targetId": "settings", "type": "page", "url": "app://localhost/work-settings.html"},
				map[string]any{"targetId": "evil", "type": "page", "url": "https://www.kimi.com.evil.test/chat/abc"},
				map[string]any{"targetId": "worker", "type": "worker", "url": "https://www.kimi.com/chat/abc"},
			}
		case "Target.attachToTarget":
			if p.attachFailure {
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
			default:
				result["result"] = nil
			}
		}
		response, _ := json.Marshal(map[string]any{"id": request.ID, "result": result, "sessionId": request.SessionID})
		write(string(response))
	}
}

func (p *fakePage) emit(write func(string), message map[string]any) {
	encoded, _ := json.Marshal(message)
	write(string(encoded))
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
