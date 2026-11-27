package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

type takeoverFixture struct {
	n       *Native
	c       *autoTakeoverCoordinator
	ctx     context.Context
	install kimi.Installation
	home    string
	now     time.Time
	active  atomic.Int32
	calls   atomic.Int32
	managed int
}

func newTakeoverFixture(t *testing.T, enabled bool) *takeoverFixture {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	store, err := core.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		_, revision, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Update(revision, func(d core.Document) error {
			d[core.AutoTakeoverKey] = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	install := kimi.Installation{Application: filepath.Join(dir, "Kimi"), Archive: filepath.Join(dir, "app.asar"), Version: "3.2.29"}
	controller, err := kimi.NewController(kimi.ControllerSettings{
		StateDir: stateDir, Installation: install, UserDataDir: filepath.Join(dir, "profile"), ProfileMode: kimi.ProfileModeOfficial,
	})
	if err != nil {
		t.Fatal(err)
	}
	home, err := controller.WorkHome()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Native{
		service: &Service{Store: store, StateDir: stateDir}, controller: controller,
		policies: &nativePolicies{home: home}, compatFingerprint: kimi.ReadOfficialFingerprint(install, home),
		discoverInstallation: func() []kimi.Installation { return nil },
	}
	f := &takeoverFixture{n: n, ctx: ctx, install: install, home: home, now: time.Now()}
	n.autoSync = newAutoSyncCoordinator(ctx, n)
	n.gatewayActiveRequests = func() int { return int(f.active.Load()) }
	f.c = newAutoTakeoverCoordinator(n)
	f.c.now = func() time.Time { return f.now }
	f.c.managedPID = func(context.Context) int { return f.managed }
	f.c.restartFn = func(context.Context) error { f.calls.Add(1); return nil }
	n.autoTakeover = f.c
	t.Cleanup(func() {
		cancel()
		f.c.stop()
		n.autoSync.stop()
	})
	return f
}

func (f *takeoverFixture) observe(pids ...int) {
	f.c.observe(f.ctx, pids, true, f.install)
}

func (f *takeoverFixture) expect(t *testing.T, enabled bool, state, reason string, calls int32) autoTakeoverSnapshot {
	t.Helper()
	snapshot := f.c.status(enabled)
	if snapshot.Enabled != enabled || snapshot.State != state || snapshot.Reason != reason || f.calls.Load() != calls {
		t.Fatalf("snapshot = %+v, restarts = %d, want %v %s %q %d", snapshot, f.calls.Load(), enabled, state, reason, calls)
	}
	return snapshot
}

func TestAutoTakeoverDisabledByDefault(t *testing.T) {
	f := newTakeoverFixture(t, false)
	f.observe(70)
	f.now = f.now.Add(time.Minute)
	f.observe(70)
	f.expect(t, false, "disabled", "", 0)
}

func TestAutoTakeoverWaitsForIdleAndRestartsOnce(t *testing.T) {
	f := newTakeoverFixture(t, true)
	f.observe(70, 80)
	f.expect(t, true, "waiting_idle", "settling", 0)
	f.now = f.now.Add(f.c.graceWindow)
	f.active.Store(1)
	f.observe(80, 70)
	f.expect(t, true, "waiting_idle", "gateway_requests_active", 0)
	f.active.Store(0)

	activity := filepath.Join(f.home, "sessions", "one", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(activity), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activity, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.observe(70, 80)
	f.expect(t, true, "waiting_idle", "work_activity_recent", 0)
	old := time.Now().Add(-21 * time.Second)
	if err := os.Chtimes(activity, old, old); err != nil {
		t.Fatal(err)
	}

	f.n.mu.Lock()
	f.n.opWaiter = true
	f.n.mu.Unlock()
	f.observe(70, 80)
	f.expect(t, true, "waiting_idle", "operation_busy", 0)
	f.n.mu.Lock()
	f.n.opWaiter = false
	f.n.mu.Unlock()
	restartStarted, complete := make(chan struct{}), make(chan struct{})
	f.c.restartFn = func(ctx context.Context) error {
		f.calls.Add(1)
		close(restartStarted)
		select {
		case <-complete:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.observe(70, 80)
	select {
	case <-restartStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("restart did not begin")
	}
	f.expect(t, true, "taking_over", "", 1)
	f.observe(80, 70)
	f.expect(t, true, "taking_over", "", 1)
	close(complete)
	f.c.wg.Wait()
	if snap := f.expect(t, true, "recovered", "", 1); snap.LastSuccessAt != f.now.UnixMilli() || snap.UpdatedAt != f.now.UnixMilli() {
		t.Fatalf("recovery timestamps: %+v", snap)
	}
	f.observe(80, 70)
	f.expect(t, true, "recovered", "", 1)
	f.n.mu.Lock()
	state := f.n.operation["state"]
	suspended := f.n.opSuspended
	f.n.mu.Unlock()
	f.n.autoSync.mu.Lock()
	autoSyncSuspended := f.n.autoSync.suspended
	f.n.autoSync.mu.Unlock()
	if state != "done" || suspended || autoSyncSuspended != 0 {
		t.Fatalf("operation not released: %v, %v, %d", state, suspended, autoSyncSuspended)
	}
}

func TestAutoTakeoverFailedInstanceCanBeReplaced(t *testing.T) {
	for _, reset := range []string{"stopped", "managed"} {
		t.Run(reset, func(t *testing.T) {
			f := newTakeoverFixture(t, true)
			f.c.graceWindow = 0
			f.c.restartFn = func(context.Context) error {
				if f.calls.Add(1) == 1 {
					return errors.New("kimi_quit_timeout")
				}
				return nil
			}
			f.observe(30, 40)
			f.c.wg.Wait()
			f.expect(t, true, "failed", "kimi_quit_timeout", 1)
			f.now = f.now.Add(time.Minute)
			f.observe(40, 30)
			f.expect(t, true, "failed", "kimi_quit_timeout", 1)
			if reset == "managed" {
				f.managed = 30
				f.observe(30, 40)
				f.managed = 0
			} else {
				f.observe()
			}
			f.expect(t, true, "watching", "", 1)
			// A cleared observation permits even a reused PID set.
			f.observe(30, 40)
			f.c.wg.Wait()
			f.expect(t, true, "recovered", "", 2)
			f.observe(60)
			f.expect(t, true, "waiting_idle", "cooldown", 2)
		})
	}
}

func TestAutoTakeoverRespectsDisconnectAndCompatibilityGuard(t *testing.T) {
	for _, reason := range []string{"disconnected", "model_sync_write_blocked"} {
		t.Run(reason, func(t *testing.T) {
			f := newTakeoverFixture(t, true)
			f.c.graceWindow = 0
			if reason == "disconnected" {
				f.n.autoSync.reset()
			} else {
				f.n.compatibility.Guards.ModelSyncWriteBlocked = true
			}
			f.observe(90)
			f.now = f.now.Add(time.Minute)
			f.observe(90)
			f.expect(t, true, "watching", reason, 0)
		})
	}
}

func TestWorkActivityChecksKnownSessionFiles(t *testing.T) {
	for _, rel := range []string{
		"sessions/conversations.sqlite", "sessions/nested/conversations.sqlite-wal", "sessions/nested/conversations.sqlite-shm",
		"agents/main/sessions/hosted-logical/conversations.sqlite", "runtime/kimi-code/home/sessions/wd_1/work/agents/main/wire.jsonl",
	} {
		t.Run(rel, func(t *testing.T) {
			home := t.TempDir()
			file := filepath.Join(home, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if !workActivityBusy(home, 20*time.Second) {
				t.Fatal("recent Work activity was missed")
			}
			old := time.Now().Add(-time.Minute)
			if err := os.Chtimes(file, old, old); err != nil {
				t.Fatal(err)
			}
			if workActivityBusy(home, 20*time.Second) {
				t.Fatal("old session file was treated as recent activity")
			}
		})
	}
}

func TestAutoTakeoverStopCancelsAndWaitsForRestart(t *testing.T) {
	f := newTakeoverFixture(t, true)
	f.c.graceWindow = 0
	started := make(chan struct{})
	f.c.restartFn = func(ctx context.Context) error {
		f.calls.Add(1)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	f.observe(90)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("restart did not begin")
	}
	stopped := make(chan struct{})
	go func() {
		f.c.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel and wait for the restart")
	}
	f.expect(t, true, "failed", core.ErrorCode(context.Canceled), 1)
	if f.n.isOperationRunning() {
		t.Fatal("shutdown left the operation running")
	}
}
