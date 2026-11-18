//go:build windows

package platform

import (
	"errors"
	"syscall"
	"unsafe"
)

var procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")

const (
	processQueryLimitedInformation = 0x1000
	processSynchronize             = 0x00100000
	stillActiveCode                = 259
)

// processAlive reports whether the pid is a running process.
func processAlive(pid int) bool {
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
	return code == stillActiveCode
}

// processExecutable reads the full Win32 image path of a pid. A pid whose path
// cannot be read is not a candidate for termination: the alternative is ending a
// process this build cannot identify.
func processExecutable(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer syscall.CloseHandle(handle)
	const limit = 32768
	size := uint32(1024)
	for size <= limit {
		buffer := make([]uint16, size)
		current := size
		ok, _, callErr := procQueryFullProcessImageNameW.Call(uintptr(handle), 0,
			uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&current)))
		if ok != 0 {
			return syscall.UTF16ToString(buffer[:current]), true
		}
		if !errors.Is(callErr, syscall.ERROR_INSUFFICIENT_BUFFER) {
			return "", false
		}
		size *= 2
	}
	return "", false
}
