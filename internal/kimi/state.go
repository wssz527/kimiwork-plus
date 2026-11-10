package kimi

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DetectState answers the one question the panel asks about the official
// application: is the installed Kimi running right now?
//
// It is strictly read-only. Nothing here starts, stops, signals or focuses the
// application, and no window or page is touched. `unknown` is a first-class
// answer: a check that could not complete is never reported as not running,
// because the panel uses this to decide whether a takeover is needed and a
// false "not running" would be the one mistake that matters.
func DetectState(ctx context.Context, options ProbeOptions) NativeState {
	platform := options.platform()
	state := NativeState{Platform: platform, Application: RunningStateUnknown}

	installations := Detect(platform, options.env())
	if len(installations) == 0 {
		state.Application = NotRunning
		state.Reason = "not_installed"
		return state
	}
	installation := installations[0]
	if options.Archive != "" {
		for _, candidate := range installations {
			if samePath(candidate.Archive, options.Archive, platform) {
				installation = candidate
				break
			}
		}
	}
	state.Installation = &installation
	if installation.Application == "" {
		state.Reason = "application_not_found"
		return state
	}

	pids, known, reason := processPIDs(ctx, installation.Application)
	if !known {
		state.Reason = reason
		return state
	}
	state.PIDs = pids
	if len(pids) == 0 {
		state.Application = NotRunning
		state.Reason = "no_matching_process"
	} else {
		state.Application = Running
	}

	// The contract probe only reads the archive. It is attached here because
	// the panel shows compatibility next to the running state, and a separate
	// call would let the two answers disagree.
	contract := Inspect(ctx, ProbeOptions{Archive: installation.Archive, Platform: platform,
		Env: options.env(), Executor: options.Executor})
	state.Contract = &contract
	launch := InspectLaunchContract(ctx, ProbeOptions{Archive: installation.Archive, Platform: platform,
		Env: options.env(), Executor: options.Executor})
	state.Launch = &launch
	return state
}

// processPIDs lists the pids whose executable is exactly this path. It answers
// `known=false` when the platform cannot tell, which the caller must surface as
// unknown rather than as "no process".
func processPIDs(ctx context.Context, application string) (pids []int, known bool, reason string) {
	switch runtime.GOOS {
	case "darwin":
		return darwinPIDs(ctx, application)
	case "windows":
		return windowsPIDs(ctx, application)
	}
	return nil, false, "unsupported_platform"
}

func darwinPIDs(ctx context.Context, application string) ([]int, bool, string) {
	command := execCommandContext(ctx, "ps", "-axo", "pid=,command=")
	command.Env = append(command.Environ(), "LC_ALL=C")
	out, err := command.Output()
	if err != nil {
		return nil, false, "process_query_failed"
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := parseSmallInt(fields[0])
		if err != nil {
			continue
		}
		executable := strings.Join(fields[1:], " ")
		if samePath(filepath.Clean(executable), filepath.Clean(application), "darwin") {
			pids = append(pids, pid)
		}
	}
	return pids, true, ""
}

// windowsPIDs asks for processes by name and then compares the full path, so a
// same-named executable elsewhere on disk is not counted as the installation.
func windowsPIDs(ctx context.Context, application string) ([]int, bool, string) {
	script := `$ErrorActionPreference = 'Stop'; foreach ($process in @(Get-Process -Name 'Kimi','kimi-desktop' -ErrorAction SilentlyContinue)) { try { [Console]::Out.Write($process.Id.ToString([Globalization.CultureInfo]::InvariantCulture) + "` + "\t" + `" + $process.Path + [Environment]::NewLine) } catch {} }`
	command := execCommandContext(ctx, "powershell.exe",
		"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := command.Output()
	if err != nil {
		return nil, false, "process_query_failed"
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		pid, err := parseSmallInt(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		if samePath(filepath.Clean(strings.TrimSpace(parts[1])), filepath.Clean(application), "windows") {
			pids = append(pids, pid)
		}
	}
	return pids, true, ""
}

// IdentityProbeTimeout bounds the identity lookups; a hung `ps` or PowerShell
// must not hold the panel.
const IdentityProbeTimeout = 5 * time.Second

// WithIdentityTimeout is the convenience wrapper for callers that have no
// context of their own.
func WithIdentityTimeout(application string) ([]int, bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), IdentityProbeTimeout)
	defer cancel()
	return processPIDs(ctx, application)
}

var execCommandContext = exec.CommandContext
