package kimi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
