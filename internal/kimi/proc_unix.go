//go:build !windows

package kimi

import (
	"errors"
	"syscall"
)

// syscallSignalZero is the portable liveness probe: it reports existence without
// delivering anything.
func syscallSignalZero() syscall.Signal { return syscall.Signal(0) }

func isProcessGone(err error) bool { return errors.Is(err, syscall.ESRCH) }
