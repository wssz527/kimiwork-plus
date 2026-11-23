package bridge

import (
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

// A refused import must never be rewritten by what an earlier readback found.
// The readback may still describe the previous import, which is exactly why it
// cannot turn a failure into an applied one.
func TestSettledNativeImportReceiptKeepsRefusedImportRefused(t *testing.T) {
	adopted := &kimi.IntegrationResult{State: kimi.IntegrationReady, Applied: true, Adopted: true}
	failed := kimi.ImportResult{State: kimi.ImportStateFailed, Reason: "native_import_bridge_timeout"}

	settled := settledNativeImportReceipt(adopted, failed)
	if settled.OK || settled.Applied || settled.Adopted || settled.State != kimi.ImportStateFailed ||
		settled.Reason != "native_import_bridge_timeout" {
		t.Fatalf("a refused import was promoted by an adopted readback: %+v", settled)
	}

	// The cleanup news is still true for any receipt that describes this
	// profile, so a refusal keeps it cleared without gaining any success.
	refusedPending := kimi.ImportResult{State: kimi.ImportStateFailed, Reason: "route_cleanup_pending", RouteCleanupPending: true}
	settled = settledNativeImportReceipt(&kimi.IntegrationResult{Applied: true}, refusedPending)
	if settled.State != kimi.ImportStateFailed || settled.RouteCleanupPending || settled.Reason != "" {
		t.Fatalf("a refused import kept a stale cleanup reason: %+v", settled)
	}
}

// An accepted receipt says the page took the request and the readback did not
// yet agree. Once the readback holds the import and the running application
// adopted it, the receipt has to settle to applied instead of leaving the panel
// on its waiting-for-disk reason forever.
func TestSettledNativeImportReceiptSettlesAcceptedImport(t *testing.T) {
	accepted := kimi.ImportResult{OK: true, State: kimi.ImportStateAccepted,
		Reason: "native_import_not_verified", Applied: true}

	settled := settledNativeImportReceipt(&kimi.IntegrationResult{State: kimi.IntegrationReady,
		Applied: true, Adopted: true}, accepted)
	if settled.State != kimi.ImportStateApplied || settled.Reason != "" ||
		!settled.Applied || !settled.Adopted || settled.RouteCleanupPending {
		t.Fatalf("an adopted import stayed accepted: %+v", settled)
	}

	// Without adoption there is no evidence the application loaded anything, so
	// the receipt keeps saying the import is not verified yet.
	pending := settledNativeImportReceipt(&kimi.IntegrationResult{State: kimi.IntegrationReady,
		Applied: true}, accepted)
	if pending.State != kimi.ImportStateAccepted || pending.Reason != "native_import_not_verified" {
		t.Fatalf("an unadopted import was settled: %+v", pending)
	}

	// A remaining cleanup is real work, not a settled import: it outranks the
	// applied state even though the new definition is already in place.
	cleaning := settledNativeImportReceipt(&kimi.IntegrationResult{State: kimi.IntegrationReady,
		Applied: true, Adopted: true, RouteCleanupPending: true},
		kimi.ImportResult{OK: true, State: kimi.ImportStateAccepted,
			Reason: "route_cleanup_pending", Applied: true, RouteCleanupPending: true})
	if cleaning.State != kimi.ImportStateAccepted || cleaning.Reason != "route_cleanup_pending" ||
		!cleaning.RouteCleanupPending {
		t.Fatalf("an accepted import hid its remaining cleanup: %+v", cleaning)
	}

	// A failure code the readback cannot explain is kept: adoption proves the
	// models are in place, not why the page refused something else.
	other := kimi.ImportResult{OK: true, State: kimi.ImportStateAccepted, Reason: "native_import_bridge_contract"}
	settled = settledNativeImportReceipt(&kimi.IntegrationResult{Applied: true, Adopted: true}, other)
	if settled.State != kimi.ImportStateApplied || settled.Reason != "native_import_bridge_contract" {
		t.Fatalf("an unrelated reason was cleared by adoption: %+v", settled)
	}
}

// The operation carries the newest action of any kind. An open-work click or a
// compatibility check must not take the import receipt with it, or the panel
// reports a restart-free, synced state while the import still needs a restart.
func TestNativeStateKeepsImportReceiptAfterUnrelatedOperation(t *testing.T) {
	n := newReceiptTestNative(t)
	current := &kimi.IntegrationResult{State: kimi.IntegrationReady,
		Readback: &kimi.ImportReadback{}, Applied: true, Adopted: false}

	deferred := kimi.ImportResult{OK: true, State: kimi.ImportStateRequiresRestart,
		Reason: kimi.NativeRequiresRestart, Applied: true, RequiresRestart: true, RestartDeferred: true}
	n.rememberImportReceipt("connect-kimi", deferred)

	n.mu.Lock()
	before, ok := n.publishedNativeImport(current)
	n.mu.Unlock()
	if !ok || !before.RequiresRestart || before.State != kimi.ImportStateRequiresRestart {
		t.Fatalf("the import receipt was lost before any other operation: %+v", before)
	}

	// A compatibility check replaces Native.operation with a result that is not
	// an import at all. The receipt has to survive it.
	n.mu.Lock()
	n.operation = core.Document{"id": "later", "action": "check-compatibility", "state": "done",
		"result": core.Document{"compatibility": "current"}}
	published, ok := n.publishedNativeImport(current)
	n.mu.Unlock()
	if !ok || !published.RequiresRestart || !published.RestartDeferred {
		t.Fatalf("an unrelated operation dropped the pending restart: %+v", published)
	}

	// Once the readback adopts the import, the same receipt settles to applied
	// and stops asking for a restart.
	adopted := *current
	adopted.Adopted = true
	n.mu.Lock()
	settled, ok := n.publishedNativeImport(&adopted)
	n.mu.Unlock()
	if !ok || settled.RequiresRestart || settled.RestartDeferred ||
		settled.State != kimi.ImportStateApplied || settled.Reason != "" {
		t.Fatalf("an adopted import kept its restart request: %+v", settled)
	}
}

// The receipt describes one profile in one mode. A different account or the
// other integration mode must not borrow it, while a route the mode switch left
// behind stays visible because the selected mode cannot settle it.
func TestPublishedNativeImportRejectsOtherProfile(t *testing.T) {
	n := newReceiptTestNative(t)
	n.rememberImportReceipt("connect-kimi", kimi.ImportResult{OK: true, State: kimi.ImportStateRequiresRestart,
		Reason: kimi.NativeRequiresRestart, Applied: true, RequiresRestart: true,
		Readback: &kimi.ImportReadback{AccountDigest: "account-a"}})

	n.mu.Lock()
	_, ok := n.publishedNativeImport(&kimi.IntegrationResult{State: kimi.IntegrationReady, Applied: true, AccountDigest: "account-b"})
	n.mu.Unlock()
	if ok {
		t.Fatal("another account inherited the import receipt")
	}

	n.mu.Lock()
	_, ok = n.publishedNativeImport(&kimi.IntegrationResult{State: kimi.IntegrationReady,
		Applied: true, Mode: kimi.IntegrationModeDirect, AccountDigest: "account-a"})
	n.mu.Unlock()
	if ok {
		t.Fatal("the other integration mode inherited the import receipt")
	}

	n = newReceiptTestNative(t)
	n.rememberImportReceipt("prepare-native", kimi.ImportResult{OK: true, State: kimi.ImportStateAccepted,
		Reason: "route_cleanup_pending", Applied: true, RouteCleanupPending: true})
	n.mu.Lock()
	published, ok := n.publishedNativeImport(&kimi.IntegrationResult{State: kimi.IntegrationReady,
		Applied: true, Mode: kimi.IntegrationModeDirect})
	n.mu.Unlock()
	if !ok || !published.RouteCleanupPending || published.Reason != "route_cleanup_pending" ||
		published.RequiresRestart {
		t.Fatalf("the mode switch hid its remaining cleanup: %+v", published)
	}
}

func TestNativeImportReceiptKeepsAccountBoundaries(t *testing.T) {
	adopted := &kimi.IntegrationResult{State: kimi.IntegrationReady, Applied: true, Adopted: true}
	mismatch := kimi.ImportResult{OK: false, State: kimi.ImportStateAccountChanged,
		Reason: kimi.NativeAccountChanged, Applied: false, Adopted: false}
	settled := settledNativeImportReceipt(adopted, mismatch)
	if settled.OK || settled.Applied || settled.Adopted || settled.State != kimi.ImportStateAccountChanged {
		t.Fatalf("an account mismatch was promoted: %+v", settled)
	}

	n := newReceiptTestNative(t)
	n.rememberImportReceipt("prepare-native", kimi.ImportResult{OK: false, State: kimi.ImportStateFailed,
		Reason: "native_import_failed", RouteCleanupPending: true,
		Readback: &kimi.ImportReadback{AccountDigest: "account-a"}})
	n.mu.Lock()
	_, ok := n.publishedNativeImport(&kimi.IntegrationResult{Mode: kimi.IntegrationModeDirect, AccountDigest: "account-a"})
	n.mu.Unlock()
	if ok {
		t.Fatal("a failed import was promoted to accepted across modes")
	}

	n = newReceiptTestNative(t)
	n.rememberImportReceipt("prepare-native", kimi.ImportResult{OK: true, State: kimi.ImportStateAccepted,
		Reason: "route_cleanup_pending", RouteCleanupPending: true,
		Readback: &kimi.ImportReadback{AccountDigest: "account-a"}})
	n.mu.Lock()
	_, ok = n.publishedNativeImport(&kimi.IntegrationResult{Mode: kimi.IntegrationModeDirect, AccountDigest: "account-b"})
	n.mu.Unlock()
	if ok {
		t.Fatal("cleanup pending leaked into another account")
	}
}

func newReceiptTestNative(t *testing.T) *Native {
	t.Helper()
	n := newDirectTestNative(t, false)
	if err := n.initEnhancements(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.web.Close() })
	return n
}
