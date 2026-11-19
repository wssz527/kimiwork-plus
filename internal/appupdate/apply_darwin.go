//go:build darwin

package appupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wssz527/kimi-work-plus/internal/platform"
)

func configureDetachedHelper(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func stopTargetDesktop(targetApp string, logf func(string, ...any)) {
	desktopBin := filepath.Join(targetApp, "Contents", "MacOS", "KimiWorkPlus")
	pids := findProcessPIDsForBinary(desktopBin)
	if len(pids) == 0 {
		logf("no running desktop processes matched %s", desktopBin)
		return
	}
	logf("stopping %d desktop process(es) for %s: %v", len(pids), targetApp, pids)
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pids = findProcessPIDsForBinary(desktopBin)
		if len(pids) == 0 {
			logf("stopped desktop processes cleanly via SIGTERM for %s", targetApp)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	logf("desktop process(es) still alive after SIGTERM, sending SIGKILL: %v", pids)
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pids = findProcessPIDsForBinary(desktopBin)
		if len(pids) == 0 {
			logf("stopped desktop processes via SIGKILL for %s", targetApp)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	logf("warning: desktop process(es) remained alive after SIGKILL: %v", pids)
}

func findProcessPIDsForBinary(binaryPath string) []int {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	candidates := []string{filepath.Clean(binaryPath)}
	if resolved, err := filepath.EvalSymlinks(binaryPath); err == nil && resolved != "" {
		cleanResolved := filepath.Clean(resolved)
		if cleanResolved != candidates[0] {
			candidates = append(candidates, cleanResolved)
		}
	}
	seen := map[int]bool{}
	var pids []int
	for _, target := range candidates {
		for _, pid := range matchingBinaryPIDs(string(out), target, os.Getpid()) {
			if !seen[pid] {
				seen[pid] = true
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

func matchingBinaryPIDs(psOutput, binaryPath string, selfPID int) []int {
	if binaryPath == "" {
		return nil
	}
	cleanTarget := filepath.Clean(binaryPath)
	var pids []int
	for _, line := range strings.Split(psOutput, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		sp := strings.IndexAny(trimmed, " \t")
		if sp <= 0 {
			continue
		}
		pid := 0
		if _, err := fmt.Sscanf(trimmed[:sp], "%d", &pid); err != nil || pid <= 0 || pid == selfPID {
			continue
		}
		cmdLine := strings.TrimLeft(trimmed[sp:], " \t")
		if cmdLine == cleanTarget || strings.HasPrefix(cmdLine, cleanTarget+" ") || strings.HasPrefix(cmdLine, cleanTarget+"\t") {
			pids = append(pids, pid)
		}
	}
	return pids
}

func validateStagedArtifact(stagedApp string) error {
	return validateMacAppBundle(stagedApp)
}

func validateInstalledArtifact(installedApp string) error {
	return validateMacAppBundle(installedApp)
}

func validateMacAppBundle(appPath string) error {
	if !strings.HasSuffix(appPath, ".app") {
		return fmt.Errorf("expected .app bundle, got %s", appPath)
	}
	for _, rel := range []string{
		"Contents/Info.plist",
		"Contents/MacOS/KimiWorkPlus",
		"Contents/MacOS/gateway-daemon",
	} {
		info, err := os.Stat(filepath.Join(appPath, rel))
		if err != nil || info.IsDir() || info.Size() == 0 {
			return fmt.Errorf("incomplete app bundle: missing %s", rel)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", appPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codesign verify failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func swapInstallation(targetApp, stagedApp, _ string, logf func(string, ...any)) error {
	if !strings.HasSuffix(targetApp, ".app") {
		return errors.New("darwin update target must be a .app bundle")
	}
	// Strip quarantine if any ever appeared; Go downloads do not add quarantine.
	_ = exec.Command("/usr/bin/xattr", "-dr", "com.apple.quarantine", stagedApp).Run()

	parent := filepath.Dir(targetApp)
	stagingSibling := filepath.Join(parent, fmt.Sprintf(".kwp-update-next-%d.app", time.Now().UnixNano()))
	prevSibling := filepath.Join(parent, fmt.Sprintf(".kwp-update-prev-%d.app", time.Now().UnixNano()))

	// Copy/ditto into the same volume first so the final swap is an atomic rename.
	if out, err := exec.Command("/usr/bin/ditto", stagedApp, stagingSibling).CombinedOutput(); err != nil {
		_ = os.RemoveAll(stagingSibling)
		return fmt.Errorf("stage sibling copy failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("/usr/bin/xattr", "-dr", "com.apple.quarantine", stagingSibling).Run()

	if err := os.Rename(targetApp, prevSibling); err != nil {
		_ = os.RemoveAll(stagingSibling)
		return fmt.Errorf("move current app aside: %w", err)
	}
	if err := os.Rename(stagingSibling, targetApp); err != nil {
		_ = os.Rename(prevSibling, targetApp)
		_ = os.RemoveAll(stagingSibling)
		return fmt.Errorf("install new app: %w", err)
	}
	_ = os.RemoveAll(prevSibling)
	logf("macOS bundle swapped cleanly at %s", targetApp)
	return nil
}

func restoreInstallation(targetApp, backupApp string, logf func(string, ...any)) error {
	parent := filepath.Dir(targetApp)
	restoredSibling := filepath.Join(parent, fmt.Sprintf(".kwp-restore-%d.app", time.Now().UnixNano()))
	brokenSibling := filepath.Join(parent, fmt.Sprintf(".kwp-broken-%d.app", time.Now().UnixNano()))

	if out, err := exec.Command("/usr/bin/ditto", backupApp, restoredSibling).CombinedOutput(); err != nil {
		_ = os.RemoveAll(restoredSibling)
		return fmt.Errorf("stage rollback copy failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	_ = os.Rename(targetApp, brokenSibling)
	if err := os.Rename(restoredSibling, targetApp); err != nil {
		_ = os.Rename(brokenSibling, targetApp)
		_ = os.RemoveAll(restoredSibling)
		return err
	}
	_ = os.RemoveAll(brokenSibling)
	logf("macOS bundle restored from %s to %s", backupApp, targetApp)
	return nil
}

func launchInstallation(targetApp, stateDir, launchMode string, logf func(string, ...any)) error {
	defaultDir, _ := platform.DefaultStateDir()
	customState := stateDir != "" && stateDir != defaultDir

	if launchMode == "daemon" {
		daemonBin := filepath.Join(targetApp, "Contents", "MacOS", "gateway-daemon")
		args := []string{}
		if stateDir != "" {
			args = append(args, "--state-dir", stateDir)
		}
		cmd := exec.Command(daemonBin, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		_ = cmd.Process.Release()
		logf("launched macOS daemon directly: %s", daemonBin)
		return nil
	}

	args := []string{"-n"}
	if feed := strings.TrimSpace(os.Getenv("KIMI_WORK_PLUS_UPDATE_FEED_URL")); feed != "" {
		args = append(args, "--env", "KIMI_WORK_PLUS_UPDATE_FEED_URL="+feed)
	}
	args = append(args, targetApp)
	if customState {
		args = append(args, "--args", "--state-dir", stateDir)
	}
	cmd := exec.Command("/usr/bin/open", args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	logf("launched macOS app bundle: %s", targetApp)
	return nil
}

func postInstallUpdateMetadata(_ ApplyPlan, _ daemonHealth) {}
