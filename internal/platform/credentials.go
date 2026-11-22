package platform

import (
	"errors"
	"runtime"
	"strings"
)

// Credentials stores encrypted secrets: application-managed AES-GCM files on
// macOS and DPAPI-protected files on Windows.
//
// There is no plaintext fallback. A platform that cannot provide a real store
// reports ErrUnsupported instead of silently writing the value as plaintext.
// Values encrypted by the old Electron build with safeStorage remain a separate
// format; migration of those secrets is an explicit, user-authorised operation.
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
		// Preview migration reads must not delete the source before the stable
		// store has committed its copy, so the preview namespace stays read-only.
		if strings.HasPrefix(service, "dev.kimi-work-plus.preview") {
			return newKeychain(service)
		}
		current, err := newLocalSecretStore(service)
		if err != nil {
			return nil, err
		}
		legacy, err := newKeychain(service)
		if err != nil {
			return nil, err
		}
		return &migratingCredentialStore{current: current, legacy: legacy}, nil
	case "windows":
		return newDPAPI(service)
	}
	return nil, ErrUnsupported
}

// validateService refuses a name that could collide or confuse the store. The
// value scopes the encrypted file path, authenticates macOS ciphertext, and is
// also used to find a legacy Keychain item during migration.
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

// maxSecretBytes bounds one value. A model key is far smaller; the limit also
// bounds ciphertext reads and rejects unexpectedly large stored values.
const maxSecretBytes = 64 << 10

func validateValue(value string) error {
	if len(value) > maxSecretBytes {
		return ErrCredentialInvalid
	}
	return nil
}

// Available reports whether this host has an encrypted credential store, so the
// panel can disable the field instead of failing on save.
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

// ErrCredentialAccessDenied means an item exists but cannot be read or
// authenticated: a legacy Keychain ACL may deny it, or local ciphertext may fail
// authentication. Background reads fail rather than request a hidden prompt.
var ErrCredentialAccessDenied = errors.New("credential_access_denied")
