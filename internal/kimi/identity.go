package kimi

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// ProcessStartIdentity returns a platform-native proof of which process a pid
// is. A pid alone is not identity: it is recycled, and a record that only kept
// a number could match an unrelated program after a restart.
//
// Each platform uses the same source the official daemon uses:
//
//	darwin   `ps -p <pid> -o lstart=`
//	windows  Get-Process StartTime as FILETIME ticks
//
// Anything unreadable answers `unknown`, never a value that compares equal.
func ProcessStartIdentity(ctx context.Context, pid int) ProcessIdentity {
	if pid <= 0 {
		return ProcessIdentity{Kind: IdentityUnknown, Reason: "invalid_pid"}
	}
	switch runtime.GOOS {
	case "darwin":
		return darwinIdentity(ctx, pid)
	case "windows":
		return windowsIdentity(ctx, pid)
	}
	return ProcessIdentity{Kind: IdentityUnknown, Reason: "unsupported_platform"}
}

func darwinIdentity(ctx context.Context, pid int) ProcessIdentity {
	command := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	command.Env = append(command.Environ(), "LC_ALL=C")
	out, err := command.Output()
	value := strings.TrimSpace(string(out))
	if err != nil {
		if value == "" && command.ProcessState != nil && command.ProcessState.ExitCode() == 1 {
			return ProcessIdentity{Kind: IdentityNotFound}
		}
		return ProcessIdentity{Kind: IdentityUnknown, Reason: "query_failed"}
	}
	if value == "" {
		return ProcessIdentity{Kind: IdentityNotFound}
	}
	return ProcessIdentity{Kind: IdentityDarwinLStart, Value: value}
}

const (
	windowsNotFoundMarker     = "__DAIMON_PROCESS_NOT_FOUND__"
	windowsPermissionMarker   = "__DAIMON_PROCESS_PERMISSION_DENIED__"
	windowsFiletimeUnixOffset = 116444736000000000
)

// windowsIdentityScript is fixed text with one integer interpolated. It writes
// the creation time as FILETIME ticks so the value compares without locale or
// timezone luck.
func windowsIdentityScript(pid int) string {
	return `$ErrorActionPreference = 'Stop'; try { $process = Get-Process -Id ` + strconv.Itoa(pid) +
		` -ErrorAction Stop; [Console]::Out.Write($process.StartTime.ToUniversalTime().ToFileTimeUtc()` +
		`.ToString([Globalization.CultureInfo]::InvariantCulture)) } catch { if ($_.FullyQualifiedErrorId -like ` +
		`'NoProcessFoundForGivenId*') { [Console]::Error.Write('` + windowsNotFoundMarker + `'); exit 3 }; ` +
		`if (($_.Exception.NativeErrorCode -eq 5) -or ($_.Exception -is [System.UnauthorizedAccessException])) { ` +
		`[Console]::Error.Write('` + windowsPermissionMarker + `'); exit 4 }; exit 5 }`
}

func windowsIdentity(ctx context.Context, pid int) ProcessIdentity {
	command := exec.CommandContext(ctx, "powershell.exe",
		"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", windowsIdentityScript(pid))
	var stderr strings.Builder
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		if strings.Contains(stderr.String(), windowsNotFoundMarker) {
			return ProcessIdentity{Kind: IdentityNotFound}
		}
		if strings.Contains(stderr.String(), windowsPermissionMarker) {
			return ProcessIdentity{Kind: IdentityUnknown, Reason: "permission_denied"}
		}
		return ProcessIdentity{Kind: IdentityUnknown, Reason: "query_failed"}
	}
	value := strings.TrimSpace(string(out))
	if !validFiletime(value) {
		return ProcessIdentity{Kind: IdentityUnknown, Reason: "invalid_output"}
	}
	return ProcessIdentity{Kind: IdentityWindowsFT, Value: value}
}

// validFiletime accepts only a positive tick count that converts to a real
// Unix millisecond value: a syntactically valid but impossible count is not
// identity evidence.
func validFiletime(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	ticks, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ticks <= windowsFiletimeUnixOffset {
		return false
	}
	return true
}

// SameProcess reports whether a live pid still matches a recorded identity.
// `unknown` never compares equal: a check that could not run must not authorise
// acting on the process.
func SameProcess(ctx context.Context, pid int, recorded ProcessIdentity) bool {
	if recorded.Kind == IdentityUnknown || recorded.Kind == IdentityNotFound {
		return false
	}
	current := ProcessStartIdentity(ctx, pid)
	return current.Kind == recorded.Kind && current.Value != "" && current.Value == recorded.Value
}
