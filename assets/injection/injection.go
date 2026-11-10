package injection

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The injection assets are versioned JavaScript payloads. They are executed by
// the page, not by Go, and this package deliberately does not rewrite them: the
// renderer speaks a fixed protocol with the CDP host
// (__kwgTranslation / __kwgTranslate / __kwgTranslationReceive /
// __kwgTranslationConfig / __kwgTranslationGeneration) and any edit here is a
// protocol change, not a tweak.
//
//go:embed translation-renderer.v1.js
var assets embed.FS

// Asset is one versioned payload.
type Asset struct {
	// Name is the file name inside this package.
	Name string `json:"name"`
	// Version is the protocol generation the payload speaks.
	Version int `json:"version"`
	// SHA256 is the digest of the exact bytes that are injected.
	SHA256 string `json:"sha256"`
	// Bytes is the payload size, for the panel's diagnostics.
	Bytes int `json:"bytes"`
}

// ErrAssetMissing means the embedded payload is not what this build expects.
var ErrAssetMissing = errors.New("injection_asset_missing")

// ErrContractMismatch means the payload does not declare the host protocol.
var ErrContractMismatch = errors.New("injection_contract_mismatch")

const (
	rendererFile    = "translation-renderer.v1.js"
	rendererVersion = 1
)

// requiredMarkers are the globals the payload must define for the CDP host to
// speak to it. `__kwgTranslationScope` is not listed: the host writes that
// preamble itself, before the payload, so the payload never names it.
var requiredMarkers = []string{
	"__kwgTranslation",
	"__kwgTranslate",
	"__kwgTranslationReceive",
	"__kwgTranslationConfig",
	"__kwgTranslationGeneration",
}

// TranslationRenderer returns the payload bytes and its descriptor.
//
// The bytes are returned exactly as embedded. The host appends a small preamble
// (scope, generation, settings) before them; this package never prepends or
// rewrites the payload itself.
func TranslationRenderer() ([]byte, Asset, error) {
	data, err := assets.ReadFile(rendererFile)
	if err != nil {
		return nil, Asset{}, fmt.Errorf("%w: %s", ErrAssetMissing, rendererFile)
	}
	for _, marker := range requiredMarkers {
		if !strings.Contains(string(data), marker) {
			return nil, Asset{}, fmt.Errorf("%w: %s", ErrContractMismatch, marker)
		}
	}
	digest := sha256.Sum256(data)
	return data, Asset{Name: rendererFile, Version: rendererVersion,
		SHA256: hex.EncodeToString(digest[:]), Bytes: len(data)}, nil
}

// Describe lists every embedded payload without returning its bytes.
func Describe() ([]Asset, error) {
	_, asset, err := TranslationRenderer()
	if err != nil {
		return nil, err
	}
	out := []Asset{asset}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
