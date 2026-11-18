//go:build windows

package kimi

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestWindowsProcessAliveTracksChildLifecycle(t *testing.T) {
	if !processAlivePlatform(os.Getpid()) {
		t.Fatal("current process reported as dead")
	}
	if processAlivePlatform(-1) || processAlivePlatform(0) {
		t.Fatal("invalid pid reported as alive")
	}
	child := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Milliseconds 150")
	hideBackgroundCommand(child)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	if !processAlivePlatform(pid) {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatalf("running child pid %d reported as dead", pid)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if processAlivePlatform(pid) {
		t.Fatalf("exited child pid %d still reported as alive", pid)
	}
}

func TestWindowsRequestApplicationQuitHandlesMissingAndCanceled(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := requestApplicationQuit(canceled, os.Getpid()); err == nil || err.Error() != "kimi_quit_timeout" {
		t.Fatalf("canceled quit = %v, want kimi_quit_timeout", err)
	}
	if err := requestApplicationQuit(context.Background(), 0); err == nil || err.Error() != "kimi_quit_refused" {
		t.Fatalf("zero pid quit = %v, want kimi_quit_refused", err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := requestApplicationQuit(ctx, 99999999); err != nil {
		t.Fatalf("missing pid quit = %v, want nil", err)
	}
}
