//go:build !darwin && !windows

package kimi

import (
	"context"
	"errors"
)

func requestApplicationQuit(context.Context, int) error {
	return errors.New("kimi_quit_unavailable")
}
