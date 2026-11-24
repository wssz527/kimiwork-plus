package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

// Saving a configuration is two effects, not one. The gateway routing table is
// replaced in memory by the same call, so a key, an address, a standby route or
// a price is live the moment the save answers. Only the catalog Kimi itself
// reads needs a file write, and that is what the coordinator below schedules.
//
// `catalogSurfaceDigest` is the line between the two: it covers exactly what
// Kimi's menu renders, so a save that does not move it never touches the
// official profile and never asks for a restart.

const (
	autoSyncIdle             = "idle"
	autoSyncScheduled        = "scheduled"
	autoSyncSyncing          = "syncing"
	autoSyncSynced           = "synced"
	autoSyncDeferredOffline  = "deferred_offline"
	autoSyncWaitingUnmanaged = "waiting_unmanaged"
	autoSyncRequiresRestart  = "requires_restart"
	autoSyncWaitingLogin     = "waiting_login"
	autoSyncFailed           = "failed"
)

// autoSyncDebounce merges a burst of saves into one catalog write. The panel
// answers immediately; only this background step waits for the edits to stop.
const autoSyncDebounce = 800 * time.Millisecond

const (
	autoSyncReasonDisconnected  = "disconnected"
	autoSyncReasonNotRunning    = "kimi_not_running"
	autoSyncReasonUnmanaged     = "unmanaged_kimi_running"
	autoSyncReasonWriteBlocked  = "model_sync_write_blocked"
	autoSyncReasonPrepareNeeded = "direct_prepare_required"
)

// autoSyncTimeout bounds one background catalog write. A direct import settles
// the static hosts entry first, which is the slower of the two paths.
const (
	autoSyncGatewayTimeout = 120 * time.Second
	autoSyncDirectTimeout  = 180 * time.Second
)

// autoSyncPath reports whether a save can change the Kimi catalog. Translation,
// subagents and budget are gateway-side features Kimi never reads.
func autoSyncPath(path string) bool {
	switch path {
	case "/api/workspace", "/api/models", "/api/connections", "/api/integration-mode", "/api/kimi-k3-compatibility":
		return true
	}
	return false
}

// catalogSurfaceDigest hashes what Kimi sees, in document order. It deliberately
// leaves out every field that is only a gateway concern: credentials, remarks,
// standby routes, budget, subagents, translation and prices.
func catalogSurfaceDigest(d core.Document) string {
	sum := sha256.New()
	line := func(parts ...string) {
		_, _ = sum.Write([]byte(strings.Join(parts, "|")))
		_, _ = sum.Write([]byte{'\n'})
	}
	mode := core.IntegrationMode(d)
	line("mode", mode)
	if mode == core.IntegrationModeDirect {
		for _, raw := range core.Rows(d["connections"]) {
			connection := core.Object(raw)
			base, _ := core.BaseURL(core.String(connection["baseURL"]))
			line("connection", core.String(connection["id"]), core.String(connection["name"]), base, credentialReference(connection))
		}
		if spec := buildDirectSpec(d); spec != nil {
			for _, provider := range spec.Providers {
				line("provider", provider.ID, provider.DisplayName, provider.Protocol, provider.BaseURL)
			}
			for _, model := range spec.Models {
				line("model", model.ID, model.Model, model.DisplayName,
					strconv.FormatInt(model.MaxContextSize, 10), strconv.FormatInt(model.MaxOutputSize, 10),
					strings.Join(model.Capabilities, ","), strings.Join(model.SupportEfforts, ","), model.DefaultEffort)
			}
		}
		return hex.EncodeToString(sum.Sum(nil))
	}
	compatible := core.KimiK3Compatibility(d)
	line("kimiK3Compatibility", strconv.FormatBool(compatible))
	for _, raw := range core.Rows(d["models"]) {
		row := core.Object(raw)
		id := core.String(row["id"])
		key := protocol.KimiCompatibleModelID(id)
		if !compatible {
			key = core.String(row["protocol"]) + "/" + id
		}
		line("model", key, core.String(row["name"]),
			strconv.FormatInt(jsonInt(row["contextWindow"]), 10), strconv.FormatInt(jsonInt(row["maxOutputTokens"]), 10),
			strconv.FormatBool(row["tools"] == true), strconv.FormatBool(row["reasoning"] == true),
			digestList(row["inputModalities"]), digestList(row["reasoningEfforts"]), core.String(row["defaultEffort"]))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func digestList(raw any) string {
	values := []string{}
	switch typed := raw.(type) {
	case []any:
		for _, value := range typed {
			values = append(values, core.String(value))
		}
	case []string:
		values = append(values, typed...)
	}
	return strings.Join(values, ",")
}

// autoSyncCoordinator runs one catalog write at a time, behind the debounce and
// behind Native.operation. It never starts or restarts Kimi: a state that needs
// that is published for the user to act on.
type autoSyncCoordinator struct {
	native   *Native
	debounce time.Duration

	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once

	mu        sync.Mutex
	stopped   bool
	state     string
	reason    string
	updatedAt time.Time
	// synced is the surface digest the managed profile was last aligned to. A
	// save is compared with it rather than with the previous save, so an edit
	// that lands while an earlier generation is still queued cannot drop it.
	synced string
	// pending says a surface change (or a catch-up request) still owes Kimi a
	// write. target is the newest saved digest; empty means "ask the readback".
	pending bool
	target  string
	// disconnected is set when the user removed the managed models on purpose.
	// Only a save that changes the surface lifts it again.
	disconnected bool
	// blocked says a run is due but another operation holds the slot; only that
	// operation finishing can wake it. deferred says a run came due while the
	// coordinator itself was running.
	blocked  bool
	deferred bool
	running  bool
	timer    *time.Timer
}

func newAutoSyncCoordinator(ctx context.Context, n *Native) *autoSyncCoordinator {
	parent, cancel := context.WithCancel(ctx)
	return &autoSyncCoordinator{native: n, debounce: autoSyncDebounce, ctx: parent, cancel: cancel, state: autoSyncIdle}
}

func (c *autoSyncCoordinator) stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		c.cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stopped = true
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
	})
}

// status is the document published as `integration.autoSync`.
func (c *autoSyncCoordinator) status() core.Document {
	if c == nil {
		return core.Document{"state": autoSyncIdle, "reason": "", "updatedAt": int64(0), "pending": false}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state
	if state == "" {
		state = autoSyncIdle
	}
	out := core.Document{"state": state, "reason": c.reason, "updatedAt": int64(0), "pending": c.pending}
	if !c.updatedAt.IsZero() {
		out["updatedAt"] = c.updatedAt.UnixMilli()
		out["updatedAtISO"] = c.updatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (c *autoSyncCoordinator) setStateLocked(state, reason string) {
	if c.state == state && c.reason == reason {
		return
	}
	c.state, c.reason = state, reason
	c.updatedAt = time.Now()
}

// schedule is the save hook. `prev` and `next` are the documents before and
// after one committed save.
func (c *autoSyncCoordinator) schedule(path string, prev, next core.Document) {
	if c == nil || !autoSyncPath(path) {
		return
	}
	previous := catalogSurfaceDigest(prev)
	target := catalogSurfaceDigest(next)
	changed := previous != target
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if changed {
		// A surface change is the user asking for the catalog again, which is
		// also the one thing that ends a deliberate disconnect.
		c.disconnected = false
		if target == c.synced {
			// The newest surface is the one already in place.
			c.pending, c.target = false, ""
			c.stopTimerLocked()
			c.setStateLocked(autoSyncSynced, "")
			return
		}
		c.pending, c.target = true, target
	} else if !c.pending {
		// Nothing about this save reaches Kimi, and nothing older is waiting.
		return
	}
	c.setStateLocked(autoSyncScheduled, "")
	c.armLocked(c.debounce)
}

// reset ends everything queued when the user removes the managed models.
func (c *autoSyncCoordinator) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.pending, c.target, c.blocked, c.disconnected = false, "", false, true
	c.stopTimerLocked()
	c.setStateLocked(autoSyncIdle, autoSyncReasonDisconnected)
}

// operationFinished is the wake-up for a run that could not take the slot. The
// caller has already released Native.mu.
func (c *autoSyncCoordinator) operationFinished(action string, err error) {
	if c == nil {
		return
	}
	if err == nil && action == "disconnect-models" {
		c.reset()
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if c.blocked {
		c.blocked = false
		c.armLocked(0)
		return
	}
	if err == nil && (action == "launch-kimi" || action == "restart-kimi") && !c.disconnected {
		// The managed profile exists now: whatever the last save owed it, and any
		// drift the readback finds, is settled without another save.
		c.pending = true
		c.setStateLocked(autoSyncScheduled, "")
		c.armLocked(0)
	}
}

// observe converges the published state with a readback the periodic loop
// already made: once the running application adopted the catalog, a pending
// restart is over. It never turns an unproven readback into `synced`.
func (c *autoSyncCoordinator) observe(current *kimi.IntegrationResult, d core.Document) {
	if c == nil || current == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || c.running || c.disconnected {
		return
	}
	if !current.Applied || !current.Adopted {
		return
	}
	digest := catalogSurfaceDigest(d)
	if c.pending && c.target != digest {
		// A newer surface is queued; adopted evidence about the older one says
		// nothing about it.
		return
	}
	// The running application holds the newest saved surface, so there is
	// nothing left for a queued generation to write.
	c.synced = digest
	c.pending, c.target = false, ""
	c.stopTimerLocked()
	c.setStateLocked(autoSyncSynced, "")
}

func (c *autoSyncCoordinator) stopTimerLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

func (c *autoSyncCoordinator) armLocked(delay time.Duration) {
	if c.stopped {
		return
	}
	c.stopTimerLocked()
	c.timer = time.AfterFunc(delay, c.fire)
}

func (c *autoSyncCoordinator) fire() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.timer = nil
	if c.running {
		c.deferred = true
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()
	go c.run()
}

func (c *autoSyncCoordinator) run() {
	defer func() {
		c.mu.Lock()
		c.running = false
		retry := c.deferred
		c.deferred = false
		if retry {
			c.armLocked(0)
		}
		c.mu.Unlock()
	}()
	if c.ctx.Err() != nil {
		return
	}
	id, ok := c.native.beginAutoSyncOperation()
	if !ok {
		// Another operation owns the slot; its completion wakes this run.
		c.mu.Lock()
		c.blocked = true
		c.mu.Unlock()
		if c.native.autoSyncOperationIdle() {
			// The other operation finished between the claim and the flag. Its
			// wake-up has already passed, so this run has to ask for its own.
			c.mu.Lock()
			if c.blocked {
				c.blocked = false
				c.armLocked(0)
			}
			c.mu.Unlock()
		}
		return
	}
	state, _, result, err := c.sync()
	c.native.finishOperation(id, state, result, err)
}

// sync performs at most one catalog write and settles the published state from
// the readback that follows it.
func (c *autoSyncCoordinator) sync() (string, string, any, error) {
	n := c.native
	if n.service == nil || n.service.Store == nil {
		c.abandon(autoSyncFailed, "configuration_unavailable")
		return autoSyncFailed, "configuration_unavailable", nil, nil
	}
	d, _, err := n.service.Store.Read()
	if err != nil {
		c.abandon(autoSyncFailed, "configuration_unavailable")
		return autoSyncFailed, "configuration_unavailable", nil, nil
	}
	digest := catalogSurfaceDigest(d)
	c.mu.Lock()
	target, disconnected := c.target, c.disconnected
	baseline := c.synced
	c.mu.Unlock()
	if disconnected {
		c.hold(autoSyncIdle, autoSyncReasonDisconnected)
		return autoSyncIdle, autoSyncReasonDisconnected, nil, nil
	}
	if target != "" && target == baseline {
		c.finish(autoSyncSynced, "", false, digest, target)
		return autoSyncSynced, "", nil, nil
	}
	if n.modelSyncWriteBlocked() {
		c.finish(autoSyncFailed, autoSyncReasonWriteBlocked, false, digest, target)
		return autoSyncFailed, autoSyncReasonWriteBlocked, nil, nil
	}
	if !n.autoSyncManaged() {
		// Nothing is written into a process this build does not own, and a
		// background timer never launches one. The generation stays queued.
		if n.autoSyncKimiRunning() {
			c.hold(autoSyncWaitingUnmanaged, autoSyncReasonUnmanaged)
			return autoSyncWaitingUnmanaged, autoSyncReasonUnmanaged, nil, nil
		}
		c.hold(autoSyncDeferredOffline, autoSyncReasonNotRunning)
		return autoSyncDeferredOffline, autoSyncReasonNotRunning, nil, nil
	}
	timeout := autoSyncGatewayTimeout
	if n.isDirectMode() {
		timeout = autoSyncDirectTimeout
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	imported := kimi.ImportResult{}
	if target == "" {
		observed := c.reconcile(ctx)
		if !observed.NeedsNativeImport() {
			// A catch-up with nothing to write: the readback is the answer.
			state, reason := autoSyncOutcome(imported, observed)
			c.finish(state, reason, observed.Applied, digest, target)
			return state, reason, nil, nil
		}
	}
	c.mu.Lock()
	c.setStateLocked(autoSyncSyncing, "")
	c.mu.Unlock()
	if n.isDirectMode() && !n.staticHostsReady() {
		// Settling the static hosts entry can need an administrator prompt, and a
		// background timer must never open one.
		c.finish(autoSyncFailed, autoSyncReasonPrepareNeeded, false, digest, target)
		return autoSyncFailed, autoSyncReasonPrepareNeeded, nil, nil
	}
	imported, err = c.importCatalog(ctx)
	current := c.reconcile(ctx)
	state, reason := autoSyncOutcome(imported, current)
	c.finish(state, reason, current.Applied || imported.Applied, digest, target)
	if state == autoSyncFailed {
		if err == nil {
			err = errors.New(reason)
		}
		return state, reason, imported, err
	}
	return state, reason, imported, nil
}

func (c *autoSyncCoordinator) importCatalog(ctx context.Context) (kimi.ImportResult, error) {
	n := c.native
	if n.isDirectMode() {
		if err := n.prepareBYOK(ctx); err != nil {
			return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: core.ErrorCode(err)}, err
		}
		return n.importDirect(ctx)
	}
	run := n.importModels
	if run == nil {
		run = n.controller.ImportModelsAuthorized
	}
	// No ensure step: the managed child is already there, and a background timer
	// must never launch Kimi.
	return runWithManagedKimi(ctx, func(context.Context) error { return nil }, n.autoOpenWork, run, importWaitingWork)
}

func (c *autoSyncCoordinator) reconcile(ctx context.Context) kimi.IntegrationResult {
	if c.native.autoSyncReconcile != nil {
		return c.native.autoSyncReconcile(ctx)
	}
	return c.native.controller.ReconcileWithReadback(ctx)
}

// finish publishes the outcome and ends the queued generation. `applied` means
// the profile holds `digest`, which is then the baseline the next save is
// compared with; it says nothing about adoption. A save that landed while this
// run was writing is newer than `ranTarget` and stays queued.
func (c *autoSyncCoordinator) finish(state, reason string, applied bool, digest, ranTarget string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if applied {
		c.synced = digest
	}
	if c.pending && c.target != ranTarget {
		c.setStateLocked(autoSyncScheduled, "")
		c.armLocked(c.debounce)
		return
	}
	c.pending, c.target = false, ""
	c.setStateLocked(state, reason)
}

// hold publishes a state that still owes Kimi the queued write. The generation
// stays pending so the next managed launch, or the periodic check, runs it.
func (c *autoSyncCoordinator) hold(state, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.setStateLocked(state, reason)
}

// kickIfReady runs a queued generation once a managed Kimi is there. It is what
// makes a save landed while Kimi was closed catch up even when the window was
// opened outside the panel.
func (c *autoSyncCoordinator) kickIfReady() {
	if c == nil {
		return
	}
	c.mu.Lock()
	queued := !c.stopped && c.pending && !c.running && !c.disconnected
	c.mu.Unlock()
	if !queued || !c.native.autoSyncManaged() {
		return
	}
	c.mu.Lock()
	if !c.stopped && c.pending && !c.running && !c.disconnected {
		c.armLocked(0)
	}
	c.mu.Unlock()
}

// abandon ends the queued generation without a retry: the document itself
// could not be read, so another attempt would fail the same way.
func (c *autoSyncCoordinator) abandon(state, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.pending, c.target = false, ""
	c.setStateLocked(state, reason)
}

// autoSyncOutcome maps an import and its readback onto the published state.
func autoSyncOutcome(imported kimi.ImportResult, current kimi.IntegrationResult) (string, string) {
	switch current.State {
	case kimi.IntegrationDisabled:
		return autoSyncIdle, autoSyncReasonDisconnected
	case kimi.IntegrationAccountUnknown:
		return autoSyncWaitingLogin, firstCode(current.Reason, kimi.NativeWaitingLogin)
	}
	switch imported.State {
	case kimi.ImportStateWaitingLogin, kimi.ImportStateAccountChanged:
		return autoSyncWaitingLogin, firstCode(imported.Reason, current.Reason, kimi.NativeWaitingLogin)
	}
	if current.Applied || imported.Applied {
		if current.Adopted {
			return autoSyncSynced, ""
		}
		return autoSyncRequiresRestart, firstCode(imported.Reason, kimi.NativeRequiresRestart)
	}
	if imported.RequiresRestart {
		return autoSyncRequiresRestart, firstCode(imported.Reason, kimi.NativeRequiresRestart)
	}
	if imported.State == kimi.ImportStateFailed || (!imported.OK && imported.Reason != "") {
		return autoSyncFailed, firstCode(imported.Reason, "native_import_failed")
	}
	return autoSyncFailed, firstCode(imported.Reason, current.Reason, "native_import_not_verified")
}

func firstCode(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ScheduleAutoSync is the save hook AttachNative installs on the service.
func (n *Native) ScheduleAutoSync(path string, prev, next core.Document) {
	if n == nil || n.autoSync == nil {
		return
	}
	n.autoSync.schedule(path, prev, next)
}

// autoSyncOperationIdle reports whether the single operation slot is free.
func (n *Native) autoSyncOperationIdle() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.operation == nil || n.operation["state"] != "running"
}

// beginAutoSyncOperation claims Native.operation for one background catalog
// write. It is the same single slot an action takes, so a rebind, a launch or a
// manual sync can never interleave with it.
func (n *Native) beginAutoSyncOperation() (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.operation != nil && n.operation["state"] == "running" {
		return "", false
	}
	id := RandomKey()[:16]
	n.operation = core.Document{"id": id, "action": "auto-sync", "state": "running", "startedAt": time.Now().UnixMilli()}
	return id, true
}

func (n *Native) autoSyncManaged() bool {
	if n.managedProbe != nil {
		return n.managedProbe()
	}
	return n.controller != nil && n.controller.HasManagedBridge()
}

func (n *Native) autoSyncKimiRunning() bool {
	if n.runningProbe != nil {
		return n.runningProbe()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.process["running"] == true
}
