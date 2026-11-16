package bridge

import (
	"errors"
	"sync"
	"testing"
	"time"

	"kimiworkplus.local/migration/internal/core"
)

type visibleTestKeys struct {
	testKeys
	visible       bool
	visibleWrites int
	plainWrites   int
	writeErr      error
	readErr       error
}

func (k *visibleTestKeys) SessionVisible() bool { return k.visible }
func (k *visibleTestKeys) SetVisible(ref, value string) error {
	k.visibleWrites++
	if k.writeErr != nil {
		return k.writeErr
	}
	return k.testKeys.Set(ref, value)
}
func (k *visibleTestKeys) Set(ref, value string) error {
	k.plainWrites++
	return k.testKeys.Set(ref, value)
}
func (k *visibleTestKeys) Get(ref string) (string, error) {
	if k.readErr != nil {
		return "", k.readErr
	}
	return k.testKeys[ref], nil
}

func TestVisibleCredentialSaveUsesSystemInteraction(t *testing.T) {
	store := &visibleTestKeys{testKeys: testKeys{}, visible: true}
	keys := CacheKeys(store)
	if err := stageCredential(keys, "connection-fixture", "synthetic"); err != nil {
		t.Fatal(err)
	}
	if store.visibleWrites != 1 || store.plainWrites != 0 {
		t.Fatalf("visible save did not use the system store: %+v", store)
	}
	actual, err := store.Get("connection-fixture")
	if err != nil || actual != "synthetic" {
		t.Fatal("the value was not readable without the cache")
	}
}

func TestVisibleCredentialFailureAndReadback(t *testing.T) {
	for _, test := range []struct {
		name     string
		visible  bool
		writeErr error
		readErr  error
		want     string
	}{
		{name: "background", want: "credential_session_unavailable"},
		{name: "denied", visible: true, writeErr: errors.New("credential_authorization_cancelled"), want: "credential_authorization_cancelled"},
		{name: "readback", visible: true, readErr: errors.New("credential_keychain_locked"), want: "credential_keychain_locked"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &visibleTestKeys{testKeys: testKeys{}, visible: test.visible, writeErr: test.writeErr, readErr: test.readErr}
			err := stageCredential(CacheKeys(store), "connection-fixture", "synthetic")
			if err == nil || err.Error() != test.want {
				t.Fatalf("wrong error: %v", err)
			}
			if _, exists := store.testKeys["connection-fixture"]; exists {
				t.Fatal("failed staging retained a new credential")
			}
		})
	}
}

type reauthorizeTestKeys struct {
	visibleTestKeys
	visibleReads int
	visibleErr   error
}

func (k *reauthorizeTestKeys) GetVisible(ref string) (string, error) {
	k.visibleReads++
	if k.visibleErr != nil {
		return "", k.visibleErr
	}
	return k.testKeys[ref], nil
}

func TestExplicitConnectionApplyReauthorizesUnreadableCredentials(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "denied"}[denied], func(t *testing.T) {
			store := &reauthorizeTestKeys{visibleTestKeys: visibleTestKeys{testKeys: testKeys{"connection-fixture": "synthetic"}, readErr: errors.New("credential_authorization_failed")}}
			if denied {
				store.visibleErr = errors.New("credential_authorization_cancelled")
			}
			keys := CacheKeys(store)
			connection := map[string]any{"id": "fixture", "apiKeyRef": "connection-fixture"}
			service := &Service{Keys: keys}
			err := service.authorizeConnectionCredentials(core.Document{"connections": []any{connection}})
			if store.visibleReads != 1 || store.visibleWrites != 0 || store.plainWrites != 0 {
				t.Fatal("explicit apply did not authorize the existing reference read-only")
			}
			if denied {
				if err == nil || err.Error() != "credential_authorization_cancelled" {
					t.Fatalf("denied authorization: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				value, err := credentialFor(connection, keys)
				if err != nil || value != "synthetic" {
					t.Fatal("authorized credential was not available to the runtime")
				}
			}
			if connection["apiKeyRef"] != "connection-fixture" || store.testKeys["connection-fixture"] != "synthetic" {
				t.Fatal("authorization changed the saved credential")
			}
		})
	}
}

func TestConnectionAuthorizationDoesNotPromptForReadableCredentials(t *testing.T) {
	store := &reauthorizeTestKeys{visibleTestKeys: visibleTestKeys{testKeys: testKeys{"connection-fixture": "synthetic"}}}
	service := &Service{Keys: CacheKeys(store)}
	err := service.authorizeConnectionCredentials(core.Document{"connections": []any{map[string]any{"apiKeyRef": "connection-fixture"}}})
	if err != nil || store.visibleReads != 0 {
		t.Fatal("readable credential requested unnecessary system authorization")
	}
}

func TestCredentialRowRetainsDiagnosticCode(t *testing.T) {
	store := &visibleTestKeys{testKeys: testKeys{}, readErr: errors.New("credential_keychain_locked")}
	rows := connectionRows(map[string]any{"connections": []any{map[string]any{"id": "fixture", "apiKeyRef": "connection-fixture"}}}, store)
	row := core.Object(rows[0])
	if row["credentialError"] != "credential_keychain_locked" || row["credentialRecovery"] != "resave-required" {
		t.Fatalf("diagnostic lost: %+v", row)
	}
}

// blockedVisibleKeys models a store whose authorization prompt is an open
// channel. Visible calls take the process-wide interaction switch a real
// keychain uses and block until the test releases them; background reads take
// the same switch, so a read issued while the prompt is open waits for the user
// exactly as the macOS platform does.
type blockedVisibleKeys struct {
	interaction sync.Mutex
	mu          sync.Mutex
	values      map[string]string
	promptErr   error
	promptOpen  int

	entered chan string
	release chan struct{}

	plainDuringPrompt int
	promptCount       int
}

func newBlockedVisibleKeys() *blockedVisibleKeys {
	return &blockedVisibleKeys{values: map[string]string{}, entered: make(chan string, 8), release: make(chan struct{})}
}

func (k *blockedVisibleKeys) awaitPrompt(label string) {
	k.mu.Lock()
	k.promptOpen++
	k.promptCount++
	k.mu.Unlock()
	k.entered <- label
	<-k.release
	k.mu.Lock()
	k.promptOpen--
	k.mu.Unlock()
}

func (k *blockedVisibleKeys) promptFailure() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.promptErr
}

func (k *blockedVisibleKeys) SessionVisible() bool { return true }

// GetVisible answers with the value as it stood before the dialog was answered,
// the way a stale system read can: a write or delete that lands while the
// prompt is open makes that answer obsolete, which the cache must survive.
func (k *blockedVisibleKeys) GetVisible(ref string) (string, error) {
	if !k.interaction.TryLock() {
		k.interaction.Lock()
	}
	defer k.interaction.Unlock()
	k.mu.Lock()
	value, ok := k.values[ref]
	k.mu.Unlock()
	k.awaitPrompt("read:" + ref)
	if err := k.promptFailure(); err != nil {
		return "", err
	}
	if !ok {
		value = "synthetic-visible"
	}
	return value, nil
}

func (k *blockedVisibleKeys) SetVisible(ref, value string) error {
	k.interaction.Lock()
	defer k.interaction.Unlock()
	k.awaitPrompt("write:" + ref)
	if err := k.promptFailure(); err != nil {
		return err
	}
	k.mu.Lock()
	k.values[ref] = value
	k.mu.Unlock()
	return nil
}

func (k *blockedVisibleKeys) Get(ref string) (string, error) {
	k.mu.Lock()
	if k.promptOpen > 0 {
		k.plainDuringPrompt++
	}
	k.mu.Unlock()
	k.interaction.Lock()
	defer k.interaction.Unlock()
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.values[ref], nil
}

func (k *blockedVisibleKeys) Set(ref, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.values[ref] = value
	return nil
}

func (k *blockedVisibleKeys) Delete(ref string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.values, ref)
	return nil
}

func (k *blockedVisibleKeys) readsDuringPrompt() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.plainDuringPrompt
}

func (k *blockedVisibleKeys) prompts() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.promptCount
}

func (k *blockedVisibleKeys) setPromptErr(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.promptErr = err
}

func (k *blockedVisibleKeys) stored(ref string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.values[ref]
}

// mustReturn runs fn and fails the test when it has not returned while the
// authorization prompt is still open.
func mustReturn(t *testing.T, budget time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(budget):
		t.Fatalf("%s did not return while the authorization dialog was open", what)
	}
}

func waitForPrompt(t *testing.T, store *blockedVisibleKeys) {
	t.Helper()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the visible store never received the authorization call")
	}
}

func releasePrompt(t *testing.T, store *blockedVisibleKeys, done <-chan struct{}) {
	t.Helper()
	select {
	case store.release <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked authorization call did not request release")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the authorized call did not finish after release")
	}
}

func TestBackgroundReadsStayResponsiveWhileAuthorizationIsPending(t *testing.T) {
	store := newBlockedVisibleKeys()
	store.values["cached"] = "synthetic-cached"
	keys := CacheKeys(store)
	if value, err := keys.Get("cached"); err != nil || value != "synthetic-cached" {
		t.Fatalf("test setup did not cache the readable credential: %q %v", value, err)
	}

	var visibleErr error
	visibleDone := make(chan struct{})
	go func() {
		defer close(visibleDone)
		_, visibleErr = keys.GetVisible("connection-pending")
	}()
	waitForPrompt(t, store)

	var cached string
	var cachedErr error
	mustReturn(t, 3*time.Second, "a cached read", func() { cached, cachedErr = keys.Get("cached") })
	if cachedErr != nil || cached != "synthetic-cached" {
		t.Fatalf("a cached value was not readable while authorization was pending: %q %v", cached, cachedErr)
	}

	var pending string
	var pendingErr error
	mustReturn(t, 3*time.Second, "a background read", func() { pending, pendingErr = keys.Get("connection-pending") })
	if pendingErr == nil || pendingErr.Error() != "credential_authorization_required" {
		t.Fatalf("an uncached background read did not report the pending authorization: %q %v", pending, pendingErr)
	}

	var rows []any
	mustReturn(t, 3*time.Second, "the panel state", func() {
		rows = connectionRows(core.Document{"connections": []any{
			core.Document{"id": "pending", "apiKeyRef": "connection-pending"},
			core.Document{"id": "cached", "apiKeyRef": "cached"},
		}}, keys)
	})
	first, second := core.Object(rows[0]), core.Object(rows[1])
	if first["credentialError"] != "credential_authorization_required" || first["apiKeySet"] != false {
		t.Fatalf("state row lost the pending authorization: %+v", first)
	}
	if second["apiKeySet"] != true || second["credentialError"] != nil {
		t.Fatalf("state row lost the readable credential: %+v", second)
	}

	releasePrompt(t, store, visibleDone)
	if visibleErr != nil {
		t.Fatalf("the authorized read failed: %v", visibleErr)
	}
	if value, err := keys.Get("connection-pending"); err != nil || value != "synthetic-visible" {
		t.Fatalf("the authorized value was not cached: %q %v", value, err)
	}
	if reads := store.readsDuringPrompt(); reads != 0 {
		t.Fatalf("background store reads ran while the prompt was open: %d", reads)
	}
}

func TestServiceStateStaysResponsiveWhileAuthorizationIsPending(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	d["connections"] = []any{core.Document{"id": "pending", "name": "Pending", "baseURL": "http://127.0.0.1:9999/v1", "apiKeyEnv": "PENDING_KEY", "apiKeyRef": "connection-pending"}}
	if _, _, err := store.Commit(d, rev); err != nil {
		t.Fatal(err)
	}
	blocked := newBlockedVisibleKeys()
	service := &Service{Store: store, Keys: CacheKeys(blocked)}

	visibleDone := make(chan struct{})
	go func() {
		defer close(visibleDone)
		_, _ = getVisibleCredential(service.Keys, "connection-pending")
	}()
	waitForPrompt(t, blocked)

	var state core.Document
	var stateErr error
	mustReturn(t, 3*time.Second, "/api/state", func() { state, stateErr = service.State() })
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	rows := core.Rows(core.Object(state["connections"])["saved"])
	if len(rows) != 1 {
		t.Fatalf("state rows = %d", len(rows))
	}
	row := core.Object(rows[0])
	if row["credentialError"] != "credential_authorization_required" || row["apiKeySet"] != false {
		t.Fatalf("state did not report the pending authorization: %+v", row)
	}

	releasePrompt(t, blocked, visibleDone)
}

func TestCancelledAuthorizationCachesNothingAndRetries(t *testing.T) {
	store := newBlockedVisibleKeys()
	keys := CacheKeys(store)

	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err = keys.GetVisible("connection-pending")
	}()
	waitForPrompt(t, store)
	store.setPromptErr(errors.New("credential_authorization_cancelled"))
	releasePrompt(t, store, done)
	if err == nil || err.Error() != "credential_authorization_cancelled" {
		t.Fatalf("cancellation was not reported: %v", err)
	}
	keys.mu.Lock()
	_, cached := keys.values["connection-pending"]
	keys.mu.Unlock()
	if cached {
		t.Fatal("a cancelled authorization cached a value")
	}

	store.setPromptErr(nil)
	var retriedErr error
	retried := make(chan struct{})
	go func() {
		defer close(retried)
		_, retriedErr = keys.GetVisible("connection-pending")
	}()
	waitForPrompt(t, store)
	releasePrompt(t, store, retried)
	if retriedErr != nil {
		t.Fatalf("retry after cancellation failed: %v", retriedErr)
	}
	if value, err := keys.Get("connection-pending"); err != nil || value != "synthetic-visible" {
		t.Fatalf("retry did not cache the authorized value: %q %v", value, err)
	}
}

func TestVisibleWritesDoNotBlockBackgroundReads(t *testing.T) {
	store := newBlockedVisibleKeys()
	keys := CacheKeys(store)

	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = keys.SetVisible("connection-pending", "synthetic-new")
	}()
	waitForPrompt(t, store)

	var readErr error
	mustReturn(t, 3*time.Second, "a background read during a save", func() { _, readErr = keys.Get("connection-pending") })
	if readErr == nil || readErr.Error() != "credential_authorization_required" {
		t.Fatalf("a save prompt did not make background reads report the pending authorization: %v", readErr)
	}
	releasePrompt(t, store, done)
	if err != nil {
		t.Fatalf("the visible write failed: %v", err)
	}
	if value, err := keys.Get("connection-pending"); err != nil || value != "synthetic-new" {
		t.Fatalf("the visible write was not readable afterwards: %q %v", value, err)
	}
}

func TestConcurrentVisibleReadsStartOnePrompt(t *testing.T) {
	store := newBlockedVisibleKeys()
	keys := CacheKeys(store)

	values := make([]string, 2)
	errs := make([]error, 2)
	finished := make(chan int, 2)
	for i := range values {
		go func(i int) {
			values[i], errs[i] = keys.GetVisible("connection-pending")
			finished <- i
		}(i)
	}
	waitForPrompt(t, store)
	// Only the read that reached the store can be waiting on the dialog; the
	// second one is still behind the gate. Releasing once must let both finish.
	store.release <- struct{}{}
	for range values {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("a concurrent visible read did not finish")
		}
	}
	for i := range values {
		if errs[i] != nil || values[i] != "synthetic-visible" {
			t.Fatalf("visible read %d = %q %v", i, values[i], errs[i])
		}
	}
	if opened := store.prompts(); opened != 1 {
		t.Fatalf("concurrent visible reads opened %d system prompts", opened)
	}
}

func TestStaleAuthorizationDoesNotOverwriteNewerWrites(t *testing.T) {
	store := newBlockedVisibleKeys()
	keys := CacheKeys(store)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = keys.GetVisible("connection-pending")
	}()
	waitForPrompt(t, store)
	mustReturn(t, 3*time.Second, "a concurrent write", func() {
		if err := keys.Set("connection-pending", "synthetic-newer"); err != nil {
			t.Error(err)
		}
	})
	releasePrompt(t, store, done)
	if value, err := keys.Get("connection-pending"); err != nil || value != "synthetic-newer" {
		t.Fatalf("a late authorization overwrote the newer write: %q %v", value, err)
	}
	if stored := store.stored("connection-pending"); stored != "synthetic-newer" {
		t.Fatalf("the store kept a stale value: %q", stored)
	}
}

func TestStaleAuthorizationDoesNotResurrectDeletes(t *testing.T) {
	store := newBlockedVisibleKeys()
	store.values["connection-pending"] = "synthetic-old"
	keys := CacheKeys(store)

	done := make(chan struct{})
	var value string
	var err error
	go func() {
		defer close(done)
		value, err = keys.GetVisible("connection-pending")
	}()
	waitForPrompt(t, store)
	mustReturn(t, 3*time.Second, "a concurrent delete", func() {
		if deleteErr := keys.Delete("connection-pending"); deleteErr != nil {
			t.Error(deleteErr)
		}
	})
	releasePrompt(t, store, done)
	if err != nil || value != "synthetic-old" {
		t.Fatalf("the authorized read did not return the value it read: %q %v", value, err)
	}
	if cached, readErr := keys.Get("connection-pending"); readErr != nil || cached != "" {
		t.Fatalf("a late authorization resurrected a deleted credential: %q %v", cached, readErr)
	}
}
