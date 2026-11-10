//go:build windows

package platform

import "os"

func homeDirectory() string {
	if home := os.Getenv("USERPROFILE"); home != "" {
		return home
	}
	return `C:\`
}

// currentUID has no meaning on Windows; the scheduled task is per-user already.
func currentUID() string { return "" }
