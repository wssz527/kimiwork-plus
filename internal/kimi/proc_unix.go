//go:build !windows

package kimi

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

func hideBackgroundCommand(*exec.Cmd) {}

// platformWindowsPIDs is the non-Windows stub. It exists so the Windows probe can
// live in its own build-tagged file, and it never claims to have looked.
func platformWindowsPIDs(context.Context, string) ([]int, bool, string) {
	return nil, false, "unsupported_platform"
}

func processAlivePlatform(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
