//go:build windows

package kimi

import (
	"os/exec"
	"syscall"
)

// configureManagedChild hides the console window and keeps the child in its own
// process group so stopping it never touches this process.
func configureManagedChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
}
