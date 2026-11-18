//go:build windows

package kimi

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	smtoAbortIfHung   = 0x0002
)

var (
	user32Quit                   = syscall.NewLazyDLL("user32.dll")
	procFindWindowExW            = user32Quit.NewProc("FindWindowExW")
	procGetWindowThreadProcessId = user32Quit.NewProc("GetWindowThreadProcessId")
	procSendMessageTimeoutW      = user32Quit.NewProc("SendMessageTimeoutW")
)

func requestApplicationQuit(ctx context.Context, pid int) error {
	if ctx.Err() != nil {
		return errors.New("kimi_quit_timeout")
	}
	if pid <= 0 {
		return errors.New("kimi_quit_refused")
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation|processSynchronize, false, uint32(pid))
	if err != nil {
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return errors.New("kimi_quit_refused")
		}
		return nil
	}
	defer syscall.CloseHandle(handle)

	if processExited(handle) {
		return nil
	}
	if isKimiHelperChild(uint32(pid)) {
		return nil
	}

	hwnds := collectProcessWindows(uint32(pid))
	if len(hwnds) == 0 {
		if processExited(handle) {
			return nil
		}
		return errors.New("kimi_quit_refused")
	}
	for _, hwnd := range hwnds {
		if ctx.Err() != nil {
			return errors.New("kimi_quit_timeout")
		}
		if processExited(handle) {
			return nil
		}
		timeoutMs := quitMessageTimeoutMs(ctx)
		var result uintptr
		_, _, _ = procSendMessageTimeoutW.Call(hwnd, wmQueryEndSession, 0, 0, smtoAbortIfHung, timeoutMs, uintptr(unsafe.Pointer(&result)))
		_, _, _ = procSendMessageTimeoutW.Call(hwnd, wmEndSession, 1, 0, smtoAbortIfHung, timeoutMs, uintptr(unsafe.Pointer(&result)))
	}
	return nil
}

func processExited(handle syscall.Handle) bool {
	state, err := syscall.WaitForSingleObject(handle, 0)
	return err == nil && state == uint32(syscall.WAIT_OBJECT_0)
}

func quitMessageTimeoutMs(ctx context.Context) uintptr {
	const maxMs = 2000
	deadline, ok := ctx.Deadline()
	if !ok {
		return maxMs
	}
	remaining := time.Until(deadline).Milliseconds()
	if remaining <= 100 {
		return 100
	}
	if remaining > maxMs {
		return maxMs
	}
	return uintptr(remaining)
}

func isKimiHelperChild(targetPID uint32) bool {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(snapshot)

	names := make(map[uint32]string, 64)
	var parentPID uint32
	var found bool

	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = syscall.Process32First(snapshot, &entry); err == nil; err = syscall.Process32Next(snapshot, &entry) {
		names[entry.ProcessID] = strings.ToLower(syscall.UTF16ToString(entry.ExeFile[:]))
		if entry.ProcessID == targetPID {
			parentPID = entry.ParentProcessID
			found = true
		}
	}
	if !found || parentPID == 0 || parentPID == targetPID {
		return false
	}
	parentName := names[parentPID]
	if parentName != "kimi.exe" && parentName != "kimi-desktop.exe" {
		return false
	}
	return processAlivePlatform(int(parentPID))
}

func collectProcessWindows(targetPID uint32) []uintptr {
	seen := make(map[uintptr]struct{})
	var windows []uintptr
	for _, parent := range []uintptr{0, ^uintptr(2)} {
		var current uintptr
		for range 4096 {
			next, _, _ := procFindWindowExW.Call(parent, current, 0, 0)
			if next == 0 {
				break
			}
			current = next
			var ownerPID uint32
			_, _, _ = procGetWindowThreadProcessId.Call(next, uintptr(unsafe.Pointer(&ownerPID)))
			if ownerPID != targetPID {
				continue
			}
			if _, exists := seen[next]; exists {
				continue
			}
			seen[next] = struct{}{}
			windows = append(windows, next)
		}
	}
	return windows
}
