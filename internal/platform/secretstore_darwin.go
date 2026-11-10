//go:build darwin

package platform

import (
	"errors"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The macOS store is the Security framework's generic-secret API, called through
// purego so this package needs no cgo and stays cross-compilable.
//
// SecItemAdd / SecItemUpdate / SecItemCopyMatching / SecItemDelete operate on the
// login keychain of the calling user. An item is addressed by
// (class=generic, service, account).
//
// This store never reads a value the old Electron build encrypted with
// safeStorage: those blobs are bound to that build's code identity and are not a
// format this package decrypts.

const (
	securityFramework = "/System/Library/Frameworks/Security.framework/Security"
	coreFoundation    = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"

	errSecSuccess       = 0
	errSecItemNotFound  = -25300
	errSecDuplicateItem = -25299
	errSecAuthFailed    = -25293
	errSecUserCanceled  = -128
	// A keychain item whose ACL does not include the current binary answers with
	// one of these instead of prompting, once interaction is forbidden.
	errSecInteractionRequired   = -25308
	errSecInteractionNotAllowed = -25315

	cfStringEncodingUTF8 = 0x08000100
)

type cfRef = uintptr

// unsafeLoadUintptr reads one word from a linker-provided address.
//
// The address is data owned by CoreFoundation, not by Go, and it stays mapped
// for the life of the process, so no Go pointer is being laundered here. The
// value is read as bytes and assembled by hand, which is what vet accepts for a
// foreign address.
func unsafeLoadUintptr(address uintptr) uintptr {
	var value uintptr
	target := unsafe.Slice((*byte)(unsafe.Add(nil, address)), unsafe.Sizeof(value))
	for i := uint(0); i < uint(unsafe.Sizeof(value)); i++ {
		value |= uintptr(target[i]) << (8 * i)
	}
	return value
}

type keychain struct {
	service string

	secItemAdd          func(cfRef, *cfRef) int32
	secItemUpdate       func(cfRef, cfRef) int32
	secItemCopyMatching func(cfRef, *cfRef) int32
	secItemDelete       func(cfRef) int32

	cfStringCreateWithBytes func(allocator cfRef, bytes *byte, length int, encoding uint32, external bool) cfRef
	cfDataCreate            func(allocator cfRef, bytes *byte, length int) cfRef
	cfDataGetBytePtr        func(data cfRef) *byte
	cfDataGetLength         func(data cfRef) int
	cfDictionaryCreate      func(allocator cfRef, keys *cfRef, values *cfRef, count int,
		keyCallbacks cfRef, valueCallbacks cfRef) cfRef
	cfRelease func(value cfRef)

	kSecClass, kSecAttrService, kSecAttrAccount                    cfRef
	kSecValueData, kSecReturnData, kSecMatchLimit                  cfRef
	kSecClassGenericPassword, kSecMatchLimitOne                    cfRef
	kSecUseAuthenticationUI, kSecUseAuthenticationUIFail           cfRef
	kCFBooleanTrue                                                 cfRef
	kCFTypeDictionaryKeyCallBacks, kCFTypeDictionaryValueCallBacks cfRef

	// SecKeychainSetUserInteractionAllowed / ...Get... are the legacy
	// file-keychain switches. They are process-scoped: they change what this
	// process may prompt for, not any system setting, item ACL or other
	// application.
	secKeychainSetUserInteractionAllowed func(bool) int32
	secKeychainGetUserInteractionAllowed func() bool
}

// interactionMu serialises the process-wide interaction flag. The switch is a
// property of the process, not of a query, so two concurrent reads must not
// interleave "forbid / restore" around each other's SecItemCopyMatching.
var interactionMu sync.Mutex

// withoutInteraction runs one Keychain operation with prompting forbidden and
// restores the previous process setting afterwards. Both reads and writes use it:
// a daemon cannot display an ACL or unlock dialog.
func (k *keychain) withoutInteraction(operation func() (string, error)) (string, error) {
	if k.secKeychainSetUserInteractionAllowed == nil {
		return operation()
	}
	interactionMu.Lock()
	defer interactionMu.Unlock()
	previous := true
	if k.secKeychainGetUserInteractionAllowed != nil {
		previous = k.secKeychainGetUserInteractionAllowed()
	}
	k.secKeychainSetUserInteractionAllowed(false)
	defer k.secKeychainSetUserInteractionAllowed(previous)
	return operation()
}

func newKeychain(service string) (Credentials, error) {
	store := &keychain{service: service}

	security, err := purego.Dlopen(securityFramework, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, ErrUnsupported
	}
	cf, err := purego.Dlopen(coreFoundation, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, ErrUnsupported
	}

	purego.RegisterLibFunc(&store.secItemAdd, security, "SecItemAdd")
	purego.RegisterLibFunc(&store.secItemUpdate, security, "SecItemUpdate")
	purego.RegisterLibFunc(&store.secItemCopyMatching, security, "SecItemCopyMatching")
	purego.RegisterLibFunc(&store.secItemDelete, security, "SecItemDelete")
	purego.RegisterLibFunc(&store.cfStringCreateWithBytes, cf, "CFStringCreateWithBytes")
	purego.RegisterLibFunc(&store.cfDataCreate, cf, "CFDataCreate")
	purego.RegisterLibFunc(&store.cfDataGetBytePtr, cf, "CFDataGetBytePtr")
	purego.RegisterLibFunc(&store.cfDataGetLength, cf, "CFDataGetLength")
	purego.RegisterLibFunc(&store.cfDictionaryCreate, cf, "CFDictionaryCreate")
	purego.RegisterLibFunc(&store.cfRelease, cf, "CFRelease")
	// A missing symbol leaves the field nil; the read path then reports the
	// interaction denial it gets rather than panicking.
	if address, err := purego.Dlsym(security, "SecKeychainSetUserInteractionAllowed"); err == nil && address != 0 {
		purego.RegisterFunc(&store.secKeychainSetUserInteractionAllowed, address)
	}
	if address, err := purego.Dlsym(security, "SecKeychainGetUserInteractionAllowed"); err == nil && address != 0 {
		purego.RegisterFunc(&store.secKeychainGetUserInteractionAllowed, address)
	}

	store.kSecClass = cfVariable(security, "kSecClass")
	store.kSecAttrService = cfVariable(security, "kSecAttrService")
	store.kSecAttrAccount = cfVariable(security, "kSecAttrAccount")
	store.kSecValueData = cfVariable(security, "kSecValueData")
	store.kSecReturnData = cfVariable(security, "kSecReturnData")
	store.kSecMatchLimit = cfVariable(security, "kSecMatchLimit")
	store.kSecClassGenericPassword = cfVariable(security, "kSecClassGenericPassword")
	store.kSecMatchLimitOne = cfVariable(security, "kSecMatchLimitOne")
	store.kSecUseAuthenticationUI = cfVariable(security, "kSecUseAuthenticationUI")
	store.kSecUseAuthenticationUIFail = cfVariable(security, "kSecUseAuthenticationUIFail")
	store.kCFBooleanTrue = cfVariable(cf, "kCFBooleanTrue")
	store.kCFTypeDictionaryKeyCallBacks = cfVariable(cf, "kCFTypeDictionaryKeyCallBacks")
	store.kCFTypeDictionaryValueCallBacks = cfVariable(cf, "kCFTypeDictionaryValueCallBacks")
	if store.kSecClass == 0 || store.kSecClassGenericPassword == 0 || store.kCFBooleanTrue == 0 {
		return nil, ErrUnsupported
	}
	return store, nil
}

// cfVariable resolves an exported CoreFoundation data symbol. The symbol address
// holds the reference, so it is dereferenced once.
func cfVariable(library uintptr, name string) cfRef {
	address, err := purego.Dlsym(library, name)
	if err != nil || address == 0 {
		return 0
	}
	// The exported symbol is a variable holding the reference. It is read with
	// an explicit unsafe load, and the address comes from the dynamic linker
	// rather than from Go memory, so the pointer is valid for the life of the
	// process.
	return cfRef(unsafeLoadUintptr(address))
}

// pair is one dictionary member. Ownership is tracked so every created
// reference is released exactly once.
type pair struct {
	key, value cfRef
	owned      bool
}

func (k *keychain) dictionary(pairs []pair) (cfRef, []cfRef) {
	keys := make([]cfRef, len(pairs))
	values := make([]cfRef, len(pairs))
	var owned []cfRef
	for i, item := range pairs {
		keys[i] = item.key
		values[i] = item.value
		if item.owned {
			owned = append(owned, item.key, item.value)
		}
	}
	dictionary := k.cfDictionaryCreate(0, &keys[0], &values[0], len(pairs),
		k.kCFTypeDictionaryKeyCallBacks, k.kCFTypeDictionaryValueCallBacks)
	if dictionary != 0 {
		owned = append(owned, dictionary)
	}
	return dictionary, owned
}

func (k *keychain) cfString(value string) (cfRef, bool) {
	bytes := []byte(value)
	var pointer *byte
	if len(bytes) > 0 {
		pointer = &bytes[0]
	}
	ref := k.cfStringCreateWithBytes(0, pointer, len(bytes), cfStringEncodingUTF8, false)
	return ref, true
}

func (k *keychain) cfData(value string) (cfRef, bool) {
	bytes := []byte(value)
	var pointer *byte
	if len(bytes) > 0 {
		pointer = &bytes[0]
	}
	return k.cfDataCreate(0, pointer, len(bytes)), true
}

// stringMember and dataMember build the owned pair for a created reference.
func (k *keychain) stringMember(value string) pair {
	ref, _ := k.cfString(value)
	return pair{value: ref, owned: true}
}

func (k *keychain) dataMember(value string) pair {
	ref, _ := k.cfData(value)
	return pair{value: ref, owned: true}
}

// identity is the (class, service, account) triple every operation starts from.
func (k *keychain) identity(key string) []pair {
	service := k.stringMember(k.service)
	account := k.stringMember(key)
	return []pair{
		{key: k.kSecClass, value: k.kSecClassGenericPassword},
		{key: k.kSecAttrService, value: service.value, owned: true},
		{key: k.kSecAttrAccount, value: account.value, owned: true},
	}
}

func (k *keychain) release(refs []cfRef) {
	for _, ref := range refs {
		if ref != 0 {
			k.cfRelease(ref)
		}
	}
}

// readPairs is the query for one item. It carries the documented query-level
// switch `kSecUseAuthenticationUI = kSecUseAuthenticationUIFail`, which keeps a
// data-protection-keychain read from asking for authentication. It is not
// sufficient on its own: the legacy file keychain ignores this key, which is why
// Get also sets the process-scoped switch below.
func (k *keychain) readPairs(key string) []pair {
	pairs := append(k.identity(key),
		pair{key: k.kSecReturnData, value: k.kCFBooleanTrue},
		pair{key: k.kSecMatchLimit, value: k.kSecMatchLimitOne})
	if k.kSecUseAuthenticationUI != 0 && k.kSecUseAuthenticationUIFail != 0 {
		pairs = append(pairs, pair{key: k.kSecUseAuthenticationUI, value: k.kSecUseAuthenticationUIFail})
	}
	return pairs
}

func (k *keychain) Get(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	// The query-level switch only covers the data-protection keychain. An item
	// written by an earlier, differently signed build of this same product lives
	// in the legacy file keychain, where `kSecUseAuthenticationUI` is ignored and
	// SecItemCopyMatching instead waits for an unlock dialog a background daemon
	// can never show. `SecKeychainSetUserInteractionAllowed` is the documented
	// switch for that path; it is process-scoped and touches no system setting,
	// item ACL or other application.
	return k.withoutInteraction(func() (string, error) { return k.read(key) })
}

// read performs the query itself.
func (k *keychain) read(key string) (string, error) {
	query, owned := k.dictionary(k.readPairs(key))
	if query == 0 {
		k.release(owned)
		return "", ErrUnsupported
	}
	defer k.release(owned)

	var result cfRef
	switch k.secItemCopyMatching(query, &result) {
	case errSecSuccess:
	case errSecItemNotFound:
		return "", ErrCredentialMissing
	case errSecAuthFailed, errSecUserCanceled, errSecInteractionRequired, errSecInteractionNotAllowed:
		// The item exists but this binary's code identity is not on its ACL. The
		// user can only resolve that in Keychain Access, so the panel gets a
		// reason it can act on instead of a blocked call.
		return "", ErrCredentialAccessDenied
	default:
		return "", errors.New("credential_store_failed")
	}
	if result == 0 {
		return "", ErrCredentialMissing
	}
	defer k.cfRelease(result)
	length := k.cfDataGetLength(result)
	if length <= 0 || length > maxSecretBytes {
		return "", errors.New("credential_store_failed")
	}
	pointer := k.cfDataGetBytePtr(result)
	if pointer == nil {
		return "", errors.New("credential_store_failed")
	}
	return string(unsafe.Slice(pointer, length)), nil
}

func (k *keychain) Set(key, value string) error {
	_, err := k.withoutInteraction(func() (string, error) { return "", k.set(key, value) })
	return err
}

func (k *keychain) set(key, value string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateValue(value); err != nil {
		return err
	}
	identity, identityOwned := k.dictionary(k.identity(key))
	if identity == 0 {
		k.release(identityOwned)
		return ErrUnsupported
	}
	defer k.release(identityOwned)

	payload := k.dataMember(value)
	attributes, attributesOwned := k.dictionary([]pair{{key: k.kSecValueData, value: payload.value, owned: true}})
	if attributes == 0 {
		k.release(attributesOwned)
		return ErrUnsupported
	}
	defer k.release(attributesOwned)

	status := k.secItemUpdate(identity, attributes)
	if status == errSecInteractionRequired || status == errSecInteractionNotAllowed {
		if _, err := k.read(key); !errors.Is(err, ErrCredentialMissing) {
			return credentialWriteError(status)
		}
		status = errSecItemNotFound
	}
	if status == errSecSuccess {
		return nil
	}
	if status != errSecItemNotFound {
		return credentialWriteError(status)
	}
	// No existing item: add one dictionary carrying both the identity and the
	// value, which is what SecItemAdd expects.
	added := k.dataMember(value)
	add, addOwned := k.dictionary(append(k.identity(key), pair{key: k.kSecValueData, value: added.value, owned: true}))
	if add == 0 {
		k.release(addOwned)
		return ErrUnsupported
	}
	defer k.release(addOwned)
	status = k.secItemAdd(add, nil)
	if status == errSecSuccess {
		return nil
	}
	if status == errSecDuplicateItem {
		// Another writer won the race; updating is the correct retry.
		status = k.secItemUpdate(identity, attributes)
		if status == errSecSuccess {
			return nil
		}
	}
	return credentialWriteError(status)
}

func credentialWriteError(status int32) error {
	switch status {
	case errSecAuthFailed, errSecUserCanceled, errSecInteractionRequired, errSecInteractionNotAllowed:
		return ErrCredentialAccessDenied
	default:
		return errors.New("credential_store_failed")
	}
}

func (k *keychain) Delete(key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	query, owned := k.dictionary(k.identity(key))
	if query == 0 {
		k.release(owned)
		return ErrUnsupported
	}
	defer k.release(owned)
	switch k.secItemDelete(query) {
	case errSecSuccess, errSecItemNotFound:
		return nil
	}
	return errors.New("credential_store_failed")
}
