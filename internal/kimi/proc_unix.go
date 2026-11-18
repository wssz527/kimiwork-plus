//go:build !windows

package kimi

import (
	"errors"
	"os/exec"
	"syscall"
)

func hideBackgroundCommand(*exec.Cmd) {}

func processAlivePlatform(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
