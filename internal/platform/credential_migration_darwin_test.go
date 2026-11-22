//go:build darwin && cgo

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreviewCredentialsStayInLegacyKeychain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := OpenCredentials("dev.kimi-work-plus.preview.test-migration")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*keychain); !ok {
		t.Fatalf("preview store is %T, want legacy keychain", store)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, LegacyPreviewStateDirName, "secrets")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opening preview credentials created a local secret store")
	}
}
