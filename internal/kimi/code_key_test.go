package kimi

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalKimiCodeKeyUsesCredentialStoreRatherThanRefreshLock(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "credentials"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "oauth"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "oauth", "kimi-code"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "credentials", "kimi-code.json")
	now := time.Unix(1000, 0)
	for _, tt := range []struct {
		name, json, want string
	}{
		{"valid", `{"access_token":"local-key","expires_at":2000}`, "local-key"},
		{"expired", `{"access_token":"local-key","expires_at":999}`, ""},
		{"missing-expiry", `{"access_token":"local-key"}`, ""},
		{"invalid", `{"access_token":"key\nother","expires_at":2000}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tt.json), 0600); err != nil {
				t.Fatal(err)
			}
			if got := localKimiCodeKey(home, now); got != tt.want {
				t.Fatal("unexpected credential availability")
			}
		})
	}
	os.Remove(path)
	outside := filepath.Join(t.TempDir(), "token.json")
	os.WriteFile(outside, []byte(`{"access_token":"private","expires_at":2000}`), 0600)
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if localKimiCodeKey(home, now) != "" {
		t.Fatal("followed credential symlink")
	}
}
