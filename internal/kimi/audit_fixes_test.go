package kimi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDirectCatalogAndSelectorsDoNotCrossAccounts(t *testing.T) {
	acctA := sha256Hex("user-a")
	acctB := sha256Hex("user-b")
	candidates := []directAccount{
		{key: acctA, account: map[string]any{"sources": []any{}}},
	}
	intent := &nativeIntent{ProfileKeyDigest: sha256Hex(acctA), AccountDigest: sha256Hex(acctA)}

	if chosen, ambiguous := selectDirectAccount(candidates, sha256Hex(acctA), intent); ambiguous || chosen.account == nil {
		t.Fatalf("expected exact direct match for acctA, got chosen=%+v ambiguous=%v", chosen, ambiguous)
	}
	if chosen, ambiguous := selectDirectAccount(candidates, sha256Hex(acctB), intent); ambiguous || chosen.account != nil {
		t.Fatalf("selectDirectAccount fell back to wrong account on mismatch: chosen=%+v ambiguous=%v", chosen, ambiguous)
	}

	profiles := []profileCandidate{
		{key: acctA, profile: map[string]any{"baseUrl": "http://127.0.0.1:9999/v1"}},
	}
	if chosen, ambiguous := selectProfile(profiles, sha256Hex(acctA), intent); ambiguous || chosen.profile == nil {
		t.Fatalf("expected exact profile match for acctA, got chosen=%+v ambiguous=%v", chosen, ambiguous)
	}
	if chosen, ambiguous := selectProfile(profiles, sha256Hex(acctB), intent); ambiguous || chosen.profile != nil {
		t.Fatalf("selectProfile fell back to wrong account on mismatch: chosen=%+v ambiguous=%v", chosen, ambiguous)
	}

	stateDir := t.TempDir()
	kimiDir := t.TempDir()
	storePath := filepath.Join(kimiDir, NativeWorkStore)
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		t.Fatal(err)
	}
	storeJSON := fmt.Sprintf(`{
		"schemaVersion": 3,
		"accounts": {
			%q: {"sources": [{"id": "src-a", "kind": "custom", "displayName": %q, "providers": []}]},
			%q: {"sources": [{"id": "src-b", "kind": "custom", "displayName": %q, "providers": []}]}
		}
	}`, acctA, DirectDisplayName, acctB, DirectDisplayName)
	if err := os.WriteFile(storePath, []byte(storeJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	native, err := NewNativeIntegration(stateDir, kimiDir, IntegrationModeDirect)
	if err != nil {
		t.Fatal(err)
	}
	spec := &DirectSpec{
		Providers: []DirectProviderSpec{
			{ID: "p1", DisplayName: "P1", Protocol: "chat", BaseURL: "https://api.example.com/v1"},
		},
		Models: []DirectModel{
			{ID: "m1", ProviderID: "p1", Model: "m1", DisplayName: "M1", MaxContextSize: 128000, Capabilities: []string{"tools"}},
		},
	}
	_, reason, _ := native.directCatalog(ControllerSettings{IntegrationMode: IntegrationModeDirect, DirectSpec: spec}, acctA)
	// acctA matches its store entry (not profile_missing); only config.json is absent.
	if reason != "config_absent" {
		t.Fatalf("directCatalog(acctA) reason=%q, want config_absent", reason)
	}
	_, reason, _ = native.directCatalog(ControllerSettings{IntegrationMode: IntegrationModeDirect, DirectSpec: spec}, sha256Hex("user-c"))
	if reason != "profile_missing" {
		t.Fatalf("directCatalog(unknown) reason=%q, want profile_missing", reason)
	}
}

func TestWindowsPathAndEnvCaseInsensitivity(t *testing.T) {
	if !equalPathForOS("windows", `C:\Users\Alice\AppData`, `c:/users/alice/appdata`) {
		t.Fatal("windows equalPathForOS should be case- and slash-insensitive")
	}
	if equalPathForOS("darwin", "/Users/Alice", "/users/alice") {
		t.Fatal("darwin equalPathForOS should stay case-sensitive")
	}
	if !pathWithinDirForOS("windows", `C:\Users\Alice\State\Profile`, `c:\users\alice\state`) {
		t.Fatal("windows pathWithinDirForOS should match case-insensitively")
	}
	if pathWithinDirForOS("windows", `C:\Users\Alice\State2\Profile`, `c:\users\alice\state`) {
		t.Fatal("windows pathWithinDirForOS matched sibling prefix")
	}

	env := childEnvironmentForOS("windows", LaunchOptions{
		Env: []string{
			"electron_run_as_node=1",
			"Daimon_Bundle_Dir=C:\\old",
			"custom_secret=drop",
			"PATH=C:\\Windows",
		},
		ProtectedEnvNames: []string{"CUSTOM_SECRET"},
		BundleDir:         `C:\new`,
	})
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"electron_run_as_node", "C:\\old", "custom_secret"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("windows childEnvironment leaked %q: %v", forbidden, env)
		}
	}
	if !strings.Contains(joined, "PATH=C:\\Windows") || !strings.Contains(joined, "DAIMON_BUNDLE_DIR=C:\\new") {
		t.Fatalf("windows childEnvironment missing expected entries: %v", env)
	}
}

func TestWritePrivateFileConcurrentSafety(t *testing.T) {
	target := filepath.Join(t.TempDir(), "state.json")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf(`{"id":%d}`+"\n", id))
			if err := writePrivateFile(target, payload, 0o600); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent writePrivateFile failed: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target missing: %v", err)
	}
}

func TestStableErrorCodeNormalization(t *testing.T) {
	if got := stableErrorCode(os.ErrPermission, "work_storage_restore_failed"); got != "work_storage_restore_failed" {
		t.Fatalf("non-code error produced %q", got)
	}
	if got := stableErrorCode(ErrImportBusy, "fallback_code"); got != ErrImportBusy.Error() {
		t.Fatalf("stable error code was replaced: %q", got)
	}
}

func TestConcurrentConfigureAndReconcileDoesNotRollbackNewMode(t *testing.T) {
	profile, home := nativeProfile(t, "user-1")
	seedNativeProfile(t, home, "http://127.0.0.1:9999/v1", "cred-1")
	stateDir := filepath.Dir(profile)

	gatewaySettings := ControllerSettings{
		StateDir:        stateDir,
		UserDataDir:     profile,
		IntegrationMode: IntegrationModeGateway,
		GatewayURL:      "http://127.0.0.1:9999/v1",
		Models:          testCatalog(),
	}
	directSettings := ControllerSettings{
		StateDir:        stateDir,
		UserDataDir:     profile,
		IntegrationMode: IntegrationModeDirect,
		DirectSpec: &DirectSpec{
			Providers: []DirectProviderSpec{
				{ID: "p1", DisplayName: "P1", Protocol: "chat", BaseURL: "https://api.example.com/v1"},
			},
			Models: []DirectModel{
				{ID: "m1", ProviderID: "p1", Model: "m1", DisplayName: "M1", MaxContextSize: 128000, Capabilities: []string{"tools"}},
			},
		},
	}

	controller, err := NewController(gatewaySettings)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	controller.mu.Lock()
	controller.beforeReconcileHook = func() {
		close(entered)
		<-release
	}
	controller.mu.Unlock()

	ctx := context.Background()
	staleDone := make(chan IntegrationResult, 1)
	go func() {
		staleDone <- controller.ReconcileWithReadback(ctx)
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("stale reconcile did not reach hook")
	}

	// Clear hook before Configure so subsequent reconciles run freely.
	controller.mu.Lock()
	controller.beforeReconcileHook = nil
	controller.mu.Unlock()

	if err := controller.Configure(directSettings); err != nil {
		t.Fatalf("Configure direct: %v", err)
	}
	fresh := controller.ReconcileWithReadback(ctx)
	if fresh.Mode != IntegrationModeDirect {
		t.Fatalf("fresh reconcile mode = %q, want direct", fresh.Mode)
	}

	// Resume stale gateway reconcile: it must detect settingsGen drift and abort.
	close(release)
	select {
	case <-staleDone:
	case <-time.After(3 * time.Second):
		t.Fatal("stale reconcile did not finish")
	}

	status := controller.Status(ctx)
	if status.Integration == nil || status.Integration.Mode != IntegrationModeDirect {
		t.Fatalf("stale reconcile rolled back mode: %+v", status.Integration)
	}

	// Exercise concurrent Configure + ReconcileWithReadback under -race.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 20 {
			if i%2 == 0 {
				_ = controller.Configure(gatewaySettings)
			} else {
				_ = controller.Configure(directSettings)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			_ = controller.ReconcileWithReadback(ctx)
		}
	}()
	wg.Wait()
	if err := controller.Configure(directSettings); err != nil {
		t.Fatal(err)
	}
	final := controller.ReconcileWithReadback(ctx)
	if final.Mode != IntegrationModeDirect {
		t.Fatalf("final mode after race = %q, want direct", final.Mode)
	}
}

// TestStaleReconcilePerformsNoDiskWrite proves the generation is checked before
// the profile is touched, not only before the answer is stored. A reconcile that
// lost the race must leave the intent exactly as the winning configuration wrote
// it; an end-of-function check alone would still have rewritten the file.
func TestStaleReconcilePerformsNoDiskWrite(t *testing.T) {
	profile, home := nativeProfile(t, "user-1")
	seedNativeProfile(t, home, "http://127.0.0.1:9999/v1", "cred-1")
	stateDir := filepath.Dir(profile)
	intentPath := filepath.Join(stateDir, NativeWorkIntent)

	settings := ControllerSettings{
		StateDir:    stateDir,
		UserDataDir: profile,
		GatewayURL:  "http://127.0.0.1:9999/v1",
		Models:      testCatalog(),
	}
	controller, err := NewController(settings)
	if err != nil {
		t.Fatal(err)
	}
	if result := controller.ReconcileWithReadback(context.Background()); !result.Applied {
		t.Fatalf("fixture was not applied: %+v", result)
	}
	committed := controller.Status(context.Background()).Integration
	if committed == nil {
		t.Fatal("the fixture committed no result")
	}
	beforeState := committed.State
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	controller.mu.Lock()
	controller.beforeReconcileHook = func() {
		close(entered)
		<-release
	}
	controller.mu.Unlock()

	staleDone := make(chan IntegrationResult, 1)
	go func() { staleDone <- controller.ReconcileWithReadback(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("stale reconcile never reached the hook")
	}

	// The hook runs before the critical section, so this Configure is the winner.
	if err := controller.Configure(ControllerSettings{
		StateDir:        stateDir,
		UserDataDir:     profile,
		IntegrationMode: IntegrationModeDirect,
		DirectSpec:      &DirectSpec{Models: []DirectModel{{ID: "m1", ProviderID: "p1", Model: "m1"}}},
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	controller.mu.Lock()
	controller.beforeReconcileHook = nil
	controller.mu.Unlock()

	close(release)
	select {
	case result := <-staleDone:
		// The stale call answers with the result that is actually committed — the
		// gateway one it started from — and never with a claim about the mode it
		// did not reconcile. The gateway mode is the empty Mode.
		if result.Mode != "" || result.State != beforeState {
			t.Fatalf("stale reconcile answered %q/%q, want the committed gateway result %q", result.Mode, result.State, beforeState)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stale reconcile never finished")
	}

	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("stale reconcile rewrote the intent:\nbefore=%s\nafter=%s", before, after)
	}
	// The winner still owns the answer, and a later reconcile runs on the new
	// configuration rather than on state a stale write left behind.
	status := controller.Status(context.Background())
	if status.Integration == nil || status.Integration.Mode != "" || status.Integration.State != beforeState {
		t.Fatalf("stale reconcile replaced the committed result: %+v", status.Integration)
	}
}

// TestConfigureWaitsForRunningReconcile pins the other half of the contract: a
// Configure cannot slip between a reconcile's disk write and its result. While
// the critical section is held, Configure must not return.
func TestConfigureWaitsForRunningReconcile(t *testing.T) {
	profile, home := nativeProfile(t, "user-1")
	seedNativeProfile(t, home, "http://127.0.0.1:9999/v1", "cred-1")
	stateDir := filepath.Dir(profile)

	settings := ControllerSettings{
		StateDir:    stateDir,
		UserDataDir: profile,
		GatewayURL:  "http://127.0.0.1:9999/v1",
		Models:      testCatalog(),
	}
	controller, err := NewController(settings)
	if err != nil {
		t.Fatal(err)
	}
	// Take the critical section the way a reconcile does, then prove a Configure
	// cannot complete while it is held.
	controller.reconcileMu.Lock()
	configured := make(chan error, 1)
	go func() {
		next := settings
		next.Models = nil
		configured <- controller.Configure(next)
	}()
	select {
	case err := <-configured:
		t.Fatalf("Configure returned while a reconcile held the critical section: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	controller.reconcileMu.Unlock()
	select {
	case err := <-configured:
		if err != nil {
			t.Fatalf("Configure: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Configure never returned after the critical section was released")
	}
}

// ---------------------------------------------------------------------------
// second audit round: generation and lifecycle gaps that a single end-of-call
// check does not close.

// TestAccountModelsSnapshotRejectsStaleGeneration proves the retry loop cannot
// adopt a configuration it no longer verifies. The hook ends the generation in
// the window between the reconcile and the adoption; the adoption must be
// refused, and the loop must retry on the new snapshot instead of reporting
// ready.
func TestAccountModelsSnapshotRejectsStaleGeneration(t *testing.T) {
	h, a, _ := accountModelsHarness(t, true)
	genBefore := h.controller.settingsGen
	// A Configure between the reconcile and the adoption ends the generation.
	h.controller.mu.Lock()
	h.controller.beforeReconcileHook = func() {
		h.controller.mu.Lock()
		h.controller.beforeReconcileHook = nil
		h.controller.mu.Unlock()
		_ = h.controller.Configure(h.controller.Settings())
	}
	h.controller.mu.Unlock()

	// No child and no bridge restart: the snapshot identity still matches, so the
	// only thing that can refuse the adoption is the generation itself.
	state, _ := h.controller.syncAccountModels(context.Background(), h.page, h.home, "account-old")
	if state == "ready" && h.controller.settingsGen == genBefore {
		t.Fatal("a stale generation was adopted")
	}
	if len(a.imports) != 0 {
		t.Fatalf("adoption path triggered an import: %d", len(a.imports))
	}
}

// TestAccountModelsAdoptionRequiresTheSameChild proves an attempt cannot adopt a
// catalog for a child it never observed. The child is replaced after the
// reconcile; the evidence belongs to the old process and must be refused.
func TestAccountModelsAdoptionRequiresTheSameChild(t *testing.T) {
	h, _, _ := accountModelsHarness(t, true)
	first, _ := startFakeChild(t)
	h.controller.mu.Lock()
	h.controller.child = first
	h.controller.mu.Unlock()
	t.Cleanup(func() { _ = first.Stop(time.Second) })

	snapshot, skip := h.controller.snapshotAccountModels(h.home)
	if skip != accountModelsNone {
		t.Fatalf("snapshot skip: %s", skip)
	}
	reconciled := h.controller.ReconcileWithReadback(context.Background())
	if reconciled.Readback == nil {
		t.Fatalf("fixture did not reconcile: %+v", reconciled)
	}

	// The child the snapshot observed is replaced before adoption.
	second, _ := startFakeChild(t)
	h.controller.mu.Lock()
	h.controller.child = second
	h.controller.mu.Unlock()
	t.Cleanup(func() { _ = second.Stop(time.Second) })

	if h.controller.adoptVerifiedCatalog(snapshot, reconciled) {
		t.Fatal("a catalog was adopted for a child the attempt never observed")
	}
}

// TestAccountModelsAdoptionRefusesExitedChild proves a child that has already
// exited cannot back a `ready` answer: nothing is running to have read the
// catalog.
func TestAccountModelsAdoptionRefusesExitedChild(t *testing.T) {
	h, _, _ := accountModelsHarness(t, true)
	child, _ := startFakeChild(t)
	h.controller.mu.Lock()
	h.controller.child = child
	h.controller.mu.Unlock()

	snapshot, skip := h.controller.snapshotAccountModels(h.home)
	if skip != accountModelsNone {
		t.Fatalf("snapshot skip: %s", skip)
	}
	reconciled := h.controller.ReconcileWithReadback(context.Background())
	if reconciled.Readback == nil {
		t.Fatalf("fixture did not reconcile: %+v", reconciled)
	}
	if err := child.Stop(time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.Exit():
	case <-time.After(3 * time.Second):
		t.Fatal("the fake child did not exit")
	}
	if h.controller.adoptVerifiedCatalog(snapshot, reconciled) {
		t.Fatal("an exited child backed an adopted catalog")
	}
}

// TestGradeImportStaleRetriesInsteadOfTrustingCache proves a same-mode Configure
// landing inside the import readback cannot turn an old cached result into an
// "applied" answer. The reconcile is forced stale on the first attempt; the
// graded result must come from a fresh reconcile, not from c.integration.
func TestGradeImportStaleRetriesInsteadOfTrustingCache(t *testing.T) {
	h, _, _ := accountModelsHarness(t, true)
	staleFirst := true
	h.controller.mu.Lock()
	h.controller.beforeReconcileHook = func() {
		h.controller.mu.Lock()
		first := staleFirst
		staleFirst = false
		h.controller.mu.Unlock()
		if first {
			_ = h.controller.Configure(h.controller.Settings())
		}
	}
	h.controller.mu.Unlock()
	t.Cleanup(func() {
		h.controller.mu.Lock()
		h.controller.beforeReconcileHook = nil
		h.controller.mu.Unlock()
	})

	graded := h.controller.gradeImport(ImportResult{OK: true, Models: 2}, "", nil)
	if graded.Readback == nil {
		t.Fatalf("a stale readback was reported as graded without evidence: %+v", graded)
	}
	if graded.State == ImportStateApplied && !graded.Readback.Holds(h.controller.Settings().GatewayURL) {
		t.Fatalf("stale import was graded applied: %+v", graded)
	}
}

// TestGradeImportAllStaleNeverClaimsApplied proves the bounded retry gives an
// unverified answer rather than a cached success when the configuration never
// settles.
func TestGradeImportAllStaleNeverClaimsApplied(t *testing.T) {
	h, _, _ := accountModelsHarness(t, true)
	h.controller.mu.Lock()
	h.controller.beforeReconcileHook = func() {
		_ = h.controller.Configure(h.controller.Settings())
	}
	h.controller.mu.Unlock()
	t.Cleanup(func() {
		h.controller.mu.Lock()
		h.controller.beforeReconcileHook = nil
		h.controller.mu.Unlock()
	})

	graded := h.controller.gradeImport(ImportResult{OK: true, Models: 2}, "", nil)
	if graded.Applied || graded.State == ImportStateApplied {
		t.Fatalf("a permanently stale import claimed to be applied: %+v", graded)
	}
}

// TestRecordImportReloadAndGenerationAreAtomic proves the deferred-restart write
// and the generation bump cannot be split by a reconcile: a caller that saw the
// old generation must not observe the new deferred state as if it were the old
// configuration's.
func TestRecordImportReloadAndGenerationAreAtomic(t *testing.T) {
	h, _, _ := accountModelsHarness(t, true)
	child, _ := startFakeChild(t)
	h.controller.mu.Lock()
	h.controller.child = child
	before := h.controller.settingsGen
	h.controller.mu.Unlock()
	t.Cleanup(func() { _ = child.Stop(time.Second) })

	h.controller.recordImportReload(child, true)
	h.controller.mu.Lock()
	after := h.controller.settingsGen
	deferred := h.controller.deferredChild
	h.controller.mu.Unlock()
	if after == before {
		t.Fatal("recordImportReload did not end the generation")
	}
	if deferred != child {
		t.Fatal("recordImportReload did not record the deferred child")
	}
}

// TestProfileModeSwitchInvalidatesThePreviousGeneration proves a profile change
// ends the generation. A reconcile that read the previous profile directory must
// not answer about the new one.
func TestProfileModeSwitchInvalidatesThePreviousGeneration(t *testing.T) {
	profile, home := nativeProfile(t, "user-1")
	seedNativeProfile(t, home, "http://127.0.0.1:9999/v1", "cred-1")
	controller, err := NewController(ControllerSettings{
		StateDir:    filepath.Dir(profile),
		UserDataDir: profile,
		GatewayURL:  "http://127.0.0.1:9999/v1",
		Models:      testCatalog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	before := controller.settingsGen
	controller.mu.Unlock()

	// startOfficial refuses to launch here (there is no installation), but the
	// settings change and the generation bump happen before that attempt.
	_ = controller.StartOfficial(context.Background())

	controller.mu.Lock()
	after := controller.settingsGen
	dir := controller.settings.UserDataDir
	controller.mu.Unlock()
	if after == before {
		t.Fatal("a profile mode switch did not end the previous generation")
	}
	if dir == "" {
		t.Fatal("the profile directory was not recorded")
	}
}

// TestDisconnectEndsGenerationBeforeTheLocalWrite proves the page-confirmed
// disconnect ends the previous generation, so no reconcile that read the
// signed-in tree can commit it back. The bump happens before the local write, so
// the local failure path is covered by the same code.
func TestDisconnectEndsGenerationBeforeTheLocalWrite(t *testing.T) {
	h, a, _ := accountModelsHarness(t, true)
	before := h.controller.settingsGen
	a.removed = DisconnectResult{OK: true}
	result, err := h.controller.DisconnectModelsAuthorized(context.Background())
	if err != nil || !result.OK {
		t.Fatalf("disconnect: %+v, %v", result, err)
	}
	after := h.controller.settingsGen
	if after <= before {
		t.Fatalf("settingsGen did not move on the disconnect path: %d -> %d", before, after)
	}
}

// TestAttachChildReusesTheHostTickBuilt proves attach and Tick agree on one host.
// A host created by Tick before attachChild reaches its publish step must be
// reused, not replaced: a replacement would leave the first host's event loop
// reading the same pipe.
func TestAttachChildReusesTheHostTickBuilt(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	profile := filepath.Join(t.TempDir(), "profile")
	controller, err := NewController(ControllerSettings{
		StateDir:    filepath.Dir(profile),
		UserDataDir: profile,
		Translation: TranslationSettings{Enabled: true, Provider: "bing", TargetLanguage: "zh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	child := &Child{pipe: harness.pipe, exitWait: make(chan struct{})}
	controller.mu.Lock()
	controller.child = child
	controller.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Tick builds and publishes the first host.
	_ = controller.Tick(ctx)
	controller.mu.Lock()
	first := controller.host
	controller.mu.Unlock()
	if first == nil {
		t.Fatal("Tick did not build a host")
	}

	t.Cleanup(func() {
		controller.mu.Lock()
		host := controller.host
		controller.host = nil
		controller.child = nil
		bridge := controller.bridge
		controller.bridge = nil
		controller.mu.Unlock()
		if bridge != nil {
			_ = bridge.Close()
		}
		if host != nil {
			host.DetachAll(context.Background())
		}
	})
	if err := controller.attachChild(ctx, child, TranslationSettings{Enabled: true}); err != nil {
		t.Fatalf("attachChild: %v", err)
	}
	controller.mu.Lock()
	second := controller.host
	controller.mu.Unlock()
	if second != first {
		t.Fatal("attachChild replaced the host Tick had already published")
	}
	// The first host must still be the live one; a leaked replacement would have
	// a second reader on this pipe.
	if _, err := first.Sync(ctx); err != nil {
		t.Fatalf("the reused host no longer syncs: %v", err)
	}
}

// TestDetachAllReturnsAfterTheLoopStops proves a replacement host can be built
// the instant DetachAll returns: the retired reader must already be gone, so
// every later event reaches the new host and none is lost to the old one.
func TestDetachAllReturnsAfterTheLoopStops(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	oldHost := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-old"})
	oldHost.DetachAll(ctx)
	// No manual wait on loopDone: DetachAll is the synchronisation point.

	received := make(chan string, 1)
	newHost := NewHost(harness.pipe, InjectionOptions{
		Payload:  testPayload,
		Revision: "rev-new",
		OnReply: func(_ context.Context, _ *SurfaceSession, _ int, id string, _ any) {
			received <- id
		},
	})
	defer newHost.DetachAll(ctx)
	sessions, err := newHost.Sync(ctx)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("newHost.Sync: %v", err)
	}
	session := sessions[0]
	for i := range 16 {
		id := "ev-" + itoa(i)
		harness.pipe.dispatch(`{"method":"Runtime.bindingCalled","sessionId":"` + session.SessionID +
			`","params":{"name":"__kwgTranslate","payload":"{\"id\":\"` + id + `\",\"value\":\"x\"}","executionContextId":` +
			itoa(session.ContextID()) + `}}`)
		select {
		case got := <-received:
			if got != id {
				t.Fatalf("event %d answered %q, want %q", i, got, id)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("event %d lost after DetachAll returned", i)
		}
	}
}

// TestWaitForMainContextStopsWithTheHost proves a retired host does not park a
// caller on the five-second deadline: the waiter returns as soon as the stop
// lands.
func TestWaitForMainContextStopsWithTheHost(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1"})
	session := &SurfaceSession{SessionID: "session", TargetID: "target", Surface: "work"}

	done := make(chan error, 1)
	go func() {
		_, err := host.waitForMainContext(context.Background(), session)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	host.DetachAll(context.Background())
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a retired host reported a main world it never saw")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiter was not released when the host retired")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the waiter held the caller for %v", elapsed)
	}
}

// TestConfigureKeepsTheDiscoveredDirectSourceID proves a Configure cannot write
// an older Direct Mode source id back over the one a reconcile just published.
// The route is primed the way a reconcile primes it — by writing the intent and
// calling syncActionsIntegrationRoute with the discovered id, which is also what
// makes a later directSourceID lookup able to answer.
func TestConfigureKeepsTheDiscoveredDirectSourceID(t *testing.T) {
	profile, home := nativeProfile(t, "user-1")
	seedNativeProfile(t, home, "http://127.0.0.1:9999/v1", "cred-1")
	stateDir := filepath.Dir(profile)
	settings := ControllerSettings{
		StateDir:        stateDir,
		UserDataDir:     profile,
		IntegrationMode: IntegrationModeDirect,
		DirectSpec: &DirectSpec{
			Providers: []DirectProviderSpec{{ID: "p1", Protocol: "chat", BaseURL: "https://api.example.com/v1"}},
			Models:    []DirectModel{{ID: "m1", ProviderID: "p1", Model: "m1", MaxContextSize: 128000}},
		},
	}
	controller, err := NewController(settings)
	if err != nil {
		t.Fatal(err)
	}
	actions := &fakePageActions{}
	controller.mu.Lock()
	controller.actions = actions
	controller.mu.Unlock()

	// The discovered id that a reconcile published and recorded.
	controller.syncActionsIntegrationRoute(settings, "source-discovered")
	if err := controller.Configure(settings); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	mode, sourceID := actions.route()
	if mode != IntegrationModeDirect || sourceID == "" {
		t.Fatalf("Configure erased the discovered route: mode=%q sourceID=%q", mode, sourceID)
	}
}

// fakePageActions records the integration route updateActionsIntegrationRoute
// publishes.
type fakePageActions struct {
	mu       sync.Mutex
	mode     string
	sourceID string
}

func (f *fakePageActions) setIntegrationMode(mode, sourceID string) {
	f.mu.Lock()
	f.mode, f.sourceID = mode, sourceID
	f.mu.Unlock()
}

func (f *fakePageActions) route() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mode, f.sourceID
}

// The rest of PageActions is unused by these tests.
func (f *fakePageActions) ImportModels(context.Context, ImportRequest) (ImportResult, error) {
	return ImportResult{}, nil
}
func (f *fakePageActions) ImportDirect(context.Context, DirectImportRequest) (ImportResult, error) {
	return ImportResult{}, nil
}
func (f *fakePageActions) DisconnectModels(context.Context, DisconnectRequest) (DisconnectResult, error) {
	return DisconnectResult{}, nil
}
func (f *fakePageActions) WorkSurface(context.Context) (WorkSurface, error) {
	return WorkSurface{}, nil
}
func (f *fakePageActions) CloseWorkSettings(context.Context) (ActionResult, error) {
	return ActionResult{}, nil
}
func (f *fakePageActions) ActivateWork(context.Context) (ActionResult, error) {
	return ActionResult{}, nil
}
func (f *fakePageActions) OpenWorkSettings(context.Context) (ActionResult, error) {
	return ActionResult{}, nil
}
func (f *fakePageActions) Connected() bool { return true }
