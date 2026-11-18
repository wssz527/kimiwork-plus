//go:build !windows

package platform

import (
	"os/exec"
	"runtime"
	"syscall"
)

// detach puts the helper in its own session so it survives the termination of the
// daemon that spawned it. Setsid is the portable Unix answer; on macOS it is the
// same call launchd jobs are expected to honour.
func detach(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = runtime.GOOS
}

func hideCommandConsole(*exec.Cmd) {}

func isWindows() bool { return false }
