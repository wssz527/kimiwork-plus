package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

const autoTakeoverAction = "auto-takeover"

type autoTakeoverSnapshot struct {
	Enabled       bool   `json:"enabled"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	UpdatedAt     int64  `json:"updatedAt,omitempty"`
	LastSuccessAt int64  `json:"lastSuccessAt,omitempty"`
}

type autoTakeoverCoordinator struct {
	native      *Native
	now         func() time.Time
	managedPID  func(context.Context) int
	restartFn   func(context.Context) error
	graceWindow time.Duration
	cooldown    time.Duration

	mu                 sync.Mutex
	wg                 sync.WaitGroup
	stopped            bool
	cancel             context.CancelFunc
	snapshot           autoTakeoverSnapshot
	unmanagedSig       string
	unmanagedFirstSeen time.Time
	attemptedSig       string
	lastAttemptAt      time.Time
}

func newAutoTakeoverCoordinator(n *Native) *autoTakeoverCoordinator {
	return &autoTakeoverCoordinator{
		native: n, now: time.Now, graceWindow: 6 * time.Second, cooldown: 30 * time.Second,
		managedPID: func(ctx context.Context) int { return n.controller.Status(ctx).ManagedPID },
		restartFn:  n.controller.RestartOfficial,
		snapshot:   autoTakeoverSnapshot{State: "disabled"},
	}
}

func (c *autoTakeoverCoordinator) status(enabled bool) autoTakeoverSnapshot {
	if c == nil {
		return autoTakeoverSnapshot{State: "disabled"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := c.snapshot
	if enabled != snapshot.Enabled {
		snapshot.Enabled, snapshot.Reason = enabled, ""
		snapshot.State = "disabled"
		if enabled {
			snapshot.State = "watching"
		}
	}
	return snapshot
}

func (c *autoTakeoverCoordinator) stop() {
	c.mu.Lock()
	c.stopped = true
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

func (c *autoTakeoverCoordinator) setStateLocked(state, reason string) {
	if state != c.snapshot.State || reason != c.snapshot.Reason {
		c.snapshot.State, c.snapshot.Reason = state, reason
		c.snapshot.UpdatedAt = c.now().UnixMilli()
	}
}

func (c *autoTakeoverCoordinator) clearUnmanagedLocked() {
	c.unmanagedSig, c.attemptedSig = "", ""
	c.unmanagedFirstSeen = time.Time{}
}

func (c *autoTakeoverCoordinator) observe(ctx context.Context, pids []int, known bool, install kimi.Installation) {
	n := c.native
	d, _, err := n.service.Store.Read()
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || ctx.Err() != nil {
		return
	}
	c.snapshot.Enabled = core.AutoTakeover(d)
	if !c.snapshot.Enabled {
		c.clearUnmanagedLocked()
		c.setStateLocked("disabled", "")
		return
	}
	if c.cancel != nil {
		return
	}
	if c.managedPID(ctx) > 0 || !known || len(pids) == 0 {
		c.clearUnmanagedLocked()
		if c.snapshot.State != "recovered" || c.snapshot.LastSuccessAt == 0 {
			c.setStateLocked("watching", "")
		}
		return
	}
	if n.autoSync.isDisconnected() {
		c.setStateLocked("watching", "disconnected")
		return
	}
	if n.modelSyncWriteBlocked() {
		c.setStateLocked("watching", "model_sync_write_blocked")
		return
	}

	now := c.now()
	sig := unmanagedSignature(pids, install)
	if sig != c.unmanagedSig {
		c.unmanagedSig, c.unmanagedFirstSeen = sig, now
	}
	if sig == c.attemptedSig {
		return
	}
	if now.Sub(c.unmanagedFirstSeen) < c.graceWindow {
		c.setStateLocked("waiting_idle", "settling")
		return
	}
	if !c.lastAttemptAt.IsZero() && now.Sub(c.lastAttemptAt) < c.cooldown {
		c.setStateLocked("waiting_idle", "cooldown")
		return
	}
	if n.isOperationRunning() {
		c.setStateLocked("waiting_idle", "operation_busy")
		return
	}
	if n.gatewayActiveRequests != nil && n.gatewayActiveRequests() > 0 {
		c.setStateLocked("waiting_idle", "gateway_requests_active")
		return
	}
	workHome, _ := n.controller.WorkHome()
	if workActivityBusy(workHome, now, 20*time.Second) {
		c.setStateLocked("waiting_idle", "work_activity_recent")
		return
	}
	id, ok := n.tryBeginBackgroundOperation(autoTakeoverAction)
	if !ok {
		c.setStateLocked("waiting_idle", "operation_busy")
		return
	}
	c.attemptedSig, c.lastAttemptAt = sig, now
	c.setStateLocked("taking_over", "")
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.wg.Add(1)
	go c.takeOver(ctx, cancel, id)
}

func (c *autoTakeoverCoordinator) takeOver(ctx context.Context, cancel context.CancelFunc, id string) {
	defer c.wg.Done()
	defer cancel()
	deadline, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	defer cancelTimeout()

	n := c.native
	n.logOperationStarted(id, autoTakeoverAction)
	if deadline.Err() == nil {
		_ = n.refreshControllerInstallationForce()
	}
	err := deadline.Err()
	if err == nil {
		err = c.restartFn(deadline)
	}
	if err == nil {
		_ = n.syncWorkHome()
		if n.compatibilityFingerprintChanged() {
			n.runCompatibilityCheck(deadline, nil, true)
		}
	}

	n.finishOperation(id, "done", nil, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel = nil
	if err == nil {
		c.snapshot.LastSuccessAt = c.now().UnixMilli()
	}
	if !c.snapshot.Enabled {
		return
	}
	if err != nil {
		c.setStateLocked("failed", core.ErrorCode(err))
		return
	}
	c.setStateLocked("recovered", "")
}

func unmanagedSignature(pids []int, install kimi.Installation) string {
	pids = slices.Clone(pids)
	slices.Sort(pids)
	var modified int64
	if info, err := os.Stat(install.Archive); err == nil {
		modified = info.ModTime().UnixNano()
	}
	return fmt.Sprintf("%v %q %d %q", pids, install.Archive, modified, install.Version)
}

func workActivityBusy(workHome string, now time.Time, activityWindow time.Duration) bool {
	if workHome == "" {
		return false
	}
	cutoff := now.Add(-activityWindow)
	mainSessions := filepath.Join(workHome, "agents", "main", "sessions")
	database := filepath.Join(mainSessions, "hosted-logical", "conversations.sqlite")
	for _, path := range []string{database, database + "-wal", database + "-shm"} {
		if recentSessionFile(path, cutoff) {
			return true
		}
	}
	remaining := 1000
	for _, root := range []string{
		filepath.Join(workHome, "sessions"),
		mainSessions,
		filepath.Join(workHome, "runtime", "kimi-code", "home", "sessions"),
	} {
		if recentSessionActivity(root, cutoff, 4, &remaining) {
			return true
		}
	}
	return false
}

func recentSessionActivity(root string, cutoff time.Time, depth int, remaining *int) bool {
	if *remaining == 0 {
		return false
	}
	dir, err := os.Open(root)
	if err != nil {
		return false
	}
	defer dir.Close()
	for *remaining > 0 {
		entries, _ := dir.ReadDir(1)
		if len(entries) == 0 {
			return false
		}
		*remaining -= 1
		entry := entries[0]
		if entry.IsDir() {
			if depth > 0 && recentSessionActivity(filepath.Join(root, entry.Name()), cutoff, depth-1, remaining) {
				return true
			}
			continue
		}
		switch entry.Name() {
		case "conversations.sqlite", "conversations.sqlite-wal", "conversations.sqlite-shm", "wire.jsonl":
			if recentSessionFile(filepath.Join(root, entry.Name()), cutoff) {
				return true
			}
		}
	}
	return false
}

func recentSessionFile(path string, cutoff time.Time) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && !info.ModTime().Before(cutoff)
}
