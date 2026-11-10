//go:build darwin && cgo

package platform

/*
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>

// kimi_sdk_status_codes reports the OSStatus values the installed SDK assigns to
// the states this store distinguishes. The test compares them with the constants
// hard-coded in secretstore_darwin.go, so a wrong name cannot ship as a wrong
// diagnosis: if Apple ever renumbered a code, the test fails instead.
static int32_t kimi_sdk_interaction_not_allowed(void) { return errSecInteractionNotAllowed; }
static int32_t kimi_sdk_interaction_required(void) { return errSecInteractionRequired; }
static int32_t kimi_sdk_item_not_found(void) { return errSecItemNotFound; }
static int32_t kimi_sdk_duplicate_item(void) { return errSecDuplicateItem; }
static int32_t kimi_sdk_auth_failed(void) { return errSecAuthFailed; }
static int32_t kimi_sdk_user_canceled(void) { return errSecUserCanceled; }
static int32_t kimi_sdk_not_available(void) { return errSecNotAvailable; }
static int32_t kimi_sdk_no_such_keychain(void) { return errSecNoSuchKeychain; }
static int32_t kimi_sdk_decode(void) { return errSecDecode; }
static int32_t kimi_sdk_missing_entitlement(void) { return errSecMissingEntitlement; }

// kimi_sdk_abbreviations confirms the ABI the shim relies on: the interaction
// flag is a Boolean (one byte), and the getter writes it through a pointer while
// returning an OSStatus.
static int kimi_sdk_boolean_size(void) { return (int)sizeof(Boolean); }
*/
import "C"

// sdkStatusCodes is what the installed headers say. It is only reachable from
// the darwin cgo test.
func sdkStatusCodes() map[string]int32 {
	return map[string]int32{
		"errSecInteractionNotAllowed": int32(C.kimi_sdk_interaction_not_allowed()),
		"errSecInteractionRequired":   int32(C.kimi_sdk_interaction_required()),
		"errSecItemNotFound":          int32(C.kimi_sdk_item_not_found()),
		"errSecDuplicateItem":         int32(C.kimi_sdk_duplicate_item()),
		"errSecAuthFailed":            int32(C.kimi_sdk_auth_failed()),
		"errSecUserCanceled":          int32(C.kimi_sdk_user_canceled()),
		"errSecNotAvailable":          int32(C.kimi_sdk_not_available()),
		"errSecNoSuchKeychain":        int32(C.kimi_sdk_no_such_keychain()),
		"errSecDecode":                int32(C.kimi_sdk_decode()),
		"errSecMissingEntitlement":    int32(C.kimi_sdk_missing_entitlement()),
	}
}

func sdkBooleanSize() int { return int(C.kimi_sdk_boolean_size()) }
