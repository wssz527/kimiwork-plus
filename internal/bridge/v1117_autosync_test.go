package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

// A gateway background write needs the shared BYOK gate too. A machine that
// still needs the elevated step is published for the panel; the timer never
// prompts and never restarts Kimi.
func TestAutoSyncGatewayPrepareRefusalAsksForPreparation(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	h.n.byokPrepare = func(context.Context) error { return errors.New(autoSyncReasonPrepareNeeded) }
	restarts := 0
	h.n.restartOfficial = func(context.Context) error {
		restarts++
		return nil
	}

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	waitAutoSyncState(t, h, autoSyncFailed)
	if reason := core.String(h.n.autoSync.status()["reason"]); reason != autoSyncReasonGatewayPrepareNeeded {
		t.Fatalf("gateway background reason = %q", reason)
	}
	if imports := h.counts(); imports != 0 {
		t.Fatalf("an unprepared gateway install ran %d imports", imports)
	}
	if restarts != 0 {
		t.Fatalf("a background write restarted Kimi %d times", restarts)
	}
}

// Any other prepare failure keeps its own code: the panel has to show the step
// that really failed.
func TestAutoSyncGatewayPrepareFailureKeepsItsCode(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted = true, true, true })
	h.n.byokPrepare = func(context.Context) error { return errors.New("native_byok_intranet_unavailable") }

	next := h.doc(t)
	next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
	h.save(t, "/api/models", next)
	waitAutoSyncState(t, h, autoSyncFailed)
	if reason := core.String(h.n.autoSync.status()["reason"]); reason != "native_byok_intranet_unavailable" {
		t.Fatalf("gateway background reason = %q", reason)
	}
	if imports := h.counts(); imports != 0 {
		t.Fatalf("an unprepared gateway install ran %d imports", imports)
	}
}

// An account switch this product performed owns the catch-up: the readback names
// the previous account's import record, and the queued write must cross it.
func TestAutoSyncAccountSwitchCatchUpRebindsTheQueuedImport(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted, h.accountChanged = true, true, true, true })
	h.finishOperation(t, "account-switch")
	waitAutoSync(t, "the account catch-up", func() bool { return h.counts() == 1 })
	waitAutoSyncState(t, h, autoSyncSynced)
	h.n.autoSync.mu.Lock()
	rebind := h.n.autoSync.allowAccountRebind
	h.n.autoSync.mu.Unlock()
	if rebind {
		t.Fatal("the settled catch-up kept the rebind")
	}
}

// A sign-in inside the Kimi window is the user's decision, not this product's,
// so the same readback still refuses a silent import.
func TestAutoSyncExternalAccountChangeStillRefusesAnImport(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	h.set(func(h *autoSyncHarness) { h.managed, h.running, h.adopted, h.accountChanged = true, true, true, true })
	h.finishOperation(t, "launch-kimi")
	waitAutoSyncState(t, h, autoSyncFailed)
	if reason := core.String(h.n.autoSync.status()["reason"]); reason != "account_changed" {
		t.Fatalf("external account change reason = %q", reason)
	}
	if imports := h.counts(); imports != 0 {
		t.Fatalf("an external account change imported %d times", imports)
	}
}

func TestAutoSyncAccountRebindBelongsToTheQueuedGeneration(t *testing.T) {
	h := newAutoSyncHarness(t, false, autoSyncGatewayDoc())
	coordinator := h.n.autoSync
	flag := func() bool {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return coordinator.allowAccountRebind
	}
	set := func(value bool) {
		coordinator.mu.Lock()
		coordinator.allowAccountRebind = value
		coordinator.mu.Unlock()
	}
	for name, clear := range map[string]func(){
		"reset":   coordinator.reset,
		"abandon": func() { coordinator.abandon(autoSyncFailed, "configuration_unavailable") },
		"finish":  func() { coordinator.finish(autoSyncSynced, "", false, "", "") },
	} {
		t.Run(name+" clears the rebind", func(t *testing.T) {
			set(true)
			clear()
			if flag() {
				t.Fatalf("%s kept the rebind", name)
			}
		})
	}
	t.Run("a surface save clears the rebind", func(t *testing.T) {
		set(true)
		next := h.doc(t)
		next["models"] = append(core.Rows(next["models"]), autoSyncModel("gpt-beta", "Beta"))
		h.save(t, "/api/models", next)
		if flag() {
			t.Fatal("a surface save kept the rebind")
		}
	})
	t.Run("an unrelated save keeps it", func(t *testing.T) {
		set(true)
		h.save(t, "/api/models", h.doc(t))
		if !flag() {
			t.Fatal("an unchanged save consumed the rebind")
		}
	})
	t.Run("a disconnected switch never allows it", func(t *testing.T) {
		coordinator.mu.Lock()
		coordinator.disconnected = true
		coordinator.mu.Unlock()
		coordinator.operationFinished("account-switch", "", nil, nil)
		if flag() {
			t.Fatal("a disconnected account switch allowed a rebind")
		}
	})
}
