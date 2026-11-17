package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// The page expressions are executed for real: goja runs the JavaScript the
// renderer would run, against a kimiAgentAPI stand-in. That is the only way to
// prove the ordering the contract depends on — a status push arrives, the
// transition is seen, and only then is the models query sent.

// runtimePage stands in for the Work shell page. `current` is the coordinator
// state: a status push and `getStatus()` both answer it, because in the desktop
// they both come from the same `#setStatus`. A push moves it, so a test drives
// the page the way a switch does.
type runtimePage struct {
	runtime *goja.Runtime
	// listeners are the callbacks registered through onEvent.
	listeners []func(payload any)
	// current is what getStatus answers right now.
	current string
	// rpcResult is what rpc answers. rpcCalls counts the calls.
	rpcResult any
	rpcCalls  int
	// rpcThrows makes rpc reject instead of answering.
	rpcThrows bool
	// rpcMissing drops the rpc entry point.
	rpcMissing bool
	// api is the published stand-in, kept so a test can change it.
	api map[string]any
}

func newRuntimePage(t *testing.T) *runtimePage {
	t.Helper()
	page := &runtimePage{runtime: goja.New(), current: "disconnected"}
	api := map[string]any{
		"onEvent": func(callback func(any) any) any {
			page.listeners = append(page.listeners, func(payload any) { callback(payload) })
			// The official preload returns an unsubscribe function.
			return func() any { return nil }
		},
		// The production status object also carries the transport endpoint,
		// which embeds credentials.
		"getStatus": func() any {
			return map[string]any{"state": page.current,
				"endpoint": map[string]any{"transport": "ws", "url": "ws://127.0.0.1:1",
					"auth": "secret-token-value"}}
		},
		"rpc": func(method string) any {
			page.rpcCalls++
			if page.rpcThrows {
				panic(page.runtime.NewGoError(errors.New("rpc rejected")))
			}
			if method != "conversations.getKimiModelList" {
				panic(page.runtime.NewGoError(fmt.Errorf("unexpected method %q", method)))
			}
			return page.rpcResult
		},
	}
	if page.rpcMissing {
		delete(api, "rpc")
	}
	if err := page.runtime.Set("kimiAgentAPI", api); err != nil {
		t.Fatal(err)
	}
	page.api = api
	return page
}

// dropRPC republishes the stand-in without the rpc entry point, which is what a
// build that cannot query the runtime looks like.
func (page *runtimePage) dropRPC(t *testing.T) {
	t.Helper()
	page.rpcMissing = true
	delete(page.api, "rpc")
	if err := page.runtime.Set("kimiAgentAPI", page.api); err != nil {
		t.Fatal(err)
	}
}

// push delivers one event payload to every registered listener and, for a status
// push, moves the coordinator state the way `#setStatus` does.
func (page *runtimePage) push(t *testing.T, payload any) {
	t.Helper()
	if object, ok := payload.(map[string]any); ok && object["t"] == "status" {
		if status, ok := object["status"].(map[string]any); ok {
			if state, ok := status["state"].(string); ok && state != "" {
				page.current = state
			}
		}
	}
	for _, listener := range page.listeners {
		listener(payload)
	}
}

// statusPush is the exact payload the main process sends: one argument, shaped
// `{t:"status", status:{state, endpoint?}}`.
func statusPush(state string) map[string]any {
	return map[string]any{"t": "status", "status": map[string]any{"state": state,
		"endpoint": map[string]any{"transport": "ws", "url": "ws://127.0.0.1:1", "auth": "secret-token-value"}}}
}

// runOn evaluates one expression against a runtime and reads back the value it
// settles on. The production calls carry awaitPromise:true; goja drains the
// microtask queue when the script ends, so the answer is collected through a
// global instead of a returned promise.
func runOn(t *testing.T, runtime *goja.Runtime, expression string) map[string]any {
	t.Helper()
	script := "globalThis.__probeResult=null;globalThis.__probeError=null;" +
		"Promise.resolve(" + expression + ").then(function(value){globalThis.__probeResult=value;}," +
		"function(error){globalThis.__probeError=String(error&&error.message||error);});"
	if _, err := runtime.RunString(script); err != nil {
		t.Fatalf("expression failed: %v", err)
	}
	if failure := runtime.Get("__probeError"); failure != nil && !goja.IsNull(failure) {
		t.Fatalf("expression rejected: %v", failure.Export())
	}
	exported := runtime.Get("__probeResult").Export()
	result, ok := exported.(map[string]any)
	if !ok {
		t.Fatalf("expression answered %#v, want an object", exported)
	}
	return result
}

func (page *runtimePage) run(t *testing.T, expression string) map[string]any {
	t.Helper()
	return runOn(t, page.runtime, expression)
}

func TestAccountRuntimeProbeWaitsForTheTransitionBeforeQuerying(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"
	page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"models": []any{
		map[string]any{"model": "kimi-k2"},
	}}}

	if watch := page.run(t, accountRuntimeWatchExpression); watch["ok"] != true {
		t.Fatalf("watch: %#v", watch)
	}
	if len(page.listeners) != 1 {
		t.Fatalf("listeners: %d", len(page.listeners))
	}

	// The runtime is ready, but this watch never saw it leave ready. A ready left
	// over from before a switch must not be accepted, and the query must not be
	// sent on that basis.
	pre := page.run(t, accountRuntimeReadyExpression(true))
	if pre["installed"] != true || pre["state"] != "ready" || pre["readyAfterNotReady"] != false {
		t.Fatalf("pre-transition read: %#v", pre)
	}
	if page.rpcCalls != 0 {
		t.Fatalf("the models query was sent before the transition: %d calls", page.rpcCalls)
	}

	page.push(t, statusPush("switching"))
	if mid := page.run(t, accountRuntimeReadyExpression(true)); mid["state"] != "switching" {
		t.Fatalf("non-ready push: %#v", mid)
	}
	page.push(t, statusPush("ready"))
	if page.rpcCalls != 0 {
		t.Fatal("the models query was sent from the push handler instead of the poll")
	}

	post := page.run(t, accountRuntimeReadyExpression(true))
	if post["state"] != "ready" || post["readyAfterNotReady"] != true ||
		post["current"] != "ready" || post["rpc"] != "ok" {
		t.Fatalf("post-transition read: %#v", post)
	}
	if page.rpcCalls != 1 {
		t.Fatalf("models queries: %d, want 1", page.rpcCalls)
	}

	// A verified runtime keeps answering ok without being re-queried on every
	// poll of the same generation.
	if again := page.run(t, accountRuntimeReadyExpression(true)); again["rpc"] != "ok" {
		t.Fatalf("repeat read: %#v", again)
	}
	if page.rpcCalls != 1 {
		t.Fatalf("the models query was repeated: %d calls", page.rpcCalls)
	}
}

func TestAccountRuntimeProbeRequiresAFreshTransitionForEverySwitch(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"
	page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"models": []any{}}}
	page.run(t, accountRuntimeWatchExpression)
	page.push(t, statusPush("disconnected"))
	page.push(t, statusPush("ready"))
	if first := page.run(t, accountRuntimeReadyExpression(true)); first["rpc"] != "ok" {
		t.Fatalf("first transition: %#v", first)
	}

	// A second switch has to be observed on its own: the earlier transition and
	// the earlier verification do not carry over to the next generation.
	page.push(t, statusPush("switching"))
	if result := page.run(t, accountRuntimeReadyExpression(true)); result["readyAfterNotReady"] != false {
		t.Fatalf("the transition survived a non-ready status: %#v", result)
	}
	before := page.rpcCalls
	page.push(t, statusPush("ready"))
	if second := page.run(t, accountRuntimeReadyExpression(true)); second["rpc"] != "ok" {
		t.Fatalf("second transition: %#v", second)
	}
	if page.rpcCalls != before+1 {
		t.Fatalf("the new generation was not queried: %d -> %d", before, page.rpcCalls)
	}
}

func TestAccountRuntimeProbeAcceptsACurrentReadyWithoutATransition(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"
	page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"models": []any{}}}
	page.run(t, accountRuntimeWatchExpression)

	// The post-restart path has no history to observe: one current ready plus a
	// successful query is the whole check.
	current := page.run(t, accountRuntimeReadyExpression(false))
	if current["rpc"] != "ok" || current["readyAfterNotReady"] != false {
		t.Fatalf("current ready: %#v", current)
	}
	if page.rpcCalls != 1 {
		t.Fatalf("models queries: %d", page.rpcCalls)
	}
	// The same page satisfies the current path but not the transition path, which
	// is what keeps a pre-switch ready from passing the switch check.
	if result := page.run(t, accountRuntimeReadyExpression(true)); result["rpc"] != nil {
		t.Fatalf("a current ready passed the transition check: %#v", result)
	}
}

func TestAccountRuntimeProbeNeverCarriesAnEndpointOrAToken(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"
	page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"models": []any{}}}
	page.run(t, accountRuntimeWatchExpression)

	for _, result := range []map[string]any{
		page.run(t, accountRuntimeReadyExpression(false)),
		page.run(t, accountRuntimeReadyExpression(true)),
	} {
		serialized := fmt.Sprint(result)
		for _, secret := range []string{"endpoint", "secret-token-value", "ws://", "auth", "transport"} {
			if strings.Contains(serialized, secret) {
				t.Fatalf("the probe returned %q: %s", secret, serialized)
			}
		}
	}

	// A state longer than the bound is truncated rather than carried out whole.
	long := newRuntimePage(t)
	long.run(t, accountRuntimeWatchExpression)
	long.push(t, map[string]any{"t": "status", "status": map[string]any{"state": strings.Repeat("x", 4096)}})
	if got, _ := long.run(t, accountRuntimeReadyExpression(false))["state"].(string); len(got) > 64 {
		t.Fatalf("state was not bounded: %d chars", len(got))
	}
}

func TestAccountRuntimeProbeTreatsEveryFailedQueryAsNotReady(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(page *runtimePage)
		rpc   string
	}{
		{"ok-false", func(page *runtimePage) {
			page.rpcResult = map[string]any{"ok": false, "error": map[string]any{"code": "unavailable"}}
		}, "error"},
		{"models-missing", func(page *runtimePage) {
			page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"items": []any{}}}
		}, "shape"},
		{"models-scalar", func(page *runtimePage) {
			page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"models": "none"}}
		}, "shape"},
		{"no-result", func(page *runtimePage) {
			page.rpcResult = map[string]any{"ok": true}
		}, "shape"},
		{"rejected", func(page *runtimePage) { page.rpcThrows = true }, "error"},
		{"no-rpc", func(page *runtimePage) {}, "unsupported"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			page := newRuntimePage(t)
			page.current = "ready"
			tt.setup(page)
			if tt.rpc == "unsupported" {
				page.dropRPC(t)
			}
			page.run(t, accountRuntimeWatchExpression)
			result := page.run(t, accountRuntimeReadyExpression(false))
			if result["rpc"] != tt.rpc {
				t.Fatalf("rpc: %#v", result)
			}
			if page.rpcMissing {
				return
			}
			// A query that did not prove the kernel must not be cached as verified:
			// the next poll asks again.
			page.run(t, accountRuntimeReadyExpression(false))
			if page.rpcCalls < 2 {
				t.Fatalf("a failed query was cached as verified: %d calls", page.rpcCalls)
			}
		})
	}
}

func TestAccountRuntimeProbeIgnoresForeignEventShapes(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "switching"
	page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"models": []any{}}}
	page.run(t, accountRuntimeWatchExpression)
	for _, payload := range []any{
		// The control-plane envelope is a different channel and must not be read
		// as the renderer's status push.
		map[string]any{"method": "daimon.status", "params": map[string]any{"state": "ready"}},
		// Only a complete status push counts.
		map[string]any{"t": "status"},
		map[string]any{"t": "status", "status": map[string]any{"state": ""}},
		map[string]any{"t": "status", "status": map[string]any{"state": 1}},
		map[string]any{"t": "quota", "status": map[string]any{"state": "ready"}},
		"ready",
		nil,
	} {
		page.push(t, payload)
	}
	result := page.run(t, accountRuntimeReadyExpression(true))
	if result["state"] != "switching" || result["readyAfterNotReady"] != false {
		t.Fatalf("a foreign payload moved the observation: %#v", result)
	}
	if page.rpcCalls != 0 {
		t.Fatalf("a foreign payload triggered the models query: %d calls", page.rpcCalls)
	}
}

func TestAccountRuntimeProbeReportsAStaleOrAbsentWatch(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"

	// No watch: a transition cannot be answered at all, and the current-ready
	// path has to install one itself.
	if result := page.run(t, accountRuntimeReadyExpression(true)); result["installed"] != false ||
		result["reason"] != "missing" {
		t.Fatalf("unwatched transition read: %#v", result)
	}
	// A page without the official entry points is unsupported, not ready.
	bare := goja.New()
	if result := runOn(t, bare, accountRuntimeWatchExpression); result["ok"] != false {
		t.Fatalf("bare watch: %#v", result)
	}
	if result := runOn(t, bare, accountRuntimeReadyExpression(false)); result["installed"] != false ||
		result["reason"] != "unsupported" {
		t.Fatalf("bare read: %#v", result)
	}

	// A reload drops the global and with it the listener the old page held.
	page.run(t, accountRuntimeWatchExpression)
	if _, err := page.runtime.RunString("globalThis." + accountRuntimeProbeGlobal + "=null"); err != nil {
		t.Fatal(err)
	}
	if result := page.run(t, accountRuntimeReadyExpression(true)); result["installed"] != false {
		t.Fatalf("a stale watch answered: %#v", result)
	}
}

func TestAccountRuntimeProbeReplacesTheListenerAndStopsClean(t *testing.T) {
	page := newRuntimePage(t)
	page.current = "ready"
	page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"models": []any{}}}

	// Installing again retires the previous probe instead of leaving a second
	// listener that keeps observing after the watch was replaced.
	page.run(t, accountRuntimeWatchExpression)
	page.run(t, accountRuntimeWatchExpression)
	page.push(t, statusPush("switching"))
	if result := page.run(t, accountRuntimeReadyExpression(true)); result["state"] != "switching" {
		t.Fatalf("replaced probe: %#v", result)
	}

	if value, err := page.runtime.RunString(accountRuntimeStopExpression); err != nil || value.Export() != true {
		t.Fatalf("stop: %v %v", value, err)
	}
	if got, err := page.runtime.RunString("globalThis." + accountRuntimeProbeGlobal + " === null"); err != nil || got.Export() != true {
		t.Fatalf("the probe survived stop: %v %v", got, err)
	}
	// Stopping twice, or on a page that never watched, is not an error.
	if value, err := page.runtime.RunString(accountRuntimeStopExpression); err != nil || value.Export() != true {
		t.Fatalf("second stop: %v %v", value, err)
	}
	if result := page.run(t, accountRuntimeReadyExpression(false)); result["installed"] != false {
		t.Fatalf("a stopped probe answered: %#v", result)
	}
}

func TestAccountRuntimeProbeCompletesATransitionSeenOnlyByPolling(t *testing.T) {
	page := newRuntimePage(t)
	page.rpcResult = map[string]any{"ok": true, "result": map[string]any{"models": []any{}}}
	page.current = "ready"
	page.run(t, accountRuntimeWatchExpression)
	// A push the listener never received (a status set before the watch existed,
	// or a delivery that was dropped) must not hide the transition: the poll folds
	// its own getStatus answer into the same observation.
	page.current = "switching"
	if result := page.run(t, accountRuntimeReadyExpression(true)); result["state"] != "switching" {
		t.Fatalf("polling did not observe the runtime leaving ready: %#v", result)
	}
	page.current = "ready"
	result := page.run(t, accountRuntimeReadyExpression(true))
	if result["readyAfterNotReady"] != true || result["rpc"] != "ok" {
		t.Fatalf("a transition seen only by polling was not completed: %#v", result)
	}
	if page.rpcCalls != 1 {
		t.Fatalf("models queries: %d", page.rpcCalls)
	}
}

func TestAccountRuntimeExpressionsFitTheActionLimit(t *testing.T) {
	for name, expression := range map[string]string{
		"watch":      accountRuntimeWatchExpression,
		"transition": accountRuntimeReadyExpression(true),
		"current":    accountRuntimeReadyExpression(false),
		"stop":       accountRuntimeStopExpression,
	} {
		if len(expression) > maxActionExpression {
			t.Fatalf("%s: %d bytes over the %d limit", name, len(expression), maxActionExpression)
		}
	}
}

// ---------------------------------------------------------------------------
// the CDP half

// newCDPRecorderPipeWith wires a Pipe to a fake browser that answers the calls
// the account surface makes and lets one method answer from the test instead.
// The account_session tests own the shared fake; this one exists because that
// fake answers every Runtime.evaluate with a single fixed value.
func newCDPRecorderPipeWith(t *testing.T, recorder *cdpRecorder,
	answer func(method string, params map[string]any) (map[string]any, bool)) *Pipe {
	t.Helper()
	toHostReader, toHostWriter := io.Pipe()
	toPageReader, toPageWriter := io.Pipe()
	pipe := NewPipe(toHostReader, toPageWriter)

	handle := func(frame string) {
		var request struct {
			ID        int            `json:"id"`
			Method    string         `json:"method"`
			Params    map[string]any `json:"params"`
			SessionID string         `json:"sessionId"`
		}
		if json.Unmarshal([]byte(frame), &request) != nil {
			return
		}
		recorder.mu.Lock()
		recorder.record(request.Method, request.Params, request.SessionID)
		recorder.mu.Unlock()

		var result map[string]any
		if custom, ok := answer(request.Method, request.Params); ok {
			result = custom
		}
		if result == nil {
			failure, _ := json.Marshal(map[string]any{"id": request.ID,
				"error": map[string]any{"code": -32601, "message": "Method not found"}})
			_, _ = toHostWriter.Write(append(failure, 0))
			return
		}
		response := map[string]any{"id": request.ID, "result": result}
		if request.SessionID != "" {
			response["sessionId"] = request.SessionID
		}
		encoded, _ := json.Marshal(response)
		_, _ = toHostWriter.Write(append(encoded, 0))
	}
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
				handle(string(buffer[:at]))
				buffer = buffer[at+1:]
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { pipe.Dispose() })
	return pipe
}

// evaluateAnswers answers Runtime.evaluate from a queue, so one test can walk a
// page through several states in the order a switch produces them.
type evaluateAnswers struct {
	values []any
}

func (q *evaluateAnswers) next() map[string]any {
	value := any(nil)
	if len(q.values) > 0 {
		value = q.values[0]
		q.values = q.values[1:]
	}
	return map[string]any{"result": map[string]any{"type": "object", "value": value}}
}

// lastExpression is the expression of the most recent Runtime.evaluate, which is
// the one a test just triggered.
func (r *cdpRecorder) lastExpression() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.methods) - 1; i >= 0; i-- {
		if r.methods[i] != "Runtime.evaluate" {
			continue
		}
		expression, _ := r.params[i]["expression"].(string)
		return expression
	}
	return ""
}

func TestAccountRuntimeWatchAndStopUseThePageSession(t *testing.T) {
	holder := &cdpRecorder{}
	answers := &evaluateAnswers{values: []any{map[string]any{"ok": true}, true}}
	actions := NewCDPActions(newCDPRecorderPipeWith(t, holder,
		func(method string, params map[string]any) (map[string]any, bool) {
			if method != "Runtime.evaluate" {
				return nil, false
			}
			return answers.next(), true
		}), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	page := accountPage{targetID: "work", sessionID: "session-work", origin: "app://localhost"}

	if err := actions.accountWatchRuntime(ctx, page); err != nil {
		t.Fatal(err)
	}
	if got := holder.sessionFor("Runtime.evaluate"); got != "session-work" {
		t.Fatalf("the watch ran on session %q", got)
	}
	expression, _ := holder.find("Runtime.evaluate")["expression"].(string)
	if !strings.Contains(expression, "onEvent") || !strings.Contains(expression, accountRuntimeProbeGlobal) {
		t.Fatalf("the watch expression does not subscribe on the page: %s", expression)
	}
	// The page is evaluated with awaitPromise, because the probe is async.
	if ready, _ := holder.find("Runtime.evaluate")["awaitPromise"].(bool); !ready {
		t.Fatal("the evaluation does not await the promise")
	}

	if err := actions.accountStopRuntimeWatch(ctx, page); err != nil {
		t.Fatal(err)
	}
	if holder.count("Runtime.evaluate") != 2 {
		t.Fatalf("evaluations: %d", holder.count("Runtime.evaluate"))
	}
	stopExpression := holder.lastExpression()
	if strings.Contains(stopExpression, "onEvent") || !strings.Contains(stopExpression, "dispose") {
		t.Fatalf("the stop expression does not remove the listener: %s", stopExpression)
	}

	// A credential page is never driven by the runtime probe: its storage is the
	// account's token store, not the Work shell.
	if err := actions.accountWatchRuntime(ctx, accountPage{sessionID: "s", credentials: true}); err == nil ||
		err.Error() != AccountErrUnsupported {
		t.Fatalf("credential page: %v", err)
	}
	if err := actions.accountStopRuntimeWatch(ctx, accountPage{sessionID: "s", credentials: true}); err != nil {
		t.Fatalf("stopping on a credential page reported a failure: %v", err)
	}
	// A page without a session cannot be evaluated at all.
	if err := actions.accountWatchRuntime(ctx, accountPage{}); err == nil || err.Error() != AccountErrUnavailable {
		t.Fatalf("sessionless page: %v", err)
	}
	if ready, err := actions.accountRuntimeReady(ctx, accountPage{}, false); ready ||
		err == nil || err.Error() != AccountErrUnavailable {
		t.Fatalf("sessionless ready: %v %v", ready, err)
	}
}

func TestAccountRuntimeReadyRequiresTransitionAndQuery(t *testing.T) {
	for _, tt := range []struct {
		name      string
		answers   []any
		require   bool
		want      bool
		resultErr string
	}{
		{"ready-after-transition", []any{map[string]any{"installed": true, "state": "ready",
			"readyAfterNotReady": true, "current": "ready", "rpc": "ok"}}, true, true, ""},
		{"ready-without-transition", []any{map[string]any{"installed": true, "state": "ready",
			"readyAfterNotReady": false, "current": "ready", "rpc": "ok"}}, true, false, ""},
		{"current-ready", []any{map[string]any{"installed": true, "state": "ready",
			"readyAfterNotReady": false, "current": "ready", "rpc": "ok"}}, false, true, ""},
		{"push-only-ready", []any{map[string]any{"installed": true, "state": "ready",
			"readyAfterNotReady": true, "current": "disconnected", "rpc": "ok"}}, true, false, ""},
		{"query-failed", []any{map[string]any{"installed": true, "state": "ready",
			"readyAfterNotReady": true, "current": "ready", "rpc": "error"}}, true, false, ""},
		{"query-shape", []any{map[string]any{"installed": true, "state": "ready",
			"readyAfterNotReady": true, "current": "ready", "rpc": "shape"}}, true, false, ""},
		{"query-timed-out", []any{map[string]any{"installed": true, "state": "ready",
			"readyAfterNotReady": true, "current": "ready", "rpc": "timeout"}}, true, false, ""},
		{"not-ready", []any{map[string]any{"installed": true, "state": "switching"}}, true, false, ""},
		{"unsupported", []any{map[string]any{"installed": false, "reason": "unsupported"}},
			true, false, AccountErrUnsupported},
		{"reloaded-transition", []any{map[string]any{"installed": false, "reason": "missing"}},
			true, false, AccountErrPageTimeout},
		{"reinstalled-current", []any{
			map[string]any{"installed": false, "reason": "missing"},
			map[string]any{"ok": true},
			map[string]any{"installed": true, "state": "ready", "current": "ready", "rpc": "ok"},
		}, false, true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			holder := &cdpRecorder{}
			answers := &evaluateAnswers{values: append([]any(nil), tt.answers...)}
			actions := NewCDPActions(newCDPRecorderPipeWith(t, holder,
				func(method string, params map[string]any) (map[string]any, bool) {
					if method != "Runtime.evaluate" {
						return nil, false
					}
					return answers.next(), true
				}), t.TempDir(), t.TempDir()).(*cdpActions)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ready, err := actions.accountRuntimeReady(ctx,
				accountPage{targetID: "work", sessionID: "session-work"}, tt.require)
			if ready != tt.want {
				t.Fatalf("ready: %v, want %v (err %v)", ready, tt.want, err)
			}
			switch {
			case tt.resultErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.resultErr != "" && (err == nil || err.Error() != tt.resultErr):
				t.Fatalf("error: %v, want %s", err, tt.resultErr)
			}
		})
	}
}

func TestAccountRuntimeReadyTreatsAnUnreadablePageAsFailure(t *testing.T) {
	holder := &cdpRecorder{}
	actions := NewCDPActions(newCDPRecorderPipeWith(t, holder,
		func(method string, params map[string]any) (map[string]any, bool) {
			// A Runtime.evaluate that answers without a value is a failed readback,
			// not a not-ready runtime: it must never be reported as success.
			if method != "Runtime.evaluate" {
				return nil, false
			}
			return map[string]any{"result": map[string]any{"type": "object"}}, true
		}), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ready, err := actions.accountRuntimeReady(ctx, accountPage{sessionID: "session-work"}, false)
	if ready || err == nil || err.Error() != AccountErrPageTimeout {
		t.Fatalf("valueless read: ready=%v err=%v", ready, err)
	}

	// A protocol error on the evaluation is a lost page, not a not-ready one.
	broken := NewCDPActions(newCDPRecorderPipeWith(t, &cdpRecorder{},
		func(method string, params map[string]any) (map[string]any, bool) {
			return nil, false
		}), t.TempDir(), t.TempDir()).(*cdpActions)
	if err := broken.accountWatchRuntime(ctx, accountPage{sessionID: "session-work"}); err == nil ||
		err.Error() != AccountErrPageTimeout {
		t.Fatalf("failed evaluation: %v", err)
	}
}
