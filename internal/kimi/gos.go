package kimi

import "runtime"

// platformGOOS is the build target, not a runtime guess.
func platformGOOS() string { return runtime.GOOS }
