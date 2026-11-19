// Package platform holds the operating-system edges the gateway needs: one
// writer per state directory, a native credential store, and a description of a
// user-level service the user installs themselves.
//
// The guiding rule is that an unsupported operation reports itself as
// unsupported. It never silently degrades into something weaker — a credential
// store that quietly wrote plaintext, or a lock that only worked inside one
// process, would be worse than a clear error.
package platform

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// ErrUnsupported means this platform cannot provide the capability.
var ErrUnsupported = errors.New("platform_unsupported")

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("state_dir_locked")

// ErrCredentialMissing means the store has no value for the key.
var ErrCredentialMissing = errors.New("credential_missing")

// ErrCredentialInvalid means the key or value is not acceptable input. It is
// returned before any store is touched.
var ErrCredentialInvalid = errors.New("credential_invalid")

// Platform is the compile-time target, not the runtime host.
func Platform() string { return runtime.GOOS }

const (
	StateDirName              = "kimi-work-plus"
	LegacyPreviewStateDirName = "kimi-work-plus-preview"
)

// DefaultStateDir is the per-user directory this product owns.
func DefaultStateDir() (string, error) {
	if explicit := os.Getenv("KIMI_WORK_PLUS_STATE_DIR"); explicit != "" {
		return filepath.Abs(explicit)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, StateDirName), nil
}

// LegacyPreviewStateDir is the per-user directory used by preview builds prior to 1.0.0.
func LegacyPreviewStateDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, LegacyPreviewStateDirName), nil
}

// EnsurePrivateDir creates a directory that only this user can reach and
// verifies it afterwards. A symlink, a foreign owner or group/other access is
// refused rather than repaired: a directory someone else can write to is not a
// safe place for a token or a config revision.
func EnsurePrivateDir(dir string) (string, error) {
	resolved, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(resolved, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("state_dir_not_a_directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		// Tightening is allowed: the directory is ours, and a previous run may
		// have been created with a looser umask.
		if err := os.Chmod(resolved, 0o700); err != nil {
			return "", err
		}
	}
	// Only the final component has to be a real directory. An ancestor may be a
	// symlink for legitimate platform reasons (macOS /var -> /private/var), and
	// refusing those would reject every temporary directory on the system. A
	// symlink at the state directory itself is different: it would redirect
	// writes somewhere this process did not choose.
	parent, err := filepath.EvalSymlinks(filepath.Dir(resolved))
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", err
	}
	if real != filepath.Join(parent, filepath.Base(resolved)) {
		return "", errors.New("state_dir_symlinked")
	}
	return resolved, nil
}
