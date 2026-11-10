//go:build !windows

package kimi

import (
	"os/exec"
	"syscall"
)

// configureManagedChild puts the managed Kimi in its own process group so a
// stop request reaches it and not this process's group.
func configureManagedChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
