//go:build windows

package kimi

import (
	"errors"
	"os/exec"
	"syscall"
)

const (
	processQueryLimitedInformation = 0x1000
	processSynchronize             = 0x00100000
	stillActiveExitCode            = 259
)

func hideBackgroundCommand(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: createNoWindow,
			HideWindow:    true,
		}
		return
	}
	command.SysProcAttr.CreationFlags |= createNoWindow
	command.SysProcAttr.HideWindow = true
}

func processAlivePlatform(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation|processSynchronize, false, uint32(pid))
	if err == nil {
		defer syscall.CloseHandle(handle)
		state, waitErr := syscall.WaitForSingleObject(handle, 0)
		return waitErr == nil && state == uint32(syscall.WAIT_TIMEOUT)
	}
	if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		return false
	}
	handle, err = syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActiveExitCode
}
