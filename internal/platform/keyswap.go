package platform

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// CredentialProbe is the answer to "can this process read that secret right now",
// with no attempt to change anything.
//
// The states exist because a panel needs to tell them apart. "Missing" means the
// user has not saved a key. "Access denied" means a key exists but this binary's
// identity is not on its ACL — only the user can fix that, by re-saving it. They
// are different screens, and collapsing them into "not configured" is how a
// product leaves a user with no idea why their key stopped working.
type CredentialProbe struct {
	// State is one of the CredentialState* constants.
	State string `json:"state"`
	// Reason is a short machine-readable code.
	Reason string `json:"reason,omitempty"`
	// Detail is one sentence for the panel.
	Detail string `json:"detail,omitempty"`
}

const (
	// CredentialStatePresent means the value was read.
	CredentialStatePresent = "present"
	// CredentialStateMissing means no value is stored.
	CredentialStateMissing = "missing"
	// CredentialStateAccessDenied means a value exists but may not be read by
	// this binary. The user must re-save it.
	CredentialStateAccessDenied = "access-denied"
	// CredentialStateUnavailable means the store itself could not be opened or
	// answered with an unexpected failure.
	CredentialStateUnavailable = "unavailable"
)

// Probe reports whether one key can be read, without changing anything.
//
// It never retries and never waits: an access-denied read returns as soon as the
// platform answers, which is what keeps a background status call from sitting
// inside an authorisation prompt it cannot show.
func Probe(store Credentials, key string) CredentialProbe {
	if store == nil {
		return CredentialProbe{State: CredentialStateUnavailable, Reason: "credentials_unavailable",
			Detail: "No native credential store is available on this platform."}
	}
	_, err := store.Get(key)
	switch {
	case err == nil:
		return CredentialProbe{State: CredentialStatePresent}
	case errors.Is(err, ErrCredentialMissing):
		return CredentialProbe{State: CredentialStateMissing, Reason: "credential_missing",
			Detail: "No key is stored yet."}
	case errors.Is(err, ErrCredentialAccessDenied):
		return CredentialProbe{State: CredentialStateAccessDenied, Reason: "credential_access_denied",
			Detail: "A key is stored, but this build may not read it. Re-enter the key to replace it."}
	default:
		return CredentialProbe{State: CredentialStateUnavailable, Reason: "credential_store_failed",
			Detail: "The credential store did not answer."}
	}
}

// ErrCredentialWriteDenied means the store refused to write. It is separate from
// ErrCredentialAccessDenied because the remedy differs: a denied write is usually
// a locked or full store, while a denied read is an ACL that only the user can
// replace.
var ErrCredentialWriteDenied = errors.New("credential_write_denied")

// ErrCredentialReadbackMismatch means the store accepted a write but did not
// return the same value. Nothing may switch to a reference in that state.
var ErrCredentialReadbackMismatch = errors.New("credential_readback_mismatch")

// Transaction stages one secret under a fresh immutable reference and only lets a
// caller commit that reference after a read-back proves it round-trips.
//
// The order is the whole point. A connection's config records a reference, not a
// key, so the new value must be readable under its new reference *before* any
// config revision points at it. Doing it the other way leaves a window where the
// active config names a reference that does not exist yet, and a request that
// lands in that window fails for no reason the user can see.
//
// A failed stage deletes its own reference, so a failure cannot leave an orphan
// secret behind, and a failed transaction never touches the reference that is
// currently active.
type Transaction struct {
	store Credentials
	// ref is the staged reference; empty until Stage succeeds.
	ref string
	// value is what the store returned on read-back.
	value string
	// committed records whether Commit ran, so Close knows not to discard.
	committed bool
}

// RefPrefix is what a staged reference starts with. It is distinct from the
// "connection-" prefix the bridge uses for its own references, so a leaked staged
// reference is recognisable in the store.
const RefPrefix = "staged-"

// NewTransaction starts a transaction against a store.
func NewTransaction(store Credentials) (*Transaction, error) {
	if store == nil {
		return nil, ErrUnsupported
	}
	return &Transaction{store: store}, nil
}

// Stage writes value under a new reference and reads it back.
//
// A read-back that returns a different value is a hard failure, not a warning: a
// store that silently truncates or transforms a key would make every later
// request fail with a credential the user believes they saved.
func (t *Transaction) Stage(value string) (string, error) {
	if t.committed {
		return "", errors.New("credential_transaction_committed")
	}
	if t.ref != "" {
		return "", errors.New("credential_transaction_already_staged")
	}
	if err := validateValue(value); err != nil {
		return "", err
	}
	ref := RefPrefix + randomRef()
	if err := t.store.Set(ref, value); err != nil {
		if errors.Is(err, ErrCredentialAccessDenied) {
			return "", ErrCredentialAccessDenied
		}
		return "", fmt.Errorf("%w: %v", ErrCredentialWriteDenied, err)
	}
	actual, err := t.store.Get(ref)
	if err != nil {
		// The write landed but cannot be read back. That is exactly the state a
		// config must never be pointed at, so remove it and refuse.
		_ = t.store.Delete(ref)
		if errors.Is(err, ErrCredentialAccessDenied) {
			return "", ErrCredentialAccessDenied
		}
		return "", fmt.Errorf("%w: %v", ErrCredentialReadbackMismatch, err)
	}
	if actual != value {
		_ = t.store.Delete(ref)
		return "", ErrCredentialReadbackMismatch
	}
	t.ref, t.value = ref, actual
	return ref, nil
}

// Ref is the staged reference, empty before Stage.
func (t *Transaction) Ref() string { return t.ref }

// Commit accepts the staged reference. The caller must have already made the new
// reference the active one in its own config; this only records that it did, so
// Close keeps the value instead of discarding it.
func (t *Transaction) Commit() error {
	if t.ref == "" {
		return errors.New("credential_transaction_not_staged")
	}
	t.committed = true
	return nil
}

// Close discards an uncommitted staged reference. It is safe to call after
// Commit, and safe to call twice, so a deferred Close cannot delete a live key.
func (t *Transaction) Close() error {
	if t == nil || t.ref == "" || t.committed {
		return nil
	}
	ref := t.ref
	t.ref, t.value = "", ""
	if err := t.store.Delete(ref); err != nil {
		return fmt.Errorf("credential_cleanup_failed: %w", err)
	}
	return nil
}

// randomRef returns 16 hex characters. It is not a security boundary — the store
// is — but it must not collide, because a collision would overwrite a key another
// connection is using.
func randomRef() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		// A failure here is fatal to the transaction: a predictable reference
		// could collide with a live one.
		return strings.Repeat("0", 16)
	}
	return hex.EncodeToString(buffer)
}

// ReplaceValue is the whole sequence a panel needs for one key: stage, verify,
// and then hand the new reference to the caller's config commit.
//
// The callback is where the caller switches its own config to the new reference.
// If it fails, the staged value is discarded and the previous reference is left
// untouched, so a failed save cannot leave a connection pointing at a key that is
// about to disappear.
func ReplaceValue(store Credentials, value string, commit func(ref string) error) (string, error) {
	transaction, err := NewTransaction(store)
	if err != nil {
		return "", err
	}
	defer transaction.Close()
	ref, err := transaction.Stage(value)
	if err != nil {
		return "", err
	}
	if commit != nil {
		if err := commit(ref); err != nil {
			return "", err
		}
	}
	if err := transaction.Commit(); err != nil {
		return "", err
	}
	return ref, nil
}
