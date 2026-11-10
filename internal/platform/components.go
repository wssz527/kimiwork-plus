package platform

import "runtime"

// NativeComponent is one capability that depends on Electron, a native addon, or
// an official application API rather than on portable Go code.
//
// The point of this type is to make a gap explicit instead of hiding it. A
// component is never reported as available because the Go build succeeded: the
// state records what actually exists on this host, and the caller renders it.
type NativeComponent struct {
	// ID is the stable key the feature matrix uses.
	ID string `json:"id"`
	// State is one of the constants below.
	State string `json:"state"`
	// Reason is a short machine-readable code.
	Reason string `json:"reason,omitempty"`
	// Detail is a human-readable sentence for the panel.
	Detail string `json:"detail,omitempty"`
	// LegacyEntry names the released 0.12.1 capability a user can still use
	// while this gap remains open. Empty means there is nothing to fall back to.
	LegacyEntry string `json:"legacyEntry,omitempty"`
}

const (
	// ComponentNative means this build provides the capability itself.
	ComponentNative = "native"
	// ComponentLegacyEntry means the released build still provides it and this
	// build does not. The user keeps that entry point.
	ComponentLegacyEntry = "legacy-entry"
	// ComponentUnavailable means neither this build nor a legacy entry covers
	// it on this host.
	ComponentUnavailable = "unavailable"
)

const (
	ComponentCredentials = "remote_credentials"
	ComponentUpdater     = "official_updater"
	ComponentCompanion   = "remote_companion"
)

// Components reports the state of every capability that is not plain Go.
//
// Two of these are deliberately not reimplemented in Go:
//
//	remote_credentials  the released build runs a headless Electron process to
//	                    reach safeStorage under a fixed bundle identity. The
//	                    Keychain ACL of that identity is what existing
//	                    ciphertext is bound to. Go can read and write its own
//	                    Keychain items (see OpenCredentials), but it cannot
//	                    reproduce that identity, and it must not try to decrypt
//	                    the old blobs.
//	official_updater    remote-official-updater.node drives Squirrel/Sparkle
//	                    inside the official application's framework, including
//	                    signature checks of ShipIt and the update requirement.
//	                    Reimplementing it means reimplementing that contract,
//	                    not just calling an API.
//
// Neither is "impossible in principle". Both are out of scope for this build,
// and saying so is more useful than a partial implementation that would change
// the official update behaviour silently.
func Components() []NativeComponent {
	platform := runtime.GOOS
	components := []NativeComponent{
		{
			ID: ComponentCredentials, State: ComponentLegacyEntry,
			Reason:      "electron_safe_storage_identity",
			Detail:      "Existing Kimi credentials were encrypted by the released build's Electron safeStorage under its own code identity. This build stores its own secrets natively but does not decrypt those blobs.",
			LegacyEntry: "0.12.1 credential helper",
		},
		{
			ID: ComponentUpdater, State: ComponentLegacyEntry,
			Reason:      "native_squirrel_bridge",
			Detail:      "Official update checks run through a native addon bound to the application's Squirrel framework and its code-signature requirements. This build does not drive it.",
			LegacyEntry: "0.12.1 update helper",
		},
	}
	if platform != "darwin" {
		// The remote companion builds a signed bundle from the installed app and
		// is macOS-only in the released build as well.
		components = append(components, NativeComponent{
			ID: ComponentCompanion, State: ComponentUnavailable,
			Reason: "macos_only",
			Detail: "The remote companion is built from the macOS application bundle and has no equivalent on this platform.",
		})
	} else {
		components = append(components, NativeComponent{
			ID: ComponentCompanion, State: ComponentLegacyEntry,
			Reason:      "requires_signed_companion_bundle",
			Detail:      "Remote model support launches a companion bundle derived from the installed application. This build does not create or launch it.",
			LegacyEntry: "0.12.1 remote companion",
		})
	}
	return components
}

// ComponentByID returns one entry, so the panel can render a single row without
// scanning.
func ComponentByID(id string) (NativeComponent, bool) {
	for _, component := range Components() {
		if component.ID == id {
			return component, true
		}
	}
	return NativeComponent{}, false
}
