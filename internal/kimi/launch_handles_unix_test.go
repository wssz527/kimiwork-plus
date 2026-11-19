//go:build !windows

package kimi

import "os/exec"

// inheritedHandles reports how many handles the child would inherit outside the
// standard streams. On Unix the only channel is ExtraFiles, and the Windows-only
// handle list does not exist.
func inheritedHandles(*exec.Cmd) int { return 0 }

// managedChildWindowHidden has no equivalent on Unix: the child is a GUI
// application and nothing suppresses its window.
func managedChildWindowHidden(*exec.Cmd) bool { return false }
