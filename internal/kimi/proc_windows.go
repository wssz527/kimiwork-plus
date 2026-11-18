//go:build windows

package kimi

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	processQueryLimitedInformation = 0x1000
	processSynchronize             = 0x00100000
	stillActiveExitCode            = 259

	// fullImageNameLimit is the documented maximum for a Win32 image path, and
	// the ceiling this probe stops growing its buffer at.
	fullImageNameLimit = 32768
)

var (
	kernel32Identity               = syscall.NewLazyDLL("kernel32.dll")
	procQueryFullProcessImageNameW = kernel32Identity.NewProc("QueryFullProcessImageNameW")
)

// platformWindowsPIDs enumerates processes through the Toolhelp32 snapshot and
// compares each candidate's full image path.
//
// It deliberately runs no child process: the panel polls this every few seconds,
// and a PowerShell probe answered "query failed" when neither candidate name was
// running, which read as an unknown state instead of "not running".
func platformWindowsPIDs(ctx context.Context, application string) ([]int, bool, string) {
	if ctx.Err() != nil {
		return nil, false, "process_query_failed"
	}
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, false, "process_query_failed"
	}
	defer syscall.CloseHandle(snapshot)

	wanted := strings.ToLower(filepath.Base(application))
	var pids []int
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = syscall.Process32First(snapshot, &entry); err == nil; err = syscall.Process32Next(snapshot, &entry) {
		name := strings.ToLower(syscall.UTF16ToString(entry.ExeFile[:]))
		if name != "kimi.exe" && name != "kimi-desktop.exe" && name != wanted {
			continue
		}
		// A process that exited between the snapshot and the open, or one this
		// user may not inspect, is skipped rather than failing the whole query.
		handle, openErr := syscall.OpenProcess(processQueryLimitedInformation, false, entry.ProcessID)
		if openErr != nil {
			continue
		}
		fullPath, ok := fullProcessImageName(handle)
		syscall.CloseHandle(handle)
		if !ok {
			continue
		}
		if samePath(filepath.Clean(fullPath), filepath.Clean(application), "windows") {
			pids = append(pids, int(entry.ProcessID))
		}
	}
	if err != nil && !errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
		return nil, false, "process_query_failed"
	}
	return pids, true, ""
}

// fullProcessImageName reads the Win32 image path, growing the buffer when the
// path does not fit.
func fullProcessImageName(handle syscall.Handle) (string, bool) {
	size := uint32(1024)
	for size <= fullImageNameLimit {
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
