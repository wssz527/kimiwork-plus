//go:build !windows

package kimi

func residentBytesPlatform(int) (int64, bool) { return 0, false }
