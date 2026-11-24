package appupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/platform"
)

const ApplyHelperFlag = "--apply-update"

var (
	applyValidateStaged        = validateStagedArtifact
	applyValidateInstalled     = validateInstalledArtifact
	applyStopDesktop           = stopTargetDesktop
	applyStopDaemon            = stopRunningDaemon
	applyForceStopDaemon       = forceStopTargetDaemon
	applySwapInstallation      = swapInstallation
	applyRestoreInstallation   = restoreInstallation
	applyLaunchInstallation    = launchInstallation
	applyStabilityDelay        = 1500 * time.Millisecond
	applyRollbackHealthTimeout = 15 * time.Second
	openPlatformServiceManager = platform.OpenManager
)

type ApplyPlan struct {
	Mode              string `json:"mode"` // "update" or "rollback"
	StateDir          string `json:"stateDir"`
	TargetPath        string `json:"targetPath"`
	StagedPath        string `json:"stagedPath"`
	StagingRoot       string `json:"stagingRoot,omitempty"`
	BackupPath        string `json:"backupPath"`
	PrevVersion       string `json:"prevVersion"`
	PrevBuildID       string `json:"prevBuildId"`
	TargetVersion     string `json:"targetVersion"`
	TargetBuildID     string `json:"targetBuildId,omitempty"`
	DaemonPID         int    `json:"daemonPid"`
	LaunchMode        string `json:"launchMode,omitempty"`
	StartupTimeoutSec int    `json:"startupTimeoutSec,omitempty"`
}

func RunApplyHelperFromArgs(args []string) (int, bool) {
	if len(args) < 3 || args[1] != ApplyHelperFlag {
		return 0, false
	}
	planPath := args[2]
	raw, err := os.ReadFile(planPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update helper: read plan: %v\n", err)
		return 2, true
	}
	var plan ApplyPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		fmt.Fprintf(os.Stderr, "update helper: parse plan: %v\n", err)
		return 2, true
	}
	code := executeApplyPlan(plan, planPath)
	return code, true
}

func executeApplyPlan(plan ApplyPlan, planPath string) int {
	logFile := openApplyLog(plan.StateDir)
	defer logFile.Close()
	logf := func(format string, args ...any) {
		if logFile == nil {
			return
		}
		fmt.Fprintf(logFile, "[%s] "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
	}

	logf("apply helper start: mode=%s target=%s prev=%s(%s) next=%s",
		plan.Mode, plan.TargetPath, plan.PrevVersion, plan.PrevBuildID, plan.TargetVersion)

	defer func() {
		if planPath != "" {
			_ = os.Remove(planPath)
		}
		if plan.StagingRoot != "" && (strings.Contains(filepath.Base(plan.StagingRoot), "wails-update-") || strings.Contains(filepath.Base(plan.StagingRoot), "kwp-rollback-stage-")) {
			_ = os.RemoveAll(plan.StagingRoot)
		}
		if selfExe, err := os.Executable(); err == nil && strings.HasPrefix(filepath.Base(selfExe), "kwp-update-helper-") {
			_ = os.Remove(selfExe)
		}
	}()

	if err := applyValidateStaged(plan.StagedPath); err != nil {
		logf("staged artifact validation failed: %v", err)
		recordApplyFailure(plan, "staged_validation_failed: "+err.Error())
		return 10
	}

	applyStopDesktop(plan.TargetPath, logf)
	if !applyStopDaemon(plan.StateDir, plan.DaemonPID, logf) {
		applyForceStopDaemon(plan.TargetPath, logf)
	}

	if err := applySwapInstallation(plan.TargetPath, plan.StagedPath, plan.BackupPath, logf); err != nil {
		logf("swap failed: %v — attempting relaunch of existing installation", err)
		_ = applyLaunchInstallation(plan.TargetPath, plan.StateDir, plan.LaunchMode, logf)
		recordApplyFailure(plan, "swap_failed: "+err.Error())
		return 11
	}

	if err := applyValidateInstalled(plan.TargetPath); err != nil {
		logf("installed artifact validation failed: %v — rolling back", err)
		autoRollback(plan, "installed_validation_failed", logf)
		return 12
	}

	if err := applyLaunchInstallation(plan.TargetPath, plan.StateDir, plan.LaunchMode, logf); err != nil {
		logf("launch new installation failed: %v — rolling back", err)
		autoRollback(plan, "new_version_launch_failed", logf)
		return 13
	}

	timeout := time.Duration(plan.StartupTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	health, err := waitForUpdatedHealth(plan, timeout)
	if err != nil {
		logf("health watchdog failed: %v — rolling back", err)
		autoRollback(plan, "new_version_health_failed: "+err.Error(), logf)
		return 14
	}

	// Second stability probe after 1.5s to ensure the new version did not crash right after startup.
	time.Sleep(applyStabilityDelay)
	if second, err := probeDaemonHealth(plan.StateDir); err != nil || !second.Alive || second.PID != health.PID {
		logf("stability probe failed (first pid=%d, second=%+v, err=%v) — rolling back", health.PID, second, err)
		autoRollback(plan, "new_version_crashed_after_startup", logf)
		return 15
	}

	postInstallUpdateMetadata(plan, health, logf)
	recordApplySuccess(plan, health)
	logf("apply succeeded: version=%s buildId=%s pid=%d", health.Version, health.BuildID, health.PID)
	return 0
}

func autoRollback(plan ApplyPlan, reason string, logf func(string, ...any)) {
	applyStopDesktop(plan.TargetPath, logf)
	if !applyStopDaemon(plan.StateDir, 0, logf) {
		applyForceStopDaemon(plan.TargetPath, logf)
	}

	if plan.BackupPath != "" {
		if _, err := os.Stat(plan.BackupPath); err == nil {
			if err := applyRestoreInstallation(plan.TargetPath, plan.BackupPath, logf); err != nil {
				logf("rollback restore failed: %v", err)
				recordApplyFailure(plan, "rollback_restore_failed: "+err.Error())
				return
			}
		} else {
			logf("rollback backup unavailable at %s: %v", plan.BackupPath, err)
			recordApplyFailure(plan, "rollback_backup_unavailable: "+err.Error())
			return
		}
	}

	h := loadHistory(plan.StateDir)
	h.LastRollback = &RollbackRecord{
		Automatic:   plan.Mode == "update",
		FromVersion: plan.TargetVersion,
		ToVersion:   plan.PrevVersion,
		ToBuildID:   plan.PrevBuildID,
		Reason:      reason,
		At:          time.Now().UTC().Format(time.RFC3339),
	}
	_ = saveHistory(plan.StateDir, h)

	if err := applyLaunchInstallation(plan.TargetPath, plan.StateDir, plan.LaunchMode, logf); err != nil {
		logf("relaunch after rollback failed: %v", err)
	} else {
		logf("relaunch after rollback dispatched")
		rbDeadline := time.Now().Add(applyRollbackHealthTimeout)
		for time.Now().Before(rbDeadline) {
			if h, err := probeDaemonHealth(plan.StateDir); err == nil && h.Alive {
				logf("rollback recovery healthy: version=%s buildId=%s pid=%d", h.Version, h.BuildID, h.PID)
				postInstallUpdateMetadata(plan, h, logf)
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
}

func postInstallUpdateMetadata(plan ApplyPlan, health daemonHealth, logf func(string, ...any)) {
	refreshStaleServiceDefinition(plan, health, logf)
	postInstallPlatformMetadata(plan, health)
}

func refreshStaleServiceDefinition(plan ApplyPlan, health daemonHealth, logf func(string, ...any)) {
	if plan.TargetPath == "" || plan.StateDir == "" || health.BuildID == "" {
		return
	}
	daemonBin := installedDaemonBinary(plan.TargetPath)
	if daemonBin == "" {
		return
	}
	absBin, err := filepath.Abs(daemonBin)
	if err != nil {
		logf("service refresh skipped: resolve binary: %v", err)
		return
	}
	absState, err := filepath.Abs(plan.StateDir)
	if err != nil {
		logf("service refresh skipped: resolve state dir: %v", err)
		return
	}
	opts := platform.ManagerOptions{
		Binary:   absBin,
		StateDir: absState,
		BuildID:  health.BuildID,
	}
	if raw, readErr := os.ReadFile(filepath.Join(absState, "service-definition.json")); readErr == nil {
		var prev struct {
			Port    string `json:"port"`
			LogPath string `json:"logPath"`
		}
		if json.Unmarshal(raw, &prev) == nil {
			opts.Port = prev.Port
			opts.LogPath = prev.LogPath
		}
	}
	mgr, err := openPlatformServiceManager(opts)
	if err != nil {
		logf("service refresh skipped: open manager: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := mgr.Status(ctx)
	if err != nil {
		logf("service refresh skipped: status failed: %v", err)
		return
	}
	if !st.Registered || !st.Stale {
		return
	}
	if st.Enabled {
		if _, err := mgr.Enable(ctx); err != nil {
			logf("service refresh (enable) failed: %v", err)
			return
		}
		logf("refreshed stale login-enabled service definition to buildId=%s", health.BuildID)
		return
	}
	if _, err := mgr.Register(ctx); err != nil {
		logf("service refresh (register) failed: %v", err)
		return
	}
	logf("refreshed stale manual service definition to buildId=%s", health.BuildID)
}

func recordApplySuccess(plan ApplyPlan, health daemonHealth) {
	h := loadHistory(plan.StateDir)
	now := time.Now().UTC().Format(time.RFC3339)
	if plan.Mode == "rollback" {
		_ = os.RemoveAll(filepath.Join(plan.StateDir, rollbackDirName, rollbackPrevName))
		h.LastRollback = &RollbackRecord{
			Automatic:   false,
			FromVersion: plan.PrevVersion,
			FromBuildID: plan.PrevBuildID,
			ToVersion:   health.Version,
			ToBuildID:   health.BuildID,
			Reason:      "manual_rollback",
			At:          now,
		}
	} else {
		h.LastApplied = &RollbackRecord{
			Automatic:   false,
			FromVersion: plan.PrevVersion,
			FromBuildID: plan.PrevBuildID,
			ToVersion:   health.Version,
			ToBuildID:   health.BuildID,
			At:          now,
		}
	}
	_ = saveHistory(plan.StateDir, h)
}

func recordApplyFailure(plan ApplyPlan, reason string) {
	h := loadHistory(plan.StateDir)
	h.LastRollback = &RollbackRecord{
		Automatic:   true,
		FromVersion: plan.TargetVersion,
		ToVersion:   plan.PrevVersion,
		ToBuildID:   plan.PrevBuildID,
		Reason:      reason,
		At:          time.Now().UTC().Format(time.RFC3339),
	}
	_ = saveHistory(plan.StateDir, h)
}

type daemonHealth struct {
	Alive   bool
	PID     int
	Version string
	BuildID string
}

func probeDaemonHealth(stateDir string) (daemonHealth, error) {
	transport := platform.HTTPTransport{Timeout: 2 * time.Second}
	ep, err := transport.Endpoint(stateDir)
	if err != nil {
		return daemonHealth{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL+"/api/health", nil)
	if err != nil {
		return daemonHealth{}, err
	}
	req.Header.Set("Authorization", "Bearer "+ep.Key)
	client := http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return daemonHealth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daemonHealth{}, fmt.Errorf("health HTTP %d", resp.StatusCode)
	}
	var payload struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
		BuildID string `json:"buildId"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&payload); err != nil {
		return daemonHealth{}, err
	}
	if payload.PID != ep.PID {
		return daemonHealth{}, errors.New("health pid mismatch")
	}
	return daemonHealth{
		Alive:   true,
		PID:     payload.PID,
		Version: payload.Version,
		BuildID: payload.BuildID,
	}, nil
}

func waitForUpdatedHealth(plan ApplyPlan, timeout time.Duration) (daemonHealth, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h, err := probeDaemonHealth(plan.StateDir)
		if err == nil && h.Alive {
			if plan.DaemonPID > 0 && h.PID == plan.DaemonPID {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if plan.TargetVersion != "" && strings.TrimPrefix(h.Version, "v") != strings.TrimPrefix(plan.TargetVersion, "v") {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if plan.TargetBuildID != "" && h.BuildID != plan.TargetBuildID {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if plan.Mode == "update" && plan.PrevBuildID != "" && h.BuildID == plan.PrevBuildID {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return h, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return daemonHealth{}, fmt.Errorf("new version %s did not become healthy within %s", plan.TargetVersion, timeout)
}

func stopRunningDaemon(stateDir string, prevPID int, logf func(string, ...any)) bool {
	transport := platform.HTTPTransport{Timeout: 2 * time.Second}
	endpointPID := 0
	endpointFound := false
	stillAlive := false
	if ep, err := transport.Endpoint(stateDir); err == nil {
		endpointFound = true
		endpointPID = ep.PID
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		_ = transport.Stop(ctx, ep)
		cancel()
		stillAlive = true
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			probeCtx, probeCancel := context.WithTimeout(context.Background(), time.Second)
			res := transport.Health(probeCtx, ep)
			probeCancel()
			if !res.Alive {
				stillAlive = false
				break
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
	if prevPID > 0 && !waitForProcessExit(prevPID, 5*time.Second) {
		stillAlive = true
	}
	if endpointPID > 0 && endpointPID != prevPID && !waitForProcessExit(endpointPID, 5*time.Second) {
		stillAlive = true
	}
	if !endpointFound && prevPID <= 0 {
		logf("no daemon endpoint or pid to stop gracefully")
		return false
	}
	logf("previous daemon stopped (planPid=%d endpointPid=%d graceful=%t)", prevPID, endpointPID, !stillAlive)
	return !stillAlive
}

func openApplyLog(stateDir string) *os.File {
	if stateDir == "" {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "update-apply.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
