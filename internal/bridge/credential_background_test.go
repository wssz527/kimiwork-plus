package bridge

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

type mismatchedReadbackKeys struct {
	testKeys
}

func (k *mismatchedReadbackKeys) Set(ref, value string) error {
	k.testKeys["stored-elsewhere"] = value
	delete(k.testKeys, ref)
	return nil
}

func TestBackgroundStagingNeverUsesTheVisiblePath(t *testing.T) {
	store := &visibleTestKeys{testKeys: testKeys{}, visible: true}
	keys := CacheKeys(store)
	if err := stageCredentialBackground(keys, "connection-fixture", "synthetic"); err != nil {
		t.Fatal(err)
	}
	if store.visibleWrites != 0 {
		t.Fatalf("a background capture opened the visible path: %+v", store)
	}
	if store.plainWrites != 1 {
		t.Fatalf("a background capture did not use the background write once: %+v", store)
	}
	if value, err := keys.Get("connection-fixture"); err != nil || value != "synthetic" {
		t.Fatalf("the verified value was not readable: %q %v", value, err)
	}
	if err := deleteCredentialBackground(keys, "connection-fixture"); err != nil {
		t.Fatal(err)
	}
	if _, exists := store.testKeys["connection-fixture"]; exists {
		t.Fatal("a background delete retained the credential")
	}
}

func TestBackgroundStagingFailsSafelyAndRetries(t *testing.T) {
	for _, test := range []struct {
		name string
		keys func() (KeyStore, map[string]string)
		code string
	}{
		{
			name: "readback mismatch",
			keys: func() (KeyStore, map[string]string) {
				values := map[string]string{}
				return &mismatchedReadbackKeys{testKeys: values}, values
			},
			code: "credentials_readback_failed",
		},
		{
			name: "locked keychain",
			keys: func() (KeyStore, map[string]string) {
				values := map[string]string{}
				return &visibleTestKeys{testKeys: values, readErr: errors.New("credential_keychain_locked")}, values
			},
			code: "credential_keychain_locked",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, values := test.keys()
			keys := CacheKeys(store)
			if err := stageCredentialBackground(keys, "connection-fixture", "synthetic"); err == nil || err.Error() != test.code {
				t.Fatalf("wrong error: %v", err)
			}
			if values["connection-fixture"] != "" {
				t.Fatal("a failed background stage left the new value behind")
			}
			if _, cached := keys.cached("connection-fixture"); cached {
				t.Fatal("a failed background stage left a cached value")
			}
		})
	}
}

func TestBackgroundAccountCaptureKeepsIndexOnStageFailure(t *testing.T) {
	keys := &mismatchedReadbackKeys{testKeys: testKeys{}}
	vault := newAccountVault(t.TempDir(), keys)
	if _, err := vault.captureBackground(accountCapture{UserID: "first", Session: json.RawMessage(`{"token":"secret"}`)}); err == nil || err.Error() != "credentials_readback_failed" {
		t.Fatalf("wrong error: %v", err)
	}
	rows, current, err := vault.summaries()
	if err != nil || len(rows) != 0 || current != "" {
		t.Fatalf("a failed capture updated the index: %+v %q %v", rows, current, err)
	}
	if len(keys.testKeys["stored-elsewhere"]) == 0 {
		t.Fatal("test setup did not write the mismatched value")
	}
	index, err := vault.read()
	if err != nil || len(index.Accounts) != 0 || len(index.RetiredCredentials) != 0 {
		t.Fatalf("a failed capture rewrote the index: %+v %v", index, err)
	}
}

func TestBackgroundAccountCaptureFailsFastAndRetriesAfterAuthorization(t *testing.T) {
	store := newBlockedVisibleKeys()
	keys := CacheKeys(store)
	vault := newAccountVault(t.TempDir(), keys)
	first, err := vault.captureBackground(accountCapture{UserID: "first", Session: json.RawMessage(`{"token":"first-secret"}`)})
	if err != nil {
		t.Fatal(err)
	}

	visibleDone := make(chan struct{})
	go func() {
		defer close(visibleDone)
		_, _ = keys.GetVisible("connection-pending")
	}()
	waitForPrompt(t, store)

	var captureErr error
	mustReturn(t, 3*time.Second, "a rotation during a prompt", func() {
		_, captureErr = vault.captureBackground(accountCapture{UserID: "second", Session: json.RawMessage(`{"token":"second-secret"}`)})
	})
	if captureErr == nil || captureErr.Error() != "credential_authorization_required" {
		t.Fatalf("a background capture did not report the pending authorization: %v", captureErr)
	}
	rows, current, stateErr := vault.summaries()
	if stateErr != nil || len(rows) != 1 || current != first.ID || rows[0].ID != first.ID {
		t.Fatalf("a pending authorization changed the account index: %+v %q %v", rows, current, stateErr)
	}
	index, err := vault.read()
	if err != nil || len(index.RetiredCredentials) != 0 {
		t.Fatalf("a refused capture retired a credential: %+v %v", index, err)
	}
	if store.plainStageCount != 0 {
		t.Fatalf("a background capture wrote while the prompt was open: %d", store.plainStageCount)
	}
	releasePrompt(t, store, visibleDone)

	// The next tick, after the prompt is answered, captures the same session.
	if _, err := vault.captureBackground(accountCapture{UserID: "second", Session: json.RawMessage(`{"token":"second-secret"}`)}); err != nil {
		t.Fatalf("the retried capture failed: %v", err)
	}
	rows, current, err = vault.summaries()
	if err != nil || len(rows) != 2 || current != accountID("second") {
		t.Fatalf("the retried capture did not update the index: %+v %q %v", rows, current, err)
	}
	index, err = vault.read()
	if err != nil || len(index.RetiredCredentials) != 0 {
		t.Fatalf("the retirement cleanup did not resume: %+v %v", index, err)
	}
}

func TestRetirementCleanupDefersWhileAuthorizationIsPending(t *testing.T) {
	store := newBlockedVisibleKeys()
	keys := CacheKeys(store)
	vault := newAccountVault(t.TempDir(), keys)
	first, err := vault.captureBackground(accountCapture{UserID: "first", Session: json.RawMessage(`{"token":"first-secret"}`)})
	if err != nil {
		t.Fatal(err)
	}
	index, err := vault.read()
	if err != nil || len(index.Accounts) != 1 {
		t.Fatalf("setup failed: %+v %v", index, err)
	}
	// Turn the single account's live reference into a pending retirement, which
	// is the state a cleanup that could not remove it leaves behind.
	retiredRef := index.Accounts[0].CredentialRef
	if err := vault.setPending("", false); err != nil {
		t.Fatal(err)
	}
	index.RetiredCredentials = []string{retiredRef}
	index.CurrentID = ""
	index.Accounts = []accountRecord{}
	if err := core.AtomicJSON(vault.path, index); err != nil {
		t.Fatal(err)
	}

	visibleDone := make(chan struct{})
	go func() {
		defer close(visibleDone)
		_, _ = keys.GetVisible("connection-pending")
	}()
	waitForPrompt(t, store)

	var cleanupErr error
	mustReturn(t, 3*time.Second, "a retirement cleanup during a prompt", func() {
		cleanupErr = func() error {
			vault.mu.Lock()
			defer vault.mu.Unlock()
			loaded, err := vault.read()
			if err != nil {
				return err
			}
			return vault.cleanup(&loaded)
		}()
	})
	if cleanupErr == nil || cleanupErr.Error() != "account_credential_cleanup_failed" {
		t.Fatalf("a cleanup that had to wait reported success: %v", cleanupErr)
	}
	if store.stored(retiredRef) == "" {
		t.Fatal("a pending prompt deleted a retired credential it could not remove")
	}
	loaded, err := vault.read()
	if err != nil || len(loaded.RetiredCredentials) != 1 || loaded.RetiredCredentials[0] != retiredRef {
		t.Fatalf("a pending prompt dropped the retirement reference: %+v %v", loaded, err)
	}

	releasePrompt(t, store, visibleDone)
	vault.mu.Lock()
	loaded, err = vault.read()
	if err == nil {
		err = vault.cleanup(&loaded)
	}
	vault.mu.Unlock()
	if err != nil || len(loaded.RetiredCredentials) != 0 {
		t.Fatalf("the deferred cleanup did not resume: %+v %v", loaded, err)
	}
	if store.stored(retiredRef) != "" {
		t.Fatal("the retired credential was not removed after the prompt")
	}
	_ = first
}

type alwaysFailingWriteKeys struct{ testKeys }

func (k *alwaysFailingWriteKeys) Set(string, string) error   { return errors.New("denied") }
func (k *alwaysFailingWriteKeys) Get(string) (string, error) { return "", nil }
func (k *alwaysFailingWriteKeys) Delete(string) error        { return nil }
