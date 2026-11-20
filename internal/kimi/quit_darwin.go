//go:build darwin

package kimi

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

func filterQuitRootPIDs(pids []int) []int { return pids }

func requestApplicationQuit(ctx context.Context, pid int) error {
	script := `ObjC.import("AppKit"); const app = $.NSRunningApplication.runningApplicationWithProcessIdentifier(` + strconv.Itoa(pid) + `); if (!app || app.isNil()) { "missing" } else { String(app.terminate) }`
	output, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", script).Output()
	if ctx.Err() != nil {
		return errors.New("kimi_quit_timeout")
	}
	if err != nil {
		return errors.New("kimi_quit_unavailable")
	}
	switch strings.TrimSpace(string(output)) {
	case "missing", "true":
		return nil
	default:
		return errors.New("kimi_quit_refused")
	}
}
