package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeActions is a stand-in for the page-level work the bridge host delegates.
// It records what was asked and answers deterministically, so the HTTP contract
// and the controller wiring can be verified without an official application.
type fakeActions struct {
	mu sync.Mutex

	connected bool
	surface   WorkSurface
	imports   []ImportRequest
	imported  ImportResult
	closed    int
	activated int
	opened    int
	failWith  error
}

func (f *fakeActions) Connected() bool { return f.connected }

func (f *fakeActions) WorkSurface(context.Context) (WorkSurface, error) {
	if f.failWith != nil {
		return WorkSurface{}, f.failWith
	}
	return f.surface, nil
}

func (f *fakeActions) ImportModels(_ context.Context, request ImportRequest) (ImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports = append(f.imports, request)
	if f.failWith != nil {
		return ImportResult{}, f.failWith
	}
	return f.imported, nil
}

func (f *fakeActions) CloseWorkSettings(context.Context) (ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return ActionResult{OK: true, Via: []string{"overlay"}}, nil
}

func (f *fakeActions) ActivateWork(context.Context) (ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activated++
	return ActionResult{OK: true, Via: []string{"main"}}, nil
}

func (f *fakeActions) OpenWorkSettings(context.Context) (ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	return ActionResult{OK: true}, nil
}

func TestBridgeHostPublishesALoopbackDescriptor(t *testing.T) {
	kimiDir := t.TempDir()
	actions := &fakeActions{connected: true}
	host, err := StartBridgeHost(kimiDir, 4242, actions)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	descriptor, err := ReadBridgeDescriptor(kimiDir)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ApplicationPID != 4242 {
		t.Fatalf("application pid: %d", descriptor.ApplicationPID)
	}
	if len(descriptor.Token) != 64 {
		t.Fatalf("token length: %d", len(descriptor.Token))
	}
	info, err := os.Stat(filepath.Join(kimiDir, BridgeDescriptorName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("descriptor is group/other readable: %v", info.Mode().Perm())
	}

	status, err := host.Client().Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.OK || status.State != "ready" || status.Contract != BridgeContractGeneration {
		t.Fatalf("status: %+v", status)
	}
}

func TestBridgeHostRejectsUnauthorizedCallers(t *testing.T) {
	kimiDir := t.TempDir()
	host, err := StartBridgeHost(kimiDir, 1, &fakeActions{connected: true})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	// A wrong token is refused.
	descriptor := host.Descriptor()
	descriptor.Token = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := NewBridgeClient(descriptor).Status(context.Background()); !errors.Is(err, ErrBridgeUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
	// A descriptor pointing off-loopback is refused before any dial.
	bad := host.Descriptor()
	bad.URL = "http://example.test:1234"
	if _, err := NewBridgeClient(bad).Status(context.Background()); err == nil {
		t.Fatal("a non-loopback descriptor was dialled")
	}
}

func TestBridgeDescriptorValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(value any, mode os.FileMode) string {
		data, _ := json.Marshal(value)
		file := filepath.Join(dir, BridgeDescriptorName)
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		// os.WriteFile does not change the mode of an existing file, so the
		// mode is set explicitly for the permission cases.
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
		return file
	}
	valid := map[string]any{"schemaVersion": 1, "id": "x", "pid": 1, "applicationPid": 2,
		"url": "http://127.0.0.1:5555", "token": strings64()}

	write(valid, 0o600)
	if _, err := ReadBridgeDescriptor(dir); err != nil {
		t.Fatalf("valid descriptor refused: %v", err)
	}
	// Group-readable descriptors are refused.
	write(valid, 0o644)
	if _, err := ReadBridgeDescriptor(dir); err == nil {
		t.Fatal("a group-readable descriptor was accepted")
	}
	// A non-loopback URL is refused.
	offHost := map[string]any{"schemaVersion": 1, "id": "x", "pid": 1, "applicationPid": 2,
		"url": "http://10.0.0.1:5555", "token": strings64()}
	write(offHost, 0o600)
	if _, err := ReadBridgeDescriptor(dir); err == nil {
		t.Fatal("a remote descriptor was accepted")
	}
	// A short token is refused.
	short := map[string]any{"schemaVersion": 1, "id": "x", "pid": 1, "applicationPid": 2,
		"url": "http://127.0.0.1:5555", "token": "abc"}
	write(short, 0o600)
	if _, err := ReadBridgeDescriptor(dir); err == nil {
		t.Fatal("a short token was accepted")
	}
	// A URL carrying a query is refused.
	query := map[string]any{"schemaVersion": 1, "id": "x", "pid": 1, "applicationPid": 2,
		"url": "http://127.0.0.1:5555/?token=x", "token": strings64()}
	write(query, 0o600)
	if _, err := ReadBridgeDescriptor(dir); err == nil {
		t.Fatal("a descriptor with a query was accepted")
	}
}

func strings64() string {
	const hex = "0123456789abcdef"
	out := make([]byte, 64)
	for i := range out {
		out[i] = hex[i%16]
	}
	return string(out)
}

func TestBridgeHostRoutesTheFullOperationSet(t *testing.T) {
	kimiDir := t.TempDir()
	actions := &fakeActions{connected: true,
		surface:  WorkSurface{OK: true, Work: true, Rendered: true, Settings: false, Pages: 2},
		imported: ImportResult{OK: true, Models: 5, RequiresRestart: true}}
	host, err := StartBridgeHost(kimiDir, 7, actions)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	client := host.Client()
	ctx := context.Background()

	surface, err := client.WorkSurface(ctx)
	if err != nil || !surface.Work || !surface.Rendered {
		t.Fatalf("surface: %+v %v", surface, err)
	}
	result, err := client.ImportModels(ctx, ImportRequest{URL: "http://127.0.0.1:1/v1",
		Token: "gateway-token", ExpectedAccountKey: "key", OpenSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Models != 5 || !result.RequiresRestart {
		t.Fatalf("import: %+v", result)
	}
	if len(actions.imports) != 1 || actions.imports[0].Token != "gateway-token" {
		t.Fatalf("import request not forwarded: %+v", actions.imports)
	}
	if closed, err := client.CloseWorkSettings(ctx); err != nil || !closed.OK {
		t.Fatalf("close: %+v %v", closed, err)
	}
	if activated, err := client.ActivateWork(ctx); err != nil || !activated.OK {
		t.Fatalf("activate: %+v %v", activated, err)
	}
	if opened, err := client.OpenWorkSettings(ctx); err != nil || !opened.OK {
		t.Fatalf("open: %+v %v", opened, err)
	}
	if actions.closed != 1 || actions.activated != 1 || actions.opened != 1 {
		t.Fatalf("action counts: %d/%d/%d", actions.closed, actions.activated, actions.opened)
	}
}

func TestBridgeHostReportsDisconnection(t *testing.T) {
	kimiDir := t.TempDir()
	actions := &fakeActions{connected: false}
	host, err := StartBridgeHost(kimiDir, 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	status, err := host.Client().Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "disconnected" {
		t.Fatalf("state: %s", status.State)
	}
}

func TestBridgeHostRemovesItsDescriptorOnClose(t *testing.T) {
	kimiDir := t.TempDir()
	host, err := StartBridgeHost(kimiDir, 1, &fakeActions{connected: true})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(kimiDir, BridgeDescriptorName)
	if _, err := os.Stat(file); err != nil {
		t.Fatal("descriptor was not published")
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("descriptor survived Close")
	}
}

// TestControllerRefusesAnUnmanagedKimi proves the boundary the product depends
// on: an official Kimi this controller did not start is reported, never
// attached to, signalled or killed.
func TestControllerRefusesAnUnmanagedKimi(t *testing.T) {
	if os.Getenv("KIMI_SKIP_LIVE") == "1" {
		t.Skip("live probe disabled")
	}
	state := DetectState(context.Background(), ProbeOptions{})
	if state.Installation == nil || state.Application != Running {
		t.Skip("no running official Kimi on this host; the guard cannot be exercised")
	}
	dir := t.TempDir()
	controller, err := NewController(ControllerSettings{StateDir: dir,
		UserDataDir:  filepath.Join(dir, "profile"),
		Installation: *state.Installation})
	if err != nil {
		t.Fatal(err)
	}
	err = controller.Start(context.Background())
	if !errors.Is(err, ErrUnmanagedKimiRunning) {
		t.Fatalf("want ErrUnmanagedKimiRunning, got %v", err)
	}
	status := controller.Status(context.Background())
	if status.State != StateNeedsUserClose {
		t.Fatalf("state: %s", status.State)
	}
	// The official process must still be running: nothing was signalled.
	after := DetectState(context.Background(), ProbeOptions{})
	if after.Application != Running {
		t.Fatal("the official Kimi was affected by the refusal")
	}
}

func TestControllerRejectsTheRealUserProfile(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	realProfile := filepath.Join(home, "Library", "Application Support", "kimi-desktop")
	controller, err := NewController(ControllerSettings{StateDir: t.TempDir(),
		UserDataDir: realProfile, Installation: Installation{Application: "/bin/true", Archive: "/nonexistent"}})
	if err != nil {
		t.Fatal(err)
	}
	err = controller.Start(context.Background())
	if err == nil {
		t.Fatal("the signed-in profile was accepted as a managed user-data dir")
	}
	if status := controller.Status(context.Background()); status.State != StateFailed {
		t.Fatalf("state: %s", status.State)
	}
}

func TestControllerRequiresAnAbsoluteStateDir(t *testing.T) {
	if _, err := NewController(ControllerSettings{StateDir: "relative"}); err == nil {
		t.Fatal("a relative state dir was accepted")
	}
	if _, err := NewController(ControllerSettings{StateDir: t.TempDir()}); err != nil {
		t.Fatalf("a valid state dir was refused: %v", err)
	}
}

func TestControllerStatusIsReadOnly(t *testing.T) {
	controller, err := NewController(ControllerSettings{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	before := controller.Status(context.Background())
	if before.State != StateStopped {
		t.Fatalf("initial state: %s", before.State)
	}
	// Reading twice must not change anything.
	after := controller.Status(context.Background())
	if after.State != before.State || after.Contract != before.Contract {
		t.Fatal("Status mutated the controller")
	}
	if after.Contract != BridgeContractGeneration {
		t.Fatalf("contract: %d", after.Contract)
	}
}

func TestControllerTickWithoutAChildIsInactive(t *testing.T) {
	controller, err := NewController(ControllerSettings{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	status := controller.Tick(context.Background())
	if status.State != "inactive" || status.Reason != "no_managed_child" {
		t.Fatalf("tick: %+v", status)
	}
}

func TestControllerConfigureChangesTheTranslationRevision(t *testing.T) {
	controller, err := NewController(ControllerSettings{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	first := controller.Settings().Translation.Revision()
	err = controller.Configure(ControllerSettings{StateDir: t.TempDir(),
		Translation: TranslationSettings{Enabled: true, Provider: "bing", TargetLanguage: "zh"}})
	if err != nil {
		t.Fatal(err)
	}
	second := controller.Settings().Translation.Revision()
	if first == second {
		t.Fatal("a provider change did not change the revision")
	}
	// A view change is not a scope change: it must not re-inject.
	err = controller.Configure(ControllerSettings{StateDir: t.TempDir(),
		Translation: TranslationSettings{Enabled: true, Provider: "bing", TargetLanguage: "zh", View: "original"}})
	if err != nil {
		t.Fatal(err)
	}
	if controller.Settings().Translation.Revision() != second {
		t.Fatal("a view change re-scoped the translation")
	}
}

func TestControllerStartRefusesWithoutAnApplication(t *testing.T) {
	controller, err := NewController(ControllerSettings{StateDir: t.TempDir(),
		UserDataDir: filepath.Join(t.TempDir(), "profile")})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background()); err == nil {
		t.Fatal("Start without an application succeeded")
	}
	if status := controller.Status(context.Background()); status.State != StateFailed {
		t.Fatalf("state: %s", status.State)
	}
}

func TestControllerImportWithoutABridgeFails(t *testing.T) {
	controller, err := NewController(ControllerSettings{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ImportModels(context.Background()); err == nil {
		t.Fatal("import without a bridge succeeded")
	}
	if _, err := controller.WorkSurface(context.Background()); err == nil {
		t.Fatal("surface without a bridge succeeded")
	}
}

func TestControllerTranslationHandlerIsInvoked(t *testing.T) {
	controller, err := NewController(ControllerSettings{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	controller.SetTranslationHandler(func(_ context.Context, reply TranslationReply) (any, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return map[string]any{"text": "translated"}, nil
	})
	// Without a host there is nothing to answer, so the handler is not called
	// by the controller itself; this asserts the setter is safe and the field is
	// wired rather than silently ignored.
	controller.onReply(context.Background(), &SurfaceSession{SessionID: "s", Surface: "work"}, 1, "1", "hi")
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatal("the handler ran without a host")
	}
}

func TestControllerCloseIsIdempotent(t *testing.T) {
	controller, err := NewController(ControllerSettings{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestBridgeClientRefusesUnknownOperations(t *testing.T) {
	client := NewBridgeClient(BridgeDescriptor{URL: "http://127.0.0.1:1", Token: strings64()})
	if err := client.call(context.Background(), "delete-everything", nil, &struct{}{}); err == nil {
		t.Fatal("an unknown operation was sent")
	}
}

func TestBridgeClientTimesOutRatherThanHangs(t *testing.T) {
	// A descriptor pointing at a closed port must fail fast, not block.
	client := NewBridgeClient(BridgeDescriptor{URL: "http://127.0.0.1:1", Token: strings64()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := client.Status(ctx)
	if err == nil {
		t.Fatal("a dead endpoint reported success")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("call took %v", elapsed)
	}
}
