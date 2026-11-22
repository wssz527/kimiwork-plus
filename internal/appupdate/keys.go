package appupdate

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"strings"
)

var (
	// DefaultPublicKeyHex is the pinned Ed25519 public key used to authenticate
	// signed release artifacts. Overridable at build time via -ldflags -X.
	DefaultPublicKeyHex = "ecbf04bb5823c4b7936c47c64477c812b56b123b3f1a6a1e49e1794f209b1367"

	// DefaultGitHubRepo is the GitHub Releases repository ("owner/repo") queried
	// when no explicit feed URL is configured. Overridable at build time via -ldflags -X.
	DefaultGitHubRepo = "wssz527/kimiwork-plus"

	// DefaultGitHubAPIBase is the GitHub API origin.
	DefaultGitHubAPIBase = "https://api.github.com"

	// DefaultMirrorBase is the CNB domestic mirror origin queried before GitHub.
	// Overridable at build time via -ldflags -X.
	DefaultMirrorBase = "https://cnb.cool"

	// DefaultGitHubWebBase is the GitHub web origin used for direct release
	// manifest and artifact fallback downloads. Overridable at build time via -ldflags -X.
	DefaultGitHubWebBase = "https://github.com"
)

func PinnedPublicKey() ([]byte, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(DefaultPublicKeyHex))
	if err != nil {
		return nil, fmt.Errorf("invalid pinned update public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("unexpected pinned update public key length: %d", len(raw))
	}
	return raw, nil
}
