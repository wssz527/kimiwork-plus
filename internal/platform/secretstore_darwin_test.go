//go:build darwin

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A read must never block on a modal unlock prompt. Two switches are needed
// because the installed keychains differ:
//
//   - `kSecUseAuthenticationUI = kSecUseAuthenticationUIFail` covers the
//     data-protection keychain;
//   - `SecKeychainSetUserInteractionAllowed(false)` covers the legacy file
//     keychain, where the query key is ignored.
//
// The second is the one that matters for an item written by an earlier, ad-hoc
// signed build of this same product: its ACL does not name the current binary,
// and without the process-scoped switch SecItemCopyMatching waits inside
// SecurityServer's decrypt for a prompt a background daemon cannot show.
func TestKeychainReadDoesNotBlockOnAnUnreadableItem(t *testing.T) {
	if testing.Short() {
		t.Skip("touches the OS credential store")
	}
	store, err := OpenCredentials("dev.kimi-work-plus.preview.latency.test")
	if err != nil {
		t.Skipf("no keychain here: %v", err)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := store.Get("absent-key")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCredentialMissing) {
			t.Fatalf("absent key: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("a read took %v; it is waiting on interaction", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read never returned: it is blocked inside the Security framework")
	}
}

// The service name the daemon derives for a state directory, reproduced here so
// a fixture item can be addressed without hard-coding a digest.
func credentialServiceFor(stateDir string) string {
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		absolute = stateDir
	}
	identity := sha256.Sum256([]byte(absolute))
	return "dev.kimi-work-plus.preview." + hex.EncodeToString(identity[:8])
}

// An item this process may not read must come back as an access denial within a
// bounded time — not as a missing credential, and not as a hang.
//
// The fixture is created by this test through the same store, so nothing outside
// this test's own state is touched. The value is synthetic and is never printed.
// A keychain whose ACL already admits this binary returns the value instead,
// which is also a correct outcome: the assertion is that the call returns.
func TestKeychainReadOfAnACLDeniedItemReturns(t *testing.T) {
	if testing.Short() {
		t.Skip("touches the OS credential store")
	}
	stateDir := filepath.Join(t.TempDir(), "gui-state-current")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenCredentials(credentialServiceFor(stateDir))
	if err != nil {
		t.Skipf("no keychain here: %v", err)
	}
	const key = "acl-denied-fixture"
	const secret = "fixture-value-not-a-credential"
	if err := store.Set(key, secret); err != nil {
		t.Skipf("cannot create the fixture item here: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(key) })

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := store.Get(key)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ErrCredentialAccessDenied) {
			t.Fatalf("unexpected failure: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("the read took %v; it is waiting on interaction", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read never returned: it is blocked inside the Security framework")
	}
}

// The query the store builds must carry the non-interactive switch, otherwise
// the protection above is only incidental.
func TestKeychainQueryForbidsInteraction(t *testing.T) {
	store, err := OpenCredentials("dev.kimi-work-plus.preview.query.test")
	if err != nil {
		t.Skipf("no keychain here: %v", err)
	}
	keychain, ok := store.(*keychain)
	if !ok {
		t.Fatalf("store type: %T", store)
	}
	if keychain.kSecUseAuthenticationUI == 0 || keychain.kSecUseAuthenticationUIFail == 0 {
		t.Fatal("the interaction-forbidding constants did not resolve")
	}
	var found bool
	for _, item := range keychain.readPairs("probe") {
		if item.key == keychain.kSecUseAuthenticationUI && item.value == keychain.kSecUseAuthenticationUIFail {
			found = true
		}
	}
	if !found {
		t.Fatal("the read query does not forbid interaction")
	}
	if keychain.secKeychainSetUserInteractionAllowed == nil {
		t.Fatal("the legacy-keychain interaction switch did not resolve")
	}
}

// An item this binary may not read is reported as an access denial, which is a
// state the panel can act on, rather than as a missing credential or a hang.
func TestCredentialAccessDeniedIsDistinguishable(t *testing.T) {
	if errors.Is(ErrCredentialAccessDenied, ErrCredentialMissing) {
		t.Fatal("an access denial must not read as a missing credential")
	}
	if ErrCredentialAccessDenied.Error() != "credential_access_denied" {
		t.Fatalf("reason code: %q", ErrCredentialAccessDenied.Error())
	}
}

func TestNoninteractiveWriteDistinguishesMissingFromDenied(t *testing.T) {
	store, err := OpenCredentials("dev.kimi-work-plus.preview.write.test")
	if err != nil {
		t.Skipf("no keychain here: %v", err)
	}
	keychain := store.(*keychain)
	for _, denied := range []bool{false, true} {
		added := 0
		keychain.secItemUpdate = func(cfRef, cfRef) int32 { return errSecInteractionRequired }
		keychain.secItemCopyMatching = func(_ cfRef, _ *cfRef) int32 {
			if denied {
				return errSecInteractionRequired
			}
			return errSecItemNotFound
		}
		keychain.secItemAdd = func(cfRef, *cfRef) int32 { added++; return errSecSuccess }
		err := keychain.Set("new-reference", "fixture")
		if denied {
			if !errors.Is(err, ErrCredentialAccessDenied) || added != 0 {
				t.Fatalf("a denied item was treated as missing: %v, adds=%d", err, added)
			}
		} else if err != nil || added != 1 {
			t.Fatalf("a missing item was not created: %v, adds=%d", err, added)
		}
	}
}
