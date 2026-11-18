//go:build windows

package byokdns

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func hideElevationConsole(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow,
		HideWindow:    true,
	}
}
