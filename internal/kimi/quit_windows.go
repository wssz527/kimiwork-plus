//go:build windows

package kimi

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

func requestApplicationQuit(ctx context.Context, pid int) error {
	script := `$p = Get-Process -Id ` + strconv.Itoa(pid) + ` -ErrorAction SilentlyContinue; if ($p) { [Console]::Out.Write([string]$p.CloseMainWindow()) } else { [Console]::Out.Write('missing') }`
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if ctx.Err() != nil {
		return errors.New("kimi_quit_timeout")
	}
	if err != nil {
		return errors.New("kimi_quit_unavailable")
	}
	switch strings.TrimSpace(string(output)) {
	case "missing", "True", "true":
		return nil
	default:
		return errors.New("kimi_quit_refused")
	}
}
