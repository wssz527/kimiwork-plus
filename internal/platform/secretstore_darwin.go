//go:build darwin && cgo

package platform

import (
	"sync"
)

// The macOS store is the Security framework's generic-secret API, reached
// through a thin cgo layer (secretstore_darwin_shim.go) so the SDK headers, not
// hand-written declarations, define the ABI, the CoreFoundation callback
// structures and reference ownership.
//
// SecItemAdd / SecItemUpdate / SecItemCopyMatching / SecItemDelete operate on
// the login keychain of the calling user. An item is addressed by
// (class=generic, service, account).
//
// This store never reads a value the old Electron build encrypted with
// safeStorage: those blobs are bound to that build's code identity and are not a
// format this package decrypts.

// OSStatus values used by this file. They mirror SecBase.h; the dedicated test
// compares them with the constants the SDK exports, so a mismatch between this
// table and the installed headers fails a build-time check instead of becoming a
// wrong diagnosis at runtime.
const (
	errSecSuccess = 0
	// errSecUserCanceled is the Carbon-era -128 the framework still returns when
	// the user dismisses an authorization dialog.
	errSecUserCanceled = -128
	// errSecAuthFailed means the credentials offered to unlock or authorize were
	// rejected.
	errSecAuthFailed = -25293
	// errSecDuplicateItem means the item already exists; the write path retries
	// as an update.
	errSecDuplicateItem = -25299
	// errSecItemNotFound is the only status that means "no such item". It is
	// distinct from every refusal below.
	errSecItemNotFound = -25300
	// errSecInteractionNotAllowed (-25308) means the operation needs user
	// interaction and interaction is currently not allowed. It says nothing by
	// itself about the item's age or its ACL.
	errSecInteractionNotAllowed = -25308
	// errSecInteractionRequired (-25315) means user interaction is required but
	// is currently not allowed. Apple documents the two codes separately; the
	// store still classifies them together because both mean "an interaction was
	// needed and could not happen".
	errSecInteractionRequired = -25315
	// errSecNotAvailable means no keychain is available to this session.
	errSecNotAvailable = -25291
	// errSecNoSuchKeychain means the addressed keychain does not exist.
	errSecNoSuchKeychain = -25294
	// errSecDecode means the stored blob is not the CFData this store wrote.
	errSecDecode = -26275
	// errSecBufferTooSmall means the stored value is larger than the buffer this
	// store reads into; the item exists but is not a value this build wrote.
	errSecBufferTooSmall = -25301
	// errSecMissingEntitlement means the process lacks an entitlement the store
	// requires; a development build can see this for data-protection items.
	errSecMissingEntitlement = -34018
)

// Error codes. Each is a stable machine-readable identifier that also passes the
// panel's error-code pattern, so a caller can map it to a sentence without
// matching on an OSStatus number.
const (
	// CodeCredentialLocked: the login keychain is locked. The remedy is to unlock
	// it, not to re-enter the key.
	CodeCredentialLocked = "credential_keychain_locked"
	// CodeCredentialUnavailable: no keychain is reachable from this session, or
	// an entitlement is missing.
	CodeCredentialUnavailable = "credential_keychain_unavailable"
	// CodeCredentialAuthorizationRequired: an item exists but reading or
	// replacing it needs an authorization this process may not request. A
	// background read must return this instead of showing a hidden prompt.
	CodeCredentialAuthorizationRequired = "credential_authorization_required"
	// CodeCredentialSessionUnavailable: the operation needed an interaction, but
	// this process has no console GUI session in which the system could show it.
	// A launchd job started outside the user's login session sees this. It is a
	// calling-context problem, not an item ACL problem, and the remedy is to run
	// the save from the app window or to start the service in the user's session.
	CodeCredentialSessionUnavailable = "credential_session_unavailable"
	// CodeCredentialAuthorizationCancelled: the user dismissed the dialog.
	CodeCredentialAuthorizationCancelled = "credential_authorization_cancelled"
	// CodeCredentialAuthorizationFailed: the authorization was rejected.
	CodeCredentialAuthorizationFailed = "credential_authorization_failed"
	// CodeCredentialStoreCorrupt: the stored value is not the format written
	// here. It is never reported as a missing credential.
	CodeCredentialStoreCorrupt = "credential_store_corrupt"
	// CodeCredentialStoreFailed: an unexpected Security framework failure.
	CodeCredentialStoreFailed = "credential_store_failed"
	// CodeCredentialInteractionNotRestored: the process-wide interaction flag
	// could not be put back after an operation. The store refuses to pretend the
	// process state is unchanged.
	CodeCredentialInteractionNotRestored = "credential_interaction_not_restored"
)

// KeychainError describes a failed store operation with the state the panel
// needs: what was attempted, whether the item already existed, and whether the
// caller is in a session that could have shown a dialog.
//
// It never carries the secret. Errors.Is works against ErrCredentialAccessDenied
// for the states a user resolves by re-entering the key, and against
// ErrCredentialMissing for an absent item.
type KeychainError struct {
	// Code is the machine-readable reason and also the Error() text.
	Code string
	// Op is "read", "write" or "delete".
	Op string
	// Status is the OSStatus the framework returned, or 0 when the failure came
	// from this layer.
	Status int32
	// Existed is 1 when the item was already present, 0 when the write was
	// creating it, and -1 when that could not be determined.
	Existed int
	// Stage is "update" or "add" for a write, empty otherwise.
	Stage string
	// Interactive records whether the system dialog was permitted for this call.
	Interactive bool
	// Session is "console", "background" or "unknown".
	Session string
	// Detail is one sentence for diagnostics; it never contains a secret.
	Detail string

	cause error
}

func (e *KeychainError) Error() string { return e.Code }

// Unwrap exposes the sentinel callers already match on.
func (e *KeychainError) Unwrap() error { return e.cause }

// StoreState is one observation of the system context around the keychain.
type StoreState struct {
	// Available reports whether the default keychain could be read at all.
	Available bool
	// Locked reports that the default keychain exists but is locked.
	Locked bool
	// InteractionAllowed is the current process-wide switch.
	InteractionAllowed bool
	// VisibleSession reports that this process belongs to a console GUI session
	// that could display a SecurityAgent prompt.
	VisibleSession bool
	// LoginComplete reports that the console session finished logging in.
	LoginComplete bool
}

// ScreenState renders the observation for a diagnostics panel.
func (s StoreState) ScreenState() string {
	switch {
	case !s.Available:
		return CodeCredentialUnavailable
	case s.Locked:
		return CodeCredentialLocked
	case !s.VisibleSession:
		return CodeCredentialSessionUnavailable
	default:
		return "credential_session_console"
	}
}

// keychainSDK is the platform surface. It exists so the decision logic below is
// testable without touching the user's keychain.
type keychainSDK interface {
	state() sdkState
	read(service, account string, interactive bool) keychainRead
	write(service, account, secret string, interactive bool) keychainWrite
	remove(service, account string, interactive bool) (int32, bool)
}

type keychainRead struct {
	status   int32
	value    string
	restored bool
}

// Write stages. A create can be refused because the keychain is locked, while an
// update can be refused because the existing item's ACL does not admit this
// binary. Keeping the stage lets the error say which happened.
const (
	stageUpdate = 0
	stageAdd    = 1
)

type keychainWrite struct {
	status   int32
	existed  int
	stage    int
	restored bool
}

type keychain struct {
	service string
	sdk     keychainSDK
}

// interactionMu serialises every call that can change the process-wide
// interaction flag. The flag belongs to the process, so two concurrent calls
// must not interleave "forbid / allow / restore"; the shim reads and restores the
// exact previous value inside one locked call.
var interactionMu sync.Mutex

var (
	_ Credentials      = (*keychain)(nil)
	_ VisibleSaveStore = (*keychain)(nil)
)

func newKeychain(service string) (Credentials, error) {
	return &keychain{service: service, sdk: sdkKeychain{}}, nil
}

// newDPAPI is the Windows store; on macOS the Keychain is used instead.
func newDPAPI(string) (Credentials, error) { return nil, ErrUnsupported }

// VisibleSaveStore is implemented by a store that can complete one save from a
// user-visible action, permitting the system authorization dialog that a
// background read must never request.
//
// Callers must only reach this from a GUI gesture, and only after
// SessionVisible reports true; the store checks the session again itself.
type VisibleSaveStore interface {
	// SetVisible stores the value, permitting at most the interaction the
	// platform itself initiates for this one call.
	SetVisible(key, value string) error
	// SessionVisible reports whether a dialog started now could be shown.
	SessionVisible() bool
}

// SessionVisible reports whether a prompt started now could reach the user.
func (k *keychain) SessionVisible() bool {
	interactionMu.Lock()
	defer interactionMu.Unlock()
	return k.sdk.state().hasVisibleSession()
}

// State reports the keychain and session context without changing anything.
func (k *keychain) State() StoreState {
	interactionMu.Lock()
	defer interactionMu.Unlock()
	state := k.sdk.state()
	return StoreState{
		Available:          state.available == 1,
		Locked:             state.locked == 1,
		InteractionAllowed: state.interactionAllowed != 0,
		VisibleSession:     state.hasVisibleSession(),
		LoginComplete:      state.loginComplete == 1,
	}
}

func (k *keychain) Get(key string) (string, error) {
	return k.get(key, false)
}

func (k *keychain) GetVisible(key string) (string, error) {
	return k.get(key, true)
}

func (k *keychain) get(key string, interactive bool) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	interactionMu.Lock()
	defer interactionMu.Unlock()
	if interactive && !k.sdk.state().hasVisibleSession() {
		return "", &KeychainError{Code: CodeCredentialSessionUnavailable, Op: "read", Existed: -1}
	}
	result := k.sdk.read(k.service, key, interactive)
	if !result.restored {
		return "", k.notRestored("read", result.status, -1, "", false)
	}
	if result.status == errSecSuccess {
		if len(result.value) > maxSecretBytes {
			return "", &KeychainError{Code: CodeCredentialStoreCorrupt, Op: "read",
				Detail: "The stored value is larger than this build writes."}
		}
		return result.value, nil
	}
	return "", k.readError(result.status, interactive)
}

func (k *keychain) Set(key, value string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateValue(value); err != nil {
		return err
	}
	// The default write is background-safe: no dialog, no waiting. An item that
	// needs authorization is reported, and SetVisible is the user-driven path
	// that can complete it.
	interactionMu.Lock()
	defer interactionMu.Unlock()
	return k.writeResult(k.sdk.write(k.service, key, value, false), false)
}

func (k *keychain) SetVisible(key, value string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateValue(value); err != nil {
		return err
	}
	interactionMu.Lock()
	defer interactionMu.Unlock()
	state := k.sdk.state()
	if !state.hasVisibleSession() {
		// Starting the dialog here would leave the caller waiting for a prompt
		// nobody can see. Refuse immediately and let the panel explain why.
		return &KeychainError{Code: CodeCredentialSessionUnavailable, Op: "write",
			Existed: -1, Interactive: false, Session: sessionName(state),
			Detail: "This process is not in a console session that could show the authorization dialog."}
	}
	return k.writeResult(k.sdk.write(k.service, key, value, true), true)
}

func (k *keychain) Delete(key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	interactionMu.Lock()
	defer interactionMu.Unlock()
	status, restored := k.sdk.remove(k.service, key, false)
	if !restored {
		return k.notRestored("delete", status, -1, "", false)
	}
	switch status {
	case errSecSuccess, errSecItemNotFound:
		// Deleting an absent key succeeds: the caller asked for it to be gone.
		return nil
	}
	return k.readError(status, false)
}

func (k *keychain) writeResult(result keychainWrite, interactive bool) error {
	if !result.restored {
		return k.notRestored("write", result.status, result.existed, stageName(result.stage), interactive)
	}
	if result.status == errSecSuccess {
		return nil
	}
	return k.classify(result.status, "write", result.existed, stageName(result.stage), interactive)
}

func (k *keychain) readError(status int32, interactive bool) error {
	if status == errSecItemNotFound {
		return ErrCredentialMissing
	}
	return k.classify(status, "read", -1, "", interactive)
}

func (k *keychain) notRestored(op string, status int32, existed int, stage string, interactive bool) error {
	return &KeychainError{
		Code:        CodeCredentialInteractionNotRestored,
		Op:          op,
		Status:      status,
		Existed:     existed,
		Stage:       stage,
		Interactive: interactive,
		Session:     sessionName(k.sdk.state()),
		Detail:      "The process-wide interaction setting could not be restored; restart the service before using the credential store again.",
	}
}

// classify turns an OSStatus into a state. The same number means different
// things depending on what was attempted: -25308 on an update can be an existing
// item that needs authorization, while the same number on a create usually means
// the keychain is locked, because a brand-new item's ACL names the caller.
func (k *keychain) classify(status int32, op string, existed int, stage string, interactive bool) error {
	state := k.sdk.state()
	base := KeychainError{
		Op:          op,
		Status:      status,
		Existed:     existed,
		Stage:       stage,
		Interactive: interactive,
		Session:     sessionName(state),
	}
	switch status {
	case errSecInteractionNotAllowed, errSecInteractionRequired:
		if state.locked == 1 {
			base.Code = CodeCredentialLocked
			base.Detail = "The login keychain is locked; unlock it and retry."
			return &base
		}
		if op == "write" && existed == 0 {
			// The interaction is needed to show a dialog, so the process's
			// session is the deciding factor: a create from a launchd job without
			// a console session cannot show one, and the remedy is the app window.
			if state.hasVisibleSession() {
				base.Code = CodeCredentialAuthorizationRequired
				base.Detail = "Creating the item needs an authorization the system did not allow; save it again from the app window."
			} else {
				base.Code = CodeCredentialSessionUnavailable
				base.Detail = "This process has no console session that could show the authorization dialog; save from the app window."
			}
			return &base
		}
		base.Code = CodeCredentialAuthorizationRequired
		if interactive {
			base.Detail = "The system did not allow the authorization this save required."
			if !state.hasVisibleSession() {
				base.Code = CodeCredentialSessionUnavailable
				base.Detail = "This process has no console session that could show the authorization dialog."
			}
		} else {
			base.Detail = "The item exists but needs an authorization a background operation must not request. Save it again from the app window."
		}
		if op == "read" || (op == "write" && existed == 1) {
			base.cause = ErrCredentialAccessDenied
		}
		return &base
	case errSecUserCanceled:
		base.Code = CodeCredentialAuthorizationCancelled
		base.Detail = "The authorization was dismissed."
		base.cause = ErrCredentialAccessDenied
		return &base
	case errSecAuthFailed:
		base.Code = CodeCredentialAuthorizationFailed
		base.Detail = "The keychain rejected the authorization."
		base.cause = ErrCredentialAccessDenied
		return &base
	case errSecNotAvailable, errSecNoSuchKeychain, errSecMissingEntitlement:
		base.Code = CodeCredentialUnavailable
		base.Detail = "No usable keychain is reachable from this session."
		return &base
	case errSecDecode, errSecBufferTooSmall:
		base.Code = CodeCredentialStoreCorrupt
		base.Detail = "The stored item is not a value this build wrote."
		return &base
	default:
		base.Code = CodeCredentialStoreFailed
		base.Detail = "The Security framework did not complete the operation."
		return &base
	}
}

func sessionName(state sdkState) string {
	switch state.onConsole {
	case 1:
		return "console"
	case 0:
		return "background"
	default:
		return "unknown"
	}
}

func stageName(stage int) string {
	if stage == stageAdd {
		return "add"
	}
	return "update"
}
