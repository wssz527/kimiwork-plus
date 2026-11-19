//go:build windows

package kimi

import (
	"context"
	"errors"
	"strconv"
	"syscall"
)

const errorInvalidParameter = syscall.Errno(87)

// windowsIdentity asks the kernel for the process creation time. Opening the
// process and reading its times are two calls with no child process, so no
// console host, PowerShell policy or COM object is involved, and the value is
// the same FILETIME tick count the previous probe recorded.
func windowsIdentity(ctx context.Context, pid int) ProcessIdentity {
	if ctx.Err() != nil {
		return ProcessIdentity{Kind: IdentityUnknown, Reason: "query_failed"}
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		if errors.Is(err, errorInvalidParameter) {
			return ProcessIdentity{Kind: IdentityNotFound}
		}
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return ProcessIdentity{Kind: IdentityUnknown, Reason: "permission_denied"}
		}
		return ProcessIdentity{Kind: IdentityUnknown, Reason: "query_failed"}
	}
	defer syscall.CloseHandle(handle)

	if !processAlivePlatform(pid) {
		return ProcessIdentity{Kind: IdentityNotFound}
	}
	var creation, exitTime, kernelTime, userTime syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &creation, &exitTime, &kernelTime, &userTime); err != nil {
		return ProcessIdentity{Kind: IdentityUnknown, Reason: "query_failed"}
	}
	ticks := (uint64(creation.HighDateTime) << 32) | uint64(creation.LowDateTime)
	value := strconv.FormatUint(ticks, 10)
	if !validFiletime(value) {
		return ProcessIdentity{Kind: IdentityUnknown, Reason: "invalid_output"}
	}
	return ProcessIdentity{Kind: IdentityWindowsFT, Value: value}
}
