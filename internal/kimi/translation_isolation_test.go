package kimi

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTranslationRuntimeEnableDeliversReplyAndDisableWithdrawsScripts(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)

	profile := filepath.Join(t.TempDir(), "profile")
	controller, err := NewController(ControllerSettings{
		StateDir:    filepath.Dir(profile),
		UserDataDir: profile,
		Translation: TranslationSettings{Enabled: false, Provider: "bing", TargetLanguage: "zh", View: "translated"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var replyMu sync.Mutex
	replyCalled := make(chan string, 1)
	controller.SetTranslationHandler(func(_ context.Context, reply TranslationReply) (any, error) {
		replyMu.Lock()
		defer replyMu.Unlock()
		select {
		case replyCalled <- reply.ID:
		default:
		}
		return map[string]any{"id": reply.ID, "text": "translated"}, nil
	})

	child := &Child{pipe: harness.pipe, exitWait: make(chan struct{})}
	controller.mu.Lock()
	controller.child = child
	controller.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := controller.attachChild(ctx, child, TranslationSettings{Enabled: false, Provider: "bing", TargetLanguage: "zh", View: "translated"}); err != nil {
		t.Fatalf("attachChild disabled: %v", err)
	}
	t.Cleanup(func() {
		controller.mu.Lock()
		bridge := controller.bridge
		controller.bridge = nil
		controller.child = nil
		controller.host = nil
		controller.mu.Unlock()
		if bridge != nil {
			_ = bridge.Close()
		}
	})

	// Enable translation at runtime without restarting the child.
	next := controller.Settings()
	next.Translation.Enabled = true
	if err := controller.Configure(next); err != nil {
		t.Fatalf("enable translation: %v", err)
	}
	status := controller.Tick(ctx)
	if workSurface := status.Surfaces["work"]; workSurface.State != "active" {
		t.Fatalf("work surface after runtime enable: %+v", status)
	}

	controller.mu.Lock()
	host := controller.host
	controller.mu.Unlock()
	host.mu.Lock()
	var session *SurfaceSession
	for _, item := range host.sessions {
		if item.Surface == "work" {
			session = item
			break
		}
	}
	host.mu.Unlock()
	if session == nil {
		t.Fatal("work session not attached after runtime enable")
	}

	harness.pipe.dispatch(`{"method":"Runtime.bindingCalled","sessionId":"` + session.SessionID +
		`","params":{"name":"__kwgTranslate","payload":"{\"id\":\"req-1\",\"value\":\"hello\"}","executionContextId":` +
		itoa(session.ContextID()) + `}}`)

	select {
	case id := <-replyCalled:
		if id != "req-1" {
			t.Fatalf("reply id = %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("translation handler was not called")
	}
	eventually(t, "host.Reply to evaluate __kwgTranslationReceive", func() bool {
		for _, expr := range page.snapshotEvaluations() {
			if strings.Contains(expr, "__kwgTranslationReceive") && strings.Contains(expr, "req-1") {
				return true
			}
		}
		return false
	})

	// Disable translation at runtime: must send configure({enabled:false}) and remove new-document script.
	next.Translation.Enabled = false
	if err := controller.Configure(next); err != nil {
		t.Fatalf("disable translation: %v", err)
	}
	page.mu.Lock()
	removals := page.removals
	detaches := page.detachCalls
	page.mu.Unlock()
	if removals == 0 || detaches == 0 {
		t.Fatalf("disable did not withdraw scripts/sessions: removals=%d detaches=%d", removals, detaches)
	}
	foundDisable := false
	for _, expr := range page.snapshotEvaluations() {
		if strings.Contains(expr, "__kwgTranslation?.configure(") && strings.Contains(expr, `"enabled":false`) {
			foundDisable = true
			break
		}
	}
	if !foundDisable {
		t.Fatal("disable did not send configure({enabled:false}) to page")
	}
	if host.HasSessions() {
		t.Fatal("host still holds sessions after disable")
	}
}

func TestAttachChildSyncFailureDoesNotKillChild(t *testing.T) {
	child, profile := startFakeChild(t)
	controller, err := NewController(ControllerSettings{
		StateDir:    filepath.Dir(profile),
		UserDataDir: profile,
		Translation: TranslationSettings{Enabled: true, Provider: "bing", TargetLanguage: "zh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controller.attachChild(cancelled, child, TranslationSettings{Enabled: true, Provider: "bing", TargetLanguage: "zh"}); err != nil {
		t.Fatalf("attachChild sync failure returned error: %v", err)
	}
	t.Cleanup(func() { _ = controller.Stop(context.Background()) })

	select {
	case <-child.Exit():
		t.Fatal("host.Sync failure killed the managed child")
	default:
	}
	state := controller.PublicState(context.Background())
	if state.State != StateReady {
		t.Fatalf("state = %s, want ready", state.State)
	}
	if state.Bridge == nil || !state.Bridge.Published {
		t.Fatalf("bridge not published: %+v", state.Bridge)
	}
	if state.Translation == nil || state.Translation.State != "failed" {
		t.Fatalf("translation = %+v", state.Translation)
	}
}

func TestDeliverBindingRunsAsynchronouslyWithoutBlockingEventLoop(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	host := NewHost(harness.pipe, InjectionOptions{
		Payload:  testPayload,
		Revision: "rev-1",
		OnReply: func(context.Context, *SurfaceSession, int, string, any) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-block
		},
	})
	defer close(block)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("sync: %v", err)
	}
	session := sessions[0]

	harness.pipe.dispatch(`{"method":"Runtime.bindingCalled","sessionId":"` + session.SessionID +
		`","params":{"name":"__kwgTranslate","payload":"{\"id\":\"slow-1\",\"value\":\"hi\"}","executionContextId":` +
		itoa(session.ContextID()) + `}}`)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("onReply did not start")
	}

	// While onReply is blocked, eventLoop must continue handling navigation events.
	harness.pipe.dispatch(`{"method":"Page.navigatedWithinDocument","sessionId":"` + session.SessionID + `"}`)
	eventually(t, "eventLoop to process navigation while onReply is blocked", func() bool {
		return session.State() == ""
	})
}

func TestDirectCatalogAndSelectAccountNeverFallbackAcrossAccounts(t *testing.T) {
	spec := directSpecFixture()
	accountA := sha256Hex("user-a")
	accountB := sha256Hex("user-b")
	fixture := seedDirectFixture(t, accountA)
	integration := fixture.integration(t)

	// Confirm account A works via directCatalog with single-hashed scope.
	entries, reason, needsImport := integration.directCatalog(ControllerSettings{
		IntegrationMode: IntegrationModeDirect,
		DirectSpec:      spec,
	}, accountA)
	if reason != "" || needsImport || len(entries) != 1 {
		t.Fatalf("account A directCatalog: entries=%d reason=%q needsImport=%v", len(entries), reason, needsImport)
	}

	// Account B has no entry in the store: directCatalog must NOT fall back to account A.
	entries, reason, needsImport = integration.directCatalog(ControllerSettings{
		IntegrationMode: IntegrationModeDirect,
		DirectSpec:      spec,
	}, accountB)
	if len(entries) != 0 || reason != "profile_missing" || !needsImport {
		t.Fatalf("account B leaked account A in directCatalog: entries=%d reason=%q needsImport=%v", len(entries), reason, needsImport)
	}

	// ReadbackMode for account B must NOT inherit account A's direct source or aliases.
	rb := integration.ReadbackMode(accountB, IntegrationModeDirect, "")
	if rb.DirectSourceID != "" || rb.Models != 0 || rb.HoldsDirect(spec) {
		t.Fatalf("account B direct readback leaked account A: %+v", rb)
	}

	// Gateway Readback for account B must NOT inherit account A's profile.
	gwFixture := newNativeFixture(t)
	gwKey := gwFixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	gw := gwFixture.integration(t)
	if res := gw.Reconcile(ReconcileOptions{Enabled: true, AccountScope: gwKey, URL: "http://127.0.0.1:9999/v1", Models: testCatalog()}); res.State != IntegrationRepaired {
		t.Fatalf("seed gateway reconcile: %+v", res)
	}
	otherRB := gw.Readback(sha256Hex("other-user"))
	if otherRB.Models != 0 || otherRB.URL != "" || otherRB.Active || otherRB.Holds("http://127.0.0.1:9999/v1") {
		t.Fatalf("gateway readback leaked another account: %+v", otherRB)
	}
}

func TestIntegrationResultAccountDigestHashLevelIsConsistent(t *testing.T) {
	profile, home := nativeProfile(t, "user-1")
	seedNativeProfile(t, home, "http://127.0.0.1:9999/v1", "cred-1")
	controller, err := NewController(ControllerSettings{
		StateDir:    filepath.Dir(profile),
		UserDataDir: profile,
		GatewayURL:  "http://127.0.0.1:9999/v1",
		Models:      testCatalog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantScope := sha256Hex("user-1")
	wantScopeDigest := sha256Hex(wantScope)

	withoutRB := controller.Reconcile()
	withRB := controller.ReconcileWithReadback(context.Background())

	if withoutRB.AccountDigest != wantScopeDigest {
		t.Fatalf("Reconcile().AccountDigest = %q, want %q", withoutRB.AccountDigest, wantScopeDigest)
	}
	if withRB.AccountDigest != wantScopeDigest {
		t.Fatalf("ReconcileWithReadback().AccountDigest = %q, want %q", withRB.AccountDigest, wantScopeDigest)
	}
	if withRB.Readback == nil || withRB.Readback.AccountDigest != wantScope {
		t.Fatalf("Readback.AccountDigest = %+v, want %q", withRB.Readback, wantScope)
	}
}

func TestDetachAllStopsEventLoopAndDoesNotConsumePipeEvents(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)

	oldHost := NewHost(harness.pipe, InjectionOptions{
		Payload:  testPayload,
		Revision: "rev-old",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oldHost.DetachAll(ctx)

	select {
	case <-oldHost.loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("oldHost eventLoop did not exit after DetachAll")
	}
	// The retired host also refuses every later sync and accepts a second detach
	// without touching the stop channel twice.
	if _, err := oldHost.Sync(ctx); err == nil {
		t.Fatal("a detached host still synced")
	}
	oldHost.DetachAll(ctx)
	select {
	case <-oldHost.loopDone:
	default:
		t.Fatal("loopDone must stay closed")
	}

	// Pipe stays open: a new host sharing it must receive every subsequent event.
	// The events are dispatched one at a time: the reply path only runs eight
	// handlers at once and drops the rest by design, so a burst would not prove
	// anything about the detached host.
	received := make(chan string, 1)
	newHost := NewHost(harness.pipe, InjectionOptions{
		Payload:  testPayload,
		Revision: "rev-new",
		OnReply: func(_ context.Context, _ *SurfaceSession, _ int, id string, _ any) {
			received <- id
		},
	})
	defer newHost.DetachAll(ctx)

	sessions, err := newHost.Sync(ctx)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("newHost.Sync: %v", err)
	}
	session := sessions[0]
	const total = 32
	for i := range total {
		id := "ev-" + itoa(i)
		harness.pipe.dispatch(`{"method":"Runtime.bindingCalled","sessionId":"` + session.SessionID +
			`","params":{"name":"__kwgTranslate","payload":"{\"id\":\"` + id + `\",\"value\":\"x\"}","executionContextId":` +
			itoa(session.ContextID()) + `}}`)
		select {
		case got := <-received:
			if got != id {
				t.Fatalf("event %d answered %q, want %q", i, got, id)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("event %d was never delivered; the detached host is still consuming pipe events", i)
		}
	}
}

func TestControllerTickConcurrentLazyHostCreatesSingleHost(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)

	profile := filepath.Join(t.TempDir(), "profile")
	controller, err := NewController(ControllerSettings{
		StateDir:    filepath.Dir(profile),
		UserDataDir: profile,
		Translation: TranslationSettings{Enabled: true, Provider: "bing", TargetLanguage: "zh", View: "translated"},
	})
	if err != nil {
		t.Fatal(err)
	}
	child := &Child{pipe: harness.pipe, exitWait: make(chan struct{})}
	controller.mu.Lock()
	controller.child = child
	controller.mu.Unlock()
	t.Cleanup(func() {
		controller.mu.Lock()
		host := controller.host
		controller.host = nil
		controller.child = nil
		controller.mu.Unlock()
		if host != nil {
			host.DetachAll(context.Background())
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = controller.Tick(ctx)
		}()
	}
	wg.Wait()

	controller.mu.Lock()
	host := controller.host
	controller.mu.Unlock()
	if host == nil {
		t.Fatal("expected controller.host to be initialized")
	}
	// A later Tick must reuse that host: a second construction would leave the
	// first one with a live event loop reading the same pipe.
	if status := controller.Tick(ctx); status.State == "active" || status.State == "attaching" {
		controller.mu.Lock()
		again := controller.host
		controller.mu.Unlock()
		if again != host {
			t.Fatal("Tick replaced an existing host")
		}
	}
}
