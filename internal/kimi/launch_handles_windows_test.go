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

// managedChildWindowHidden reports whether the child would start with its main
// window suppressed. The managed application is the official Kimi the user
// interacts with, so its window must be visible.
func managedChildWindowHidden(command *exec.Cmd) bool {
	return command.SysProcAttr != nil && command.SysProcAttr.HideWindow
}
