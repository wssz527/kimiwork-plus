package kimi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

const (
	CodeKeyStatusOK      = "ok"
	CodeKeyStatusMissing = "missing"
	CodeKeyStatusExpired = "expired"
	CodeKeyStatusInvalid = "invalid"
)

type CodeKeyStatus struct {
	Status    string `json:"status"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

func (c *Controller) KimiCodeKey() string {
	home, err := c.WorkHome()
	if err != nil {
		return ""
	}
	config, reason := readJSONFile(filepath.Join(home, NativeWorkConfig), maxConfigBytes)
	if reason != "" {
		return ""
	}
	credentials, _ := config["credentials"].(map[string]any)
	code, _ := credentials["kimiCode"].(map[string]any)
	return stringField(code, "apiKey")
}

func LocalKimiCodeKey() string {
	home, ok := kimiCodeHome()
	if !ok {
		return ""
	}
	return localKimiCodeKey(home, time.Now())
}

func LocalKimiCodeKeyStatus() CodeKeyStatus {
	home, ok := kimiCodeHome()
	if !ok {
		return CodeKeyStatus{Status: CodeKeyStatusMissing}
	}
	return ReadCodeKeyStatus(home, time.Now())
}

func ReadCodeKeyStatus(home string, now time.Time) CodeKeyStatus {
	if home == "" {
		var ok bool
		home, ok = kimiCodeHome()
		if !ok {
			return CodeKeyStatus{Status: CodeKeyStatusMissing}
		}
	}
	if now.IsZero() {
		now = time.Now()
	}
	status, _ := readLocalKimiCodeKey(home, now)
	return status
}

func kimiCodeHome() (string, bool) {
	if home := os.Getenv("KIMI_CODE_HOME"); home != "" {
		return home, true
	}
	userHome, err := os.UserHomeDir()
	if err != nil || userHome == "" {
		return "", false
	}
	return filepath.Join(userHome, ".kimi-code"), true
}

func localKimiCodeKey(home string, now time.Time) string {
	status, key := readLocalKimiCodeKey(home, now)
	if status.Status != CodeKeyStatusOK {
		return ""
	}
	return key
}

func readLocalKimiCodeKey(home string, now time.Time) (CodeKeyStatus, string) {
	raw, err := core.ReadPrivateFile(filepath.Join(home, "credentials", "kimi-code.json"), 64<<10)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CodeKeyStatus{Status: CodeKeyStatusMissing}, ""
		}
		return CodeKeyStatus{Status: CodeKeyStatusInvalid}, ""
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	if json.Unmarshal(raw, &token) != nil {
		return CodeKeyStatus{Status: CodeKeyStatusInvalid}, ""
	}
	key := strings.TrimSpace(token.AccessToken)
	if key == "" || len(key) > 4096 || strings.IndexFunc(key, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return CodeKeyStatus{Status: CodeKeyStatusInvalid}, ""
	}
	if token.ExpiresAt <= 0 {
		return CodeKeyStatus{Status: CodeKeyStatusInvalid}, ""
	}
	expiresAt := time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339)
	if token.ExpiresAt <= now.Unix() {
		return CodeKeyStatus{Status: CodeKeyStatusExpired, ExpiresAt: expiresAt}, ""
	}
	return CodeKeyStatus{Status: CodeKeyStatusOK, ExpiresAt: expiresAt}, key
}
