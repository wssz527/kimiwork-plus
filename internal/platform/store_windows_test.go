//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// The Windows store binds each blob to the current user through DPAPI, and the
// file it lives in is what a migration or a copied profile would carry. These
// checks are static: they assert the properties of the store that make a blob
// unusable elsewhere, without needing a Windows host to run them.
func TestTheWindowsStoreKeepsCiphertextInAPrivateDirectory(t *testing.T) {
	dir, err := dpapiDirectory()
	if err != nil {
		t.Fatal(err)
	}
	// The store must live under the per-user config directory, not a shared one:
	// DPAPI already binds the blob to this user, and putting it somewhere a
	// different account can read would only widen the blast radius.
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, base) {
		t.Fatalf("store directory %q is outside the per-user config directory %q", dir, base)
	}
	if filepath.Base(filepath.Dir(dir)) != StateDirName {
		t.Fatalf("store directory is not the product's own: %q", dir)
	}
	legacyDir, err := dpapiDirectoryForService("dev.kimi-work-plus.preview.0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(legacyDir)) != LegacyPreviewStateDirName {
		t.Fatalf("legacy preview store directory mismatch: %q", legacyDir)
	}
}

// A key maps to a file name that cannot escape the directory, whatever the key
// contains. The key is hex-encoded, so no separator, drive letter or reserved name
// survives into the path.
func TestAKeyCannotEscapeTheStoreDirectory(t *testing.T) {
	store := &dpapi{dir: filepath.Join("base", "service")}
	for _, key := range []string{"..", "../escape", `..\escape`, "C:\\absolute", "nul", "a/b", "a\x00b"} {
		path := store.path(key)
		if filepath.Dir(path) != store.dir {
			t.Fatalf("key %q produced %q outside %q", key, path, store.dir)
		}
	}
}

// CRYPTPROTECT_UI_FORBIDDEN must be passed on both calls. Without it a blob this
// user cannot open can put up a dialog, and a background status request then waits
// inside a prompt it cannot show.
func TestProtectionForbidsInteraction(t *testing.T) {
	if cryptProtectUIForbidden != 0x1 {
		t.Fatalf("CRYPTPROTECT_UI_FORBIDDEN is %#x, want 0x1", cryptProtectUIForbidden)
	}
}

// An empty blob must be refused before it reaches CryptUnprotectData, whose blob
// pointer would otherwise be nil.
func TestAnEmptyCiphertextIsRefused(t *testing.T) {
	if _, err := unprotect(nil); err == nil {
		t.Fatal("an empty ciphertext was accepted")
	}
}

func TestNativeRegistryRoundTrip(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), "kwp-registry-")
	if err != nil {
		t.Fatal(err)
	}
	subkey := `Software\` + filepath.Base(dir)
	testKey := hkcPrefix + subkey
	valueName := ServiceLabel
	cmdLine := `"C:\Program Files\Kimi Work+\gateway-daemon.exe" "--start-background"`
	t.Cleanup(func() {
		name, err := syscall.UTF16PtrFromString(subkey)
		if err != nil {
			t.Error(err)
			return
		}
		code, _, _ := advapi32Registry.NewProc("RegDeleteKeyW").Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(name)))
		if code != 0 && code != uintptr(syscall.ERROR_FILE_NOT_FOUND) {
			t.Error(syscall.Errno(code))
		}
	})
	if _, err := executeWindowsRegistry([]string{"add", testKey, "/v", valueName, "/t", "REG_SZ", "/d", cmdLine, "/f"}); err != nil {
		t.Fatalf("registry add failed: %v", err)
	}
	queried, err := executeWindowsRegistry([]string{"query", testKey, "/v", valueName})
	if err != nil {
		t.Fatalf("registry query failed: %v", err)
	}
	if !parseRegistryRunValue(queried) || !strings.Contains(queried, cmdLine) {
		t.Fatalf("unexpected registry query output: %q", queried)
	}
	if _, err := executeWindowsRegistry([]string{"delete", testKey, "/v", valueName, "/f"}); err != nil {
		t.Fatalf("registry delete failed: %v", err)
	}
	if _, err := executeWindowsRegistry([]string{"query", testKey, "/v", valueName}); err == nil {
		t.Fatal("deleted registry value still answered query")
	}
}
