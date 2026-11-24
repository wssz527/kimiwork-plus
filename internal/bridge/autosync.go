package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
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
		digestLine(sum, parts...)
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
				line("model", model.ID, model.ProviderID, model.Model, model.DisplayName,
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

// digestLine writes one row. Every part is length-prefixed, so a value that
// contains a separator cannot be mistaken for a different split of the same
// fields.
func digestLine(sum hash.Hash, parts ...string) {
	for _, part := range parts {
		_, _ = sum.Write([]byte(strconv.Itoa(len(part))))
		_, _ = sum.Write([]byte{':'})
		_, _ = sum.Write([]byte(part))
		_, _ = sum.Write([]byte{'|'})
	}
	_, _ = sum.Write([]byte{'\n'})
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
	// wg tracks the background runs so stop can wait for the last write instead
	// of leaving it behind after Service.Close.
	wg sync.WaitGroup

	mu        sync.Mutex
	stopped   bool
	state     string
	reason    string
	updatedAt time.Time
	// synced is the surface digest the managed profile was last aligned to, and
	// syncedAdopted says whether the running application is known to have loaded
	// it. A save is compared with it rather than with the previous save, so an
	// edit that lands while an earlier generation is still queued cannot drop it.
	synced        string
	syncedAdopted bool
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
		c.stopped = true
		c.stopTimerLocked()
		c.mu.Unlock()
		// cancel aborts whatever write is in flight; the group was registered
		// under the same lock that publishes `stopped`, so no run can start
		// after this wait begins.
		c.wg.Wait()
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
		if !c.running && c.synced != "" && target == c.synced {
			// The newest surface is the one already in place. While a run is
			// writing an older generation this must not short-circuit: the user
			// reverting mid-write still owes Kimi the revert.
			c.pending, c.target, c.blocked = false, "", false
			c.stopTimerLocked()
			if c.syncedAdopted {
				c.setStateLocked(autoSyncSynced, "")
			} else {
				c.setStateLocked(autoSyncRequiresRestart, kimi.NativeRequiresRestart)
			}
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
	// The profile no longer holds the catalog, so the old baseline proves
	// nothing about the next connection.
	c.synced, c.syncedAdopted = "", false
	c.stopTimerLocked()
	c.setStateLocked(autoSyncIdle, autoSyncReasonDisconnected)
}

// operationFinished is the wake-up for a run that could not take the slot. The
// caller has already released Native.mu. `result` is what the operation
// produced, so a manual prepare can settle the queue it just satisfied.
func (c *autoSyncCoordinator) operationFinished(action string, result any, err error) {
	if c == nil {
		return
	}
	if err == nil && action == "disconnect-models" {
		c.reset()
		return
	}
	manualPrepare := err == nil && (action == "connect-kimi" || action == "prepare-native")
	var digest string
	if manualPrepare {
		digest = c.native.currentSurfaceDigest()
	}
	imported, produced := importResultValue(result)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if err == nil && action == "account-switch" {
		// A different account owns a different profile; the previous baseline
		// says nothing about it. A catch-up compares the new profile again.
		c.synced, c.syncedAdopted = "", false
		if !c.disconnected {
			c.pending, c.target, c.blocked = true, "", false
			c.setStateLocked(autoSyncScheduled, "")
			c.armLocked(0)
		}
		return
	}
	if manualPrepare {
		// The user asked for the models again, whatever the import answered.
		c.disconnected = false
	}
	if manualPrepare && produced && digest != "" &&
		(imported.Applied || imported.RequiresRestart) &&
		(!c.pending || c.target == "" || c.target == digest) {
		// The manual prepare already wrote this surface. Settling here keeps the
		// queued generation from writing it a second time and clears a previous
		// failure the user just resolved.
		c.synced, c.syncedAdopted = digest, imported.Adopted && !imported.RequiresRestart
		c.pending, c.target, c.blocked = false, "", false
		c.stopTimerLocked()
		if c.syncedAdopted {
			c.setStateLocked(autoSyncSynced, "")
		} else {
			c.setStateLocked(autoSyncRequiresRestart, firstCode(imported.Reason, kimi.NativeRequiresRestart))
		}
		return
	}
	if c.blocked {
		c.blocked = false
		if c.pending {
			c.armLocked(0)
		}
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
// restart is over. It never turns an unproven readback into `synced` and never
// settles a queued generation.
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
	if c.pending {
		// The queue owns the next word: adopted evidence about the older surface
		// says nothing about the save that is still waiting, and cancelling it
		// here would drop a model removal, a rename or a rotated key.
		return
	}
	digest := catalogSurfaceDigest(d)
	switch c.state {
	case autoSyncRequiresRestart:
		if c.synced == digest {
			// The restart the user was asked for happened.
			c.syncedAdopted = true
			c.setStateLocked(autoSyncSynced, "")
		}
	case "", autoSyncIdle:
		if c.synced == "" {
			// First readback of a process that started with the catalog in place.
			c.synced, c.syncedAdopted = digest, true
			c.setStateLocked(autoSyncSynced, "")
		}
	}
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
	// Registered under the same lock that stop reads, so the wait below can
	// never miss a run that is about to start.
	c.wg.Add(1)
	c.mu.Unlock()
	go c.run()
}

func (c *autoSyncCoordinator) run() {
	defer c.wg.Done()
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
	if c.ctx.Err() != nil {
		// Shutting down: give the slot back instead of writing to a profile the
		// process is closing.
		c.native.releaseOperation(id)
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
	if c.stopped || !c.pending {
		// A generation settled before this run started (a disconnect, or a manual
		// prepare that answered first) owes nothing; return without side effects.
		c.mu.Unlock()
		return autoSyncIdle, "", nil, nil
	}
	target, disconnected, adopted := c.target, c.disconnected, c.syncedAdopted
	baseline := c.synced
	c.mu.Unlock()
	if disconnected {
		c.hold(autoSyncIdle, autoSyncReasonDisconnected)
		return autoSyncIdle, autoSyncReasonDisconnected, nil, nil
	}
	if target != "" && baseline != "" && target == baseline {
		// The saved surface is the one already written.
		if adopted {
			c.finish(autoSyncSynced, "", false, digest, target)
			return autoSyncSynced, "", nil, nil
		}
		c.finish(autoSyncRequiresRestart, kimi.NativeRequiresRestart, false, digest, target)
		return autoSyncRequiresRestart, kimi.NativeRequiresRestart, nil, nil
	}
	if target == "" && !autoSyncHasModels(n, d) {
		// A launch with nothing configured owes Kimi no write. Running the
		// import anyway would report the missing configuration as a failure.
		c.finish(autoSyncIdle, "", false, digest, target)
		return autoSyncIdle, "", nil, nil
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
			if state == autoSyncFailed && (observed.State == kimi.IntegrationWaitingConfig || observed.State == kimi.IntegrationPending) {
				// A configuration the import cannot read yet is a state to wait
				// out, not a write that failed.
				state, reason = autoSyncIdle, firstCode(observed.Reason)
			}
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
	if err != nil && autoSyncUnmanagedFailure(err) && !n.autoSyncManaged() {
		// The managed process left between the check above and the write. Nothing
		// is written into a process this build does not own, and the generation
		// stays queued for the next managed launch.
		if n.autoSyncKimiRunning() {
			c.hold(autoSyncWaitingUnmanaged, autoSyncReasonUnmanaged)
			return autoSyncWaitingUnmanaged, autoSyncReasonUnmanaged, imported, nil
		}
		c.hold(autoSyncDeferredOffline, autoSyncReasonNotRunning)
		return autoSyncDeferredOffline, autoSyncReasonNotRunning, imported, nil
	}
	current := c.reconcile(ctx)
	state, reason := autoSyncOutcome(imported, current)
	applied := current.Applied || imported.Applied
	if err != nil && imported.State == kimi.ImportStateFailed && imported.Reason == autoSyncReasonPrepareNeeded {
		// The prepare step never reached the write, so a readback of the previous
		// catalog cannot prove this generation landed.
		state, reason, applied = autoSyncFailed, autoSyncReasonPrepareNeeded, false
	}
	c.finish(state, reason, applied, digest, target)
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
		if err := n.prepareBYOKSilent(ctx); err != nil {
			return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: core.ErrorCode(err)}, err
		}
		// No ensure step: a background timer must never launch Kimi.
		return n.importDirectWithEnsure(ctx, func(context.Context) error { return nil })
	}
	run := n.importModels
	if run == nil {
		run = n.controller.ImportModelsAuthorized
	}
	// No ensure step: the managed child is already there, and a background timer
	// must never launch Kimi.
	return runWithManagedKimi(ctx, func(context.Context) error { return nil }, n.autoOpenWork, run, importWaitingWork)
}

// autoSyncHasModels reports whether the document carries anything a catalog
// write could publish.
func autoSyncHasModels(n *Native, d core.Document) bool {
	if n.isDirectMode() {
		spec := buildDirectSpec(d)
		return spec != nil && len(spec.Models) > 0
	}
	return len(core.Rows(d["models"])) > 0
}

// autoSyncUnmanagedFailure reports whether an import failed because the managed
// bridge is not there any more, rather than because the catalog was rejected.
func autoSyncUnmanagedFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, kimi.ErrUnmanagedKimiRunning) {
		return true
	}
	switch core.ErrorCode(err) {
	case autoSyncReasonNotRunning, autoSyncReasonUnmanaged, "native_bridge_unavailable":
		return true
	}
	return false
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
		c.syncedAdopted = state == autoSyncSynced
	}
	if c.pending && c.target != ranTarget {
		c.setStateLocked(autoSyncScheduled, "")
		c.armLocked(c.debounce)
		return
	}
	c.pending, c.target, c.blocked = false, "", false
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
	c.pending, c.target, c.blocked = false, "", false
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

// currentSurfaceDigest reads the saved surface. Callers use it after releasing
// Native.mu; an unreadable document has no digest.
func (n *Native) currentSurfaceDigest() string {
	if n == nil || n.service == nil || n.service.Store == nil {
		return ""
	}
	d, _, err := n.service.Store.Read()
	if err != nil {
		return ""
	}
	return catalogSurfaceDigest(d)
}

func (n *Native) autoSyncKimiRunning() bool {
	if n.runningProbe != nil {
		return n.runningProbe()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.process["running"] == true
}
