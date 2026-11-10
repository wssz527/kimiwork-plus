//go:build windows

package kimi

import (
	"errors"
	"os"
	"syscall"
)

func syscallSignalZero() syscall.Signal { return syscall.Signal(0) }

func isProcessGone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ERROR_ACCESS_DENIED) == false && errors.Is(err, os.ErrNotExist)
}
