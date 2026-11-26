package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/byokdns"
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
	rows := make([]any, 0, len(models))
	rows = append(rows, models...)
	d["models"] = rows
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
		"upstream id of a named model": func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["upstreamModelId"] = "new-upstream"
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

func TestCatalogSurfaceDigestTracksUpstreamIDOfUnnamedModel(t *testing.T) {
	d := autoSyncGatewayDoc()
	model := core.Object(core.Rows(d["models"])[0])
	delete(model, "name")
	before := catalogSurfaceDigest(d)
	model["upstreamModelId"] = "new-upstream"
	if catalogSurfaceDigest(d) == before {
		t.Fatal("the new display name did not change the Kimi catalog digest")
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
	if direct {
		// The Direct Mode prepare step is stubbed so a test never elevates, and
		// the static entry is reported present so the write reaches the import.
		// The background path's own refusal has its own test below.
		n.admin = func(_ context.Context, _, report string) error {
			if report != "" {
				return os.WriteFile(report, []byte("done\n"), 0o600)
			}
			return nil
		}
		n.staticHostsProbe = func() bool { return true }
		// A direct import reads the provider keys before it runs.
		if d, _, readErr := n.service.Store.Read(); readErr == nil {
			for _, row := range core.Rows(d["connections"]) {
				ref := credentialReference(core.Object(row))
				if ref == "" {
					continue
				}
				if err := n.service.Keys.Set(ref, "test-key"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
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
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.n.autoSync.mu.Lock()
		running := h.n.autoSync.running
		h.n.autoSync.mu.Unlock()
		// The run publishes its outcome before it clears `running`; waiting for
		// both keeps a follow-up call from seeing a slot that is still busy.
		if h.state() == want && !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for state %q; status = %+v", want, h.n.autoSync.status())
		}
		time.Sleep(2 * time.Millisecond)
	}
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

func TestAutoSyncImportsCatalogAfterConnectionCascade(t *testing.T) {
	d := autoSyncGatewayDoc()
	d["connections"] = append(core.Rows(d["connections"]), core.Document{"id": "beta", "name": "Beta", "baseURL": "https://beta.example/v1"})
	model := autoSyncModel("gpt-beta", "Beta")
	model["connectionId"] = "beta"
	d["models"] = append(core.Rows(d["models"]), model)
	h := newAutoSyncHarness(t, false, d)
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	h.n.service.ScheduleAutoSync = h.n.ScheduleAutoSync
	_, rev, err := h.n.service.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.n.service.configure("/api/connections/delete", core.Document{"revision": rev, "connectionId": "beta", "cascade": true}); err != nil {
		t.Fatal(err)
	}
	waitAutoSyncState(t, h, autoSyncSynced)
	if imports := h.counts(); imports != 1 {
		t.Fatalf("connection cascade ran %d imports, want one", imports)
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
	h.finishOperationResult(t, action, nil)
}

func (h *autoSyncHarness) finishOperationResult(t *testing.T, action string, result any) {
	t.Helper()
	id, claimErr := h.n.beginOperation(context.Background(), action)
	if claimErr != nil {
		// A background run holds the slot. Publish this action's operation the way
		// a claim would, including the start snapshot, so the completion path stays
		// the one under test.
		digest := ""
		if action == "connect-kimi" || action == "prepare-native" {
			digest = h.n.currentSurfaceDigest()
		}
		h.n.mu.Lock()
		id = action
		h.n.operation = core.Document{"id": id, "action": action, "state": "running", "startedAt": time.Now().UnixMilli()}
		h.n.recordOperationStartLocked(id, digest)
		h.n.mu.Unlock()
	}
	h.n.finishOperation(id, "done", result, nil)
}

// operationState is the state of the newest operation, or empty while a manual
// or launch operation still owns the slot.
func (h *autoSyncHarness) operationState() string {
	h.n.mu.Lock()
	defer h.n.mu.Unlock()
	if core.String(h.n.operation["action"]) != "auto-sync" {
		return ""
	}
	return core.String(h.n.operation["state"])
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

// ---------------------------------------------------------------------------
// regression: the periodic readback must not settle a queued generation

// observe used to treat an adopted readback as proof about the newest save, so
// a removal, a rename or a rotated key saved inside the debounce window was
// cancelled before it ever reached Kimi.
func TestAutoSyncObserveNeverDropsAQueuedGeneration(t *testing.T) {
	cases := []struct {
		name   string
		direct bool
		edit   func(core.Document)
	}{
		{name: "removed model", edit: func(d core.Document) {
			d["models"] = core.Rows(d["models"])[:1]
		}},
		{name: "renamed model", edit: func(d core.Document) {
			core.Object(core.Rows(d["models"])[0])["name"] = "Renamed One"
		}},
		{name: "rotated direct key", direct: true, edit: func(d core.Document) {
			core.Object(core.Rows(d["connections"])[0])["apiKeyRef"] = "connection-b2"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := autoSyncGatewayDoc()
			if tc.direct {
				doc = autoSyncGatewayDocWithMode("direct")
			}
			h := newAutoSyncHarness(t, tc.direct, doc)
			h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
			h.n.autoSync.debounce = 30 * time.Millisecond

			// The baseline the running application has adopted.
			added := h.doc(t)
			added["models"] = append(core.Rows(added["models"]), autoSyncModel("gpt-beta", "Beta"))
			h.save(t, "/api/models", added)
			waitAutoSyncState(t, h, autoSyncSynced)
			imports := h.counts()

			// Widen the window so the readback below lands while the save waits.
			h.n.autoSync.debounce = 5 * time.Second
			next := h.doc(t)
			tc.edit(next)
			h.save(t, "/api/models", next)
			if state := h.state(); state != autoSyncScheduled {
				t.Fatalf("the save published %q", state)
			}
			h.n.autoSync.observe(integrationValue(h.integration()), h.doc(t))
			if state := h.state(); state != autoSyncScheduled {
				t.Fatalf("the readback turned a queued save into %q", state)
			}
			if core.Object(h.n.autoSync.status())["pending"] != true {
				t.Fatal("the readback dropped the queued generation")
			}
			if h.counts() != imports {
				t.Fatal("the readback ran the queued import itself")
			}
		})
	}
}

// A failed write is an explicit answer for the user; a later adoption of the
// same surface must not rewrite it into `synced`.
func TestAutoSyncObserveLeavesAnExplicitFailureAlone(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.applied, h.adopted = true, true, true, true })
	h.n.autoSync.mu.Lock()
	h.n.autoSync.synced = catalogSurfaceDigest(h.doc(t))
	h.n.autoSync.setStateLocked(autoSyncFailed, "native_bridge_timeout")
	h.n.autoSync.mu.Unlock()

	h.n.autoSync.observe(integrationValue(h.integration()), h.doc(t))
	status := h.n.autoSync.status()
	if core.String(status["state"]) != autoSyncFailed || core.String(status["reason"]) != "native_bridge_timeout" {
		t.Fatalf("an adopted readback rewrote %+v", status)
	}
}

// A readback that confirms the catalog settles the finished auto-sync
// operation strip too: without it the top bar keeps asking for a restart every
// page already reports as done.
func TestAutoSyncObserveSettlesTheRestartOperationStrip(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.applied, h.adopted = true, true, true, true })
	digest := catalogSurfaceDigest(h.doc(t))
	h.n.autoSync.mu.Lock()
	h.n.autoSync.synced, h.n.autoSync.syncedAdopted = digest, false
	h.n.autoSync.setStateLocked(autoSyncRequiresRestart, kimi.NativeRequiresRestart)
	h.n.autoSync.mu.Unlock()
	started := time.Now().UnixMilli() - 2000
	h.n.mu.Lock()
	h.n.operation = core.Document{"id": "op-1", "action": "auto-sync", "state": autoSyncRequiresRestart,
		"reason": kimi.NativeRequiresRestart, "startedAt": started, "finishedAt": started}
	h.n.mu.Unlock()

	h.n.autoSync.observe(integrationValue(h.integration()), h.doc(t))

	if state := h.state(); state != autoSyncSynced {
		t.Fatalf("an adopted catalog published %q", state)
	}
	h.n.mu.Lock()
	operation := core.Document{}
	for key, value := range h.n.operation {
		operation[key] = value
	}
	h.n.mu.Unlock()
	if core.String(operation["state"]) != autoSyncSynced {
		t.Fatalf("the stale operation strip survived: %+v", operation)
	}
	if _, present := operation["reason"]; present {
		t.Fatalf("the restart reason survived: %+v", operation)
	}
	if documentInt64(operation["finishedAt"]) <= started {
		t.Fatalf("finishedAt was not refreshed: %+v", operation)
	}
}

// The same strip is retired when a save reverts to the catalog the running
// application already adopted. That path never runs the coordinator, so it
// settles the strip explicitly.
func TestAutoSyncScheduleSettlesTheRestartOperationStrip(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	doc := h.doc(t)
	h.n.autoSync.mu.Lock()
	h.n.autoSync.synced, h.n.autoSync.syncedAdopted = catalogSurfaceDigest(doc), true
	h.n.autoSync.setStateLocked(autoSyncRequiresRestart, kimi.NativeRequiresRestart)
	h.n.autoSync.mu.Unlock()
	h.n.mu.Lock()
	h.n.operation = core.Document{"id": "op-2", "action": "auto-sync", "state": autoSyncRequiresRestart,
		"reason": kimi.NativeRequiresRestart, "startedAt": time.Now().UnixMilli()}
	h.n.mu.Unlock()

	widened := h.doc(t)
	widened["models"] = append(core.Rows(widened["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.n.autoSync.schedule("/api/models", widened, doc)

	if state := h.state(); state != autoSyncSynced {
		t.Fatalf("a reverted save published %q", state)
	}
	h.n.mu.Lock()
	operationState := core.String(h.n.operation["state"])
	_, hasReason := h.n.operation["reason"]
	h.n.mu.Unlock()
	if operationState != autoSyncSynced || hasReason {
		t.Fatalf("the stale operation strip survived: state=%q reason=%v", operationState, hasReason)
	}
}

// ---------------------------------------------------------------------------
// regression: a background write never launches Kimi and never elevates

// The managed process can leave between the coordinator's check and the write.
// The generation then stays queued instead of failing for good.
func TestAutoSyncDirectImportNeverLaunchesKimi(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		want    string
		reason  string
	}{
		{name: "offline", running: false, want: autoSyncDeferredOffline, reason: autoSyncReasonNotRunning},
		{name: "unmanaged", running: true, want: autoSyncWaitingUnmanaged, reason: autoSyncReasonUnmanaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAutoSyncHarness(t, true, autoSyncGatewayDocWithMode("direct"))
			// Present for the check, gone for the write.
			var checks atomic.Int32
			h.n.managedProbe = func() bool { return checks.Add(1) == 1 }
			h.n.runningProbe = func() bool { return tc.running }
			h.n.staticHostsProbe = func() bool { return true }
			// No fake: the real controller reports that it has no managed bridge.
			h.n.importDirectModels = nil
			var launched atomic.Int32
			h.n.autoLaunch = func(context.Context) error {
				launched.Add(1)
				return nil
			}

			next := h.doc(t)
			next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
			h.save(t, "/api/models", next)
			waitAutoSyncState(t, h, tc.want)
			if reason := core.String(h.n.autoSync.status()["reason"]); reason != tc.reason {
				t.Fatalf("reason = %q", reason)
			}
			if got := launched.Load(); got != 0 {
				t.Fatalf("a background write launched Kimi %d times", got)
			}
			if core.Object(h.n.autoSync.status())["pending"] != true {
				t.Fatalf("%s dropped the queued generation", tc.want)
			}
		})
	}
}

// A direct background write must refuse the administrator step instead of
// opening a prompt, and say which step the user has to run.
func TestAutoSyncDirectPrepareNeverAsksForElevation(t *testing.T) {
	h := newAutoSyncHarness(t, true, autoSyncGatewayDocWithMode("direct"))
	// The production value: the setup layer would fall back to osascript/UAC.
	h.n.admin = nil
	h.n.probe = &byokdns.Probe{Lookup: func(context.Context, string) (string, error) {
		return "", errors.New("lookup timeout")
	}}
	if err := h.n.prepareBYOKSilent(context.Background()); core.ErrorCode(err) != autoSyncReasonPrepareNeeded {
		t.Fatalf("silent prepare = %v (%q)", err, core.ErrorCode(err))
	}

	// Through the coordinator the same refusal is published as a failure that
	// asks for the panel's prepare action.
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.applied, h.adopted = true, true, true, true })
	h.n.staticHostsProbe = func() bool { return true }
	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	waitAutoSyncState(t, h, autoSyncFailed)
	if reason := core.String(h.n.autoSync.status()["reason"]); reason != autoSyncReasonPrepareNeeded {
		t.Fatalf("background prepare reason = %q", reason)
	}
}

// ---------------------------------------------------------------------------
// regression: the surface digest covers the fields Kimi renders

// A direct model belongs to a provider; moving it to another connection changes
// the provider Kimi stores and must move the digest too.
func TestCatalogSurfaceDigestDirectCoversProviderID(t *testing.T) {
	base := core.Document{
		"integrationMode": "direct",
		"connections": []any{
			core.Document{"id": "alpha", "name": "Alpha", "baseURL": "https://alpha.example/v1",
				"apiKeyEnv": "KIMI_WORK_ALPHA_KEY", "apiKeyRef": "connection-a1", "protocols": []any{"responses"}},
			core.Document{"id": "beta", "name": "Beta", "baseURL": "https://beta.example/v1",
				"apiKeyEnv": "KIMI_WORK_BETA_KEY", "apiKeyRef": "connection-b2", "protocols": []any{"responses"}},
		},
		"models": []any{autoSyncModel("gpt-alpha", "Alpha One")},
	}
	moved := cloneDocument(base)
	core.Object(core.Rows(moved["models"])[0])["connectionId"] = "beta"
	before, after := buildDirectSpec(base), buildDirectSpec(moved)
	if before == nil || after == nil || before.Models[0].ProviderID == after.Models[0].ProviderID {
		t.Fatal("the provider id did not follow the connection")
	}
	if catalogSurfaceDigest(moved) == catalogSurfaceDigest(base) {
		t.Fatal("moving a direct model to another connection kept the surface")
	}
}

// A value that contains the row separator must not be able to stand in for a
// different split of the same fields.
func TestCatalogSurfaceDigestSeparatesFieldsContainingTheSeparator(t *testing.T) {
	first := sha256.New()
	digestLine(first, "model", "a|b", "c")
	second := sha256.New()
	digestLine(second, "model", "a", "b|c")
	if bytes.Equal(first.Sum(nil), second.Sum(nil)) {
		t.Fatal("a separator inside a field collided with two fields")
	}
}

// ---------------------------------------------------------------------------
// regression: the baseline is dropped when the profile stops holding it

// A disconnect empties the profile, so restoring the surface that was in place
// before it is a new write, not a no-op.
func TestAutoSyncReconnectAfterDisconnectImportsTheRestoredSurface(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	h.n.autoSync.debounce = 20 * time.Millisecond

	base := h.doc(t)
	added := cloneDocument(base)
	added["models"] = append(core.Rows(added["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", added)
	waitAutoSyncState(t, h, autoSyncSynced)

	h.finishOperation(t, "disconnect-models")
	if state := h.state(); state != autoSyncIdle {
		t.Fatalf("a disconnect left %q", state)
	}

	// Two saves inside one window end on the surface that was in place before
	// the disconnect. It still has to be written again.
	h.n.autoSync.debounce = 5 * time.Second
	h.save(t, "/api/models", base)
	h.save(t, "/api/models", added)
	if state := h.state(); state != autoSyncScheduled {
		t.Fatalf("restoring the surface published %q", state)
	}
	h.n.autoSync.kickIfReady()
	waitAutoSyncState(t, h, autoSyncSynced)
	if imports := h.counts(); imports != 2 {
		t.Fatalf("the restored surface ran %d imports", imports)
	}
}

// A run writing the older surface must not let a revert inside its window be
// mistaken for "already in place": the profile still holds the newer one.
func TestAutoSyncRevertWhileWritingStillImportsTheRevert(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	h.n.autoSync.debounce = 10 * time.Millisecond

	var mu sync.Mutex
	calls := 0
	entered := make(chan int, 8)
	release := make(chan struct{})
	h.n.importModels = func(context.Context) (kimi.ImportResult, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		entered <- call
		if call == 2 {
			<-release
		}
		h.set(func(h *autoSyncHarness) { h.applied = true })
		return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true}, nil
	}

	base := h.doc(t)
	added := cloneDocument(base)
	added["models"] = append(core.Rows(added["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", added)
	if call := <-entered; call != 1 {
		t.Fatalf("first import = %d", call)
	}
	waitAutoSyncState(t, h, autoSyncSynced)

	// Remove the model, and revert while that write is still holding.
	h.save(t, "/api/models", base)
	if call := <-entered; call != 2 {
		t.Fatalf("second import = %d", call)
	}
	h.save(t, "/api/models", added)
	close(release)

	select {
	case call := <-entered:
		if call != 3 {
			t.Fatalf("third import = %d", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the revert was never imported")
	}
	waitAutoSyncState(t, h, autoSyncSynced)
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("the revert produced %d imports", calls)
	}
}

// The surface that was written but never adopted keeps its restart prompt when
// the user edits away and back inside one window.
func TestAutoSyncRevertToTheUnadoptedSurfaceKeepsTheRestartPrompt(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.restart = true, true, true })
	h.n.autoSync.debounce = 20 * time.Millisecond

	first := h.doc(t)
	first["models"] = append(core.Rows(first["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", first)
	waitAutoSyncState(t, h, autoSyncRequiresRestart)

	h.n.autoSync.debounce = 5 * time.Second
	second := cloneDocument(first)
	second["models"] = append(core.Rows(second["models"]), autoSyncModel("gpt-gamma", "Gamma"))
	h.save(t, "/api/models", second)
	h.save(t, "/api/models", first)
	if state := h.state(); state != autoSyncRequiresRestart {
		t.Fatalf("reverting to the written surface published %q", state)
	}
	if reason := core.String(h.n.autoSync.status()["reason"]); reason == "" {
		t.Fatal("the restart prompt lost its reason")
	}
	time.Sleep(40 * time.Millisecond)
	if imports := h.counts(); imports != 1 {
		t.Fatalf("reverting to the written surface imported again: %d", imports)
	}
}

// ---------------------------------------------------------------------------
// regression: manual operations settle the queue they answered

// A manual prepare imports the same surface the queued write owes, so the
// background run must not repeat it, and a previous failure must be cleared.
func TestAutoSyncManualConnectSettlesTheQueuedWrite(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	h.n.autoSync.debounce = 750 * time.Millisecond

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	if state := h.state(); state != autoSyncScheduled {
		t.Fatalf("the save published %q", state)
	}
	// A failed background attempt may still be published from before.
	h.n.autoSync.hold(autoSyncFailed, autoSyncReasonPrepareNeeded)

	h.finishOperationResult(t, "connect-kimi", kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true, Adopted: true})
	if state := h.state(); state != autoSyncSynced {
		t.Fatalf("a manual prepare left %q", state)
	}
	if reason := core.String(h.n.autoSync.status()["reason"]); reason != "" {
		t.Fatalf("a manual prepare left reason %q", reason)
	}
	if core.Object(h.n.autoSync.status())["pending"] != false {
		t.Fatal("the queued write survived the manual prepare")
	}
	time.Sleep(60 * time.Millisecond)
	if imports := h.counts(); imports != 0 {
		t.Fatalf("the manual prepare was followed by another import: %d", imports)
	}
}

// A different account owns a different profile, so the next catch-up has to
// compare and write again even though the document did not change.
func TestAutoSyncAccountSwitchCatchesUp(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.applied, h.adopted = true, true, true, true })
	h.n.autoSync.debounce = 20 * time.Millisecond

	base := h.doc(t)
	added := cloneDocument(base)
	added["models"] = append(core.Rows(added["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", added)
	waitAutoSyncState(t, h, autoSyncSynced)
	imports := h.counts()

	// The other account's profile does not hold the catalog yet.
	h.set(func(h *autoSyncHarness) { h.applied = false })
	h.finishOperation(t, "account-switch")
	waitAutoSync(t, "the account catch-up", func() bool { return h.counts() > imports })
	waitAutoSyncState(t, h, autoSyncSynced)
}

// ---------------------------------------------------------------------------
// regression: an empty configuration is not a failed write

func TestAutoSyncEmptyConfigurationOnLaunchStaysIdle(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncDocWith())
	h.set(func(h *autoSyncHarness) { h.managed, h.running = true, true })
	var reconciles atomic.Int32
	h.n.autoSyncReconcile = func(context.Context) kimi.IntegrationResult {
		reconciles.Add(1)
		return kimi.IntegrationResult{State: kimi.IntegrationWaitingConfig, Reason: "invalid_request"}
	}

	h.finishOperation(t, "launch-kimi")
	waitAutoSync(t, "the empty launch to settle", func() bool { return h.operationState() == autoSyncIdle })
	if state := h.state(); state != autoSyncIdle {
		t.Fatalf("an empty configuration published %q", state)
	}
	if got := reconciles.Load(); got != 0 {
		t.Fatalf("an empty configuration ran %d reconciles", got)
	}
	if imports := h.counts(); imports != 0 {
		t.Fatalf("an empty configuration ran %d imports", imports)
	}
}

// A readback that cannot import yet is a state to wait out, not a failure.
func TestAutoSyncWaitingConfigurationIsNotAFailure(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running = true, true })
	h.n.autoSyncReconcile = func(context.Context) kimi.IntegrationResult {
		return kimi.IntegrationResult{State: kimi.IntegrationWaitingConfig, Reason: "invalid_request"}
	}
	h.finishOperation(t, "launch-kimi")
	waitAutoSync(t, "the configuration check to settle", func() bool { return h.operationState() == autoSyncIdle })
	if state := h.state(); state != autoSyncIdle {
		t.Fatalf("a waiting configuration published %q", state)
	}
	if imports := h.counts(); imports != 0 {
		t.Fatalf("a waiting configuration ran %d imports", imports)
	}
}

// ---------------------------------------------------------------------------
// regression: closing the service waits for the write it is cancelling

func TestAutoSyncStopWaitsForTheRunningWrite(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	h.n.autoSync.debounce = 5 * time.Millisecond
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.n.importModels = func(context.Context) (kimi.ImportResult, error) {
		entered <- struct{}{}
		<-release
		return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true}, nil
	}

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	<-entered

	stopped := make(chan struct{})
	go func() {
		h.n.autoSync.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while a write was still running")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not return after the write finished")
	}
}

// A process that starts with the catalog already in place is settled by the
// first adopted readback, without waiting for a restart that is not needed.
func TestAutoSyncStartupReadbackConfirmsTheCatalog(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.applied, h.adopted = true, true, true, true })
	if state := h.state(); state != autoSyncIdle {
		t.Fatalf("the harness did not start idle: %q", state)
	}
	h.n.autoSync.observe(integrationValue(h.integration()), h.doc(t))
	if state := h.state(); state != autoSyncSynced {
		t.Fatalf("an adopted catalog on startup published %q", state)
	}
	if core.Object(h.n.autoSync.status())["pending"] != false {
		t.Fatal("a startup confirmation queued a write")
	}
}

// ---------------------------------------------------------------------------
// regression: a failed write is never answered by the catalog it replaced

// The catalog of an earlier generation can still read back as applied and
// adopted. That is evidence about the write that put it there, not about a
// removal, a rename or a rotated key that just failed to land.
func TestAutoSyncFailedImportDoesNotInheritTheOlderCatalog(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.applied, h.adopted = true, true, true, true })
	h.n.autoSync.debounce = 20 * time.Millisecond

	base := h.doc(t)
	baseDigest := catalogSurfaceDigest(base)
	h.n.autoSync.mu.Lock()
	h.n.autoSync.synced, h.n.autoSync.syncedAdopted = baseDigest, true
	h.n.autoSync.mu.Unlock()

	// Removing the only model fails at the bridge.
	h.set(func(h *autoSyncHarness) { h.failImport = "native_bridge_timeout" })
	next := cloneDocument(base)
	next["models"] = []any{}
	h.save(t, "/api/models", next)

	waitAutoSyncState(t, h, autoSyncFailed)
	status := h.n.autoSync.status()
	if reason := core.String(status["reason"]); reason != "native_bridge_timeout" {
		t.Fatalf("failure reason = %q", reason)
	}
	if status["pending"] != false {
		t.Fatal("a failed import kept retrying the same generation")
	}
	h.n.autoSync.mu.Lock()
	synced := h.n.autoSync.synced
	h.n.autoSync.mu.Unlock()
	if synced != baseDigest {
		t.Fatalf("a failed write moved the baseline to %q", synced)
	}
	h.n.mu.Lock()
	operationState := core.String(h.n.operation["state"])
	h.n.mu.Unlock()
	if operationState != "failed" {
		t.Fatalf("the failed write published operation state %q", operationState)
	}
}

// A manual connect answers for the surface that was saved when it started. A
// save that lands while the import runs is a newer generation: reading the
// document again at completion used to settle it with the running application's
// digest, so the queued rename never reached Kimi.
func TestAutoSyncManualConnectDoesNotSwallowAConcurrentSave(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.applied, h.adopted = true, true, true, true })
	h.n.autoSync.debounce = 5 * time.Millisecond

	base := h.doc(t)
	h.n.autoSync.mu.Lock()
	h.n.autoSync.synced, h.n.autoSync.syncedAdopted = catalogSurfaceDigest(base), true
	h.n.autoSync.mu.Unlock()

	id, claimErr := h.n.beginOperation(context.Background(), "connect-kimi")
	if claimErr != nil {
		t.Fatal("the manual connect could not claim the slot")
	}
	// The user renames a model while the manual import is still running.
	next := cloneDocument(base)
	core.Object(core.Rows(next["models"])[0])["name"] = "Alpha Renamed"
	h.save(t, "/api/models", next)

	// The queued write cannot take the slot and waits for the manual operation.
	waitAutoSync(t, "the queued write to wait for the manual slot", func() bool {
		h.n.autoSync.mu.Lock()
		defer h.n.autoSync.mu.Unlock()
		return h.n.autoSync.deferred
	})
	if imports := h.counts(); imports != 0 {
		t.Fatalf("the queued write ran beside the manual connect: %d", imports)
	}

	h.n.finishOperation(id, "prepared", kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true, Adopted: true}, nil)
	waitAutoSync(t, "the concurrent save to be written", func() bool { return h.counts() == 1 })
	waitAutoSyncState(t, h, autoSyncSynced)

	h.n.autoSync.mu.Lock()
	pending, target, synced := h.n.autoSync.pending, h.n.autoSync.target, h.n.autoSync.synced
	h.n.autoSync.mu.Unlock()
	if pending || target != "" {
		t.Fatalf("the concurrent save stayed queued: pending=%v target=%q", pending, target)
	}
	if want := catalogSurfaceDigest(next); synced != want {
		t.Fatalf("the baseline does not describe the written surface: %q != %q", synced, want)
	}
}

// A rename or a removal queued before an account switch is still owed to the
// new account's profile. Clearing the target used to make the catch-up reconcile
// instead of importing, and a readback that already reports a catalog settled
// the queued edit without writing it.
func TestAutoSyncAccountSwitchKeepsTheQueuedTarget(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.applied, h.adopted = true, true, true, true })
	h.n.autoSync.debounce = 5 * time.Second

	base := h.doc(t)
	h.n.autoSync.mu.Lock()
	h.n.autoSync.synced, h.n.autoSync.syncedAdopted = catalogSurfaceDigest(base), true
	h.n.autoSync.mu.Unlock()

	next := cloneDocument(base)
	core.Object(core.Rows(next["models"])[0])["name"] = "Alpha Renamed"
	h.save(t, "/api/models", next)
	if target := func() string {
		h.n.autoSync.mu.Lock()
		defer h.n.autoSync.mu.Unlock()
		return h.n.autoSync.target
	}(); target != catalogSurfaceDigest(next) {
		t.Fatalf("the save queued %q", target)
	}

	h.finishOperation(t, "account-switch")
	waitAutoSync(t, "the new account's import", func() bool { return h.counts() == 1 })
	waitAutoSyncState(t, h, autoSyncSynced)
	h.n.autoSync.mu.Lock()
	pending, target := h.n.autoSync.pending, h.n.autoSync.target
	h.n.autoSync.mu.Unlock()
	if pending || target != "" {
		t.Fatalf("the switched account kept the queue: pending=%v target=%q", pending, target)
	}
}

func TestAutoSyncRepairsOneOwnedTemporaryGatewayProvider(t *testing.T) {
	for _, requireRestart := range []bool{false, true} {
		name := "applied"
		if requireRestart {
			name = "requires-restart"
		}
		t.Run(name, func(t *testing.T) {
			h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
			h.set(func(h *autoSyncHarness) { h.managed, h.running = true, true })
			mismatch := kimi.IntegrationResult{
				State: kimi.IntegrationReady, Applied: true, Adopted: true,
				GatewayProviderMismatch: "byok-55555555-5555-4555-8555-555555555555",
				Readback:                &kimi.ImportReadback{Active: true, AccountVerified: true, AccountDigest: "verified-account"},
			}
			var imports atomic.Int32
			h.n.autoSyncReconcile = func(context.Context) kimi.IntegrationResult {
				if imports.Load() == 0 || requireRestart {
					return mismatch
				}
				return kimi.IntegrationResult{State: kimi.IntegrationReady, Applied: true, Adopted: true}
			}
			h.n.importModels = func(context.Context) (kimi.ImportResult, error) {
				imports.Add(1)
				if h.n.autoSyncOperationIdle() {
					return kimi.ImportResult{}, errors.New("operation slot was not held by auto-sync")
				}
				return kimi.ImportResult{OK: true, Applied: !requireRestart, RequiresRestart: requireRestart}, nil
			}
			manualID, _ := h.n.beginOperation(context.Background(), "rebind-conversations")
			d := h.doc(t)
			h.n.autoSync.observe(&mismatch, d)
			waitAutoSync(t, "repair waiting for the operation slot", func() bool {
				h.n.autoSync.mu.Lock()
				defer h.n.autoSync.mu.Unlock()
				return h.n.autoSync.deferred
			})
			if imports.Load() != 0 {
				t.Fatal("repair ran concurrently with rebind")
			}
			h.n.finishOperation(manualID, "done", nil, nil)
			want := autoSyncSynced
			if requireRestart {
				want = autoSyncRequiresRestart
			}
			waitAutoSyncState(t, h, want)
			h.n.autoSync.observe(&mismatch, d)
			h.n.autoSync.kickIfReady()
			h.n.mu.Lock()
			action := core.String(h.n.operation["action"])
			h.n.mu.Unlock()
			if action != "auto-sync" || imports.Load() != 1 || h.n.autoSync.status()["pending"] == true {
				t.Fatalf("duplicate repair or restart: action=%q imports=%d status=%v", action, imports.Load(), h.n.autoSync.status())
			}
		})
	}
}

func TestAutoSyncProviderRepairRefusesDisconnectedAndStaleReadback(t *testing.T) {
	for _, mode := range []string{"disconnected", "vanished", "restored", "direct"} {
		t.Run(mode, func(t *testing.T) {
			h := newAutoSyncHarness(t, mode == "direct", autoSyncGatewayDoc())
			h.set(func(h *autoSyncHarness) { h.managed, h.running = true, true })
			d := h.doc(t)
			mismatch := kimi.IntegrationResult{
				State: kimi.IntegrationReady, Applied: true, Adopted: true,
				GatewayProviderMismatch: "byok-55555555-5555-4555-8555-555555555555",
				Readback:                &kimi.ImportReadback{Active: true, AccountVerified: true, AccountDigest: "verified-account"},
			}
			h.n.autoSyncReconcile = func(context.Context) kimi.IntegrationResult {
				return kimi.IntegrationResult{State: kimi.IntegrationNeedsImport, Reason: "profile_missing"}
			}
			switch mode {
			case "disconnected":
				id, _ := h.n.beginOperation(context.Background(), "disconnect-models")
				h.n.autoSync.observe(&mismatch, d)
				h.n.finishOperation(id, "done", nil, nil)
			case "restored":
				mismatch.Readback.Active = false
			case "direct":
				d["integrationMode"] = "direct"
			}
			h.n.autoSync.observe(&mismatch, d)
			if mode == "vanished" {
				waitAutoSyncState(t, h, autoSyncIdle)
			}
			if h.counts() != 0 || h.n.autoSync.status()["pending"] == true {
				t.Fatalf("inactive provider triggered import: %d, status=%v", h.counts(), h.n.autoSync.status())
			}
		})
	}
}

func TestAutoSyncProviderRepairRetriesWhileImportSurfaceMounts(t *testing.T) {
	for _, tc := range []struct{ state, reason string }{
		{kimi.ImportStateWaitingWork, kimi.NativeWaitingWork},
		{kimi.ImportStateWaitingSettings, kimi.NativeWaitingSettings},
	} {
		t.Run(tc.state, func(t *testing.T) {
			h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
			h.n.autoSync.debounce = 300 * time.Millisecond
			h.set(func(h *autoSyncHarness) { h.managed, h.running = true, true })
			mismatch := kimi.IntegrationResult{
				State: kimi.IntegrationReady, Applied: true, Adopted: true,
				GatewayProviderMismatch: "byok-55555555-5555-4555-8555-555555555555",
				Readback:                &kimi.ImportReadback{Active: true, AccountVerified: true, AccountDigest: "verified-account"},
			}
			var imports, openWork atomic.Int32
			h.n.autoSyncReconcile = func(context.Context) kimi.IntegrationResult {
				if imports.Load() < 2 {
					return mismatch
				}
				return kimi.IntegrationResult{State: kimi.IntegrationReady, Applied: true, Adopted: true}
			}
			h.n.autoOpenWork = func(context.Context) error {
				openWork.Add(1)
				return nil
			}
			h.n.importModels = func(context.Context) (kimi.ImportResult, error) {
				if imports.Add(1) == 1 {
					return kimi.ImportResult{State: tc.state, Reason: tc.reason}, nil
				}
				return kimi.ImportResult{OK: true, Applied: true, Adopted: true}, nil
			}
			d := h.doc(t)
			h.n.autoSync.observe(&mismatch, d)
			waitAutoSync(t, "scheduled repair retry", func() bool {
				status := h.n.autoSync.status()
				return status["state"] == autoSyncScheduled && status["reason"] == tc.reason && h.n.autoSyncOperationIdle()
			})
			if status := h.n.autoSync.status(); status["pending"] != true || imports.Load() != 1 {
				t.Fatalf("transient result settled the work: status=%v imports=%d", status, imports.Load())
			}
			manualID, err := h.n.beginOperation(context.Background(), "rebind-conversations")
			if err != nil {
				t.Fatal("transient wait still held the operation slot")
			}
			h.n.autoSync.mu.Lock()
			timer := h.n.autoSync.timer
			h.n.autoSync.mu.Unlock()
			h.n.autoSync.observe(&mismatch, d)
			h.n.autoSync.kickIfReady()
			h.n.autoSync.mu.Lock()
			sameTimer := timer != nil && h.n.autoSync.timer == timer
			h.n.autoSync.mu.Unlock()
			if !sameTimer {
				t.Fatal("periodic readback replaced the retry timer")
			}
			waitAutoSync(t, "retry behind the rebind", func() bool {
				h.n.autoSync.mu.Lock()
				defer h.n.autoSync.mu.Unlock()
				return h.n.autoSync.deferred
			})
			if imports.Load() != 1 {
				t.Fatal("scheduled retry ran concurrently with rebind")
			}
			h.n.finishOperation(manualID, "done", nil, nil)
			waitAutoSyncState(t, h, autoSyncSynced)
			if imports.Load() != 2 || openWork.Load() != 0 || h.n.autoSync.status()["pending"] == true {
				t.Fatalf("retry did not settle exactly once: imports=%d openWork=%d status=%v", imports.Load(), openWork.Load(), h.n.autoSync.status())
			}
		})
	}
}

func TestAutoSyncProviderRepairRetryStopsOnLimitErrorOrDisconnect(t *testing.T) {
	for _, mode := range []string{"limit", "error", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
			if mode == "disconnect" {
				h.n.autoSync.debounce = 300 * time.Millisecond
			}
			h.set(func(h *autoSyncHarness) { h.managed, h.running = true, true })
			mismatch := kimi.IntegrationResult{
				State: kimi.IntegrationReady, Applied: true, Adopted: true,
				GatewayProviderMismatch: "byok-55555555-5555-4555-8555-555555555555",
				Readback:                &kimi.ImportReadback{Active: true, AccountVerified: true, AccountDigest: "verified-account"},
			}
			var imports atomic.Int32
			const terminalReason = "native_import_store_changed"
			h.n.autoSyncReconcile = func(context.Context) kimi.IntegrationResult { return mismatch }
			h.n.importModels = func(context.Context) (kimi.ImportResult, error) {
				if imports.Add(1) > 1 && mode == "error" {
					return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: terminalReason}, errors.New(terminalReason)
				}
				return kimi.ImportResult{State: kimi.ImportStateWaitingWork, Reason: kimi.NativeWaitingWork}, nil
			}
			d := h.doc(t)
			h.n.autoSync.observe(&mismatch, d)
			wantState, wantReason := autoSyncFailed, kimi.NativeWaitingWork
			wantImports := int32(autoSyncProviderRepairRetries + 1)
			switch mode {
			case "disconnect":
				waitAutoSync(t, "waiting before retry", func() bool {
					return h.n.autoSync.status()["reason"] == kimi.NativeWaitingWork && h.n.autoSyncOperationIdle()
				})
				id, err := h.n.beginOperation(context.Background(), "disconnect-models")
				if err != nil {
					t.Fatal("cannot remove takeover while retry is delayed")
				}
				h.n.finishOperation(id, "removed", nil, nil)
				wantState, wantReason, wantImports = autoSyncIdle, autoSyncReasonDisconnected, 1
			case "error":
				wantReason, wantImports = terminalReason, 2
			}
			waitAutoSyncState(t, h, wantState)
			h.n.autoSync.observe(&mismatch, d)
			h.n.autoSync.kickIfReady()
			if status := h.n.autoSync.status(); imports.Load() != wantImports || status["pending"] == true || status["reason"] != wantReason {
				t.Fatalf("repair did not settle: imports=%d want=%d status=%v", imports.Load(), wantImports, status)
			}
		})
	}
}
