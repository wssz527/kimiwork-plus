//go:build windows

package kimi

import (
	"syscall"
	"unsafe"
)

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var procGetProcessMemoryInfo = kernel32Identity.NewProc("K32GetProcessMemoryInfo")

// Query-limited access is the documented requirement for
// K32GetProcessMemoryInfo since Windows 7; a VM_READ handle is not needed for
// the counters and would only widen what this probe may open.
func residentBytesPlatform(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(handle)

	var counters processMemoryCounters
	counters.CB = uint32(unsafe.Sizeof(counters))
	ok, _, _ := procGetProcessMemoryInfo.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&counters)),
		uintptr(counters.CB),
	)
	if ok == 0 || counters.WorkingSetSize == 0 {
		return 0, false
	}
	return int64(counters.WorkingSetSize), true
}
