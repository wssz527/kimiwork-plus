//go:build !windows

package platform

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testLocalService = "dev.kimi-work-plus.test.local"
	testOtherService = "dev.kimi-work-plus.test.other"
)

// isolateConfigDir points os.UserConfigDir at a throwaway home so these tests
// never see the real master key or the real ciphertext files.
func isolateConfigDir(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	if base != home && !strings.HasPrefix(base, home+string(os.PathSeparator)) {
		t.Fatalf("config dir %q escaped the isolated home %q", base, home)
	}
}

func openTestLocalStore(t *testing.T, service string) *localSecretStore {
	t.Helper()
	credentials, err := newLocalSecretStore(service)
	if err != nil {
		t.Fatalf("newLocalSecretStore(%q): %v", service, err)
	}
	store, ok := credentials.(*localSecretStore)
	if !ok {
		t.Fatalf("newLocalSecretStore(%q) returned %T, want *localSecretStore", service, credentials)
	}
	return store
}

func testMasterKeyPath(store *localSecretStore) string {
	return filepath.Join(filepath.Dir(store.dir), "master.key")
}

// expectSecret compares a read result without ever putting the value into the
// test log.
func expectSecret(t *testing.T, label, got, want string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error %v", label, err)
	}
	if got != want {
		t.Fatalf("%s: value mismatch (got %d bytes, want %d)", label, len(got), len(want))
	}
}

func TestLocalSecretStoreRoundTrip(t *testing.T) {
	isolateConfigDir(t)
	store := openTestLocalStore(t, testLocalService)
	const (
		key    = "provider-key"
		secret = "fixture-secret-local-store-round-trip"
	)
	if _, err := store.Get(key); !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("missing key: want ErrCredentialMissing, got %v", err)
	}
	if err := store.Set(key, secret); err != nil {
		t.Fatalf("set: %v", err)
	}
	value, err := store.Get(key)
	expectSecret(t, "get", value, secret, err)
	blob, err := os.ReadFile(store.path(key))
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if bytes.Contains(blob, []byte(secret)) {
		t.Fatal("ciphertext contains the plaintext value")
	}
	if err := store.Set(key, secret+"-v2"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	value, err = store.Get(key)
	expectSecret(t, "replaced get", value, secret+"-v2", err)
	if err := store.Delete(key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(key); !errors.Is(err, ErrCredentialMissing) {
		t.Fatalf("after delete: want ErrCredentialMissing, got %v", err)
	}
	if err := store.Delete(key); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
}

func TestLocalSecretStoreSurvivesANewInstance(t *testing.T) {
	isolateConfigDir(t)
	const (
		key    = "token"
		secret = "fixture-secret-after-restart"
	)
	if err := openTestLocalStore(t, testLocalService).Set(key, secret); err != nil {
		t.Fatalf("set: %v", err)
	}
	value, err := openTestLocalStore(t, testLocalService).Get(key)
	expectSecret(t, "get from a fresh instance", value, secret, err)
}

func TestLocalSecretStoreRejectsAnotherServicesCiphertext(t *testing.T) {
	isolateConfigDir(t)
	source := openTestLocalStore(t, testLocalService)
	other := openTestLocalStore(t, testOtherService)
	const (
		key    = "token"
		secret = "fixture-secret-bound-to-one-service"
	)
	if err := source.Set(key, secret); err != nil {
		t.Fatalf("set: %v", err)
	}
	blob, err := os.ReadFile(source.path(key))
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if err := os.WriteFile(other.path(key), blob, 0o600); err != nil {
		t.Fatalf("copy ciphertext: %v", err)
	}
	value, err := other.Get(key)
	if !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatalf("ciphertext for one service read under another: want ErrCredentialAccessDenied, got %v", err)
	}
	if value != "" {
		t.Fatalf("denied read returned %d bytes", len(value))
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("the denial error carries the plaintext value")
	}
	value, err = source.Get(key)
	expectSecret(t, "original service", value, secret, err)
}

func TestLocalSecretStoreRejectsTamperedCiphertext(t *testing.T) {
	isolateConfigDir(t)
	store := openTestLocalStore(t, testLocalService)
	const (
		key    = "token"
		secret = "fixture-secret-tamper-detected"
	)
	if err := store.Set(key, secret); err != nil {
		t.Fatalf("set: %v", err)
	}
	blob, err := os.ReadFile(store.path(key))
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0xff
	if err := os.WriteFile(store.path(key), tampered, 0o600); err != nil {
		t.Fatalf("write tampered ciphertext: %v", err)
	}
	value, err := store.Get(key)
	if !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatalf("tampered ciphertext: want ErrCredentialAccessDenied, got %v", err)
	}
	if value != "" || strings.Contains(err.Error(), secret) {
		t.Fatal("the tampered read leaked the plaintext")
	}
	if err := os.WriteFile(store.path(key), blob[:4], 0o600); err != nil {
		t.Fatalf("write truncated ciphertext: %v", err)
	}
	if _, err := store.Get(key); !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatalf("truncated ciphertext: want ErrCredentialAccessDenied, got %v", err)
	}
}

func TestLocalSecretStoreKeepsSecretsOwnerOnly(t *testing.T) {
	isolateConfigDir(t)
	store := openTestLocalStore(t, testLocalService)
	if err := store.Set("token", "fixture-secret-file-modes"); err != nil {
		t.Fatalf("set: %v", err)
	}
	files := []struct {
		name string
		path string
	}{
		{"master key", testMasterKeyPath(store)},
		{"ciphertext", store.path("token")},
	}
	for _, file := range files {
		info, err := os.Stat(file.path)
		if err != nil {
			t.Fatalf("%s %s: %v", file.name, filepath.Base(file.path), err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s %s has mode %04o, want 0600", file.name, filepath.Base(file.path), mode)
		}
	}
}

func TestLocalSecretStoreRefusesAMasterKeyOfTheWrongLength(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		t.Run(fmt.Sprintf("%d-bytes", size), func(t *testing.T) {
			isolateConfigDir(t)
			store := openTestLocalStore(t, testLocalService)
			path := testMasterKeyPath(store)
			before := bytes.Repeat([]byte{0x5a}, size)
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatalf("write master key: %v", err)
			}
			if _, err := localMasterKey(); err == nil {
				t.Fatal("a master key of the wrong length was accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read master key after rejection: %v", err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("the rejected master key was rewritten: %d bytes became %d", len(before), len(after))
			}
			if err := store.Set("token", "fixture-secret-wrong-master"); err == nil {
				t.Fatal("Set succeeded although the master key is unusable")
			}
			after, err = os.ReadFile(path)
			if err != nil {
				t.Fatalf("read master key after failed set: %v", err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("a failed Set rewrote the master key: %d bytes became %d", len(before), len(after))
			}
		})
	}
}

// A temporary name derived only from the key is shared by every writer of that
// key, so debris from an interrupted write blocks or corrupts the next one.
func TestLocalSecretStoreUsesAUniqueTemporaryFile(t *testing.T) {
	isolateConfigDir(t)
	store := openTestLocalStore(t, testLocalService)
	const (
		key    = "token"
		secret = "fixture-secret-unique-temp"
	)
	if err := os.Mkdir(store.path(key)+".tmp", 0o700); err != nil {
		t.Fatalf("prepare debris at the fixed temporary path: %v", err)
	}
	if err := store.Set(key, secret); err != nil {
		t.Fatalf("set with debris at a predictable temporary path: %v", err)
	}
	value, err := store.Get(key)
	expectSecret(t, "get", value, secret, err)
}

// Concurrent writers of one key must not share a temporary file: with a fixed
// name, the second writer fails or publishes half of the first one's bytes.
func TestLocalSecretStoreConcurrentWritesToOneKey(t *testing.T) {
	isolateConfigDir(t)
	store := openTestLocalStore(t, testLocalService)
	const key = "token"
	wanted := map[string]bool{}
	values := make([]string, 8)
	for index := range values {
		values[index] = fmt.Sprintf("fixture-secret-concurrent-%d", index)
		wanted[values[index]] = true
	}
	var (
		wait    sync.WaitGroup
		mu      sync.Mutex
		failure error
	)
	for index, value := range values {
		wait.Add(1)
		go func(index int, value string) {
			defer wait.Done()
			if err := store.Set(key, value); err != nil {
				mu.Lock()
				if failure == nil {
					failure = fmt.Errorf("writer %d: %w", index, err)
				}
				mu.Unlock()
			}
		}(index, value)
	}
	wait.Wait()
	if failure != nil {
		t.Fatalf("concurrent set: %v", failure)
	}
	value, err := store.Get(key)
	if err != nil {
		t.Fatalf("get after concurrent writes: %v", err)
	}
	if !wanted[value] {
		t.Fatalf("get returned %d bytes that no writer stored", len(value))
	}
}

func TestLocalSecretStoreLeavesNoTemporaryFiles(t *testing.T) {
	isolateConfigDir(t)
	store := openTestLocalStore(t, testLocalService)
	for _, write := range []struct{ key, value string }{
		{"token", "fixture-secret-first"},
		{"token", "fixture-secret-second"},
		{"other-token", "fixture-secret-third"},
	} {
		if err := store.Set(write.key, write.value); err != nil {
			t.Fatalf("set %q: %v", write.key, err)
		}
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatalf("read store directory: %v", err)
	}
	expected := map[string]bool{
		filepath.Base(store.path("token")):       true,
		filepath.Base(store.path("other-token")): true,
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file %q survived a completed write", entry.Name())
		}
		if !expected[entry.Name()] {
			t.Fatalf("unexpected entry %q in the store directory", entry.Name())
		}
	}
	for name := range expected {
		if _, err := os.Stat(filepath.Join(store.dir, name)); err != nil {
			t.Fatalf("expected ciphertext %q: %v", name, err)
		}
	}
}

func TestLocalSecretStoreSupportsLongKeys(t *testing.T) {
	isolateConfigDir(t)
	store := openTestLocalStore(t, testLocalService)
	for _, size := range []int{108, 128, 256} {
		key := strings.Repeat("k", size)
		if err := store.Set(key, "fixture-long-key"); err != nil {
			t.Fatalf("Set %d-byte key: %v", size, err)
		}
		value, err := store.Get(key)
		expectSecret(t, "long key", value, "fixture-long-key", err)
		if err := store.Delete(key); err != nil {
			t.Fatalf("Delete %d-byte key: %v", size, err)
		}
	}
}

func TestLocalSecretStoreDoesNotReplaceAMissingMasterKeyOnRead(t *testing.T) {
	isolateConfigDir(t)
	store := openTestLocalStore(t, testLocalService)
	if err := store.Set("token", "fixture-secret"); err != nil {
		t.Fatal(err)
	}
	master := testMasterKeyPath(store)
	if err := os.Remove(master); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("token"); !errors.Is(err, errLocalMasterKey) {
		t.Fatalf("read without master key: %v", err)
	}
	if _, err := os.Stat(master); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a read created a replacement master key")
	}
}

func TestLocalSecretStoreRejectsOversizedCiphertext(t *testing.T) {
	isolateConfigDir(t)
	store := openTestLocalStore(t, testLocalService)
	if err := os.WriteFile(store.path("token"), make([]byte, maxSecretBytes+29), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("token"); !errors.Is(err, ErrCredentialAccessDenied) {
		t.Fatalf("oversized ciphertext: %v", err)
	}
}
