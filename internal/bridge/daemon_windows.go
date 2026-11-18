//go:build windows

package bridge

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func hideDaemonConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow,
		HideWindow:    true,
	}
}
