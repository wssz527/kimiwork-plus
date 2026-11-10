//go:build !windows

package kimi

import (
	"os/exec"
	"syscall"
)

// configureChild puts the worker in its own process group so a timeout kills
// the whole group. Without this a script that spawns nothing would still be
// fine, but an interrupted child must never outlive its parent's budget.
func configureChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * 1000 * 1000 * 1000
}
