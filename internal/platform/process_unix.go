//go:build !windows

package platform

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether the pid is a running process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processExecutable reads the executable a pid runs. A process this user may not
// inspect answers false, which keeps it out of the termination path: the
// alternative is ending a process this build cannot identify.
func processExecutable(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	path, err := os.Readlink("/proc/" + itoa(pid) + "/exe")
	if err != nil {
		return "", false
	}
	return path, true
}
