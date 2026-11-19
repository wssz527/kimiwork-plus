//go:build windows

package appupdate

import "testing"

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
