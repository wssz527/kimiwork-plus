package kimi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wssz527/kimi-work-plus/internal/core"
)

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
	home := os.Getenv("KIMI_CODE_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(userHome, ".kimi-code")
	}
	return localKimiCodeKey(home, time.Now())
}

func localKimiCodeKey(home string, now time.Time) string {
	raw, err := core.ReadPrivateFile(filepath.Join(home, "credentials", "kimi-code.json"), 64<<10)
	if err != nil {
		return ""
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	if json.Unmarshal(raw, &token) != nil || token.ExpiresAt <= now.Unix() {
		return ""
	}
	key := strings.TrimSpace(token.AccessToken)
	if key == "" || len(key) > 4096 || strings.IndexFunc(key, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return ""
	}
	return key
}
