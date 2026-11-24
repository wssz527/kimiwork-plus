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
