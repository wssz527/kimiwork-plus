package bridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAccountVaultRotatesSecretsWithoutPersistingThem(t *testing.T) {
	keys := &countedKeys{values: map[string]string{}}
	vault := newAccountVault(t.TempDir(), keys)
	first, err := vault.capture(accountCapture{UserID: "user-private-id", Name: "First", Session: json.RawMessage(`{"accessToken":"secret-old","refreshToken":"secret-refresh"}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := vault.capture(accountCapture{UserID: "user-private-id", Name: "Renamed", Session: json.RawMessage(`{"accessToken":"secret-rotated"}`)})
	if err != nil || second.ID != first.ID || len(keys.values) != 1 {
		t.Fatal(second, err, len(keys.values))
	}
	raw, err := os.ReadFile(vault.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-old", "secret-refresh", "secret-rotated", "user-private-id", "accessToken"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("secret persisted in account metadata: %s", forbidden)
		}
	}
	rows, current, err := vault.summaries()
	if err != nil || len(rows) != 1 || rows[0].Name != "Renamed" || current != first.ID {
		t.Fatal(rows, current, err)
	}
	public, _ := json.Marshal(rows)
	if strings.Contains(string(public), "credentialRef") || strings.Contains(string(public), "secret-") {
		t.Fatal("account response exposed credentials")
	}
	session, err := vault.session(first.ID)
	if err != nil || !strings.Contains(string(session), "secret-rotated") {
		t.Fatal("latest rotated credential was not restored", err)
	}
	info, err := os.Stat(vault.path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o600 != 0o600 {
			t.Fatal(info.Mode())
		}
	} else if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}

func TestAccountVaultProtectsCurrentAccountAndRetainsExpiredSession(t *testing.T) {
	keys := &countedKeys{values: map[string]string{}}
	vault := newAccountVault(t.TempDir(), keys)
	first, _ := vault.capture(accountCapture{UserID: "first", Session: json.RawMessage(`{"token":"first-secret"}`)})
	if err := vault.remove(first.ID); err == nil || err.Error() != "account_current_in_use" {
		t.Fatal(err)
	}
	if err := vault.mark(first.ID, true); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := vault.summaries()
	if !rows[0].NeedsLogin {
		t.Fatal(rows)
	}
	if _, err := vault.session(first.ID); err != nil {
		t.Fatal("expired account must remain available for recovery", err)
	}
	second, _ := vault.capture(accountCapture{UserID: "second", Session: json.RawMessage(`{"token":"second-secret"}`)})
	if err := vault.remove(first.ID); err != nil {
		t.Fatal(err)
	}
	rows, current, _ := vault.summaries()
	if len(rows) != 1 || current != second.ID || len(keys.values) != 1 {
		t.Fatal(rows, current, len(keys.values))
	}
	if _, err := vault.session(first.ID); err == nil || err.Error() != "account_not_found" {
		t.Fatal(err)
	}
}

type failingAccountDeleteKeys struct {
	countedKeys
	failDelete bool
}

func (k *failingAccountDeleteKeys) Delete(ref string) error {
	if k.failDelete {
		return errors.New("keychain_unavailable")
	}
	return k.countedKeys.Delete(ref)
}

func TestAccountVaultRetriesCredentialRemovalAfterFailure(t *testing.T) {
	keys := &failingAccountDeleteKeys{countedKeys: countedKeys{values: map[string]string{}}}
	vault := newAccountVault(t.TempDir(), keys)
	first, err := vault.capture(accountCapture{UserID: "first", Session: json.RawMessage(`{"token":"first-secret"}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := vault.capture(accountCapture{UserID: "second", Session: json.RawMessage(`{"token":"second-secret"}`)})
	if err != nil {
		t.Fatal(err)
	}
	keys.failDelete = true
	if err := vault.remove(first.ID); err == nil || err.Error() != "account_credential_cleanup_failed" {
		t.Fatal(err)
	}
	index, err := vault.read()
	if err != nil || len(index.Accounts) != 1 || len(index.RetiredCredentials) != 1 || len(keys.values) != 2 {
		t.Fatal("pending credential removal was not retained", err)
	}
	keys.failDelete = false
	if err := vault.remove(first.ID); err != nil {
		t.Fatal(err)
	}
	rows, current, err := vault.summaries()
	index, readErr := vault.read()
	if err != nil || readErr != nil || len(rows) != 1 || current != second.ID || len(keys.values) != 1 || len(index.RetiredCredentials) != 0 {
		t.Fatal("credential removal did not resume", err, readErr)
	}
}

func TestAccountVaultFailedMetadataWriteCleansStagedSecret(t *testing.T) {
	keys := &countedKeys{values: map[string]string{}}
	vault := newAccountVault(filepath.Join(t.TempDir(), "absent"), keys)
	_, err := vault.capture(accountCapture{UserID: "first", Session: json.RawMessage(`{"token":"secret"}`)})
	if err == nil || err.Error() != "account_store_write_failed" || len(keys.values) != 0 {
		t.Fatal(err, len(keys.values))
	}
}
