package kimi

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Installation is one discovered official Kimi install. Every field is derived
// from the filesystem; nothing here starts, stops or signals the application.
type Installation struct {
	// Application is the executable the archive belongs to.
	Application string `json:"application"`
	// Archive is the app.asar the main bundle was read from.
	Archive string `json:"archive"`
	// Version comes from the archive's own package.json, then Info.plist.
	Version string `json:"version,omitempty"`
	// Platform is the platform the paths were resolved for.
	Platform string `json:"platform"`
}

// ExecutableCandidates maps an archive path to the executables that may own it.
// The archive's directory layout is the only source: a path outside these shapes
// is refused rather than guessed.
func ExecutableCandidates(archive string, platform string) []string {
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform == "darwin" {
		parent := path.Dir(path.Dir(path.Clean(archive)))
		return []string{path.Join(parent, "MacOS", "Kimi")}
	}
	parent := filepath.Dir(filepath.Dir(filepath.Clean(archive)))
	if platform == "win32" || platform == "windows" {
		out := make([]string, 0, 4)
		for _, name := range []string{"Kimi.exe", "kimi-desktop.exe", "Kimi", "kimi"} {
			out = append(out, filepath.Join(parent, name))
		}
		return out
	}
	return nil
}

// SameInstallation reports whether a launch record describes this exact
// install: the executable must be the one this archive implies, unless the
// caller explicitly named it.
func SameInstallation(record Installation, discovered string, explicit bool, platform string) bool {
	if record.Application == "" || record.Archive == "" {
		return false
	}
	if explicit {
		return true
	}
	if discovered == "" {
		return false
	}
	application := filepath.Clean(record.Application)
	if !samePath(application, filepath.Clean(discovered), platform) {
		return false
	}
	for _, candidate := range ExecutableCandidates(record.Archive, platform) {
		if samePath(application, filepath.Clean(candidate), platform) {
			return true
		}
	}
	return false
}

func samePath(left, right, platform string) bool {
	if platform == "win32" || platform == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

// Detect finds installations without touching any process. It reads the known
// folders, and on Windows the user's own shortcuts. A path that exists but has
// no readable archive is still reported, with an empty version, so the panel can
// say "installed but unreadable" instead of "not installed".
func Detect(platform string, env map[string]string) []Installation {
	if platform == "" {
		platform = runtime.GOOS
	}
	var out []Installation
	seen := map[string]bool{}
	for _, archive := range ArchiveCandidates(platform, env) {
		key := strings.ToLower(archive)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, err := os.Stat(archive); err != nil {
			continue
		}
		installation := Installation{Archive: archive, Platform: platform,
			Version: InstalledVersion(archive)}
		for _, candidate := range ExecutableCandidates(archive, platform) {
			if _, err := os.Stat(candidate); err == nil {
				installation.Application = candidate
				break
			}
		}
		out = append(out, installation)
	}
	return out
}

// ---------------------------------------------------------------------------
// process identity

// ProcessIdentity proves which process a pid refers to, so a recycled pid is
// never mistaken for the application a record described. `unknown` is a real
// answer: it means the platform could not tell, and callers must not read it as
// "not running".
type ProcessIdentity struct {
	Kind   string `json:"kind"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

const (
	IdentityDarwinLStart = "darwin_ps_lstart"
	IdentityWindowsFT    = "windows_process_creation_filetime"
	IdentityUnknown      = "unknown"
	IdentityNotFound     = "not_found"
)

// RunningState is the answer the panel needs about the official application.
type RunningState string

const (
	// Running means at least one process matched the installation exactly.
	Running RunningState = "running"
	// NotRunning means no process matched and the platform could prove it.
	NotRunning RunningState = "not_running"
	// RunningStateUnknown means the check could not be completed. It is never
	// reported as not running.
	RunningStateUnknown RunningState = "unknown"
)

// NativeState is the read-only view the UI renders. It never starts, stops or
// signals the official application.
type NativeState struct {
	Platform     string          `json:"platform"`
	Installation *Installation   `json:"installation,omitempty"`
	Application  RunningState    `json:"application"`
	Reason       string          `json:"reason,omitempty"`
	PIDs         []int           `json:"pids,omitempty"`
	Contract     *ContractResult `json:"contract,omitempty"`
	Launch       *LaunchResult   `json:"launch,omitempty"`
}

var ErrIdentityUnknown = errors.New("process_identity_unknown")

// ClassifySurface decides which reasoning surface a CDP target is.
//
//	work  the local renderer (kimi-agent.html)
//	chat  the remote Kimi Chat page
//	other everything else: settings, membership, plugins, a lookalike host,
//	      an iframe or a worker
//
// Only `work` and `chat` may receive a script, a binding or reasoning text. This
// allowlist is the only thing standing between the reasoning text and an
// arbitrary page, so it is written as a positive match rather than a denylist.
func ClassifySurface(targetType, targetURL string) string {
	if targetType != "page" {
		return "other"
	}
	parsed, ok := surfaceURL(targetURL)
	if !ok {
		return "other"
	}
	switch parsed.Scheme {
	case "app":
		if localhostDesktop(parsed) && parsed.Path == "/kimi-agent.html" {
			return "work"
		}
	case "file", "kimi":
		if strings.HasSuffix(parsed.Path, "/kimi-agent.html") {
			return "work"
		}
	case "https":
		if parsed.Host == "www.kimi.com" && parsed.RawQuery == "" && chatPath(parsed.Path) {
			return "chat"
		}
	}
	return "other"
}

func chatPath(path string) bool {
	if path == "/" || path == "/chat/history" {
		return true
	}
	const prefix = "/chat/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	id := path[len(prefix):]
	if id == "" || len(id) > 128 {
		return false
	}
	for _, character := range id {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

// IsTranslationTarget is the boolean form used by target polling.
func IsTranslationTarget(targetType, targetURL string) bool {
	return ClassifySurface(targetType, targetURL) != "other"
}

// SanitizeTargetURL removes anything that could carry data out of a URL before
// it is logged or shown: query, fragment and credentials are dropped.
func SanitizeTargetURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func describeInstallation(installations []Installation) string {
	if len(installations) == 0 {
		return "not_installed"
	}
	return fmt.Sprintf("%s", installations[0].Archive)
}
