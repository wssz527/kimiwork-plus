package kimi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const (
	ProfileModeIsolated           = "isolated"
	ProfileModeExistingAuthorized = "existing-authorized"
)

var ErrProfileModeUnsupported = errors.New("profile_mode_existing_authorized_unsupported")

type ProfileStatus struct {
	Mode          string `json:"mode"`
	Implemented   bool   `json:"implemented"`
	CanReuseLogin bool   `json:"canReuseLogin"`
	Reason        string `json:"reason,omitempty"`
	ProfileDigest string `json:"profileDigest,omitempty"`
	ScopeDigest   string `json:"scopeDigest,omitempty"`
	ModeDigest    string `json:"modeDigest,omitempty"`
}

func (s ControllerSettings) profileMode() string {
	if s.ProfileMode == "" {
		return ProfileModeIsolated
	}
	return s.ProfileMode
}

func existingLoginDirectory() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", ErrProfileModeUnsupported
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("existing_login_profile_unavailable")
	}
	path := filepath.Join(home, "Library", "Application Support", "kimi-desktop")
	if _, err := existingProfileIdentity(path); err != nil {
		return "", err
	}
	return path, nil
}

func (c *Controller) ProfileStatus() ProfileStatus {
	c.mu.Lock()
	settings := c.settings
	account := settings.AccountScope
	c.mu.Unlock()
	_, available := existingLoginDirectory()
	mode := settings.profileMode()
	implemented := mode == ProfileModeIsolated || (mode == ProfileModeExistingAuthorized && runtime.GOOS == "darwin" && settings.LoginDataDir != "")
	status := ProfileStatus{Mode: mode, Implemented: implemented, CanReuseLogin: available == nil}
	if !implemented {
		status.Reason = ErrProfileModeUnsupported.Error()
	}
	if settings.UserDataDir != "" {
		status.ProfileDigest = sha256Hex(filepath.Clean(settings.UserDataDir))
	}
	status.ScopeDigest = sha256If(account)
	status.ModeDigest = sha256Hex(stableJSON(map[string]any{"mode": mode,
		"profile": status.ProfileDigest, "scope": status.ScopeDigest}))
	return status
}

func (c *Controller) requireSupportedProfileMode() error {
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()
	if settings.profileMode() == ProfileModeIsolated {
		return nil
	}
	if settings.profileMode() == ProfileModeExistingAuthorized && runtime.GOOS == "darwin" && settings.LoginDataDir != "" {
		return nil
	}
	return ErrProfileModeUnsupported
}

func (c *Controller) StartWithExistingLogin(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	profile, err := existingLoginDirectory()
	if err != nil {
		return err
	}
	c.reapChildLocked()
	c.mu.Lock()
	if c.isolation != nil && c.child == nil {
		c.mu.Unlock()
		return errWorkStorageConflict
	}
	if c.child != nil && c.settings.profileMode() != ProfileModeExistingAuthorized {
		c.mu.Unlock()
		return errors.New("profile_mode_change_requires_exit")
	}
	c.settings.ProfileMode = ProfileModeExistingAuthorized
	c.settings.LoginDataDir = profile
	c.mu.Unlock()
	return c.start(ctx)
}

func (c *Controller) restoreWorkIsolation() error {
	c.mu.Lock()
	lease := c.isolation
	child := c.child
	c.mu.Unlock()
	if lease != nil && child != nil {
		select {
		case <-child.Exit():
		default:
			return errors.New("work_storage_restore_requires_exit")
		}
	}
	if err := lease.Restore(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.isolation == lease {
		c.isolation = nil
	}
	c.mu.Unlock()
	return nil
}

// The storage pointer must be reviewed again when the official main process changes.
func verifyExistingLoginStorage(installation Installation) error {
	source, err := ReadAsarEntry(installation.Archive, EntryPath)
	if err == nil {
		switch sha256Hex(source) {
		case "04556e91118cab7a2ead8e7e4aaf308ca3f8e0f7dfdb231ec512fb639a0ec5b6",
			"581d239df2b6174651baae70cb2a694a8aecd8786600e3386df0059d6e3d6394":
			return nil
		}
	}
	return errors.New("work_storage_contract_unverified")
}
