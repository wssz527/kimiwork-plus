//go:build windows

package kimi

import (
	"os/exec"
	"strconv"
	"strings"
)

// windowsResidentBytes uses the documented PowerShell working-set counter.
// `Get-Process` exists on every supported Windows build and needs no elevation
// for a process the same user started.
func residentBytesPlatform(pid int) (int64, bool) {
	out, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command",
		"[Console]::Out.Write((Get-Process -Id "+strconv.Itoa(pid)+" -ErrorAction Stop).WorkingSet64)").Output()
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}
