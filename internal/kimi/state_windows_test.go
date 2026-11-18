//go:build windows

package kimi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A machine where the official application is not running is a normal answer.
// The previous PowerShell probe exited non-zero in exactly that case, which the
// panel rendered as an unknown state and which blocked the first launch.
func TestWindowsProbeReportsNoProcessAsKnown(t *testing.T) {
	pids, known, reason := processPIDs(context.Background(), filepath.Join(t.TempDir(), "Kimi.exe"))
	if !known {
		t.Fatalf("a query for an absent executable was not known: %q", reason)
	}
	if reason != "" {
		t.Fatalf("an absent executable carried reason %q", reason)
	}
	if len(pids) != 0 {
		t.Fatalf("an absent executable matched %v", pids)
	}
	// The same answer must come from the installed path when nothing runs: that
	// is the state a first launch starts from.
	for _, installation := range Detect("windows", nil) {
		pids, known, reason = processPIDs(context.Background(), installation.Application)
		if !known || reason != "" {
			t.Fatalf("probe of the installed application: known=%v reason=%q", known, reason)
		}
		if len(pids) != 0 {
			t.Logf("the official application is running as %v", pids)
		}
	}
}

// Detection must be exact: a same-named executable in another directory is a
// different installation, and a process that exited must stop being counted.
func TestWindowsProbeMatchesOnlyTheExactPath(t *testing.T) {
	base := t.TempDir()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "install")
	other := filepath.Join(base, "other")
	for _, target := range []string{dir, other} {
		if err = os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		data, readErr := os.ReadFile(source)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(target, "Kimi.exe"), data, 0o700); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	application := filepath.Join(dir, "Kimi.exe")
	decoy := filepath.Join(other, "Kimi.exe")
	child := exec.Command(application, "-test.run=TestWindowsProbeChildProcess")
	child.Env = append(os.Environ(), "KIMI_PROBE_CHILD=1")
	hideBackgroundCommand(child)
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	exited := false
	defer func() {
		if !exited {
			_ = child.Process.Kill()
			_, _ = child.Process.Wait()
		}
	}()
	waitForProcess(t, int32(pid), true)

	pids, known, reason := processPIDs(context.Background(), application)
	if !known || reason != "" {
		t.Fatalf("probe of a running executable: known=%v reason=%q", known, reason)
	}
	if len(pids) != 1 || pids[0] != pid {
		t.Fatalf("probe returned %v, want [%d]", pids, pid)
	}
	decoyPIDs, known, reason := processPIDs(context.Background(), decoy)
	if !known || reason != "" {
		t.Fatalf("probe of the other path: known=%v reason=%q", known, reason)
	}
	if len(decoyPIDs) != 0 {
		t.Fatalf("a same-named executable in another directory matched %v", decoyPIDs)
	}

	if err = child.Process.Kill(); err == nil {
		_, _ = child.Process.Wait()
		exited = true
	}
	waitForProcess(t, int32(pid), false)
	pids, known, reason = processPIDs(context.Background(), application)
	if !known || reason != "" {
		t.Fatalf("probe after exit: known=%v reason=%q", known, reason)
	}
	if len(pids) != 0 {
		t.Fatalf("an exited process was still counted: %v", pids)
	}
}

// TestWindowsProbeChildProcess is the child the probe test starts. It sleeps so
// the parent has a live process whose path it can compare.
func TestWindowsProbeChildProcess(t *testing.T) {
	if os.Getenv("KIMI_PROBE_CHILD") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(30 * time.Second)
}

func waitForProcess(t *testing.T, pid int32, alive bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if processAlivePlatform(int(pid)) == alive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d did not become alive=%v", pid, alive)
}
