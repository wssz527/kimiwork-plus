//go:build windows

package kimi

import "os/exec"

// configureChild bounds how long Wait waits for inherited descriptors to close.
// Windows has no process-group kill from Go's exec package, so the context
// deadline plus WaitDelay is the mechanism; the RSS watchdog still calls the
// context's cancel function directly.
func configureChild(command *exec.Cmd) {
	command.WaitDelay = 2 * 1000 * 1000 * 1000
}
