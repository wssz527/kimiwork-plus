package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/byokdns"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func revealBYOKFlag(t *testing.T, n *Native) {
	t.Helper()
	flag := n.byokFlagFile()
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flag, []byte(`{"revealed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newManagedKimiHome places the Work data directory inside this Native's own
// profile and signs it in as one account. `store` is written as the official
// byok-providers.json when it is not nil.
func newManagedKimiHome(t *testing.T, n *Native, store map[string]any) string {
	t.Helper()
	home := filepath.Join(n.controller.Settings().UserDataDir, "work")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{
		"model": map[string]any{},
		"credentials": map[string]any{
			"kimiWeb": map[string]any{"userId": "test-current-account"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	if store != nil {
		encoded, err := json.Marshal(store)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "byok-providers.json"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings := n.controller.Settings()
	settings.KimiHome = home
	if err := n.controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	return home
}

func managedGatewayStore(account string) map[string]any {
	return map[string]any{"v": 3, "accounts": map[string]any{account: map[string]any{
		"revision": "account-revision",
		"sources": []any{map[string]any{
			"id": "byok-kth", "kind": "kth", "displayName": "KimiWork +", "revision": "source-revision",
			"registryUrl": "http://127.0.0.1:9100/v1/models/api.json",
		}},
		"active":  map[string]any{"providers": []any{}, "models": []any{}},
		"pending": map[string]any{"providers": []any{}, "models": []any{}},
	}}}
}

// The gateway connect path has to settle the same BYOK gate the direct path
// does; without it a machine whose hosts entry is gone can never import.
func TestNativeGatewayConnectPreparesBYOKBeforeImport(t *testing.T) {
	n := newBYOKTestNative(t)
	var stages []string
	currentStage := func() string {
		n.mu.Lock()
		defer n.mu.Unlock()
		return core.String(n.operation["stage"])
	}
	n.byokPrepare = func(context.Context) error {
		stages = append(stages, "prepare:"+currentStage())
		return nil
	}
	n.importModels = func(context.Context) (kimi.ImportResult, error) {
		stages = append(stages, "import:"+currentStage())
		return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true}, nil
	}
	if _, err := n.Action(context.Background(), core.Document{"action": "connect-kimi"}); err != nil {
		t.Fatal(err)
	}
	op := waitBYOKOperation(t, n)
	if op["state"] != "prepared" {
		t.Fatalf("operation = %+v, want prepared", op)
	}
	if len(stages) != 2 || stages[0] != "prepare:"+diagnosticStagePrepare || stages[1] != "import:"+diagnosticStageImportModels {
		t.Fatalf("stages = %v", stages)
	}
}

// The official 3.2.29 build caches a failed gate probe for a minute. When the
// flag and the name are already in place, one managed restart clears it and the
// explicit connect succeeds.
func TestNativeGatewayConnectClearsTheStaleFeatureGate(t *testing.T) {
	n := newBYOKTestNative(t)
	revealBYOKFlag(t, n)
	n.managedProbe = func() bool { return true }
	n.byokPrepare = func(context.Context) error { return nil }
	attempts := 0
	n.importModels = func(context.Context) (kimi.ImportResult, error) {
		attempts++
		if attempts == 1 {
			return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: "native_import_feature-disabled"}, nil
		}
		return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true}, nil
	}
	restarts := 0
	n.restartOfficial = func(context.Context) error {
		restarts++
		return nil
	}
	if _, err := n.Action(context.Background(), core.Document{"action": "connect-kimi"}); err != nil {
		t.Fatal(err)
	}
	op := waitBYOKOperation(t, n)
	if op["state"] != "prepared" || attempts != 2 || restarts != 1 {
		t.Fatalf("operation = %+v, attempts = %d, restarts = %d", op, attempts, restarts)
	}
}

func TestNativeConnectKeepsTheRefusalWhenARestartCannotHelp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Native)
	}{
		{name: "kimi not managed", mutate: func(*Native) {}},
		{name: "intranet not ready", mutate: func(n *Native) {
			n.managedProbe = func() bool { return true }
			n.probe = &byokdns.Probe{Lookup: func(context.Context, string) (string, error) { return "8.8.8.8", nil }}
		}},
		{name: "flag missing", mutate: func(n *Native) {
			n.managedProbe = func() bool { return true }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newBYOKTestNative(t)
			if tc.name == "kimi not managed" {
				revealBYOKFlag(t, n)
			}
			tc.mutate(n)
			n.byokPrepare = func(context.Context) error { return nil }
			attempts := 0
			n.importModels = func(context.Context) (kimi.ImportResult, error) {
				attempts++
				return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: "native_import_feature_disabled"}, nil
			}
			restarts := 0
			n.restartOfficial = func(context.Context) error {
				restarts++
				return nil
			}
			if _, err := n.Action(context.Background(), core.Document{"action": "connect-kimi"}); err != nil {
				t.Fatal(err)
			}
			op := waitBYOKOperation(t, n)
			if op["state"] != "failed" || op["reason"] != "native_import_feature_disabled" {
				t.Fatalf("operation = %+v", op)
			}
			if attempts != 1 || restarts != 0 {
				t.Fatalf("attempts = %d, restarts = %d, want one attempt and no restart", attempts, restarts)
			}
		})
	}
}

func TestNativeConnectRestartsAtMostOnceForTheStaleGate(t *testing.T) {
	t.Run("retry still refused", func(t *testing.T) {
		n := newBYOKTestNative(t)
		revealBYOKFlag(t, n)
		n.managedProbe = func() bool { return true }
		n.byokPrepare = func(context.Context) error { return nil }
		attempts := 0
		n.importModels = func(context.Context) (kimi.ImportResult, error) {
			attempts++
			return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: "native_import_feature_disabled"}, nil
		}
		restarts := 0
		n.restartOfficial = func(context.Context) error {
			restarts++
			return nil
		}
		if _, err := n.Action(context.Background(), core.Document{"action": "connect-kimi"}); err != nil {
			t.Fatal(err)
		}
		op := waitBYOKOperation(t, n)
		if op["state"] != "failed" || op["reason"] != "native_import_feature_disabled" {
			t.Fatalf("operation = %+v", op)
		}
		if attempts != 2 || restarts != 1 {
			t.Fatalf("attempts = %d, restarts = %d", attempts, restarts)
		}
	})
	t.Run("restart fails", func(t *testing.T) {
		n := newBYOKTestNative(t)
		revealBYOKFlag(t, n)
		n.managedProbe = func() bool { return true }
		n.byokPrepare = func(context.Context) error { return nil }
		attempts := 0
		n.importModels = func(context.Context) (kimi.ImportResult, error) {
			attempts++
			return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: "native_import_feature_disabled"}, nil
		}
		n.restartOfficial = func(context.Context) error { return errors.New("kimi_restart_failed") }
		if _, err := n.Action(context.Background(), core.Document{"action": "connect-kimi"}); err != nil {
			t.Fatal(err)
		}
		op := waitBYOKOperation(t, n)
		if op["state"] != "failed" || op["reason"] != "native_import_feature_disabled" {
			t.Fatalf("operation = %+v", op)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want one", attempts)
		}
	})
}

func TestNativePrepareRecoversFromTheStaleFeatureGate(t *testing.T) {
	n := newBYOKTestNative(t)
	revealBYOKFlag(t, n)
	n.managedProbe = func() bool { return true }
	n.byokPrepare = func(context.Context) error { return nil }
	attempts := 0
	n.importModels = func(context.Context) (kimi.ImportResult, error) {
		attempts++
		if attempts == 1 {
			return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: "native_import_feature-disabled"}, nil
		}
		return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true}, nil
	}
	restarts := 0
	n.restartOfficial = func(context.Context) error {
		restarts++
		return nil
	}
	if _, err := n.Action(context.Background(), core.Document{"action": "prepare-native"}); err != nil {
		t.Fatal(err)
	}
	op := waitBYOKOperation(t, n)
	if op["state"] != "prepared" || attempts != 2 || restarts != 1 {
		t.Fatalf("operation = %+v, attempts = %d, restarts = %d", op, attempts, restarts)
	}
}

// A brand-new account with no managed source in the official store has nothing
// to disconnect there; only this product's local record is left.
func TestNativeDisconnectFeatureDisabledClearsCleanLocalState(t *testing.T) {
	n := newBYOKTestNative(t)
	home := newManagedKimiHome(t, n, nil)
	if account := kimi.AccountScope(home); account == "" {
		t.Fatal("the fixture has no account identity")
	}
	disconnects := 0
	n.disconnectModels = func(context.Context) (kimi.DisconnectResult, error) {
		disconnects++
		return kimi.DisconnectResult{State: kimi.ImportStateFailed, Reason: "model_disconnect_feature_disabled"}, nil
	}
	prepared := 0
	n.byokPrepare = func(context.Context) error {
		prepared++
		return nil
	}
	if _, err := n.Action(context.Background(), core.Document{"action": "disconnect-models"}); err != nil {
		t.Fatal(err)
	}
	op := waitBYOKOperation(t, n)
	if op["state"] != "removed" {
		t.Fatalf("operation = %+v, want removed", op)
	}
	result, ok := op["result"].(kimi.DisconnectResult)
	if !ok || !result.OK || result.State != "disconnected" || result.Sources != 0 {
		t.Fatalf("local disconnect result = %+v", op["result"])
	}
	if disconnects != 1 || prepared != 0 {
		t.Fatalf("official calls = %d, prepares = %d", disconnects, prepared)
	}
	if _, err := os.Stat(filepath.Join(home, "byok-providers.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the local-only disconnect wrote the official store: %v", err)
	}
}

// With a managed source still in the store the official disconnect is required,
// so the gate is settled and the write is retried.
func TestNativeDisconnectFeatureDisabledRetriesWithTheGateSettled(t *testing.T) {
	n := newBYOKTestNative(t)
	home := newManagedKimiHome(t, n, nil)
	store := managedGatewayStore(kimi.AccountScope(home))
	encoded, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "byok-providers.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	n.byokPrepare = func(context.Context) error { return nil }
	attempts := 0
	n.disconnectModels = func(context.Context) (kimi.DisconnectResult, error) {
		attempts++
		if attempts == 1 {
			return kimi.DisconnectResult{State: kimi.ImportStateFailed, Reason: "model_disconnect_feature_disabled"}, nil
		}
		return kimi.DisconnectResult{OK: true, State: "disconnected", Sources: 1}, nil
	}
	if _, err := n.Action(context.Background(), core.Document{"action": "disconnect-models"}); err != nil {
		t.Fatal(err)
	}
	op := waitBYOKOperation(t, n)
	if op["state"] != "removed" || attempts != 2 {
		t.Fatalf("operation = %+v, attempts = %d", op, attempts)
	}
}

func TestNativeDisconnectFeatureDisabledWithUnverifiedStoreSettlesTheGate(t *testing.T) {
	n := newBYOKTestNative(t)
	home := newManagedKimiHome(t, n, nil)
	if err := os.WriteFile(filepath.Join(home, "byok-providers.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared := 0
	n.byokPrepare = func(context.Context) error {
		prepared++
		return nil
	}
	attempts := 0
	n.disconnectModels = func(context.Context) (kimi.DisconnectResult, error) {
		attempts++
		if attempts == 1 {
			return kimi.DisconnectResult{State: kimi.ImportStateFailed, Reason: "model_disconnect_feature_disabled"}, nil
		}
		return kimi.DisconnectResult{OK: true, State: "disconnected", Sources: 1}, nil
	}
	if _, err := n.Action(context.Background(), core.Document{"action": "disconnect-models"}); err != nil {
		t.Fatal(err)
	}
	op := waitBYOKOperation(t, n)
	if op["state"] != "removed" || attempts != 2 || prepared != 1 {
		t.Fatalf("operation = %+v, attempts = %d, prepares = %d", op, attempts, prepared)
	}
}

func TestNativeDisconnectFeatureDisabledRestartsOnceForTheStaleGate(t *testing.T) {
	newFixture := func(t *testing.T) *Native {
		t.Helper()
		n := newBYOKTestNative(t)
		revealBYOKFlag(t, n)
		n.managedProbe = func() bool { return true }
		n.byokPrepare = func(context.Context) error { return nil }
		home := newManagedKimiHome(t, n, nil)
		encoded, err := json.Marshal(managedGatewayStore(kimi.AccountScope(home)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "byok-providers.json"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("retry after the restart succeeds", func(t *testing.T) {
		n := newFixture(t)
		attempts := 0
		n.disconnectModels = func(context.Context) (kimi.DisconnectResult, error) {
			attempts++
			if attempts < 3 {
				return kimi.DisconnectResult{State: kimi.ImportStateFailed, Reason: "model_disconnect_feature_disabled"}, nil
			}
			return kimi.DisconnectResult{OK: true, State: "disconnected", Sources: 1}, nil
		}
		restarts := 0
		n.restartOfficial = func(context.Context) error {
			restarts++
			return nil
		}
		if _, err := n.Action(context.Background(), core.Document{"action": "disconnect-models"}); err != nil {
			t.Fatal(err)
		}
		op := waitBYOKOperation(t, n)
		if op["state"] != "removed" || attempts != 3 || restarts != 1 {
			t.Fatalf("operation = %+v, attempts = %d, restarts = %d", op, attempts, restarts)
		}
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			if n.dns.Port() != 0 {
				t.Fatal("the listener was left running")
			}
		}
	})
	t.Run("restart failure keeps the refusal", func(t *testing.T) {
		n := newFixture(t)
		attempts := 0
		n.disconnectModels = func(context.Context) (kimi.DisconnectResult, error) {
			attempts++
			return kimi.DisconnectResult{State: kimi.ImportStateFailed, Reason: "model_disconnect_feature_disabled"}, nil
		}
		restarts := 0
		n.restartOfficial = func(context.Context) error {
			restarts++
			return errors.New("kimi_restart_failed")
		}
		if _, err := n.Action(context.Background(), core.Document{"action": "disconnect-models"}); err != nil {
			t.Fatal(err)
		}
		op := waitBYOKOperation(t, n)
		if op["state"] != "failed" || op["reason"] != "model_disconnect_feature_disabled" {
			t.Fatalf("operation = %+v", op)
		}
		if attempts != 2 || restarts != 1 {
			t.Fatalf("attempts = %d, restarts = %d", attempts, restarts)
		}
	})
}

// Removing the DNS entry after a recovered disconnect still runs.
func TestNativeDisconnectRecoveryStillRemovesDNS(t *testing.T) {
	n := newBYOKTestNative(t)
	home := newManagedKimiHome(t, n, nil)
	store := managedGatewayStore(kimi.AccountScope(home))
	encoded, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "byok-providers.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	n.byokPrepare = func(context.Context) error { return nil }
	attempts := 0
	n.disconnectModels = func(context.Context) (kimi.DisconnectResult, error) {
		attempts++
		if attempts == 1 {
			return kimi.DisconnectResult{State: kimi.ImportStateFailed, Reason: "model_disconnect_feature_disabled"}, nil
		}
		return kimi.DisconnectResult{OK: true, State: "disconnected", Sources: 1}, nil
	}
	if _, err := n.Action(context.Background(), core.Document{"action": "disconnect-models", "removeDns": true}); err != nil {
		t.Fatal(err)
	}
	op := waitBYOKOperation(t, n)
	if op["state"] != "removed" {
		t.Fatalf("operation = %+v, want removed", op)
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		if n.dns.Port() != 0 {
			t.Fatal("removeDns did not stop the listener")
		}
	}
}
