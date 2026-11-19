//go:build !windows

package platform

import "context"

func runPlatformRegistry(context.Context, string, []string) (bool, string, error) {
	return false, "", nil
}
