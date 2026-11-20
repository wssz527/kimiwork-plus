package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/platform"
)

type migrationMemoryKeys struct {
	mu           sync.Mutex
	values       map[string]string
	requireAuth  map[string]bool
	corruptRead  map[string]string
	deleted      []string
	visibleReads int
}

func newMigrationMemoryKeys() *migrationMemoryKeys {
	return &migrationMemoryKeys{
		values:      map[string]string{},
		requireAuth: map[string]bool{},
		corruptRead: map[string]string{},
	}
}

func (m *migrationMemoryKeys) Get(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requireAuth[name] {
		return "", errors.New("credential_authorization_required")
	}
	v, ok := m.values[name]
	if !ok {
		return "", platform.ErrCredentialMissing
	}
	if bad, hasBad := m.corruptRead[name]; hasBad {
		return bad, nil
	}
	return v, nil
}

func (m *migrationMemoryKeys) GetVisible(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.visibleReads++
	v, ok := m.values[name]
	if !ok {
		return "", platform.ErrCredentialMissing
	}
	delete(m.requireAuth, name)
	return v, nil
}

func (m *migrationMemoryKeys) Set(name, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[name] = value
	return nil
}

func (m *migrationMemoryKeys) SetVisible(name, value string) error {
	return m.Set(name, value)
}

func (m *migrationMemoryKeys) SessionVisible() bool { return true }

func (m *migrationMemoryKeys) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, name)
	m.deleted = append(m.deleted, name)
	return nil
}

func TestMigrateStateDirCopiesPersistentAndPreservesLegacy(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "kimi-work-plus-preview")
	target := filepath.Join(root, "kimi-work-plus")

	if err := os.MkdirAll(filepath.Join(legacy, "usage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacy, "service-operations"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "gateway.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "accounts.json"), []byte(`{"version":1,"accounts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "usage", "ledger.jsonl"), []byte("{\"ts\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint := `{"url":"http://127.0.0.1:49152","key":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","pid":123,"version":"1.0.0","buildId":"preview"}`
	if err := os.WriteFile(filepath.Join(legacy, "desktop-endpoint.json"), []byte(endpoint), 0o600); err != nil {
		t.Fatal(err)
	}
	// Runtime records and temporary files must not be copied into the formal state dir.
	for _, transient := range []string{"daemon.lock", "daemon.log", "service-operations/op1.json", "gateway.json.tmp"} {
		if err := os.WriteFile(filepath.Join(legacy, transient), []byte("transient"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	state, err := MigrateStateDir(target, legacy)
	if err != nil {
		t.Fatalf("MigrateStateDir failed: %v", err)
	}
	if !state.StateFilesCopied || !state.LegacyDirPreserved || state.SourceDir != legacy {
		t.Fatalf("unexpected migration state: %+v", state)
	}
	for _, want := range []string{"gateway.json", "accounts.json", "desktop-endpoint.json", filepath.Join("usage", "ledger.jsonl")} {
		if _, err := os.Stat(filepath.Join(target, want)); err != nil {
			t.Fatalf("expected %s in target dir: %v", want, err)
		}
		if _, err := os.Stat(filepath.Join(legacy, want)); err != nil {
			t.Fatalf("legacy file %s must be preserved: %v", want, err)
		}
	}
	migratedEndpoint, err := ReadEndpoint(target)
	if err != nil || migratedEndpoint.URL != "http://127.0.0.1:49152" {
		t.Fatalf("the loopback endpoint must survive migration: %+v, err=%v", migratedEndpoint, err)
	}
	for _, skip := range []string{"daemon.lock", "daemon.log", "service-operations", "gateway.json.tmp"} {
		if _, err := os.Stat(filepath.Join(target, skip)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("transient entry %s must not be copied", skip)
		}
	}

	// Re-running must be a no-op once copied.
	second, err := MigrateStateDir(target, legacy)
	if err != nil || !second.StateFilesCopied {
		t.Fatalf("second MigrateStateDir failed: %+v, err=%v", second, err)
	}
}

func TestMigrateStateDirResumesInterruptedCheckpoint(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "kimi-work-plus-preview")
	target := filepath.Join(root, "kimi-work-plus")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "gateway.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "accounts.json"), []byte(`{"version":1,"accounts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate crash after copying only accounts.json.
	if err := os.WriteFile(filepath.Join(target, "accounts.json"), []byte(`{"version":1,"accounts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveMigrationState(target, PreviewMigrationState{
		Version:            1,
		Status:             "in_progress",
		SourceDir:          legacy,
		CopiedFiles:        []string{"accounts.json"},
		LegacyDirPreserved: true,
	}); err != nil {
		t.Fatal(err)
	}

	state, err := MigrateStateDir(target, legacy)
	if err != nil {
		t.Fatalf("resume MigrateStateDir failed: %v", err)
	}
	if !state.StateFilesCopied {
		t.Fatalf("expected StateFilesCopied after resume: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(target, "gateway.json")); err != nil {
		t.Fatalf("gateway.json was not copied on resume: %v", err)
	}
}

func TestMigrateStateDirNeverOverwritesExistingTarget(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "kimi-work-plus-preview")
	target := filepath.Join(root, "kimi-work-plus")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "gateway.json"), []byte(`{"from":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "gateway.json"), []byte(`{"from":"formal"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := MigrateStateDir(target, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "not_needed" {
		t.Fatalf("expected not_needed when target already has state, got %+v", state)
	}
	raw, _ := os.ReadFile(filepath.Join(target, "gateway.json"))
	if string(raw) != `{"from":"formal"}` {
		t.Fatalf("formal gateway.json was overwritten: %s", string(raw))
	}
}

func TestMigrateCredentialsCheckpointResumeAndPreserveOld(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "preview")
	targetDir := filepath.Join(dir, "formal")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	accRef := "account-session-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	webRef := "tavily-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	gatewayDoc := `{"connections":[{"id":"c1","apiKeyRef":"conn-ref-1"}],"translation":{"apiKeyRef":"tr-ref-1"}}`
	accountsDoc := `{"version":1,"accounts":[{"id":"account-0123456789abcdef01234567","name":"A","capturedAt":1,"credentialRef":"` + accRef + `"}]}`
	webDoc := `{"credentialRef":"` + webRef + `"}`
	if err := os.WriteFile(filepath.Join(targetDir, "gateway.json"), []byte(gatewayDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "accounts.json"), []byte(accountsDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "web-tools-settings.json"), []byte(webDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	oldRaw := newMigrationMemoryKeys()
	oldRaw.values["conn-ref-1"] = "secret-conn"
	oldRaw.values["tr-ref-1"] = "secret-tr"
	oldRaw.values[accRef] = `{"cookie":"abc"}`
	oldRaw.values[webRef] = "secret-web"
	oldRaw.values["kimi-code:manual"] = "sk-kimi-manual"
	oldRaw.requireAuth[accRef] = true
	oldRaw.requireAuth["conn-ref-1"] = true

	newRaw := newMigrationMemoryKeys()
	coord := NewMigrationCoordinator(targetDir, legacyDir, CacheKeys(oldRaw), CacheKeys(newRaw))

	// First startup migration (non-interactive) migrates unlocked entries and checkpoints auth-blocked ones.
	first, err := coord.MigrateStartup()
	if err != nil {
		t.Fatalf("MigrateStartup failed: %v", err)
	}
	if first.Status != "needs_authorization" || first.CredentialsCompleted {
		t.Fatalf("expected needs_authorization after blocked startup, got %+v", first)
	}
	if len(first.PendingCredentials) != 2 {
		t.Fatalf("expected 2 pending credentials, got %v", first.PendingCredentials)
	}
	if newRaw.values["tr-ref-1"] != "secret-tr" || newRaw.values[webRef] != "secret-web" || newRaw.values["kimi-code:manual"] != "sk-kimi-manual" {
		t.Fatalf("unlocked credentials were not migrated on first pass: %+v", newRaw.values)
	}

	// Interactive resume picks up from checkpoint and finishes the remaining two.
	second, err := coord.ResumeInteractive()
	if err != nil {
		t.Fatalf("ResumeInteractive failed: %v", err)
	}
	if second.Status != "completed" || !second.CredentialsCompleted || len(second.PendingCredentials) != 0 {
		t.Fatalf("expected completed after ResumeInteractive, got %+v", second)
	}
	if newRaw.values["conn-ref-1"] != "secret-conn" || newRaw.values[accRef] != `{"cookie":"abc"}` {
		t.Fatalf("resumed credentials missing in target store: %+v", newRaw.values)
	}
	if len(oldRaw.deleted) != 0 || len(oldRaw.values) != 5 {
		t.Fatalf("old preview credentials must be preserved for one release, deleted=%v remaining=%d", oldRaw.deleted, len(oldRaw.values))
	}
	if oldRaw.visibleReads != 2 {
		t.Fatalf("expected exactly 2 visible reads for pending items, got %d", oldRaw.visibleReads)
	}
}

func TestMigrateCredentialsRejectsReadbackMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gateway.json"), []byte(`{"connections":[{"id":"c1","apiKeyRef":"conn-ref-1"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldRaw := newMigrationMemoryKeys()
	oldRaw.values["conn-ref-1"] = "expected-secret"
	newRaw := newMigrationMemoryKeys()
	newRaw.corruptRead["conn-ref-1"] = "corrupted-secret"

	state, err := MigrateCredentials(dir, dir, CacheKeys(oldRaw), CacheKeys(newRaw), false)
	if err != nil {
		t.Fatalf("MigrateCredentials returned unexpected error: %v", err)
	}
	if state.CredentialsCompleted || state.Status == "completed" {
		t.Fatalf("migration must not complete when readback mismatches: %+v", state)
	}
	if state.FailedCredentials["conn-ref-1"] != "credential_migration_verify_failed" {
		t.Fatalf("expected verify failure code, got %+v", state.FailedCredentials)
	}
}
