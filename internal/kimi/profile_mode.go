package kimi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	ProfileModeOfficial           = "official"
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
		return ProfileModeOfficial
	}
	return s.ProfileMode
}

func OfficialProfileDirectory() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("existing_login_profile_unavailable")
		}
		return filepath.Join(home, "Library", "Application Support", "kimi-desktop"), nil
	case "windows":
		root, err := os.UserConfigDir()
		if err != nil {
			return "", errors.New("existing_login_profile_unavailable")
		}
		return filepath.Join(root, "kimi-desktop"), nil
	default:
		return "", ErrProfileModeUnsupported
	}
}

func existingLoginDirectory() (string, error) {
	path, err := OfficialProfileDirectory()
	if err != nil {
		return "", err
	}
	if err := validateExistingProfileDirectory(path); err != nil {
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
	implemented := mode == ProfileModeIsolated ||
		(mode == ProfileModeOfficial && settings.UserDataDir != "" && (runtime.GOOS == "darwin" || runtime.GOOS == "windows")) ||
		(mode == ProfileModeExistingAuthorized && runtime.GOOS == "darwin" && settings.LoginDataDir != "")
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
	switch settings.profileMode() {
	case ProfileModeIsolated:
		return nil
	case ProfileModeOfficial:
		if settings.UserDataDir != "" && (runtime.GOOS == "darwin" || runtime.GOOS == "windows") {
			return nil
		}
	case ProfileModeExistingAuthorized:
		if runtime.GOOS == "darwin" && settings.LoginDataDir != "" {
			return nil
		}
	}
	return ErrProfileModeUnsupported
}

func (c *Controller) OpenWork(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.reapChildLocked()
	c.mu.Lock()
	actions := c.actions
	settings := c.settings
	c.mu.Unlock()
	if settings.profileMode() == ProfileModeOfficial && c.managedChildAlive() {
		var command *exec.Cmd
		const workURL = "kimi-work://home"
		switch runtime.GOOS {
		case "darwin":
			bundle := filepath.Dir(filepath.Dir(filepath.Dir(settings.Installation.Application)))
			command = exec.CommandContext(ctx, "/usr/bin/open", "-a", bundle, workURL)
		case "windows":
			command = exec.CommandContext(ctx, settings.Installation.Application, workURL)
		}
		if command != nil {
			if err := command.Run(); err != nil {
				return errors.New("native_bridge_activation_refused")
			}
			return nil
		}
	}
	if actions == nil {
		return errors.New("native_bridge_waiting_work")
	}
	result, err := actions.ActivateWork(ctx)
	if err != nil {
		return err
	}
	if !result.OK {
		return errors.New(result.Reason)
	}
	return nil
}

func (c *Controller) StartOfficial(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	return c.startOfficial(ctx)
}

func (c *Controller) startOfficial(ctx context.Context) error {
	profile, err := OfficialProfileDirectory()
	if err != nil {
		return err
	}
	c.reapChildLocked()
	c.reconcileMu.Lock()
	c.mu.Lock()
	if c.isolation != nil && c.child == nil {
		c.mu.Unlock()
		c.reconcileMu.Unlock()
		return errWorkStorageConflict
	}
	if c.child != nil && c.settings.profileMode() != ProfileModeOfficial {
		c.mu.Unlock()
		c.reconcileMu.Unlock()
		return errors.New("profile_mode_change_requires_exit")
	}
	c.settings.ProfileMode = ProfileModeOfficial
	c.settings.UserDataDir = profile
	c.settings.LoginDataDir = profile
	// The profile directory changed, so every reconcile that read the previous
	// one is now about the wrong tree.
	c.settingsGen++
	c.mu.Unlock()
	c.reconcileMu.Unlock()
	return c.start(ctx)
}

func (c *Controller) StartWithExistingLogin(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	profile, err := existingLoginDirectory()
	if err != nil {
		return err
	}
	c.reapChildLocked()
	c.reconcileMu.Lock()
	c.mu.Lock()
	if c.isolation != nil && c.child == nil {
		c.mu.Unlock()
		c.reconcileMu.Unlock()
		return errWorkStorageConflict
	}
	if c.child != nil && c.settings.profileMode() != ProfileModeExistingAuthorized {
		c.mu.Unlock()
		c.reconcileMu.Unlock()
		return errors.New("profile_mode_change_requires_exit")
	}
	c.settings.ProfileMode = ProfileModeExistingAuthorized
	c.settings.LoginDataDir = profile
	c.settings.UserDataDir = ProfileDirectoryForManagedChild(c.settings.StateDir)
	// The profile directory changed, so every reconcile that read the previous
	// one is now about the wrong tree.
	c.settingsGen++
	c.mu.Unlock()
	c.reconcileMu.Unlock()
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
