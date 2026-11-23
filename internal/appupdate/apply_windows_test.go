//go:build windows

package appupdate

import (
	"strings"
	"testing"
)

func TestLaunchSysProcAttrPreservesDesktopWindowVisibility(t *testing.T) {
	desktop := launchSysProcAttr("")
	if desktop.HideWindow {
		t.Fatal("desktop relaunch must not set HideWindow=true")
	}
	if desktop.CreationFlags != detachedProcess {
		t.Fatalf("desktop CreationFlags = 0x%x, want 0x%x", desktop.CreationFlags, detachedProcess)
	}

	daemon := launchSysProcAttr("daemon")
	if !daemon.HideWindow {
		t.Fatal("daemon relaunch should hide console window")
	}
	if daemon.CreationFlags != createNoWindow {
		t.Fatalf("daemon CreationFlags = 0x%x, want 0x%x", daemon.CreationFlags, createNoWindow)
	}
}

func TestBuildWindowsStopProcessScriptSeparatesDesktopAndDaemonWithTrailingBackslash(t *testing.T) {
	desktopScript := buildWindowsStopProcessScript(`C:\Users\alice\AppData\Local\Programs\Kimi Work+`, "KimiWorkPlus")
	if !strings.Contains(desktopScript, `$dir = 'C:\Users\alice\AppData\Local\Programs\Kimi Work+\'`) {
		t.Fatalf("expected trailing backslash on normalized dir: %s", desktopScript)
	}
	if strings.Contains(desktopScript, "gateway-daemon") {
		t.Fatalf("desktop stop script must not kill gateway-daemon prematurely: %s", desktopScript)
	}
	if !strings.Contains(desktopScript, "'KimiWorkPlus'") {
		t.Fatalf("desktop stop script missing KimiWorkPlus target: %s", desktopScript)
	}

	daemonScript := buildWindowsStopProcessScript(`C:\Users\alice\AppData\Local\Programs\Kimi Work+\`, "gateway-daemon")
	if !strings.Contains(daemonScript, `$dir = 'C:\Users\alice\AppData\Local\Programs\Kimi Work+\'`) {
		t.Fatalf("expected single trailing backslash on normalized dir: %s", daemonScript)
	}
	if !strings.Contains(daemonScript, "'gateway-daemon'") {
		t.Fatalf("daemon stop script missing gateway-daemon target: %s", daemonScript)
	}
}
