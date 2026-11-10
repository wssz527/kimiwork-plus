//go:build darwin && cgo

package platform

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation -framework CoreGraphics

#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CGSession.h>
#include <Security/Security.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

// This shim is the whole platform surface the macOS store uses. It exists so
// the SDK headers decide the calling convention, the CoreFoundation callback
// structures and the ownership rules, instead of hand-written declarations that
// can silently disagree with the framework:
//
//   - SecKeychainGetUserInteractionAllowed(Boolean *state) returns an OSStatus
//     and writes the flag through the pointer (SecKeychain.h:649), so the
//     previous state is read before it is changed and restored to exactly that
//     value.
//   - kCFTypeDictionaryKeyCallBacks and kCFTypeDictionaryValueCallBacks are
//     exported structure bodies, so their addresses are passed to
//     CFDictionaryCreate, never a dereferenced word.
//   - Every reference created here (service, account, data, dictionaries, the
//     matched item, the session dictionary) is released exactly once. Borrowed
//     kSec and kCF constants are never released.
//
// Reads are always non-interactive. Writes are non-interactive unless the caller
// selected the visible path, and even then the process-wide switch is restored
// to the value observed before the call.

typedef OSStatus (*kimi_body)(void *context);

// kimi_run is the only place that touches the process-wide interaction flag. It
// refuses to change a state it could not read, and it restores the exact
// previous value instead of assuming the documented default.
static OSStatus kimi_run(int32_t interactive, kimi_body body, void *context, int32_t *out_restored) {
	Boolean previous = true;
	Boolean wanted = interactive ? true : false;
	OSStatus status;

	*out_restored = 1;
	status = SecKeychainGetUserInteractionAllowed(&previous);
	if (status != errSecSuccess) {
		return status;
	}
	if (previous == wanted) {
		return body(context);
	}
	status = SecKeychainSetUserInteractionAllowed(wanted);
	if (status != errSecSuccess) {
		return status;
	}
	status = body(context);
	if (SecKeychainSetUserInteractionAllowed(previous) != errSecSuccess) {
		*out_restored = 0;
	}
	return status;
}

static CFStringRef kimi_string(const uint8_t *bytes, size_t length) {
	return CFStringCreateWithBytes(kCFAllocatorDefault, (const UInt8 *)bytes,
	                               (CFIndex)length, kCFStringEncodingUTF8, false);
}

static CFDictionaryRef kimi_dictionary(const void **keys, const void **values, CFIndex count) {
	return CFDictionaryCreate(kCFAllocatorDefault, keys, values, count,
	                          &kCFTypeDictionaryKeyCallBacks,
	                          &kCFTypeDictionaryValueCallBacks);
}

static CFDictionaryRef kimi_identity(CFStringRef service, CFStringRef account) {
	const void *keys[3];
	const void *values[3];

	keys[0] = kSecClass;             values[0] = kSecClassGenericPassword;
	keys[1] = kSecAttrService;       values[1] = service;
	keys[2] = kSecAttrAccount;       values[2] = account;
	return kimi_dictionary(keys, values, 3);
}

// kimi_existing asks only for attributes: no value is decrypted, so this never
// needs the item's ACL and never shows a prompt. It is how the store tells a new
// reference from an item an older build already wrote.
static OSStatus kimi_existing(CFStringRef service, CFStringRef account, int32_t *out_exists) {
	const void *keys[6];
	const void *values[6];
	CFDictionaryRef query = NULL;
	CFTypeRef result = NULL;
	OSStatus status;

	*out_exists = -1;
	keys[0] = kSecClass;                  values[0] = kSecClassGenericPassword;
	keys[1] = kSecAttrService;            values[1] = service;
	keys[2] = kSecAttrAccount;            values[2] = account;
	keys[3] = kSecReturnAttributes;       values[3] = kCFBooleanTrue;
	keys[4] = kSecMatchLimit;             values[4] = kSecMatchLimitOne;
	keys[5] = kSecUseAuthenticationUI;    values[5] = kSecUseAuthenticationUIFail;

	query = kimi_dictionary(keys, values, 6);
	if (query == NULL) {
		return errSecAllocate;
	}
	status = SecItemCopyMatching(query, &result);
	CFRelease(query);
	if (status == errSecSuccess) {
		*out_exists = 1;
	} else if (status == errSecItemNotFound) {
		*out_exists = 0;
	}
	if (result) {
		CFRelease(result);
	}
	return status;
}

typedef struct {
	CFStringRef service;
	CFStringRef account;
	CFDataRef secret;
	// existed is -1 while unknown, 0 once a create was attempted, and 1 once an
	// update succeeded.
	int32_t existed;
	// stage is 0 for update, 1 for create, so a failure names the call that
	// actually failed.
	int32_t stage;
} kimi_write;

static OSStatus kimi_write_body(void *opaque) {
	kimi_write *write = (kimi_write *)opaque;
	const void *attributeKeys[1] = { kSecValueData };
	const void *attributeValues[1] = { write->secret };
	const void *itemKeys[4];
	const void *itemValues[4];
	CFDictionaryRef identity = NULL;
	CFDictionaryRef attributes = NULL;
	CFDictionaryRef item = NULL;
	int32_t exists = -1;
	OSStatus status = errSecAllocate;

	write->existed = -1;
	write->stage = 0;
	identity = kimi_identity(write->service, write->account);
	attributes = kimi_dictionary(attributeKeys, attributeValues, 1);
	if (identity == NULL || attributes == NULL) {
		goto cleanup;
	}
	status = SecItemUpdate(identity, attributes);
	if (status == errSecSuccess) {
		write->existed = 1;
		goto cleanup;
	}
	if (status != errSecItemNotFound) {
		// The update was refused. That can mean an item exists whose ACL does
		// not admit this binary, or that the keychain is locked. Asking for
		// attributes only settles which, without decrypting anything.
		if (kimi_existing(write->service, write->account, &exists) == errSecSuccess) {
			write->existed = exists;
			if (exists == 1) {
				goto cleanup;
			}
		} else {
			goto cleanup;
		}
	}

	// No item to update: create one. A fresh item's ACL names this process, so
	// this is the path a first save takes.
	write->existed = 0;
	write->stage = 1;
	itemKeys[0] = kSecClass;         itemValues[0] = kSecClassGenericPassword;
	itemKeys[1] = kSecAttrService;   itemValues[1] = write->service;
	itemKeys[2] = kSecAttrAccount;   itemValues[2] = write->account;
	itemKeys[3] = kSecValueData;     itemValues[3] = write->secret;
	item = kimi_dictionary(itemKeys, itemValues, 4);
	if (item == NULL) {
		status = errSecAllocate;
		goto cleanup;
	}
	status = SecItemAdd(item, NULL);
	if (status == errSecDuplicateItem) {
		// Another writer created the item between the update and the add, so the
		// item exists even if this retry is refused.
		write->existed = 1;
		write->stage = 0;
		status = SecItemUpdate(identity, attributes);
		if (status == errSecSuccess) {
			write->existed = 1;
		}
	}

cleanup:
	if (item) CFRelease(item);
	if (attributes) CFRelease(attributes);
	if (identity) CFRelease(identity);
	return status;
}

typedef struct {
	CFStringRef service;
	CFStringRef account;
	uint8_t *out;
	size_t capacity;
	size_t length;
} kimi_read;

static OSStatus kimi_read_body(void *opaque) {
	kimi_read *read = (kimi_read *)opaque;
	const void *keys[6];
	const void *values[6];
	CFDictionaryRef query = NULL;
	CFTypeRef result = NULL;
	const UInt8 *bytes = NULL;
	CFIndex length = 0;
	OSStatus status;

	keys[0] = kSecClass;                  values[0] = kSecClassGenericPassword;
	keys[1] = kSecAttrService;            values[1] = read->service;
	keys[2] = kSecAttrAccount;            values[2] = read->account;
	keys[3] = kSecReturnData;             values[3] = kCFBooleanTrue;
	keys[4] = kSecMatchLimit;             values[4] = kSecMatchLimitOne;
	keys[5] = kSecUseAuthenticationUI;    values[5] = kSecUseAuthenticationUIFail;

	query = kimi_dictionary(keys, values, 6);
	if (query == NULL) {
		return errSecAllocate;
	}
	status = SecItemCopyMatching(query, &result);
	CFRelease(query);
	if (status != errSecSuccess) {
		// The API only assigns the result on success, but release defensively so a
		// future change cannot leak a retained object.
		if (result) CFRelease(result);
		return status;
	}
	if (result == NULL || CFGetTypeID(result) != CFDataGetTypeID()) {
		if (result) CFRelease(result);
		return errSecDecode;
	}
	length = CFDataGetLength((CFDataRef)result);
	if (length < 0) {
		CFRelease(result);
		return errSecDecode;
	}
	if ((size_t)length > read->capacity) {
		read->length = (size_t)length;
		CFRelease(result);
		return errSecBufferTooSmall;
	}
	bytes = CFDataGetBytePtr((CFDataRef)result);
	if (length > 0 && bytes == NULL) {
		CFRelease(result);
		return errSecDecode;
	}
	if (length > 0) {
		memcpy(read->out, bytes, (size_t)length);
	}
	read->length = (size_t)length;
	CFRelease(result);
	return errSecSuccess;
}

typedef struct {
	CFStringRef service;
	CFStringRef account;
} kimi_identity_pair;

static OSStatus kimi_delete_body(void *opaque) {
	kimi_identity_pair *target = (kimi_identity_pair *)opaque;
	CFDictionaryRef identity = kimi_identity(target->service, target->account);
	OSStatus status;

	if (identity == NULL) {
		return errSecAllocate;
	}
	status = SecItemDelete(identity);
	CFRelease(identity);
	return status;
}

int32_t kimi_secret_write(const uint8_t *service, size_t serviceLength,
                          const uint8_t *account, size_t accountLength,
                          const uint8_t *secret, size_t secretLength,
                          int32_t interactive,
                          int32_t *outExisted, int32_t *outStage, int32_t *outRestored) {
	kimi_write write;
	OSStatus status;

	*outExisted = -1;
	*outStage = 0;
	*outRestored = 1;
	write.existed = -1;
	write.stage = 0;
	write.service = kimi_string(service, serviceLength);
	write.account = kimi_string(account, accountLength);
	write.secret = NULL;
	if (write.service == NULL || write.account == NULL) {
		if (write.service) CFRelease(write.service);
		if (write.account) CFRelease(write.account);
		return errSecParam;
	}
	write.secret = CFDataCreate(kCFAllocatorDefault, (const UInt8 *)secret, (CFIndex)secretLength);
	if (write.secret == NULL) {
		CFRelease(write.account);
		CFRelease(write.service);
		return errSecAllocate;
	}
	status = kimi_run(interactive, kimi_write_body, &write, outRestored);
	*outExisted = write.existed;
	*outStage = write.stage;
	CFRelease(write.secret);
	CFRelease(write.account);
	CFRelease(write.service);
	return status;
}

int32_t kimi_secret_read(const uint8_t *service, size_t serviceLength,
                         const uint8_t *account, size_t accountLength,
                         uint8_t *out, size_t capacity,
                         size_t *outLength, int32_t *outRestored) {
	kimi_read read;
	OSStatus status;

	*outLength = 0;
	*outRestored = 1;
	read.service = kimi_string(service, serviceLength);
	read.account = kimi_string(account, accountLength);
	read.out = out;
	read.capacity = capacity;
	read.length = 0;
	if (read.service == NULL || read.account == NULL) {
		if (read.service) CFRelease(read.service);
		if (read.account) CFRelease(read.account);
		return errSecParam;
	}
	status = kimi_run(0, kimi_read_body, &read, outRestored);
	*outLength = read.length;
	CFRelease(read.account);
	CFRelease(read.service);
	return status;
}

int32_t kimi_secret_remove(const uint8_t *service, size_t serviceLength,
                           const uint8_t *account, size_t accountLength,
                           int32_t interactive, int32_t *outRestored) {
	kimi_identity_pair target;
	OSStatus status;

	*outRestored = 1;
	target.service = kimi_string(service, serviceLength);
	target.account = kimi_string(account, accountLength);
	if (target.service == NULL || target.account == NULL) {
		if (target.service) CFRelease(target.service);
		if (target.account) CFRelease(target.account);
		return errSecParam;
	}
	status = kimi_run(interactive, kimi_delete_body, &target, outRestored);
	CFRelease(target.account);
	CFRelease(target.service);
	return status;
}

// kimi_keychain_status reports the default keychain: available, locked, and the
// current process-wide interaction flag. -1 means "could not be observed".
int32_t kimi_keychain_status(int32_t *outAvailable, int32_t *outLocked, int32_t *outInteractionAllowed) {
	Boolean allowed = true;
	SecKeychainStatus keychainStatus = 0;
	OSStatus status;

	*outAvailable = -1;
	*outLocked = -1;
	*outInteractionAllowed = -1;
	if (SecKeychainGetUserInteractionAllowed(&allowed) == errSecSuccess) {
		*outInteractionAllowed = allowed ? 1 : 0;
	}
	status = SecKeychainGetStatus(NULL, &keychainStatus);
	if (status == errSecSuccess) {
		*outAvailable = 1;
		*outLocked = (keychainStatus & kSecUnlockStateStatus) ? 0 : 1;
	} else if (status == errSecNotAvailable || status == errSecNoSuchKeychain) {
		*outAvailable = 0;
	}
	return status;
}

// kimi_session_state reports whether this process belongs to a logged-in console
// GUI session. A SecurityAgent prompt can only reach such a session, so the
// visible save path refuses to start a prompt it could not show.
int32_t kimi_session_state(int32_t *outOnConsole, int32_t *outLoginComplete) {
	CFDictionaryRef session = NULL;
	CFTypeRef value = NULL;

	*outOnConsole = -1;
	*outLoginComplete = -1;
	session = CGSessionCopyCurrentDictionary();
	if (session == NULL) {
		return errSecNotAvailable;
	}
	value = CFDictionaryGetValue(session, kCGSessionOnConsoleKey);
	if (value != NULL && CFGetTypeID(value) == CFBooleanGetTypeID()) {
		*outOnConsole = CFBooleanGetValue((CFBooleanRef)value) ? 1 : 0;
	}
	value = CFDictionaryGetValue(session, kCGSessionLoginDoneKey);
	if (value != NULL && CFGetTypeID(value) == CFBooleanGetTypeID()) {
		*outLoginComplete = CFBooleanGetValue((CFBooleanRef)value) ? 1 : 0;
	}
	CFRelease(session);
	return errSecSuccess;
}
*/
import "C"

import "unsafe"

// sdkState is one observation of the system context. Each field is 1, 0 or -1
// for yes, no and could-not-observe.
type sdkState struct {
	available          int
	locked             int
	interactionAllowed int
	onConsole          int
	loginComplete      int
}

// hasVisibleSession reports whether a prompt started now could be shown to the
// user. Only a console session is accepted: a launchd job without one would
// wait on a dialog nobody can see.
func (s sdkState) hasVisibleSession() bool { return s.onConsole == 1 }

type sdkKeychain struct{}

func (sdkKeychain) state() sdkState {
	var available, locked, allowed, onConsole, loginComplete C.int32_t
	C.kimi_keychain_status(&available, &locked, &allowed)
	C.kimi_session_state(&onConsole, &loginComplete)
	return sdkState{
		available:          int(available),
		locked:             int(locked),
		interactionAllowed: int(allowed),
		onConsole:          int(onConsole),
		loginComplete:      int(loginComplete),
	}
}

func (sdkKeychain) read(service, account string) keychainRead {
	servicePointer, serviceLength := cBytes(service)
	accountPointer, accountLength := cBytes(account)
	// The buffer is the documented largest value this store writes. A stored item
	// larger than that is not a value this build wrote, and errSecBufferTooSmall
	// says so without returning a truncated secret.
	buffer := make([]byte, maxSecretBytes)
	var length C.size_t
	var restored C.int32_t = 1

	status := C.kimi_secret_read(servicePointer, serviceLength, accountPointer, accountLength,
		(*C.uint8_t)(unsafe.Pointer(&buffer[0])), C.size_t(len(buffer)), &length, &restored)
	result := keychainRead{status: int32(status), restored: restored != 0}
	if status == errSecSuccess && uint64(length) <= uint64(len(buffer)) {
		result.value = string(buffer[:int(length)])
	}
	return result
}

func (sdkKeychain) write(service, account, secret string, interactive bool) keychainWrite {
	servicePointer, serviceLength := cBytes(service)
	accountPointer, accountLength := cBytes(account)
	secretPointer, secretLength := cBytes(secret)
	var existed, stage, restored C.int32_t = -1, 0, 1

	status := C.kimi_secret_write(servicePointer, serviceLength, accountPointer, accountLength,
		secretPointer, secretLength, cInteractive(interactive), &existed, &stage, &restored)
	return keychainWrite{status: int32(status), existed: int(existed), stage: int(stage), restored: restored != 0}
}

func (sdkKeychain) remove(service, account string, interactive bool) (int32, bool) {
	servicePointer, serviceLength := cBytes(service)
	accountPointer, accountLength := cBytes(account)
	var restored C.int32_t = 1

	status := C.kimi_secret_remove(servicePointer, serviceLength, accountPointer, accountLength,
		cInteractive(interactive), &restored)
	return int32(status), restored != 0
}

// cBytes hands the framework a read-only view of a Go string for the duration of
// one call. The framework copies the bytes into a CFString or CFData before it
// returns, so no Go pointer is retained.
func cBytes(value string) (*C.uint8_t, C.size_t) {
	if len(value) == 0 {
		return nil, 0
	}
	return (*C.uint8_t)(unsafe.Pointer(unsafe.StringData(value))), C.size_t(len(value))
}

func cInteractive(interactive bool) C.int32_t {
	if interactive {
		return 1
	}
	return 0
}
