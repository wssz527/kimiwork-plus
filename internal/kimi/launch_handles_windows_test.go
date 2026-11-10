//go:build windows

package kimi

import "os/exec"

// inheritedHandles reports how many handles the child would inherit outside the
// standard streams. On Windows that is exactly the list Go passes to
// PROC_THREAD_ATTRIBUTE_HANDLE_LIST.
func inheritedHandles(command *exec.Cmd) int {
	if command.SysProcAttr == nil {
		return 0
	}
	return len(command.SysProcAttr.AdditionalInheritedHandles)
}
