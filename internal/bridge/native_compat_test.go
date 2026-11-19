package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func waitNativeOperation(t *testing.T, n *Native, id string) core.Document {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		op := core.Document{}
		for k, v := range n.operation {
			op[k] = v
		}
		n.mu.Unlock()
		if op["id"] == id && op["state"] != "running" {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %s did not settle", id)
	return nil
}

func TestNativeCompatibilityWriteGuards(t *testing.T) {
	stateDir := t.TempDir()
	ctrl, err := kimi.NewController(kimi.ControllerSettings{
		StateDir:    stateDir,
		UserDataDir: stateDir,
		LoginDataDir: stateDir,
		ProfileMode: kimi.ProfileModeOfficial,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("model sync guard blocks connect and prepare only", func(t *testing.T) {
		n := &Native{
			service:    &Service{StateDir: stateDir},
			controller: ctrl,
			compatibility: kimi.CompatibilityReport{
				Status:  kimi.CompatStatusDegraded,
				Summary: "模型同步不可用",
				Guards:  kimi.CompatibilityGuards{ModelSyncWriteBlocked: true},
			},
		}
		for _, action := range []string{"connect-kimi", "prepare-native"} {
			res, err := n.Action(context.Background(), core.Document{"action": action})
			if err != nil {
				t.Fatalf("%s returned sync err: %v", action, err)
			}
			op := core.Object(core.Object(res)["operation"])
			done := waitNativeOperation(t, n, core.String(op["id"]))
			if done["state"] != "failed" || done["reason"] != kimi.CompatErrModelSyncBlocked {
				t.Fatalf("%s expected %s, got %+v", action, kimi.CompatErrModelSyncBlocked, done)
			}
		}
		if n.accountSwitchWriteBlocked() {
			t.Fatal("account switch should remain unblocked when only model sync failed")
		}
	})

	t.Run("account switch guard blocks account writes but not model sync", func(t *testing.T) {
		n := &Native{
			service:    &Service{StateDir: stateDir},
			controller: ctrl,
			compatibility: kimi.CompatibilityReport{
				Status:  kimi.CompatStatusDegraded,
				Summary: "账号切换不可用",
				Guards:  kimi.CompatibilityGuards{AccountSwitchWriteBlocked: true},
			},
		}
		for _, action := range []string{"account-capture", "account-begin-add", "account-complete-add", "account-cancel-add", "account-switch"} {
			res, err := n.Action(context.Background(), core.Document{"action": action})
			if err != nil {
				t.Fatalf("%s returned sync err: %v", action, err)
			}
			op := core.Object(core.Object(res)["operation"])
			done := waitNativeOperation(t, n, core.String(op["id"]))
			if done["state"] != "failed" || done["reason"] != kimi.CompatErrAccountSwitchBlocked {
				t.Fatalf("%s expected %s, got %+v", action, kimi.CompatErrAccountSwitchBlocked, done)
			}
		}
		// account-remove only touches local keychain, not official Kimi.
		res, err := n.Action(context.Background(), core.Document{"action": "account-remove"})
		if err != nil {
			t.Fatalf("account-remove sync err: %v", err)
		}
		op := core.Object(core.Object(res)["operation"])
		done := waitNativeOperation(t, n, core.String(op["id"]))
		if done["reason"] == kimi.CompatErrAccountSwitchBlocked {
			t.Fatalf("account-remove must not be blocked by official compatibility guard: %+v", done)
		}
		if n.modelSyncWriteBlocked() {
			t.Fatal("model sync should remain unblocked when only account switch failed")
		}
	})
}
