package kimi

import (
	"errors"
	"path/filepath"
)

// Which profile a managed Kimi runs against is a product decision with a
// security boundary behind it, so it is named rather than inferred.
//
//	isolated            this product's own profile under StateDir. It is the
//	                    default and the only mode that is implemented.
//	existing-authorized the user's signed-in profile. It is refused, not
//	                    half-done: reading or writing the user's real Kimi tree
//	                    needs ownership proof and a safe handover that this
//	                    package does not have, and a soft link or a copied
//	                    cookie would be a way around state containment rather
//	                    than a feature.
const (
	ProfileModeIsolated           = "isolated"
	ProfileModeExistingAuthorized = "existing-authorized"
)

// ErrProfileModeUnsupported is returned when a caller asks for the existing
// profile. It is a stated limitation, not a failure to try harder.
var ErrProfileModeUnsupported = errors.New("profile_mode_existing_authorized_unsupported")

// ProfileStatus is the renderable profile report. It carries the mode and two
// digests, never a path: a UI can show that the profile changed without learning
// where the user's data lives.
type ProfileStatus struct {
	Mode string `json:"mode"`
	// Implemented is false for a mode that is recognised but not available.
	Implemented bool `json:"implemented"`
	// Reason explains a refused or incomplete mode.
	Reason string `json:"reason,omitempty"`
	// ProfileDigest identifies the profile directory the child uses.
	ProfileDigest string `json:"profileDigest,omitempty"`
	// ScopeDigest identifies the account scope the profile was read for.
	ScopeDigest string `json:"scopeDigest,omitempty"`
	// ModeDigest is a stable digest of mode + profile, so a UI can tell "the
	// profile changed" from "the mode changed" without either path.
	ModeDigest string `json:"modeDigest,omitempty"`
}

// profileMode answers the configured mode, defaulting to isolated. An empty
// value is the isolated mode, not "whatever is convenient".
func (s ControllerSettings) profileMode() string {
	if s.ProfileMode == "" {
		return ProfileModeIsolated
	}
	return s.ProfileMode
}

// ProfileStatus reports the profile mode without exposing the profile path.
func (c *Controller) ProfileStatus() ProfileStatus {
	c.mu.Lock()
	settings := c.settings
	account := settings.AccountScope
	c.mu.Unlock()

	mode := settings.profileMode()
	status := ProfileStatus{Mode: mode, Implemented: mode == ProfileModeIsolated}
	if mode == ProfileModeExistingAuthorized {
		status.Reason = ErrProfileModeUnsupported.Error()
	}
	// The digest is of the cleaned profile name only, so it identifies a profile
	// across calls while a renderer cannot reconstruct where it is.
	if settings.UserDataDir != "" {
		status.ProfileDigest = sha256Hex(filepath.Clean(settings.UserDataDir))
	}
	status.ScopeDigest = sha256If(account)
	status.ModeDigest = sha256Hex(stableJSON(map[string]any{"mode": mode,
		"profile": status.ProfileDigest, "scope": status.ScopeDigest}))
	return status
}

// requireSupportedProfileMode refuses a mode this build does not implement,
// before any process is started or any file is written.
func (c *Controller) requireSupportedProfileMode() error {
	c.mu.Lock()
	mode := c.settings.profileMode()
	c.mu.Unlock()
	if mode != ProfileModeIsolated {
		return ErrProfileModeUnsupported
	}
	return nil
}
