package bridge

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func autoSyncGatewayDoc() core.Document {
	return core.Document{
		"integrationMode": "gateway",
		"connections": []any{
			core.Document{"id": "alpha", "name": "Alpha", "baseURL": "https://alpha.example/v1",
				"apiKeyEnv": "KIMI_WORK_ALPHA_KEY", "apiKeyRef": "connection-a1"},
		},
		"models": []any{
			core.Document{"id": "gpt-alpha", "name": "Alpha One", "connectionId": "alpha",
				"protocol": "responses", "contextWindow": 128000, "maxOutputTokens": 8192,
				"tools": true, "reasoning": true, "inputModalities": []any{"text", "image"},
				"reasoningEfforts": []any{"low", "high"}, "defaultEffort": "high",
				"pricing": core.Document{"inputPerMillion": 1, "outputPerMillion": 2}},
		},
		"budget":    core.Document{"enabled": true, "currency": "USD"},
		"subagents": core.Document{"mode": "inherit"},
	}
}

func autoSyncDocWith(models ...any) core.Document {
	d := autoSyncGatewayDoc()
	d["models"] = append([]any(nil), models...)
	return d
}

func autoSyncModel(id, name string) core.Document {
	return core.Document{"id": id, "name": name, "connectionId": "alpha", "protocol": "responses",
		"contextWindow": 128000, "maxOutputTokens": 8192, "tools": true, "reasoning": true,
		"inputModalities": []any{"text", "image"}, "reasoningEfforts": []any{"low", "high"},
		"defaultEffort": "high", "pricing": core.Document{"inputPerMillion": 1, "outputPerMillion": 2}}
}

// TestCatalogSurfaceDigestIgnoresGatewayOnlyFields is the acceptance line for
// hot routing: none of these edits may reach Kimi's profile.
func TestCatalogSurfaceDigestIgnoresGatewayOnlyFields(t *testing.T) {
	base := autoSyncGatewayDoc()
	want := catalogSurfaceDigest(base)
	edits := map[string]func(core.Document){
		"api key": func(d core.Document) {
			core.Object(core.Rows(d["connections"])[0])["apiKeyRef"] = "connection-b2"
		},
		"connection name": func(d core.Document) {
			core.Object(core.Rows(d["connections"])[0])["name"] = "Renamed"
		},
		"base url": func(d core.Document) {
			core.Object(core.Rows(d["connections"])[0])["baseURL"] = "https://beta.example/v1"
		},
		"failover": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["failover"] = []any{core.Document{"connectionId": "alpha"}}
		},
		"pricing": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["pricing"] = core.Document{"inputPerMillion": 9, "outputPerMillion": 9}
		},
		"budget":      func(d core.Document) { d["budget"] = core.Document{"enabled": false} },
		"subagents":   func(d core.Document) { d["subagents"] = core.Document{"mode": "force"} },
		"translation": func(d core.Document) { d["translation"] = core.Document{"enabled": true} },
	}
	for name, edit := range edits {
		next := cloneDocument(base)
		edit(next)
		if got := catalogSurfaceDigest(next); got != want {
			t.Fatalf("%s changed the Kimi catalog digest", name)
		}
	}
}

func TestCatalogSurfaceDigestCoversEveryGatewaySurfaceField(t *testing.T) {
	base := autoSyncGatewayDoc()
	want := catalogSurfaceDigest(base)
	edits := map[string]func(core.Document){
		"display name": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["name"] = "Alpha Two"
		},
		"context window": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["contextWindow"] = 64000
		},
		"max output": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["maxOutputTokens"] = 4096
		},
		"tools": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["tools"] = false
		},
		"reasoning": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["reasoning"] = false
			delete(core.Object(core.Rows(d["models"])[0]), "reasoningEfforts")
			delete(core.Object(core.Rows(d["models"])[0]), "defaultEffort")
		},
		"input modalities": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["inputModalities"] = []any{"text"}
		},
		"reasoning efforts": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["reasoningEfforts"] = []any{"low"}
			core.Object(core.Rows(d["models"])[0])["defaultEffort"] = "low"
		},
		"default effort": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["defaultEffort"] = "low"
		},
		"removed model": func(d core.Document) { d["models"] = []any{} },
		"added model": func(d core.Document) {
			d["models"] = append(core.Rows(d["models"]), autoSyncModel("gpt-beta", "Beta"))
		},
		"added model at the front": func(d core.Document) {
			rows := append([]any{autoSyncModel("gpt-beta", "Beta")}, core.Rows(d["models"])...)
			d["models"] = rows
		},
		"compatibility opt-in": func(d core.Document) { d[core.KimiK3CompatibilityKey] = false },
		"integration mode":     func(d core.Document) { d["integrationMode"] = "direct" },
	}
	for name, edit := range edits {
		next := cloneDocument(base)
		edit(next)
		if got := catalogSurfaceDigest(next); got == want {
			t.Fatalf("%s did not change the Kimi catalog digest", name)
		}
	}
}

// The compatible key is what the gateway catalog actually publishes, so the
// digest must follow it rather than the logical id.
func TestCatalogSurfaceDigestGatewayKeysTheCompatibleCatalog(t *testing.T) {
	d := autoSyncGatewayDoc()
	row := core.Object(core.Rows(d["models"])[0])
	before := catalogSurfaceDigest(d)
	row["id"] = "gpt-renamed"
	if catalogSurfaceDigest(d) == before {
		t.Fatal("a logical id change did not move the compatible catalog key")
	}
	off := cloneDocument(d)
	off[core.KimiK3CompatibilityKey] = false
	if catalogSurfaceDigest(off) == catalogSurfaceDigest(d) {
		t.Fatal("turning the compatible catalog off kept the same surface")
	}
}

func TestCatalogSurfaceDigestDirectCoversConnectionsModelsAndCredentials(t *testing.T) {
	base := core.Document{
		"integrationMode": "direct",
		"connections": []any{
			core.Document{"id": "alpha", "name": "Alpha", "baseURL": "https://alpha.example/v1",
				"apiKeyEnv": "KIMI_WORK_ALPHA_KEY", "apiKeyRef": "connection-a1", "protocols": []any{"responses"}},
		},
		"models": []any{autoSyncModel("gpt-alpha", "Alpha One")},
	}
	want := catalogSurfaceDigest(base)
	edits := map[string]func(core.Document){
		"rotated key": func(d core.Document) {
			core.Object(core.Rows(d["connections"])[0])["apiKeyRef"] = "connection-b2"
		},
		"upstream address": func(d core.Document) {
			core.Object(core.Rows(d["connections"])[0])["baseURL"] = "https://beta.example/v1"
		},
		"provider name": func(d core.Document) {
			core.Object(core.Rows(d["connections"])[0])["name"] = "Beta"
		},
		"model context": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["contextWindow"] = 64000
		},
		"model display name": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["name"] = "Alpha Two"
		},
		"model tools": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["tools"] = false
		},
		"removed model": func(d core.Document) { d["models"] = []any{} },
	}
	for name, edit := range edits {
		next := cloneDocument(base)
		edit(next)
		if got := catalogSurfaceDigest(next); got == want {
			t.Fatalf("%s did not change the direct catalog digest", name)
		}
	}
	// A price is not part of what Direct Mode writes into Kimi's store.
	priced := cloneDocument(base)
	core.Object(core.Rows(priced["models"])[0])["pricing"] = core.Document{"inputPerMillion": 9, "outputPerMillion": 9}
	if catalogSurfaceDigest(priced) != want {
		t.Fatal("a direct-mode price change moved the surface")
	}
}

// ---------------------------------------------------------------------------
// coordinator

type autoSyncHarness struct {
	n *Native

	mu            sync.Mutex
	imports       int
	directImports int
	applied       bool
	adopted       bool
	waiting       bool
	restart       bool
	failImport    string
	reconciles    int

	managed bool
	running bool
}

func newAutoSyncHarness(t *testing.T, direct bool, d core.Document) *autoSyncHarness {
	t.Helper()
	n := newDirectTestNative(t, direct)
	h := &autoSyncHarness{n: n}
	if err := h.commit(d); err != nil {
		t.Fatal(err)
	}
	n.importModels = func(context.Context) (kimi.ImportResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.imports++
		if h.failImport != "" {
			return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: h.failImport}, nil
		}
		h.applied = true
		return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true,
			RequiresRestart: h.restart}, nil
	}
	n.importDirectModels = func(context.Context, kimi.DirectImportRequest) (kimi.ImportResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.directImports++
		h.applied = true
		return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true}, nil
	}
	n.autoSyncReconcile = func(context.Context) kimi.IntegrationResult { return h.integration() }
	n.managedProbe = func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.managed }
	n.runningProbe = func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.running }
	n.autoSync = newAutoSyncCoordinator(context.Background(), n)
	n.autoSync.debounce = 5 * time.Millisecond
	t.Cleanup(n.autoSync.stop)
	return h
}

func (h *autoSyncHarness) integration() kimi.IntegrationResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.waiting {
		return kimi.IntegrationResult{State: kimi.IntegrationAccountUnknown, Reason: "waiting-login"}
	}
	if !h.applied {
		return kimi.IntegrationResult{State: kimi.IntegrationNeedsImport, Reason: "models_missing"}
	}
	result := kimi.IntegrationResult{State: kimi.IntegrationReady, Applied: true, Adopted: h.adopted}
	if h.restart && !h.adopted {
		result.Reason = kimi.NativeRequiresRestart
	}
	return result
}

func (h *autoSyncHarness) commit(d core.Document) error {
	if normalized := cloneDocument(d); normalized != nil {
		// A stored document always comes back with JSON's own nested types.
		d = normalized
	}
	_, rev, err := h.n.service.Store.Read()
	if err != nil {
		return err
	}
	_, _, err = h.n.service.Store.Commit(d, rev)
	return err
}

func (h *autoSyncHarness) doc(t *testing.T) core.Document {
	t.Helper()
	d, _, err := h.n.service.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// save commits the next document and reports it exactly as Service.configure
// does, with the document that was there before.
func (h *autoSyncHarness) save(t *testing.T, path string, next core.Document) {
	t.Helper()
	prev := h.doc(t)
	if normalized := cloneDocument(next); normalized != nil {
		next = normalized
	}
	if err := h.commit(next); err != nil {
		t.Fatal(err)
	}
	h.n.ScheduleAutoSync(path, prev, next)
}

func (h *autoSyncHarness) set(change func(*autoSyncHarness)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change(h)
}

func (h *autoSyncHarness) counts() (imports int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.imports + h.directImports
}

func (h *autoSyncHarness) state() string {
	return core.String(h.n.autoSync.status()["state"])
}

func waitAutoSync(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitAutoSyncState(t *testing.T, h *autoSyncHarness, want string) {
	t.Helper()
	waitAutoSync(t, "state "+want, func() bool { return h.state() == want })
}

// A save that only moves a gateway-only field must never reach Kimi.
func TestAutoSyncGatewayOnlySaveNeverWrites(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running = true, true })

	next := h.doc(t)
	core.Object(core.Rows(next["connections"])[0])["apiKeyRef"] = "connection-b2"
	core.Object(core.Rows(next["models"])[0])["pricing"] = core.Document{"inputPerMillion": 7, "outputPerMillion": 8}
	h.save(t, "/api/connections", next)
	h.save(t, "/api/budget/save", next)
	h.save(t, "/api/subagents", next)

	time.Sleep(40 * time.Millisecond)
	if imports := h.counts(); imports != 0 {
		t.Fatalf("a gateway-only save ran %d imports", imports)
	}
	if state := h.state(); state != autoSyncIdle && state != autoSyncSynced {
		t.Fatalf("a gateway-only save published %q", state)
	}
}

// Continuous edits inside the debounce window collapse into one write.
func TestAutoSyncMergesBurstsIntoOneImport(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	// Saves touch the disk, so the window has to be wider than three of them
	// even on a loaded machine; the 800ms production default is wider still.
	h.n.autoSync.debounce = 750 * time.Millisecond

	doc := h.doc(t)
	for index, id := range []string{"gpt-one", "gpt-two", "gpt-three"} {
		doc = cloneDocument(doc)
		doc["models"] = append(core.Rows(doc["models"]), autoSyncModel(id, id))
		h.save(t, "/api/models", doc)
		if index == 0 {
			// The first save must not fire before the burst ends.
			time.Sleep(time.Millisecond)
		}
	}
	waitAutoSyncState(t, h, autoSyncSynced)
	if imports := h.counts(); imports != 1 {
		t.Fatalf("three saves ran %d imports", imports)
	}
}

// The regression this guards: adding a model and then editing only its price
// inside the window must not drop the model from the queue.
func TestAutoSyncPendingSurvivesGatewayOnlyEdit(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	h.n.autoSync.debounce = 750 * time.Millisecond

	added := h.doc(t)
	added["models"] = append(core.Rows(added["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", added)

	priced := cloneDocument(added)
	core.Object(core.Rows(priced["models"])[1])["pricing"] = core.Document{"inputPerMillion": 42, "outputPerMillion": 43}
	h.save(t, "/api/models", priced)

	waitAutoSyncState(t, h, autoSyncSynced)
	if imports := h.counts(); imports != 1 {
		t.Fatalf("the price edit lost or duplicated the queued import: %d", imports)
	}
}

func TestAutoSyncRequiresRestartUntilAdoption(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.restart = true, true, true })

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	waitAutoSyncState(t, h, autoSyncRequiresRestart)
	if reason := core.String(h.n.autoSync.status()["reason"]); reason == "" {
		t.Fatal("a pending restart published no reason")
	}
	// The coordinator has no path to RestartOfficial: the operation it produced
	// is the background write, and the state asks the user for the restart.
	h.n.mu.Lock()
	action := core.String(h.n.operation["action"])
	h.n.mu.Unlock()
	if action != "auto-sync" {
		t.Fatalf("a restart was started instead of asking: %q", action)
	}

	// Once the running application adopts the catalog, the same status settles.
	h.set(func(h *autoSyncHarness) { h.adopted = true })
	h.n.autoSync.observe(integrationValue(h.integration()), h.doc(t))
	if state := h.state(); state != autoSyncSynced {
		t.Fatalf("an adopted catalog stayed %q", state)
	}
	if imports := h.counts(); imports != 1 {
		t.Fatalf("adoption ran another import: %d", imports)
	}
}

func integrationValue(v kimi.IntegrationResult) *kimi.IntegrationResult { return &v }

func TestAutoSyncDeferredOfflineThenLaunchCatchesUp(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)

	waitAutoSyncState(t, h, autoSyncDeferredOffline)
	if imports := h.counts(); imports != 0 {
		t.Fatalf("an offline save started %d imports", imports)
	}
	if core.Object(h.n.autoSync.status())["pending"] != true {
		t.Fatal("an offline save dropped its queued generation")
	}

	// A managed Kimi that appears without a launch event is picked up by the
	// periodic check.
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	h.n.autoSync.kickIfReady()
	waitAutoSyncState(t, h, autoSyncSynced)
	if imports := h.counts(); imports != 1 {
		t.Fatalf("the periodic check did not catch up the queue: %d", imports)
	}

	// A later launch finds the catalog in place and writes nothing again.
	h.finishOperation(t, "launch-kimi")
	waitAutoSyncState(t, h, autoSyncSynced)
	time.Sleep(20 * time.Millisecond)
	if imports := h.counts(); imports != 1 {
		t.Fatalf("an aligned launch imported again: %d", imports)
	}
}

func TestAutoSyncWaitingUnmanagedThenRestartCatchesUp(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.running = true })
	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)

	waitAutoSyncState(t, h, autoSyncWaitingUnmanaged)
	if imports := h.counts(); imports != 0 {
		t.Fatalf("an unmanaged Kimi was written to: %d", imports)
	}

	h.set(func(h *autoSyncHarness) { h.managed, h.adopted = true, true })
	h.finishOperation(t, "restart-kimi")
	waitAutoSyncState(t, h, autoSyncSynced)
	if imports := h.counts(); imports != 1 {
		t.Fatalf("restart did not catch up the queue: %d", imports)
	}
}

// finishOperation is the production completion path for an action; the harness
// uses it so the wake-up it installs is the one under test.
func (h *autoSyncHarness) finishOperation(t *testing.T, action string) {
	t.Helper()
	h.n.mu.Lock()
	h.n.operation = core.Document{"id": action, "action": action, "state": "running", "startedAt": time.Now().UnixMilli()}
	h.n.mu.Unlock()
	h.n.finishOperation(action, "done", nil, nil)
}

func TestAutoSyncDisconnectStaysQuietUntilTheSurfaceChanges(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	h.finishOperation(t, "disconnect-models")
	if state := h.state(); state != autoSyncIdle {
		t.Fatalf("a disconnect left %q", state)
	}
	time.Sleep(40 * time.Millisecond)
	if imports := h.counts(); imports != 0 {
		t.Fatalf("the queued write survived the disconnect: %d", imports)
	}

	// A gateway-only edit is not a request to take the models over again.
	quiet := h.doc(t)
	core.Object(core.Rows(quiet["connections"])[0])["apiKeyRef"] = "connection-c3"
	h.save(t, "/api/connections", quiet)
	// Nor is relaunching Kimi.
	h.finishOperation(t, "launch-kimi")
	time.Sleep(40 * time.Millisecond)
	if imports := h.counts(); imports != 0 {
		t.Fatalf("a disconnected profile reconnected on its own: %d", imports)
	}

	// The first surface change is the user asking for it back.
	reconnect := h.doc(t)
	reconnect["models"] = append(core.Rows(reconnect["models"]), autoSyncModel("gpt-gamma", "Gamma"))
	h.save(t, "/api/models", reconnect)
	waitAutoSyncState(t, h, autoSyncSynced)
	if imports := h.counts(); imports != 1 {
		t.Fatalf("an explicit surface change did not reconnect: %d", imports)
	}
}

func TestAutoSyncWaitingLoginSaysSo(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.waiting = true, true, true })

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	waitAutoSyncState(t, h, autoSyncWaitingLogin)
	if reason := core.String(h.n.autoSync.status()["reason"]); reason != "waiting-login" {
		t.Fatalf("waiting login reason = %q", reason)
	}
}

func TestAutoSyncFailedImportPublishesTheReason(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.failImport = true, true, "native_bridge_timeout" })

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	waitAutoSyncState(t, h, autoSyncFailed)
	if reason := core.String(h.n.autoSync.status()["reason"]); reason != "native_bridge_timeout" {
		t.Fatalf("failure reason = %q", reason)
	}
	if core.Object(h.n.autoSync.status())["pending"] != false {
		t.Fatal("a failed import kept retrying the same generation")
	}
}

func TestAutoSyncWriteGuardSkipsTheImport(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running = true, true })
	h.n.compatibility.Guards.ModelSyncWriteBlocked = true

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	waitAutoSyncState(t, h, autoSyncFailed)
	if reason := core.String(h.n.autoSync.status()["reason"]); reason != autoSyncReasonWriteBlocked {
		t.Fatalf("guard reason = %q", reason)
	}
	if imports := h.counts(); imports != 0 {
		t.Fatalf("a blocked write still imported: %d", imports)
	}
}

func TestAutoSyncWaitsForTheRunningOperation(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })

	h.n.mu.Lock()
	h.n.operation = core.Document{"id": "manual", "action": "connect-kimi", "state": "running", "startedAt": time.Now().UnixMilli()}
	h.n.mu.Unlock()

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	waitAutoSyncState(t, h, autoSyncScheduled)
	time.Sleep(40 * time.Millisecond)
	if imports := h.counts(); imports != 0 {
		t.Fatalf("the write ran beside another operation: %d", imports)
	}

	h.n.finishOperation("manual", "done", nil, nil)
	waitAutoSyncState(t, h, autoSyncSynced)
	if imports := h.counts(); imports != 1 {
		t.Fatalf("the queued write was not woken: %d", imports)
	}
}

func TestAutoSyncDirectModeWithoutStaticHostsAsksForPreparation(t *testing.T) {
	h := newAutoSyncHarness(t, true, autoSyncGatewayDocWithMode("direct"))
	h.set(func(h *autoSyncHarness) { h.managed, h.running = true, true })
	// A direct install whose static entry was never written can need an
	// administrator prompt, which a background timer must not open.
	h.n.staticHostsProbe = func() bool { return false }

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	waitAutoSyncState(t, h, autoSyncFailed)
	if reason := core.String(h.n.autoSync.status()["reason"]); reason != autoSyncReasonPrepareNeeded {
		t.Fatalf("direct background reason = %q", reason)
	}
	if imports := h.counts(); imports != 0 {
		t.Fatalf("a background run touched an unprepared direct install: %d", imports)
	}
}

func autoSyncGatewayDocWithMode(mode string) core.Document {
	d := autoSyncGatewayDoc()
	d["integrationMode"] = mode
	return d
}

func TestAutoSyncStateIsPublishedOnTheIntegration(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.n.diagnostics = newDiagnosticsLog(h.n.service.StateDir)
	state := h.n.State()
	auto := core.Object(core.Object(state["integration"])["autoSync"])
	if core.String(auto["state"]) != autoSyncIdle {
		t.Fatalf("published autoSync = %+v", auto)
	}
	if _, ok := auto["reason"]; !ok {
		t.Fatal("autoSync published no reason field")
	}
	if _, ok := auto["updatedAt"]; !ok {
		t.Fatal("autoSync published no updatedAt field")
	}
}

// Service.configure is the only save path the panel uses, so the hook has to
// receive the document from before the save, not a copy of the new one.
func TestServiceConfigureNotifiesAutoSyncWithBothDocuments(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Commit(cloneDocument(autoSyncGatewayDoc()), rev); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, StateDir: dir}
	var path string
	var previous, next core.Document
	s.ScheduleAutoSync = func(gotPath string, prev, after core.Document) {
		path, previous, next = gotPath, prev, after
	}
	rows := append(core.Rows(autoSyncGatewayDoc()["models"]), autoSyncModel("gpt-beta", "Beta"))
	_, rev, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.configure("/api/models", core.Document{"revision": rev, "models": rows}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/models" {
		t.Fatalf("hook path = %q", path)
	}
	if got := len(core.Rows(previous["models"])); got != 1 {
		t.Fatalf("previous document had %d models", got)
	}
	if got := len(core.Rows(next["models"])); got != 2 {
		t.Fatalf("next document had %d models", got)
	}
	if catalogSurfaceDigest(previous) == catalogSurfaceDigest(next) {
		t.Fatal("adding a model did not move the surface")
	}
}
