package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func offlineAccountNative(t *testing.T, installed bool) (*Native, string) {
	t.Helper()
	dir := t.TempDir()
	profile := filepath.Join(dir, "profile")
	home := filepath.Join(profile, "daimon-share", "daimon")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	install := kimi.Installation{}
	if installed {
		install.Application = filepath.Join(dir, "Kimi.exe")
		if err := os.WriteFile(install.Application, []byte("test application"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	controller, err := kimi.NewController(kimi.ControllerSettings{Installation: install, StateDir: dir, UserDataDir: profile, ProfileMode: kimi.ProfileModeIsolated})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{StateDir: dir, Keys: CacheKeys(testKeys{}), Endpoint: Endpoint{URL: "http://127.0.0.1:4242", Key: "panel"}}
	n := &Native{service: service, controller: controller}
	if err := n.initAccounts(&Runtime{}); err != nil {
		t.Fatal(err)
	}
	n.quota = newNativeQuota(service, func() string { return "" }, func() string { return "" })
	service.Extra = func(ctx context.Context, path string, in core.Document) (any, error) {
		if path == "/api/accounts" {
			return core.Document{"accounts": n.accountsState()}, nil
		}
		return n.Action(ctx, in)
	}
	return n, home
}

func seedOfflineAccount(t *testing.T, n *Native, user string) (AccountSummary, []byte) {
	t.Helper()
	session := kimi.AccountSession{SchemaVersion: 1, UserID: user,
		Cookies:      []kimi.AccountCookie{{Name: "access_token", Domain: ".kimi.com", Path: "/", Value: user + "-cookie"}},
		LocalStorage: []kimi.AccountStorageEntry{{Key: "access_token", Value: user + "-fresh"}},
		KimiWeb:      map[string]any{"userId": user, "accessToken": user + "-stale"},
		KimiCode:     map[string]any{"apiKey": user + "-code"},
		CodeKey:      json.RawMessage(`{"key":"` + user + `-kernel"}`),
	}
	raw, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	row, err := n.accounts.vault.capture(accountCapture{UserID: user, Name: user, Session: raw})
	if err != nil {
		t.Fatal(err)
	}
	return row, raw
}

func TestOfflineAccountSwitchPersistsWithoutCapturingAndCompletesAfterLaunch(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("official process enumeration is supported on macOS and Windows")
	}
	n, home := offlineAccountNative(t, true)
	target, targetSecret := seedOfflineAccount(t, n, "target")
	original, originalSecret := seedOfflineAccount(t, n, "original")
	config := filepath.Join(home, kimi.NativeWorkConfig)
	codeKey := filepath.Join(home, kimi.AccountCodeKey)
	if err := os.WriteFile(config, []byte(`{"model":{"selected":"keep"},"credentials":{"kimiWeb":{"userId":"original"},"foreign":{"keep":true}}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codeKey, []byte(`{"key":"original-kernel"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	n.accounts.hooks.Capture = func(context.Context) (accountCapture, error) {
		t.Error("offline switch captured the previous session")
		return accountCapture{}, errors.New("unexpected_capture")
	}
	w := callPanel(t, n.service, "POST", "/api/actions", `{"action":"account-switch","accountId":"`+target.ID+`"}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"state":"offline"`) || strings.Contains(w.Body.String(), "target-fresh") {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	state := callPanel(t, n.service, "GET", "/api/accounts", "")
	var payload struct {
		Accounts struct {
			CurrentID  string                 `json:"currentId"`
			PendingID  string                 `json:"pendingAccountId"`
			RestoreID  string                 `json:"restoreAccountId"`
			Activation kimi.AccountActivation `json:"activation"`
		}
	}
	if err := json.Unmarshal(state.Body.Bytes(), &payload); err != nil || payload.Accounts.CurrentID != target.ID || payload.Accounts.PendingID != target.ID || payload.Accounts.RestoreID != "" || payload.Accounts.Activation.AccountID != target.ID || !payload.Accounts.Activation.Verified {
		t.Fatalf("accounts: %s (%v)", state.Body.String(), err)
	}
	if raw, err := n.accounts.vault.session(original.ID); err != nil || !bytes.Equal(raw, originalSecret) {
		t.Fatal("original credentials were altered")
	}
	for file, want := range map[string][]string{config: {`"target-fresh"`, `"target-code"`, `"foreign"`, `"keep"`}, codeKey: {`"target-kernel"`}} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range want {
			if !bytes.Contains(raw, []byte(value)) {
				t.Fatalf("missing %q in %s", value, filepath.Base(file))
			}
		}
		info, err := os.Stat(file)
		if err != nil || (runtime.GOOS == "darwin" && info.Mode().Perm() != 0o640) {
			t.Fatalf("file mode for %s: %v %v", filepath.Base(file), info, err)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected Work data files: %v %v", entries, err)
	}

	hooks := n.accounts.hooks
	busy := true
	hooks.Busy = func(context.Context) error {
		if busy {
			return errors.New("account_work_busy")
		}
		return nil
	}
	verified := false
	calls := 0
	hooks.SyncPending = func(_ context.Context, raw json.RawMessage) (string, error) {
		calls++
		if !bytes.Equal(raw, targetSecret) {
			t.Error("pending activation did not use the saved page credentials")
		}
		if !verified {
			return "", errors.New("account_identity_mismatch")
		}
		return "target", nil
	}
	n.accounts, err = newAccountManager(newAccountVault(n.service.StateDir, n.service.Keys), hooks)
	if err != nil {
		t.Fatal(err)
	}
	n.observeAccounts(context.Background())
	if calls != 0 || n.accountsState()["pendingAccountId"] != target.ID {
		t.Fatal("observer changed the pending account while Work was busy")
	}
	busy = false
	n.observeAccounts(context.Background())
	if calls != 1 || n.accountsState()["pendingAccountId"] != target.ID {
		t.Fatal("observer cleared the pending account after a failed activation")
	}
	verified = true
	n.observeAccounts(context.Background())
	if calls != 2 || n.accountsState()["pendingAccountId"] != nil || n.accountsState()["currentId"] != target.ID {
		t.Fatal("observer did not settle the pending account after a verified activation")
	}
}

func TestOfflinePendingSyncMarksRejectedCredentialsForLogin(t *testing.T) {
	n, _ := offlineAccountNative(t, false)
	target, _ := seedOfflineAccount(t, n, "target")
	if err := n.accounts.vault.markActivation(target.ID, false, true); err != nil {
		t.Fatal(err)
	}
	calls := 0
	n.accounts.hooks.Busy = func(context.Context) error { return nil }
	n.accounts.hooks.SyncPending = func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "", errors.New("account_credentials_rejected")
	}
	n.observeAccounts(context.Background())
	rows, current, pending, err := n.accounts.vault.state()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || pending != "" || current != target.ID {
		t.Fatalf("calls=%d pending=%q current=%q", calls, pending, current)
	}
	if len(rows) != 1 || !rows[0].NeedsLogin {
		t.Fatalf("rejected account was not marked for login: %+v", rows)
	}
	n.observeAccounts(context.Background())
	if calls != 1 {
		t.Fatalf("a rejected pending account was synced again: %d", calls)
	}
}

func TestOfflinePendingSyncBacksOffRepeatedFailures(t *testing.T) {
	n, _ := offlineAccountNative(t, false)
	target, _ := seedOfflineAccount(t, n, "target")
	if err := n.accounts.vault.markActivation(target.ID, false, true); err != nil {
		t.Fatal(err)
	}
	calls := 0
	n.accounts.hooks.Busy = func(context.Context) error { return nil }
	n.accounts.hooks.SyncPending = func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "", errors.New("account_identity_mismatch")
	}
	n.observeAccounts(context.Background())
	n.observeAccounts(context.Background())
	if calls != 2 {
		t.Fatalf("first two page syncs were not attempted: %d", calls)
	}
	n.mu.Lock()
	failures, retryAt := n.pendingFailures, n.pendingRetryAt
	n.mu.Unlock()
	if failures != 2 || retryAt.IsZero() || !retryAt.After(time.Now()) {
		t.Fatalf("no cooldown after two failures: %d %v", failures, retryAt)
	}
	n.observeAccounts(context.Background())
	if calls != 2 {
		t.Fatalf("a retry inside the cooldown reached the page: %d", calls)
	}
	n.mu.Lock()
	n.pendingRetryAt = time.Now().Add(-time.Second)
	n.mu.Unlock()
	n.observeAccounts(context.Background())
	if calls != 3 {
		t.Fatalf("the page was not retried after the cooldown: %d", calls)
	}
}

func TestOfflineAccountActionsReturnConflictWhenPrerequisitesMissing(t *testing.T) {
	n, _ := offlineAccountNative(t, false)
	target, _ := seedOfflineAccount(t, n, "target")
	for _, tc := range []struct{ body, code, message string }{
		{`{"action":"account-capture"}`, "account_session_unavailable", "请先打开并登录 Kimi Work，再保存当前账号。"},
		{`{"action":"account-switch","accountId":"` + target.ID + `"}`, "account_work_home_unavailable", "找不到 Kimi Work 的配置。"},
	} {
		w := callPanel(t, n.service, "POST", "/api/actions", tc.body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), tc.code) || !strings.Contains(w.Body.String(), tc.message) {
			t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
		}
	}
}

func TestCompletePendingAccountDefersWhileAutoSyncIsSuspendedAndKeepsReplacedOperation(t *testing.T) {
	n, _ := offlineAccountNative(t, true)
	n.autoSync = newAutoSyncCoordinator(context.Background(), n)
	t.Cleanup(n.autoSync.stop)
	row, _ := seedOfflineAccount(t, n, "pending-user")

	n.accounts.hooks.Busy = func(context.Context) error { return nil }
	started := 0
	n.accounts.hooks.SyncPending = func(context.Context, json.RawMessage) (string, error) {
		started++
		return row.ID, nil
	}

	// A suspended coordinator records the deferred intent without claiming the slot.
	n.autoSync.suspend()
	n.completePendingAccount(context.Background(), row.ID)
	if started != 0 {
		t.Fatal("a suspended coordinator must not start the page sync")
	}
	n.mu.Lock()
	claimed := n.operation
	n.mu.Unlock()
	if claimed != nil {
		t.Fatalf("operation was claimed while suspended: %v", claimed)
	}
	n.autoSync.mu.Lock()
	deferred := n.autoSync.deferred
	n.autoSync.mu.Unlock()
	if !deferred {
		t.Fatal("a skipped run must be deferred")
	}
	n.autoSync.resume()

	// A replacement operation takes the slot mid-sync; the deferred restore must keep it.
	replaced := core.Document{"id": "other-operation", "action": "connect-kimi", "state": "running"}
	n.accounts.hooks.SyncPending = func(context.Context, json.RawMessage) (string, error) {
		n.mu.Lock()
		n.operation = replaced
		n.mu.Unlock()
		return row.ID, nil
	}
	n.completePendingAccount(context.Background(), row.ID)
	n.mu.Lock()
	got := n.operation
	n.mu.Unlock()
	if got == nil || got["id"] != "other-operation" || got["action"] != "connect-kimi" {
		t.Fatalf("a replaced operation was overwritten: %v", got)
	}
}
