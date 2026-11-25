package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestActivateAccountOfflineRollsBackIfIndexCommitFails(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("official process enumeration is supported on macOS and Windows")
	}
	h := newAccountHarness(t)
	h.controller.actions = nil
	settings := h.controller.Settings()
	settings.Installation.Application = filepath.Join(h.stateDir, "Kimi.exe")
	if err := os.WriteFile(settings.Installation.Application, []byte("test application"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	target := AccountSession{UserID: "target", KimiWeb: map[string]any{"accessToken": "target"}, CodeKey: json.RawMessage(`{"key":"target"}`)}
	if err := h.controller.ActivateAccountOffline(ctx, target, func() error { t.Error("committed without Work config"); return nil }); err == nil || err.Error() != AccountErrWorkHomeUnavailable {
		t.Fatalf("missing config = %v", err)
	}
	h.writeConfig(t, accountConfigFixture("original", "old", "old-refresh"))
	if err := os.WriteFile(accountCodeKeyPath(h.home), []byte(`{"key":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	indexErr := errors.New("test_index_error")
	err := h.controller.ActivateAccountOffline(ctx, target, func() error {
		if !accountConfigMatches(h.home, "target") {
			t.Error("index committed before credential readback")
		}
		return indexErr
	})
	key, _ := readAccountCodeKey(h.home)
	if !errors.Is(err, indexErr) || !accountConfigMatches(h.home, "original") || string(key) != `{"key":"old"}` {
		t.Fatalf("rollback = %v, original config matched = %v, original key matched = %v", err, accountConfigMatches(h.home, "original"), string(key) == `{"key":"old"}`)
	}
	h.controller.actions = &accountActions{fakeAccountPage: h.page, fakeActions: &fakeActions{connected: true}}
	if err := h.controller.ActivateAccountOffline(ctx, target, func() error { t.Error("committed with a running surface"); return nil }); err == nil || err.Error() != AccountErrUnavailable {
		t.Fatalf("offline write alongside a running surface = %v", err)
	}
}

func TestAccountActivationRestoresPageAfterOfflineDiskSwitch(t *testing.T) {
	h := newAccountHarness(t)
	h.writeConfig(t, accountConfigFixture("target", "target-access", "target-refresh"))
	h.page.put("kimi-1", "msh_user_id", "original")
	h.page.put("kimi-1", "access_token", "original-access")
	h.page.runtimeWatch, h.page.runtimeTransition = true, true
	h.page.onActivation = func() {
		h.writeConfig(t, accountConfigFixture("target", "target-access", "target-refresh"))
	}
	target := AccountSession{UserID: "target", KimiWeb: map[string]any{"accessToken": "target-access"},
		Cookies:      []AccountCookie{{Name: "access_token", Domain: ".kimi.com", Path: "/", Value: "target-cookie"}},
		LocalStorage: []AccountStorageEntry{{Key: "access_token", Value: "target-access"}},
	}
	result, err := h.controller.ActivateAccount(context.Background(), target)
	if err != nil || !result.Verified || result.UserID != "target" {
		t.Fatalf("page catch-up: %+v (%v)", result, err)
	}
	if h.page.callsFor("write-storage:kimi-1") == 0 || h.page.callsFor("set-cookies:kimi-1") == 0 {
		t.Fatalf("page did not receive tokens and cookies: %v", h.page.calls)
	}
}
