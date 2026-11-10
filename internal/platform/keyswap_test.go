package platform

import (
	"errors"
	"sync"
	"testing"
)

// memoryStore is a Credentials implementation that behaves like a native store
// for the properties these tests care about, including the one that matters most:
// an unreadable item is reported as access denied rather than as missing.
type memoryStore struct {
	mu sync.Mutex
	// values is what the store holds.
	values map[string]string
	// unreadable names keys that exist but cannot be read, which is what an ACL
	// that does not include this binary looks like.
	unreadable map[string]bool
	// denyWrites makes every write fail, as a locked keychain does.
	denyWrites bool
	// mangle, when set, transforms a value on write so a read-back differs.
	mangle func(string) string
	// writes records every key written, in order, so an orphan is detectable.
	writes []string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{values: map[string]string{}, unreadable: map[string]bool{}}
}

func (m *memoryStore) Get(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.values[key]
	if !ok {
		return "", ErrCredentialMissing
	}
	if m.unreadable[key] {
		return "", ErrCredentialAccessDenied
	}
	return value, nil
}

func (m *memoryStore) Set(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.denyWrites {
		return ErrCredentialAccessDenied
	}
	if m.mangle != nil {
		value = m.mangle(value)
	}
	m.values[key] = value
	m.writes = append(m.writes, key)
	return nil
}

func (m *memoryStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
	delete(m.unreadable, key)
	return nil
}

func (m *memoryStore) live() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.values))
	for key := range m.values {
		keys = append(keys, key)
	}
	return keys
}

// A staged value must be readable under its new reference before any caller can
// commit that reference.
func TestStageWritesThenReadsBack(t *testing.T) {
	store := newMemoryStore()
	transaction, err := NewTransaction(store)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Close()
	ref, err := transaction.Stage("sk-live-key")
	if err != nil {
		t.Fatal(err)
	}
	if ref == "" || ref[:len(RefPrefix)] != RefPrefix {
		t.Fatalf("staged reference %q does not carry the staged prefix", ref)
	}
	value, err := store.Get(ref)
	if err != nil || value != "sk-live-key" {
		t.Fatalf("the staged value is not readable: %q %v", value, err)
	}
}

// An uncommitted reference must be removed, or every failed save would leave a
// secret in the store forever.
func TestAnUncommittedStageIsDiscarded(t *testing.T) {
	store := newMemoryStore()
	transaction, err := NewTransaction(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Stage("sk-live-key"); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Close(); err != nil {
		t.Fatal(err)
	}
	if live := store.live(); len(live) != 0 {
		t.Fatalf("the discarded stage survived: %v", live)
	}
}

// A committed reference must survive Close. This is the bug a deferred Close
// causes when Commit is not honoured.
func TestACommittedStageSurvivesClose(t *testing.T) {
	store := newMemoryStore()
	transaction, err := NewTransaction(store)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := transaction.Stage("sk-live-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Close(); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Get(ref); err != nil || value != "sk-live-key" {
		t.Fatalf("the committed value was removed: %q %v", value, err)
	}
}

// A store that cannot be read back must not be committed to. The value is removed
// and the caller gets a specific error instead of a reference that will fail
// later.
func TestAFailedReadbackLeavesNothingBehind(t *testing.T) {
	store := newMemoryStore()
	store.mangle = func(value string) string { return value + "-truncated" }
	transaction, err := NewTransaction(store)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Close()
	if _, err := transaction.Stage("sk-live-key"); !errors.Is(err, ErrCredentialReadbackMismatch) {
		t.Fatalf("a mismatched read-back returned %v", err)
	}
	if live := store.live(); len(live) != 0 {
		t.Fatalf("a mismatched read-back left a value: %v", live)
	}
}

// A denied write must be reported as a denial, not as a generic failure: the
// panel's remedy is different.
func TestADeniedWriteIsReportedAsDenied(t *testing.T) {
	store := newMemoryStore()
	store.denyWrites = true
	transaction, err := NewTransaction(store)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Close()
	if _, err := transaction.Stage("sk-live-key"); !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatalf("a denied write returned %v", err)
	}
}

// ReplaceValue must not commit when the caller's config switch fails. The previous
// reference is what stays active, and the staged value is discarded.
func TestReplaceValueKeepsTheOldReferenceWhenTheSwitchFails(t *testing.T) {
	store := newMemoryStore()
	// The key that is currently live.
	if err := store.Set("connection-live", "sk-old"); err != nil {
		t.Fatal(err)
	}
	commitErr := errors.New("config_commit_failed")
	_, err := ReplaceValue(store, "sk-new", func(string) error { return commitErr })
	if !errors.Is(err, commitErr) {
		t.Fatalf("a failed config switch returned %v", err)
	}
	if value, err := store.Get("connection-live"); err != nil || value != "sk-old" {
		t.Fatalf("the live reference changed: %q %v", value, err)
	}
	// Only the live key remains: the staged one was discarded.
	if live := store.live(); len(live) != 1 || live[0] != "connection-live" {
		t.Fatalf("store after a failed switch: %v", live)
	}
}

// The successful path: a new reference becomes live and the caller's config was
// switched before the transaction committed.
func TestReplaceValueCommitsAfterTheSwitch(t *testing.T) {
	store := newMemoryStore()
	var switched string
	ref, err := ReplaceValue(store, "sk-new", func(candidate string) error {
		// The value must already be readable when the config switches to it.
		if value, err := store.Get(candidate); err != nil || value != "sk-new" {
			t.Fatalf("the config switched to an unreadable reference: %q %v", value, err)
		}
		switched = candidate
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if switched != ref {
		t.Fatalf("the config switched to %q but the transaction returned %q", switched, ref)
	}
	if value, err := store.Get(ref); err != nil || value != "sk-new" {
		t.Fatalf("the committed value: %q %v", value, err)
	}
}

// Probe must tell "no key saved" from "key saved but unreadable", because only the
// second one is fixed by re-entering the same key.
func TestProbeDistinguishesMissingFromUnreadable(t *testing.T) {
	store := newMemoryStore()
	if probe := Probe(store, "web-tools-tavily"); probe.State != CredentialStateMissing {
		t.Fatalf("absent key: %+v", probe)
	}
	store.values["web-tools-tavily"] = "tvly-key"
	if probe := Probe(store, "web-tools-tavily"); probe.State != CredentialStatePresent {
		t.Fatalf("readable key: %+v", probe)
	}
	store.unreadable["web-tools-tavily"] = true
	probe := Probe(store, "web-tools-tavily")
	if probe.State != CredentialStateAccessDenied || probe.Reason != "credential_access_denied" {
		t.Fatalf("unreadable key: %+v", probe)
	}
	if probe.Detail == "" {
		t.Fatal("an access denial with no explanation for the panel")
	}
	// A nil store is unavailable, not missing: the difference is that no key can
	// ever be saved here.
	if probe := Probe(nil, "any"); probe.State != CredentialStateUnavailable {
		t.Fatalf("nil store: %+v", probe)
	}
}

// Re-saving over an unreadable item must work. The old ciphertext is replaced
// under a new reference, which is the only remedy this product offers and the one
// the panel's "re-enter the key" action performs.
func TestReplacingAnUnreadableKeySucceeds(t *testing.T) {
	store := newMemoryStore()
	store.values["connection-old"] = "sk-old"
	store.unreadable["connection-old"] = true
	if probe := Probe(store, "connection-old"); probe.State != CredentialStateAccessDenied {
		t.Fatalf("setup: %+v", probe)
	}
	ref, err := ReplaceValue(store, "sk-new", nil)
	if err != nil {
		t.Fatalf("re-saving over an unreadable item failed: %v", err)
	}
	if ref == "connection-old" {
		t.Fatal("the replacement reused the unreadable reference")
	}
	if value, err := store.Get(ref); err != nil || value != "sk-new" {
		t.Fatalf("the new reference: %q %v", value, err)
	}
	// The old item is left for the caller to delete once its config no longer
	// names it; deleting it here would break a config that still points at it.
	if _, err := store.Get("connection-old"); !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatal("the old item was removed before the config moved off it")
	}
}
