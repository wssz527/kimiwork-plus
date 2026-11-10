//go:build windows

package platform

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
	errorLockViolation      = syscall.Errno(33)
)

// lockFile uses LockFileEx with LOCKFILE_FAIL_IMMEDIATELY, the documented
// Windows equivalent of a non-blocking exclusive flock. The lock is released by
// the kernel when the handle closes, including on process death.
func lockFile(file *os.File) error {
	var overlapped syscall.Overlapped
	status, _, err := procLockFileEx.Call(file.Fd(), lockfileExclusiveLock|lockfileFailImmediately,
		0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if status != 0 {
		return nil
	}
	if err == nil {
		err = errorLockViolation
	}
	return err
}

func unlockFile(file *os.File) error {
	var overlapped syscall.Overlapped
	status, _, err := procUnlockFileEx.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if status != 0 {
		return nil
	}
	return err
}

func isWouldBlock(err error) bool {
	return errors.Is(err, errorLockViolation) || errors.Is(err, syscall.ERROR_IO_PENDING)
}
