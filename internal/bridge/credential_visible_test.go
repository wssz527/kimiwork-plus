package bridge

import (
	"errors"
	"testing"

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

func TestCredentialRowRetainsDiagnosticCode(t *testing.T) {
	store := &visibleTestKeys{testKeys: testKeys{}, readErr: errors.New("credential_keychain_locked")}
	rows := connectionRows(map[string]any{"connections": []any{map[string]any{"id": "fixture", "apiKeyRef": "connection-fixture"}}}, store)
	row := core.Object(rows[0])
	if row["credentialError"] != "credential_keychain_locked" || row["credentialRecovery"] != "resave-required" {
		t.Fatalf("diagnostic lost: %+v", row)
	}
}
