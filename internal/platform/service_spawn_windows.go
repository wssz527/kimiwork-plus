//go:build windows

package platform

import (
	"os/exec"
	"syscall"
)

// Detaching matters here: the helper's job is to stop the daemon that started it.
// A child in the daemon's job object would be killed with its parent — exactly
// the failure this design removes.
//
// DETACHED_PROCESS gives the helper no console at all, and
// CREATE_NEW_PROCESS_GROUP keeps a Ctrl-Break aimed at the daemon from reaching
// it. CREATE_NO_WINDOW is deliberately not set: the API ignores it, and Windows
// documents it as unused together with DETACHED_PROCESS.
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

func detach(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
}

func hideCommandConsole(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

func isWindows() bool { return true }
