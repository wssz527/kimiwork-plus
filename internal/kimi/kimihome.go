package kimi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Resolving the Work data directory is not a guess. Kimi derives it from its own
// Electron profile, and the released build reads exactly two sources:
//
//	<profile>/daimon-storage.json   version 1, `shareDir` relocates Work data
//	otherwise                       <profile>/daimon-share/daimon
//
// A managed child runs with `--user-data-dir=<selected profile>`. Its Work data
// must stay inside that directory. A path that resolves outside it — through
// `shareDir`, through a symlink, or through an explicit override — is refused
// rather than trusted, so every mode writes only within its selected tree.

const (
	storageFileName   = "daimon-storage.json"
	maxStorageBytes   = 64 * 1024
	defaultSharePath  = "daimon-share"
	workDataDirectory = "daimon"
)

var (
	// ErrKimiHomeOutsideProfile means the resolved Work directory escapes the
	// managed profile.
	ErrKimiHomeOutsideProfile = errors.New("kimi_home_outside_profile")
	// ErrKimiHomeUnresolved means no Work directory could be derived safely.
	ErrKimiHomeUnresolved = errors.New("kimi_home_unresolved")
)

// ResolveKimiHome derives the Work data directory for a managed profile.
//
// `override` is the caller's explicit choice; it is honoured only when it lies
// inside the profile after symlink resolution. The returned path keeps the
// caller's own prefix (so a caller that passes /var/... gets /var/... back, not
// /private/var/...), because the containment test is the only thing that needs
// the resolved form.
func ResolveKimiHome(profileDir, override string) (string, error) {
	if !filepath.IsAbs(profileDir) {
		return "", ErrKimiHomeUnresolved
	}
	profile, err := filepath.Abs(profileDir)
	if err != nil {
		return "", ErrKimiHomeUnresolved
	}
	realProfile, err := resolveAllowMissing(profile)
	if err != nil {
		return "", ErrKimiHomeUnresolved
	}

	if override != "" {
		if !filepath.IsAbs(override) {
			return "", ErrKimiHomeUnresolved
		}
		if _, err := ensureWithin(realProfile, override); err != nil {
			return "", err
		}
		return filepath.Clean(override), nil
	}

	share, ok := storedShareDirectory(realProfile)
	if !ok {
		share = filepath.Join(profile, defaultSharePath)
	}
	candidate := filepath.Join(share, workDataDirectory)
	if _, err := ensureWithin(realProfile, candidate); err != nil {
		return "", err
	}
	return filepath.Clean(candidate), nil
}

// storedShareDirectory reads `shareDir` from the profile's own settings. Anything
// unexpected answers false, so the historical layout is used instead of a guessed
// location.
func storedShareDirectory(profile string) (string, bool) {
	file := filepath.Join(profile, storageFileName)
	info, err := os.Lstat(file)
	if err != nil {
		return "", false
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxStorageBytes {
		return "", false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	var document struct {
		Version  int    `json:"version"`
		ShareDir string `json:"shareDir"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(string(data), "\ufeff")), &document) != nil {
		return "", false
	}
	if document.Version != 1 || document.ShareDir == "" || len(document.ShareDir) > 4096 {
		return "", false
	}
	if !filepath.IsAbs(document.ShareDir) {
		return "", false
	}
	for _, character := range document.ShareDir {
		if character < 0x20 || character == 0x7f {
			return "", false
		}
	}
	// A bare drive such as `D:` is drive-relative; a trailing separator would
	// survive into every joined path.
	trimmed := strings.TrimRight(document.ShareDir, `/\`)
	if trimmed == "" || !filepath.IsAbs(trimmed) {
		return "", false
	}
	return trimmed, true
}

// ensureWithin resolves a candidate and refuses it unless the result is the
// profile itself or beneath it. Symlinks are resolved first, so a link created
// inside the profile cannot point outside it.
func ensureWithin(profile, candidate string) (string, error) {
	resolved, err := resolveAllowMissing(candidate)
	if err != nil {
		return "", ErrKimiHomeUnresolved
	}
	if resolved == profile {
		return resolved, nil
	}
	prefix := profile + string(filepath.Separator)
	if !strings.HasPrefix(resolved, prefix) {
		return "", ErrKimiHomeOutsideProfile
	}
	return resolved, nil
}

// resolveAllowMissing resolves the deepest existing ancestor and appends the
// remaining components, so a directory that does not exist yet still gets a
// trustworthy absolute path.
func resolveAllowMissing(candidate string) (string, error) {
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	current := absolute
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// ProfileDirectoryForManagedChild is the isolated profile the managed child is
// started with. It exists so the controller, the actions and the integration all
// agree on one directory.
func ProfileDirectoryForManagedChild(stateDir string) string {
	return filepath.Join(stateDir, "kimi-profile")
}
