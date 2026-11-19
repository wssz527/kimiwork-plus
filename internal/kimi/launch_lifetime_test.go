package kimi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wssz527/kimi-work-plus/assets/injection"
)

// The managed child is a stand-in for Kimi: this binary re-entered with
// fakeCDPFlag, which speaks the same fd3/fd4 pipe protocol.
func startFakeChild(t *testing.T) (*Child, string) {
	t.Helper()
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	profile := filepath.Join(base, "profile")
	child, err := StartKimi(context.Background(), LaunchOptions{
		Installation: Installation{Application: executable},
		UserDataDir:  profile,
		ExtraArgs:    []string{fakeCDPFlag},
	})
	if err != nil {
		t.Fatal(err)
	}
	child.allowForceStop = true
	t.Cleanup(func() { _ = child.Stop(3 * time.Second) })
	return child, profile
}

// A managed child must outlive the request that asked for it. Binding the
// process to that request's context meant the caller's own `defer cancel()` in
// the bridge killed a healthy Kimi the moment Start returned.
func TestManagedChildOutlivesTheLaunchRequest(t *testing.T) {
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "profile")
	ctx, cancel := context.WithCancel(context.Background())
	child, err := StartKimi(ctx, LaunchOptions{
		Installation: Installation{Application: executable},
		UserDataDir:  profile,
		ExtraArgs:    []string{fakeCDPFlag},
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	// Exactly what a handler's `defer cancel()` does.
	cancel()

	select {
	case <-child.Exit():
		t.Fatalf("the child exited with the request context: %+v", child.ExitState())
	case <-time.After(500 * time.Millisecond):
	}
	// It still answers over its pipe, so it is alive rather than merely unreaped.
	call, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if _, err := child.Pipe().Send(call, "Target.getTargets", nil, "", 3*time.Second); err != nil {
		t.Fatalf("the child stopped serving after the request ended: %v", err)
	}
	// Only an explicit Stop ends it.
	if err := child.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.Exit():
	case <-time.After(5 * time.Second):
		t.Fatal("the child survived Stop")
	}
}

// A request that was abandoned before the launch must not start a process.
func TestStartKimiRefusesACancelledRequest(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	child, err := StartKimi(ctx, LaunchOptions{
		Installation: Installation{Application: executable},
		UserDataDir:  filepath.Join(t.TempDir(), "profile"),
	})
	if err == nil {
		_ = child.Stop(3 * time.Second)
		t.Fatal("a cancelled request started a child")
	}
	if child != nil {
		t.Fatal("a cancelled request returned a child")
	}
}

// With translation off the launch is control-only, but the native import bridge
// is still published: it is how the panel imports models into this child, and it
// has nothing to do with translation.
func TestAttachWithTranslationOffStillPublishesTheBridge(t *testing.T) {
	child, profile := startFakeChild(t)
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile, Translation: TranslationSettings{Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := controller.attachChild(ctx, child, TranslationSettings{Enabled: false}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = controller.Stop(context.Background()) })

	state := controller.PublicState(context.Background())
	if state.State != StateReady {
		t.Fatalf("state: %s (%s)", state.State, state.Reason)
	}
	if state.Bridge == nil || !state.Bridge.Published {
		t.Fatalf("no import bridge with translation off: %+v", state.Bridge)
	}
	// Control-only still means nothing was injected.
	if state.Translation == nil || state.Translation.State != "inactive" ||
		state.Translation.Reason != "translation_disabled" {
		t.Fatalf("translation: %+v", state.Translation)
	}
	// And the bridge really works: it answers the surface the child serves.
	surface, err := controller.WorkSurface(ctx)
	if err != nil {
		t.Fatalf("work surface through the published bridge: %v", err)
	}
	if !surface.Work {
		t.Fatalf("surface: %+v", surface)
	}
}

// A failed initialisation must not leave a child running with no handle, and it
// must not touch any process it did not start.
func TestFailedAttachStopsOnlyItsOwnChild(t *testing.T) {
	child, profile := startFakeChild(t)
	controller, err := NewController(ControllerSettings{StateDir: filepath.Dir(profile),
		UserDataDir: profile})
	if err != nil {
		t.Fatal(err)
	}
	original := translationPayload
	translationPayload = func() ([]byte, injection.Asset, error) {
		return nil, injection.Asset{}, os.ErrNotExist
	}
	t.Cleanup(func() { translationPayload = original })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := controller.attachChild(ctx, child, TranslationSettings{Enabled: false}); err == nil {
		t.Fatal("attach succeeded without a payload")
	}
	select {
	case <-child.Exit():
	case <-time.After(5 * time.Second):
		t.Fatal("the failed launch left its child running")
	}
	state := controller.Status(context.Background())
	if state.State != StateFailed || state.ManagedPID != 0 {
		t.Fatalf("state after a failed attach: %+v", state)
	}
}
