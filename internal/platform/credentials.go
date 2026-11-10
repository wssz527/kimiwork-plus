package platform

import (
	"errors"
	"runtime"
	"strings"
)

// Credentials is the native secret store: Keychain on macOS, DPAPI on Windows.
//
// Two rules are not negotiable here:
//
//  1. There is no plaintext fallback. A platform that cannot provide a real
//     store reports ErrUnsupported; writing the value to a file instead would
//     turn a missing capability into a silent downgrade.
//  2. This store never reads a value the old Electron build encrypted with
//     safeStorage. Those blobs are bound to that build's code identity and are
//     not a format this package decrypts. Migration of existing secrets is a
//     separate, explicit, user-authorised operation.
type Credentials interface {
	// Get returns the stored value, or ErrCredentialMissing.
	Get(key string) (string, error)
	// Set stores or replaces the value.
	Set(key, value string) error
	// Delete removes the value. Deleting a missing key succeeds: the caller
	// asked for the key to be gone, and it is.
	Delete(key string) error
}

// OpenCredentials returns the platform store for the given service name.
//
// The service name is part of the store identity: a distinct service keeps this
// preview's secrets separate from any other build, so the two can coexist.
func OpenCredentials(service string) (Credentials, error) {
	if err := validateService(service); err != nil {
		return nil, err
	}
	switch runtime.GOOS {
	case "darwin":
		return newKeychain(service)
	case "windows":
		return newDPAPI(service)
	}
	return nil, ErrUnsupported
}

// validateService refuses a name that could collide or confuse the store. The
// value ends up as a Keychain service attribute or a DPAPI file name.
func validateService(service string) error {
	if service == "" || len(service) > 128 {
		return ErrCredentialInvalid
	}
	for _, character := range service {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '.', character == '-', character == '_':
		default:
			return ErrCredentialInvalid
		}
	}
	return nil
}

// validateKey refuses an empty or oversized key and anything with control
// characters. Keys are ours, not user input, so this is a guard against a
// programming mistake rather than against an attacker.
func validateKey(key string) error {
	if key == "" || len(key) > 256 {
		return ErrCredentialInvalid
	}
	if strings.ContainsFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ErrCredentialInvalid
	}
	return nil
}

// maxSecretBytes bounds one value. A model key is far smaller; the limit exists
// so a mistake cannot write a megabyte into the Keychain.
const maxSecretBytes = 64 << 10

func validateValue(value string) error {
	if len(value) > maxSecretBytes {
		return ErrCredentialInvalid
	}
	return nil
}

// Available reports whether this host has a usable native store, so the panel
// can disable the field instead of failing on save.
func Available() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	}
	return false
}

// ErrPlaintextUnsupported is returned by callers that ask for an insecure
// fallback. It exists so the refusal is explicit in code.
var ErrPlaintextUnsupported = errors.New("plaintext_credential_storage_refused")

// ErrCredentialAccessDenied means the item exists but this binary may not read
// it: its code identity is not on the item's ACL. Reading must fail rather than
// block, because a background service cannot show the unlock prompt, so the
// panel turns this into "re-enter the key" instead of waiting forever.
var ErrCredentialAccessDenied = errors.New("credential_access_denied")
