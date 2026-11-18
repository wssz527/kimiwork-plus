//go:build !windows

package bridge

import "os/exec"

func hideDaemonConsole(*exec.Cmd) {}
