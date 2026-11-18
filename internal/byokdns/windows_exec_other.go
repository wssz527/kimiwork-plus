//go:build !windows

package byokdns

import "os/exec"

func hideElevationConsole(*exec.Cmd) {}
