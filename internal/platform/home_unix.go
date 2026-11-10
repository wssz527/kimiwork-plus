//go:build !windows

package platform

import "os"

// homeDirectory is the current user's home, from the environment the process was
// started with. It is used only to render a definition path, never to decide
// trust.
func homeDirectory() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return "/"
}

// currentUID is the numeric user id for launchd's gui/<uid> domain.
func currentUID() string {
	return itoa(os.Getuid())
}
