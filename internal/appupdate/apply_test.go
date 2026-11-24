package appupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/platform"
)

const testHelperDaemonKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestApplyHelperDaemonProcess acts as a real child daemon process when invoked
// with GO_WANT_HELPER_PROCESS=1 by apply lifecycle tests.
func TestApplyHelperDaemonProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	mode := os.Getenv("KWP_TEST_DAEMON_MODE")
	stateDir := os.Getenv("KWP_TEST_STATE_DIR")
	version := os.Getenv("KWP_TEST_VERSION")
	buildID := os.Getenv("KWP_TEST_BUILD_ID")

	if mode == "fail_immediately" {
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(2)
	}
	defer ln.Close()

	stopCh := make(chan struct{})
	var stopOnce sync.Once
	triggerStop := func() {
		stopOnce.Do(func() { close(stopCh) })
	}

	var healthHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testHelperDaemonKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"pid":     os.Getpid(),
			"version": version,
			"buildId": buildID,
		})
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if mode == "crash_after_first_health" && healthHits.Add(1) == 1 {
			go func() {
				time.Sleep(40 * time.Millisecond)
				os.Exit(1)
			}()
		}
	})
	mux.HandleFunc("/api/daemon/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testHelperDaemonKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
		go func() {
			time.Sleep(20 * time.Millisecond)
			triggerStop()
		}()
	})

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()

	ep := platform.DaemonEndpoint{
		URL:     "http://" + ln.Addr().String(),
		Key:     testHelperDaemonKey,
		PID:     os.Getpid(),
		Version: version,
		BuildID: buildID,
	}
	if err := core.AtomicJSON(filepath.Join(stateDir, "desktop-endpoint.json"), ep); err != nil {
		os.Exit(3)
	}

	select {
	case <-stopCh:
		_ = srv.Close()
		os.Exit(0)
	case <-time.After(30 * time.Second):
		_ = srv.Close()
		os.Exit(0)
	}
}

type trackedChild struct {
	cmd  *exec.Cmd
	done chan struct{}
}

type testChildTracker struct {
	mu    sync.Mutex
	procs []*trackedChild
}

func (tr *testChildTracker) spawn(t *testing.T, stateDir, mode, version, buildID string) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestApplyHelperDaemonProcess$")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"KWP_TEST_DAEMON_MODE="+mode,
		"KWP_TEST_STATE_DIR="+stateDir,
		"KWP_TEST_VERSION="+version,
		"KWP_TEST_BUILD_ID="+buildID,
	)
	if err := cmd.Start(); err != nil {
		return err
	}
	child := &trackedChild{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(child.done)
	}()
	tr.mu.Lock()
	tr.procs = append(tr.procs, child)
	tr.mu.Unlock()
	return nil
}

func (tr *testChildTracker) cleanup(stateDir string) {
	stopRunningDaemon(stateDir, 0, func(string, ...any) {})
	tr.mu.Lock()
	procs := append([]*trackedChild(nil), tr.procs...)
	tr.procs = nil
	tr.mu.Unlock()
	for _, child := range procs {
		select {
		case <-child.done:
		default:
			if child.cmd.Process != nil {
				_ = child.cmd.Process.Kill()
			}
			<-child.done
		}
	}
}

func saveApplyHooks(t *testing.T) {
	t.Helper()
	origValidateStaged := applyValidateStaged
	origValidateInstalled := applyValidateInstalled
	origStopDesktop := applyStopDesktop
	origStopDaemon := applyStopDaemon
	origForceStopDaemon := applyForceStopDaemon
	origSwap := applySwapInstallation
	origRestore := applyRestoreInstallation
	origLaunch := applyLaunchInstallation
	origStability := applyStabilityDelay
	origRollbackTimeout := applyRollbackHealthTimeout
	origOpenMgr := openPlatformServiceManager
	t.Cleanup(func() {
		applyValidateStaged = origValidateStaged
		applyValidateInstalled = origValidateInstalled
		applyStopDesktop = origStopDesktop
		applyStopDaemon = origStopDaemon
		applyForceStopDaemon = origForceStopDaemon
		applySwapInstallation = origSwap
		applyRestoreInstallation = origRestore
		applyLaunchInstallation = origLaunch
		applyStabilityDelay = origStability
		applyRollbackHealthTimeout = origRollbackTimeout
		openPlatformServiceManager = origOpenMgr
	})
}

func writeFakeInstallDir(t *testing.T, dir, version, buildID string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte(version+"|"+buildID), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFakeInstallDir(t *testing.T, dir string) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "version.txt"))
	if err != nil {
		t.Fatalf("read version.txt from %s: %v", dir, err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(raw)), "|", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected version.txt content: %q", string(raw))
	}
	return parts[0], parts[1]
}

func replaceDirCopy(dst, src string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return copyAny(src, dst)
}

func TestExecuteApplyPlanAutoRollbackOnStartupFailure(t *testing.T) {
	saveApplyHooks(t)

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := core.PrivateDirectory(stateDir); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(root, "Kimi Work+.app")
	backupDir := filepath.Join(root, "backup", "Kimi Work+.app")
	stagedDir := filepath.Join(root, "wails-update-stage", "Kimi Work+.app")

	writeFakeInstallDir(t, targetDir, "1.1.3", "build-113")
	writeFakeInstallDir(t, backupDir, "1.1.3", "build-113")
	writeFakeInstallDir(t, stagedDir, "1.2.0", "build-120")

	tracker := &testChildTracker{}
	t.Cleanup(func() { tracker.cleanup(stateDir) })

	applyValidateStaged = func(string) error { return nil }
	applyValidateInstalled = func(string) error { return nil }
	applyStopDesktop = func(string, func(string, ...any)) {}
	applySwapInstallation = func(target, staged, _ string, _ func(string, ...any)) error {
		return replaceDirCopy(target, staged)
	}
	applyRestoreInstallation = func(target, backup string, _ func(string, ...any)) error {
		return replaceDirCopy(target, backup)
	}
	applyLaunchInstallation = func(target, stDir, _ string, _ func(string, ...any)) error {
		ver, bid := readFakeInstallDir(t, target)
		if ver == "1.2.0" {
			return tracker.spawn(t, stDir, "fail_immediately", ver, bid)
		}
		return tracker.spawn(t, stDir, "healthy", ver, bid)
	}
	applyRollbackHealthTimeout = 5 * time.Second

	plan := ApplyPlan{
		Mode:              "update",
		StateDir:          stateDir,
		TargetPath:        targetDir,
		StagedPath:        stagedDir,
		StagingRoot:       filepath.Dir(stagedDir),
		BackupPath:        backupDir,
		PrevVersion:       "1.1.3",
		PrevBuildID:       "build-113",
		TargetVersion:     "1.2.0",
		StartupTimeoutSec: 1,
	}

	code := executeApplyPlan(plan, "")
	if code != 14 {
		t.Fatalf("expected exit code 14 (health watchdog timeout), got %d", code)
	}

	gotVer, gotBuild := readFakeInstallDir(t, targetDir)
	if gotVer != "1.1.3" || gotBuild != "build-113" {
		t.Fatalf("expected target restored to 1.1.3/build-113, got %s/%s", gotVer, gotBuild)
	}

	h := loadHistory(stateDir)
	if h.LastRollback == nil {
		t.Fatal("expected LastRollback recorded in update-history.json")
	}
	if !h.LastRollback.Automatic || h.LastRollback.FromVersion != "1.2.0" || h.LastRollback.ToVersion != "1.1.3" || h.LastRollback.ToBuildID != "build-113" {
		t.Fatalf("unexpected LastRollback record: %+v", h.LastRollback)
	}
	if !strings.Contains(h.LastRollback.Reason, "new_version_health_failed") {
		t.Fatalf("expected health failure reason, got %q", h.LastRollback.Reason)
	}

	health, err := probeDaemonHealth(stateDir)
	if err != nil || !health.Alive || health.Version != "1.1.3" || health.BuildID != "build-113" {
		t.Fatalf("expected restored daemon healthy at 1.1.3, got health=%+v err=%v", health, err)
	}
}

func TestExecuteApplyPlanAutoRollbackOnPostStartupCrash(t *testing.T) {
	saveApplyHooks(t)

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := core.PrivateDirectory(stateDir); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(root, "Kimi Work+.app")
	backupDir := filepath.Join(root, "backup", "Kimi Work+.app")
	stagedDir := filepath.Join(root, "wails-update-stage", "Kimi Work+.app")

	writeFakeInstallDir(t, targetDir, "1.1.3", "build-113")
	writeFakeInstallDir(t, backupDir, "1.1.3", "build-113")
	writeFakeInstallDir(t, stagedDir, "1.2.0", "build-120")

	tracker := &testChildTracker{}
	t.Cleanup(func() { tracker.cleanup(stateDir) })

	applyValidateStaged = func(string) error { return nil }
	applyValidateInstalled = func(string) error { return nil }
	applyStopDesktop = func(string, func(string, ...any)) {}
	applySwapInstallation = func(target, staged, _ string, _ func(string, ...any)) error {
		return replaceDirCopy(target, staged)
	}
	applyRestoreInstallation = func(target, backup string, _ func(string, ...any)) error {
		return replaceDirCopy(target, backup)
	}
	applyLaunchInstallation = func(target, stDir, _ string, _ func(string, ...any)) error {
		ver, bid := readFakeInstallDir(t, target)
		if ver == "1.2.0" {
			return tracker.spawn(t, stDir, "crash_after_first_health", ver, bid)
		}
		return tracker.spawn(t, stDir, "healthy", ver, bid)
	}
	applyStabilityDelay = 400 * time.Millisecond
	applyRollbackHealthTimeout = 5 * time.Second

	plan := ApplyPlan{
		Mode:              "update",
		StateDir:          stateDir,
		TargetPath:        targetDir,
		StagedPath:        stagedDir,
		StagingRoot:       filepath.Dir(stagedDir),
		BackupPath:        backupDir,
		PrevVersion:       "1.1.3",
		PrevBuildID:       "build-113",
		TargetVersion:     "1.2.0",
		StartupTimeoutSec: 5,
	}

	code := executeApplyPlan(plan, "")
	if code != 15 {
		t.Fatalf("expected exit code 15 (post-startup crash stability probe), got %d", code)
	}

	gotVer, gotBuild := readFakeInstallDir(t, targetDir)
	if gotVer != "1.1.3" || gotBuild != "build-113" {
		t.Fatalf("expected target restored to 1.1.3/build-113, got %s/%s", gotVer, gotBuild)
	}

	h := loadHistory(stateDir)
	if h.LastRollback == nil {
		t.Fatal("expected LastRollback recorded in update-history.json")
	}
	if !h.LastRollback.Automatic || h.LastRollback.Reason != "new_version_crashed_after_startup" || h.LastRollback.ToVersion != "1.1.3" {
		t.Fatalf("unexpected LastRollback record: %+v", h.LastRollback)
	}

	health, err := probeDaemonHealth(stateDir)
	if err != nil || !health.Alive || health.Version != "1.1.3" || health.BuildID != "build-113" {
		t.Fatalf("expected restored daemon healthy at 1.1.3, got health=%+v err=%v", health, err)
	}
}

func TestExecuteApplyPlanRecordsRollbackRestoreFailure(t *testing.T) {
	saveApplyHooks(t)

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := core.PrivateDirectory(stateDir); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(root, "Kimi Work+.app")
	backupDir := filepath.Join(root, "backup", "Kimi Work+.app")
	stagedDir := filepath.Join(root, "wails-update-stage", "Kimi Work+.app")

	writeFakeInstallDir(t, targetDir, "1.1.3", "build-113")
	writeFakeInstallDir(t, backupDir, "1.1.3", "build-113")
	writeFakeInstallDir(t, stagedDir, "1.2.0", "build-120")

	tracker := &testChildTracker{}
	t.Cleanup(func() { tracker.cleanup(stateDir) })

	applyValidateStaged = func(string) error { return nil }
	applyValidateInstalled = func(string) error { return nil }
	applyStopDesktop = func(string, func(string, ...any)) {}
	applySwapInstallation = func(target, staged, _ string, _ func(string, ...any)) error {
		return replaceDirCopy(target, staged)
	}
	applyRestoreInstallation = func(string, string, func(string, ...any)) error {
		return errors.New("permission denied restoring bundle")
	}
	applyLaunchInstallation = func(target, stDir, _ string, _ func(string, ...any)) error {
		ver, bid := readFakeInstallDir(t, target)
		return tracker.spawn(t, stDir, "fail_immediately", ver, bid)
	}

	plan := ApplyPlan{
		Mode:              "update",
		StateDir:          stateDir,
		TargetPath:        targetDir,
		StagedPath:        stagedDir,
		StagingRoot:       filepath.Dir(stagedDir),
		BackupPath:        backupDir,
		PrevVersion:       "1.1.3",
		PrevBuildID:       "build-113",
		TargetVersion:     "1.2.0",
		StartupTimeoutSec: 1,
	}

	code := executeApplyPlan(plan, "")
	if code != 14 {
		t.Fatalf("expected exit code 14, got %d", code)
	}

	h := loadHistory(stateDir)
	if h.LastRollback == nil {
		t.Fatal("expected rollback_restore_failed written to update-history.json")
	}
	if !h.LastRollback.Automatic || !strings.Contains(h.LastRollback.Reason, "rollback_restore_failed: permission denied restoring bundle") {
		t.Fatalf("unexpected LastRollback record: %+v", h.LastRollback)
	}
}

type fakeServiceManager struct {
	state         platform.ManagerState
	statusErr     error
	registerErr   error
	enableErr     error
	registerCalls int
	enableCalls   int
}

func (f *fakeServiceManager) Register(context.Context) (platform.ManagerState, error) {
	f.registerCalls++
	return f.state, f.registerErr
}
func (f *fakeServiceManager) Enable(context.Context) (platform.ManagerState, error) {
	f.enableCalls++
	return f.state, f.enableErr
}
func (f *fakeServiceManager) Disable(context.Context) (platform.ManagerState, error) {
	return f.state, nil
}
func (f *fakeServiceManager) Start(context.Context) (platform.ManagerState, error) {
	return f.state, nil
}
func (f *fakeServiceManager) Stop(context.Context) (platform.ManagerState, error) {
	return f.state, nil
}
func (f *fakeServiceManager) Status(context.Context) (platform.ManagerState, error) {
	return f.state, f.statusErr
}
func (f *fakeServiceManager) Uninstall(context.Context) (platform.ManagerState, error) {
	return f.state, nil
}
func (f *fakeServiceManager) Describe(bool) (platform.ServiceDescription, error) {
	return platform.ServiceDescription{}, nil
}

func TestExecuteApplyPlanRefreshesStaleServiceWithoutChangingLoginStart(t *testing.T) {
	cases := []struct {
		name             string
		status           platform.ManagerState
		enableErr        error
		wantRegisterCall int
		wantEnableCall   int
	}{
		{
			name:             "stale_enabled_calls_enable",
			status:           platform.ManagerState{Registered: true, Stale: true, Enabled: true},
			wantRegisterCall: 0,
			wantEnableCall:   1,
		},
		{
			name:             "stale_disabled_calls_register",
			status:           platform.ManagerState{Registered: true, Stale: true, Enabled: false},
			wantRegisterCall: 1,
			wantEnableCall:   0,
		},
		{
			name:             "not_stale_skips_refresh",
			status:           platform.ManagerState{Registered: true, Stale: false, Enabled: true},
			wantRegisterCall: 0,
			wantEnableCall:   0,
		},
		{
			name:             "unregistered_skips_refresh",
			status:           platform.ManagerState{Registered: false, Stale: false, Enabled: false},
			wantRegisterCall: 0,
			wantEnableCall:   0,
		},
		{
			name:             "refresh_error_is_nonfatal",
			status:           platform.ManagerState{Registered: true, Stale: true, Enabled: true},
			enableErr:        fmt.Errorf("launchd busy"),
			wantRegisterCall: 0,
			wantEnableCall:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saveApplyHooks(t)

			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			if err := core.PrivateDirectory(stateDir); err != nil {
				t.Fatal(err)
			}
			targetDir := filepath.Join(root, "Kimi Work+.app")
			backupDir := filepath.Join(root, "backup", "Kimi Work+.app")
			stagedDir := filepath.Join(root, "wails-update-stage", "Kimi Work+.app")

			writeFakeInstallDir(t, targetDir, "1.1.3", "build-113")
			writeFakeInstallDir(t, backupDir, "1.1.3", "build-113")
			writeFakeInstallDir(t, stagedDir, "1.2.0", "build-120")

			// Seed an existing service-definition.json with custom port to confirm preservation.
			if err := os.WriteFile(filepath.Join(stateDir, "service-definition.json"), []byte(`{"port":"18421","logPath":"/tmp/custom.log"}`), 0o600); err != nil {
				t.Fatal(err)
			}

			tracker := &testChildTracker{}
			t.Cleanup(func() { tracker.cleanup(stateDir) })

			fakeMgr := &fakeServiceManager{state: tc.status, enableErr: tc.enableErr}
			var capturedOpts platform.ManagerOptions
			openPlatformServiceManager = func(opts platform.ManagerOptions) (platform.Manager, error) {
				capturedOpts = opts
				return fakeMgr, nil
			}

			applyValidateStaged = func(string) error { return nil }
			applyValidateInstalled = func(string) error { return nil }
			applyStopDesktop = func(string, func(string, ...any)) {}
			applySwapInstallation = func(target, staged, _ string, _ func(string, ...any)) error {
				return replaceDirCopy(target, staged)
			}
			applyLaunchInstallation = func(target, stDir, _ string, _ func(string, ...any)) error {
				ver, bid := readFakeInstallDir(t, target)
				return tracker.spawn(t, stDir, "healthy", ver, bid)
			}
			applyStabilityDelay = 150 * time.Millisecond

			plan := ApplyPlan{
				Mode:              "update",
				StateDir:          stateDir,
				TargetPath:        targetDir,
				StagedPath:        stagedDir,
				StagingRoot:       filepath.Dir(stagedDir),
				BackupPath:        backupDir,
				PrevVersion:       "1.1.3",
				PrevBuildID:       "build-113",
				TargetVersion:     "1.2.0",
				StartupTimeoutSec: 5,
			}

			code := executeApplyPlan(plan, "")
			if code != 0 {
				t.Fatalf("expected exit code 0, got %d", code)
			}
			if fakeMgr.registerCalls != tc.wantRegisterCall || fakeMgr.enableCalls != tc.wantEnableCall {
				t.Fatalf("service refresh calls = (register=%d, enable=%d), want (%d, %d)",
					fakeMgr.registerCalls, fakeMgr.enableCalls, tc.wantRegisterCall, tc.wantEnableCall)
			}
			if capturedOpts.BuildID != "build-120" || capturedOpts.Port != "18421" || capturedOpts.LogPath != "/tmp/custom.log" {
				t.Fatalf("unexpected service refresh options: %+v", capturedOpts)
			}
			h := loadHistory(stateDir)
			if h.LastApplied == nil || h.LastApplied.ToVersion != "1.2.0" || h.LastApplied.ToBuildID != "build-120" {
				t.Fatalf("expected LastApplied recorded for 1.2.0, got %+v", h.LastApplied)
			}
		})
	}
}

func TestAutoRollbackRefreshesStaleServiceMetadata(t *testing.T) {
	saveApplyHooks(t)

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := core.PrivateDirectory(stateDir); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(root, "Kimi Work+.app")
	backupDir := filepath.Join(root, "backup", "Kimi Work+.app")
	stagedDir := filepath.Join(root, "wails-update-stage", "Kimi Work+.app")

	writeFakeInstallDir(t, targetDir, "1.1.6", "build-116")
	writeFakeInstallDir(t, backupDir, "1.1.6", "build-116")
	writeFakeInstallDir(t, stagedDir, "1.1.7", "build-117")

	tracker := &testChildTracker{}
	t.Cleanup(func() { tracker.cleanup(stateDir) })

	fakeMgr := &fakeServiceManager{state: platform.ManagerState{Registered: true, Stale: true, Enabled: true}}
	var capturedOpts platform.ManagerOptions
	openPlatformServiceManager = func(opts platform.ManagerOptions) (platform.Manager, error) {
		capturedOpts = opts
		return fakeMgr, nil
	}

	applyValidateStaged = func(string) error { return nil }
	applyValidateInstalled = func(string) error { return nil }
	applyStopDesktop = func(string, func(string, ...any)) {}
	applyStopDaemon = func(string, int, func(string, ...any)) bool { return true }
	applyForceStopDaemon = func(string, func(string, ...any)) {
		t.Fatal("forceStopTargetDaemon must not run when graceful stop succeeds")
	}
	applySwapInstallation = func(target, staged, _ string, _ func(string, ...any)) error {
		return replaceDirCopy(target, staged)
	}
	applyRestoreInstallation = func(target, backup string, _ func(string, ...any)) error {
		return replaceDirCopy(target, backup)
	}
	applyLaunchInstallation = func(target, stDir, _ string, _ func(string, ...any)) error {
		ver, bid := readFakeInstallDir(t, target)
		if ver == "1.1.7" {
			return tracker.spawn(t, stDir, "fail_immediately", ver, bid)
		}
		return tracker.spawn(t, stDir, "healthy", ver, bid)
	}
	applyRollbackHealthTimeout = 5 * time.Second

	plan := ApplyPlan{
		Mode:              "update",
		StateDir:          stateDir,
		TargetPath:        targetDir,
		StagedPath:        stagedDir,
		StagingRoot:       filepath.Dir(stagedDir),
		BackupPath:        backupDir,
		PrevVersion:       "1.1.6",
		PrevBuildID:       "build-116",
		TargetVersion:     "1.1.7",
		StartupTimeoutSec: 1,
	}

	code := executeApplyPlan(plan, "")
	if code != 14 {
		t.Fatalf("expected rollback exit code 14, got %d", code)
	}
	if fakeMgr.enableCalls != 1 {
		t.Fatalf("expected stale service metadata refreshed after rollback, enableCalls=%d", fakeMgr.enableCalls)
	}
	if capturedOpts.BuildID != "build-116" {
		t.Fatalf("expected refreshed buildId=build-116, got %q", capturedOpts.BuildID)
	}
}

func TestExecuteApplyPlanForceStopsDaemonOnlyOnGracefulTimeout(t *testing.T) {
	saveApplyHooks(t)

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := core.PrivateDirectory(stateDir); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(root, "Kimi Work+.app")
	stagedDir := filepath.Join(root, "wails-update-stage", "Kimi Work+.app")
	writeFakeInstallDir(t, targetDir, "1.1.6", "build-116")
	writeFakeInstallDir(t, stagedDir, "1.1.7", "build-117")

	var forceCalls int
	applyValidateStaged = func(string) error { return nil }
	applyStopDesktop = func(string, func(string, ...any)) {}
	applyStopDaemon = func(string, int, func(string, ...any)) bool { return false }
	applyForceStopDaemon = func(string, func(string, ...any)) { forceCalls++ }
	applySwapInstallation = func(string, string, string, func(string, ...any)) error {
		return errors.New("stop here")
	}
	applyLaunchInstallation = func(string, string, string, func(string, ...any)) error { return nil }

	plan := ApplyPlan{
		Mode:          "update",
		StateDir:      stateDir,
		TargetPath:    targetDir,
		StagedPath:    stagedDir,
		StagingRoot:   filepath.Dir(stagedDir),
		PrevVersion:   "1.1.6",
		PrevBuildID:   "build-116",
		TargetVersion: "1.1.7",
	}

	if code := executeApplyPlan(plan, ""); code != 11 {
		t.Fatalf("expected swap failure code 11, got %d", code)
	}
	if forceCalls != 1 {
		t.Fatalf("expected forceStopTargetDaemon called once on graceful timeout, got %d", forceCalls)
	}
}
