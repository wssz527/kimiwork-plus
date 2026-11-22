package platform

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

type migrationTestStore struct {
	values           map[string]string
	getErr           error
	visibleGetErr    error
	setErr           error
	deleteErr        error
	readbackOverride *string
	events           *[]string
	name             string
	visibleReads     int
}

func newMigrationTestStore(name string, events *[]string) *migrationTestStore {
	return &migrationTestStore{values: map[string]string{}, name: name, events: events}
}

func (s *migrationTestStore) record(operation string) {
	if s.events != nil {
		*s.events = append(*s.events, s.name+"."+operation)
	}
}

func (s *migrationTestStore) Get(key string) (string, error) {
	s.record("get")
	if s.getErr != nil {
		return "", s.getErr
	}
	value, ok := s.values[key]
	if !ok {
		return "", ErrCredentialMissing
	}
	if s.readbackOverride != nil {
		return *s.readbackOverride, nil
	}
	return value, nil
}

func (s *migrationTestStore) GetVisible(key string) (string, error) {
	s.record("get-visible")
	s.visibleReads++
	if s.visibleGetErr != nil {
		return "", s.visibleGetErr
	}
	value, ok := s.values[key]
	if !ok {
		return "", ErrCredentialMissing
	}
	return value, nil
}

func (s *migrationTestStore) Set(key, value string) error {
	s.record("set")
	if s.setErr != nil {
		return s.setErr
	}
	s.values[key] = value
	return nil
}

func (s *migrationTestStore) Delete(key string) error {
	s.record("delete")
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.values, key)
	return nil
}

func TestCredentialMigrationPrefersCurrentValue(t *testing.T) {
	current := newMigrationTestStore("current", nil)
	legacy := newMigrationTestStore("legacy", nil)
	current.values["ref"] = "current-value"
	legacy.values["ref"] = "legacy-value"
	store := &migratingCredentialStore{current: current, legacy: legacy}
	for _, read := range []func(string) (string, error){store.Get, store.GetVisible} {
		value, err := read("ref")
		if err != nil || value != "current-value" {
			t.Fatalf("read = %q, %v", value, err)
		}
	}
	if len(legacy.values) != 1 || legacy.visibleReads != 0 {
		t.Fatal("a current-value read touched the legacy store")
	}
}

func TestCredentialMigrationDeletesLegacyOnlyAfterReadback(t *testing.T) {
	var events []string
	current := newMigrationTestStore("current", &events)
	legacy := newMigrationTestStore("legacy", &events)
	legacy.values["ref"] = "legacy-value"
	store := &migratingCredentialStore{current: current, legacy: legacy}
	value, err := store.Get("ref")
	if err != nil || value != "legacy-value" {
		t.Fatalf("migration = %q, %v", value, err)
	}
	if current.values["ref"] != value {
		t.Fatal("migrated value was not stored locally")
	}
	if _, ok := legacy.values["ref"]; ok {
		t.Fatal("legacy value survived a verified migration")
	}
	want := []string{"current.get", "legacy.get", "current.set", "current.get", "legacy.delete"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("operations = %v, want %v", events, want)
	}
}

func TestCredentialMigrationReadbackFailurePreservesLegacy(t *testing.T) {
	current := newMigrationTestStore("current", nil)
	legacy := newMigrationTestStore("legacy", nil)
	legacy.values["ref"] = "legacy-value"
	unexpected := "unexpected"
	current.readbackOverride = &unexpected
	store := &migratingCredentialStore{current: current, legacy: legacy}
	if _, err := store.Get("ref"); !errors.Is(err, ErrCredentialReadbackMismatch) {
		t.Fatalf("migration error = %v", err)
	}
	if legacy.values["ref"] != "legacy-value" {
		t.Fatal("legacy value was deleted before a matching readback")
	}
	if _, ok := current.values["ref"]; ok {
		t.Fatal("unverified current value survived the failed migration")
	}
	current.readbackOverride = nil
	value, err := store.Get("ref")
	if err != nil || value != "legacy-value" {
		t.Fatalf("retry returned %q, %v", value, err)
	}
}

func TestCredentialMigrationVisibleReadMigratesOnce(t *testing.T) {
	current := newMigrationTestStore("current", nil)
	legacy := newMigrationTestStore("legacy", nil)
	legacy.values["ref"] = "legacy-value"
	legacy.getErr = ErrCredentialAccessDenied
	store := &migratingCredentialStore{current: current, legacy: legacy}
	if _, err := store.Get("ref"); !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatalf("background read error = %v", err)
	}
	value, err := store.GetVisible("ref")
	if err != nil || value != "legacy-value" {
		t.Fatalf("visible migration = %q, %v", value, err)
	}
	if _, err := store.GetVisible("ref"); err != nil {
		t.Fatal(err)
	}
	if legacy.visibleReads != 1 {
		t.Fatalf("visible legacy reads = %d, want 1", legacy.visibleReads)
	}
}

func TestCredentialMigrationWritesDoNotRequireLegacyAccess(t *testing.T) {
	current := newMigrationTestStore("current", nil)
	legacy := newMigrationTestStore("legacy", nil)
	legacy.deleteErr = ErrCredentialAccessDenied
	store := &migratingCredentialStore{current: current, legacy: legacy}
	if err := store.SetVisible("ref", "new-value"); err != nil {
		t.Fatalf("local visible write failed: %v", err)
	}
	if current.values["ref"] != "new-value" || !store.SessionVisible() {
		t.Fatal("local write unexpectedly depends on a system authorization session")
	}
}

func TestCredentialMigrationDeleteRemovesBothCopies(t *testing.T) {
	current := newMigrationTestStore("current", nil)
	legacy := newMigrationTestStore("legacy", nil)
	current.values["ref"] = "current-value"
	legacy.values["ref"] = "legacy-value"
	store := &migratingCredentialStore{current: current, legacy: legacy}
	if err := store.Delete("ref"); err != nil {
		t.Fatal(err)
	}
	if len(current.values) != 0 || len(legacy.values) != 0 {
		t.Fatal("delete left a credential behind")
	}
}

func TestCredentialMigrationDeleteFailureKeepsCurrentValue(t *testing.T) {
	current := newMigrationTestStore("current", nil)
	legacy := newMigrationTestStore("legacy", nil)
	current.values["ref"] = "current-value"
	legacy.values["ref"] = "legacy-value"
	legacy.deleteErr = ErrCredentialAccessDenied
	store := &migratingCredentialStore{current: current, legacy: legacy}
	if err := store.Delete("ref"); !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatalf("delete error = %v", err)
	}
	if current.values["ref"] != "current-value" {
		t.Fatal("current value was deleted while an older legacy value remained")
	}
	legacy.deleteErr = nil
	if err := store.Delete("ref"); err != nil {
		t.Fatal(err)
	}
	if len(current.values) != 0 || len(legacy.values) != 0 {
		t.Fatal("retry did not delete both copies")
	}
}

func TestCredentialMigrationSerializesConcurrentWrites(t *testing.T) {
	current := newMigrationTestStore("current", nil)
	legacy := newMigrationTestStore("legacy", nil)
	store := &migratingCredentialStore{current: current, legacy: legacy}
	values := []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}
	var wait sync.WaitGroup
	errorsCh := make(chan error, len(values))
	for _, value := range values {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := store.Set("ref", value); err != nil {
				errorsCh <- err
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent Set: %v", err)
	}
	value, err := store.Get("ref")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range values {
		found = found || value == candidate
	}
	if !found {
		t.Fatal("final value was not written by any caller")
	}
}
