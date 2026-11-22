package kimi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestReadCodeKeyStatusDistinguishesStates(t *testing.T) {
	home := t.TempDir()
	now := time.Unix(1_700_000_000, 0).UTC()

	if status := ReadCodeKeyStatus(home, now); status.Status != CodeKeyStatusMissing || status.ExpiresAt != "" {
		t.Fatalf("missing file status = %+v", status)
	}

	credDir := filepath.Join(home, "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(credDir, "kimi-code.json")

	validExp := now.Add(2 * time.Hour).Unix()
	if err := os.WriteFile(path, []byte(`{"access_token":"secret-code-key-value","expires_at":1700007200}`), 0o600); err != nil {
		t.Fatal(err)
	}
	okStatus := ReadCodeKeyStatus(home, now)
	if okStatus.Status != CodeKeyStatusOK || okStatus.ExpiresAt != time.Unix(validExp, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("ok status = %+v", okStatus)
	}
	encoded, err := json.Marshal(okStatus)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-code-key-value") {
		t.Fatalf("CodeKeyStatus leaked key material: %s", string(encoded))
	}
	if got := localKimiCodeKey(home, now); got != "secret-code-key-value" {
		t.Fatalf("localKimiCodeKey = %q", got)
	}

	expiredExp := now.Add(-10 * time.Minute).Unix()
	if err := os.WriteFile(path, []byte(`{"access_token":"secret-code-key-value","expires_at":1699999400}`), 0o600); err != nil {
		t.Fatal(err)
	}
	expiredStatus := ReadCodeKeyStatus(home, now)
	if expiredStatus.Status != CodeKeyStatusExpired || expiredStatus.ExpiresAt != time.Unix(expiredExp, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("expired status = %+v", expiredStatus)
	}
	if got := localKimiCodeKey(home, now); got != "" {
		t.Fatalf("expired key returned non-empty: %q", got)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"malformed-json", `{not-json`},
		{"missing-expiry", `{"access_token":"secret-code-key-value"}`},
		{"empty-token", `{"access_token":"   ","expires_at":1700007200}`},
		{"control-char-token", `{"access_token":"bad\ntoken","expires_at":1700007200}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if status := ReadCodeKeyStatus(home, now); status.Status != CodeKeyStatusInvalid {
				t.Fatalf("%s status = %+v, want invalid", tc.name, status)
			}
		})
	}

	if runtime.GOOS != "windows" {
		if err := os.WriteFile(path, []byte(`{"access_token":"secret-code-key-value","expires_at":1700007200}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if status := ReadCodeKeyStatus(home, now); status.Status != CodeKeyStatusInvalid {
			t.Fatalf("world-readable file status = %+v, want invalid", status)
		}
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(outside, []byte(`{"access_token":"secret-code-key-value","expires_at":1700007200}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if status := ReadCodeKeyStatus(home, now); status.Status != CodeKeyStatusInvalid {
		t.Fatalf("symlink status = %+v, want invalid", status)
	}
}
