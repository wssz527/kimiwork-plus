package kimi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestControllerTranslationRoundTrip drives the host with the real payload and a
// caller-supplied provider, and asserts the answer reaches the binding reply.
func TestControllerTranslationRoundTrip(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)

	var mu sync.Mutex
	var seen []TranslationReply
	var host *Host
	host = NewHost(harness.pipe, InjectionOptions{
		Payload:  testPayload,
		Revision: "rev-1",
		Settings: map[string]any{"enabled": true, "provider": "bing", "targetLanguage": "zh"},
		OnReply: func(_ context.Context, session *SurfaceSession, generation int, id string, value any) {
			mu.Lock()
			seen = append(seen, TranslationReply{SessionID: session.SessionID, Surface: session.Surface,
				Generation: generation, ID: id, Value: value})
			mu.Unlock()
			_ = host.Reply(context.Background(), session, generation, id, map[string]any{"text": "译文"})
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) == 0 {
		t.Fatal("no sessions attached")
	}
	session := sessions[0]

	// A binding call from the pinned world must reach the handler and the reply
	// must be evaluated in that same world.
	harness.pipe.dispatch(`{"method":"Runtime.bindingCalled","sessionId":"` + session.SessionID +
		`","params":{"name":"__kwgTranslate","payload":"{\"id\":\"7\",\"value\":{\"text\":\"hello\"}}",` +
		`"executionContextId":` + itoa(session.ContextID()) + `}}`)
	eventually(t, "the provider to be called", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 1
	})
	mu.Lock()
	reply := seen[0]
	mu.Unlock()
	if reply.ID != "7" || reply.Surface != "work" || reply.Generation != session.Generation() {
		t.Fatalf("reply: %+v", reply)
	}
	eventually(t, "the answer to be written back", func() bool {
		for _, expression := range page.snapshotEvaluations() {
			if containsText(expression, "__kwgTranslationReceive") && containsText(expression, "译文") {
				return true
			}
		}
		return false
	})
}

func (p *fakePage) snapshotInjections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.injections
}

func (p *fakePage) snapshotEvaluations() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.evaluations...)
}

func containsText(haystack, needle string) bool {
	for at := 0; at+len(needle) <= len(haystack); at++ {
		if haystack[at:at+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestControllerReconcileAgainstMockKimi runs the full reconcile against a
// synthetic Kimi home and checks the result the panel would render.
func TestControllerReconcileAgainstMockKimi(t *testing.T) {
	base := t.TempDir()
	// The Work data directory follows Kimi's own layout: without a
	// daimon-storage.json shareDir, it is <profile>/daimon-share/daimon.
	kimiHome := filepath.Join(base, "profile", "daimon-share", "daimon")
	if err := os.MkdirAll(kimiHome, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := map[string]any{
		"kind": "kth", "id": "byok-kth", "baseUrl": "http://127.0.0.1:9999/v1", "credentialRef": "cred-1",
		"models": []any{map[string]any{"alias": "byok-kth-kimi/kimi-probe-model", "providerId": "byok-kth-kimi",
			"model": "kimi-probe-model", "family": "kimi", "maxContextSize": 131072}},
	}
	write := func(name string, value any) {
		encoded, _ := json.MarshalIndent(value, "", "  ")
		if err := os.WriteFile(filepath.Join(kimiHome, name), append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(NativeWorkStore, map[string]any{"v": 1,
		"profiles": map[string]any{"key-1": map[string]any{"byok-kth": profile}}})
	write(NativeWorkConfig, map[string]any{
		"model":       map[string]any{"providers": map[string]any{}, "models": map[string]any{}},
		"credentials": map[string]any{"cred-1": map[string]any{"apiKey": "synthetic"}},
	})

	controller, err := NewController(ControllerSettings{
		StateDir: t.TempDir(), UserDataDir: filepath.Join(base, "profile"),
		GatewayURL: "http://127.0.0.1:9999/v1", GatewayToken: "token",
		Models: []Model{{ID: "kimi-probe-model", Protocol: "chat", ContextWindow: 131072}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := controller.Reconcile()
	if result.State != IntegrationRepaired {
		t.Fatalf("reconcile: %s/%s", result.State, result.Reason)
	}
	status := controller.Status(context.Background())
	if status.Integration == nil || status.Integration.State != IntegrationRepaired {
		t.Fatalf("status did not record the reconcile: %+v", status.Integration)
	}
}

// TestManagedChildSpeaksCDPOverPrivatePipe starts a real child process of this
// test binary with -remote-debugging-pipe and drives it through the production
// launch path: fd3 commands, fd4 responses, the isolated profile, and the host
// attaching to the Work surface.
//
// The child is a stand-in, not Kimi. What this proves is the transport and the
// injection contract; what it cannot prove is any behaviour of the official
// build, which is recorded as a separate evidence level.
func TestManagedChildSpeaksCDPOverPrivatePipe(t *testing.T) {
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "profile")
	child, err := StartKimi(context.Background(), LaunchOptions{
		Installation: Installation{Application: executable},
		UserDataDir:  profile,
		ExtraArgs:    []string{fakeCDPFlag},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Stop(3 * time.Second) }()

	info, err := os.Stat(profile)
	if err != nil || !info.IsDir() {
		t.Fatalf("isolated profile was not created: %v", err)
	}

	host := NewHost(child.Pipe(), InjectionOptions{Payload: testPayload, Revision: "rev-1",
		Settings: map[string]any{"enabled": true, "provider": "bing", "targetLanguage": "zh"}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil {
		t.Fatalf("sync over the private pipe: %v", err)
	}
	// Two surfaces are legitimate: the Work page and the remote Chat page. The
	// settings overlay is not a translation surface and must never be attached.
	if len(sessions) != 2 {
		t.Fatalf("attached %d sessions, want 2 (work + chat)", len(sessions))
	}
	surfaces := map[string]bool{}
	for _, session := range sessions {
		surfaces[session.Surface] = true
		if session.TargetID == "settings" {
			t.Fatal("the settings page was attached")
		}
		if session.State() != "active" {
			t.Fatalf("state for %s: %q", session.Surface, session.State())
		}
	}
	if !surfaces["work"] || !surfaces["chat"] {
		t.Fatalf("surfaces: %v", surfaces)
	}
}

// TestManagedChildCDPActionsImport is the wiring proof the earlier round was
// missing: the production PageActions run over a real private pipe against a
// child that models the preload API surface.
//
// The child is a stand-in, so this proves the calls, the ordering and the
// account gate — not that the official build exposes the same API.
func TestManagedChildCDPActionsImport(t *testing.T) {
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	profile := filepath.Join(base, "profile")
	kimiHome := filepath.Join(profile, "daimon-share", "daimon")
	if err := os.MkdirAll(kimiHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// The account the import is expected to land in.
	config, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(kimiHome, NativeWorkConfig), config, 0o600); err != nil {
		t.Fatal(err)
	}

	child, err := StartKimi(context.Background(), LaunchOptions{
		Installation: Installation{Application: executable},
		UserDataDir:  profile,
		ExtraArgs:    []string{fakeCDPFlag},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Stop(3 * time.Second) }()

	actions := NewCDPActions(child.Pipe(), kimiHome, kimiHome)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	surface, err := actions.WorkSurface(ctx)
	if err != nil {
		t.Fatalf("work surface: %v", err)
	}
	if !surface.Work || !surface.Rendered {
		t.Fatalf("surface: %+v", surface)
	}
	// The remote chat page is not a desktop page and must not be read.
	for _, shown := range surface.Shown {
		if shown["path"] == nil {
			t.Fatal("a non-desktop target was read")
		}
	}

	result, err := actions.ImportModels(ctx, ImportRequest{
		URL: "http://127.0.0.1:9999/v1", Token: "gateway-token",
		ExpectedAccountKey: sha256Hex("user-1"), OpenSettings: false,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !result.OK || result.Models != 3 || !result.RequiresRestart {
		t.Fatalf("import result: %+v", result)
	}

	closed, err := actions.CloseWorkSettings(ctx)
	if err != nil || !closed.OK {
		t.Fatalf("close: %+v %v", closed, err)
	}
	activated, err := actions.ActivateWork(ctx)
	if err != nil || !activated.OK {
		t.Fatalf("activate: %+v %v", activated, err)
	}
}
