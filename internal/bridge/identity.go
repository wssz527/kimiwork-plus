package bridge

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	AppIdentifier              = "dev.kimi-work-plus"
	LegacyPreviewAppIdentifier = "dev.kimi-work-plus.preview"
)

var (
	Version = "1.0.0"
	BuildID = "development"
)

func CredentialNamespace(stateDir string) string {
	identity := sha256.Sum256([]byte(stateDir))
	return AppIdentifier + "." + hex.EncodeToString(identity[:8])
}

func LegacyPreviewCredentialNamespace(stateDir string) string {
	identity := sha256.Sum256([]byte(stateDir))
	return LegacyPreviewAppIdentifier + "." + hex.EncodeToString(identity[:8])
}
