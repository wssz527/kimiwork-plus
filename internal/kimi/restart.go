package kimi

import (
	"context"
	"errors"
	"time"
)

var (
	listOfficialProcesses   = processPIDs
	officialProcessIdentity = ProcessStartIdentity
	selectQuitTargets       = filterQuitRootPIDs
	requestOfficialQuit     = requestApplicationQuit
)

func (c *Controller) RestartOfficial(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.mu.Lock()
	application := c.settings.Installation.Application
	c.mu.Unlock()
	if application == "" {
		return errors.New("kimi_application_missing")
	}
	if err := stopOfficialProcesses(ctx, application); err != nil {
		c.setState(StateFailed, err.Error())
		return err
	}
	return c.startOfficial(ctx)
}

func stopOfficialProcesses(ctx context.Context, application string) error {
	pids, known, reason := listOfficialProcesses(ctx, application)
	if !known {
		return errors.New(reason)
	}
	type target struct {
		pid      int
		identity ProcessIdentity
	}
	targets := make([]target, 0, len(pids))
	for _, pid := range pids {
		identity := officialProcessIdentity(ctx, pid)
		switch identity.Kind {
		case IdentityNotFound:
			continue
		case IdentityUnknown:
			return ErrIdentityUnknown
		}
		targets = append(targets, target{pid: pid, identity: identity})
	}
	quitPIDs := make([]int, 0, len(targets))
	for _, item := range targets {
		quitPIDs = append(quitPIDs, item.pid)
	}
	for _, pid := range selectQuitTargets(quitPIDs) {
		if err := requestOfficialQuit(ctx, pid); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for len(targets) > 0 {
		remaining := targets[:0]
		for _, item := range targets {
			current := officialProcessIdentity(ctx, item.pid)
			if ctx.Err() != nil {
				return errors.New("kimi_quit_timeout")
			}
			switch {
			case current.Kind == IdentityNotFound:
			case current.Kind == IdentityUnknown:
				return ErrIdentityUnknown
			case current.Kind == item.identity.Kind && current.Value == item.identity.Value:
				remaining = append(remaining, item)
			}
		}
		targets = remaining
		if len(targets) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("kimi_quit_timeout")
		case <-ticker.C:
		}
	}
	return nil
}
