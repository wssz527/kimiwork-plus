//go:build !windows

package platform

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes a non-blocking exclusive flock. The kernel releases it when the
// process exits, so a crash never leaves the state directory locked.
func lockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func isWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
