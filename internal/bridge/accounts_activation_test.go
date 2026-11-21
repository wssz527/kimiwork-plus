package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

// activationNative builds a Native whose account manager is the fixture's, so a
// test can drive the real action switch instead of poking the fields the action
// sets.
func activationNative(t *testing.T, f *accountManagerFixture) *Native {
	t.Helper()
	dir := t.TempDir()
	n := &Native{accounts: f.manager,
		service: &Service{StateDir: dir, Keys: CacheKeys(testKeys{})}}
	n.quota = newNativeQuota(n.service, func() string { return "" }, func() string { return "" })
	n.accountActivation = core.Document{"state": "restored", "requiresLogin": true, "rolledBack": false,
		"reason": "account_credentials_rejected"}
	return n
}

func waitActivationOperation(t *testing.T, n *Native) core.Document {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		n.mu.Lock()
		operation := n.operation
		if operation != nil && operation["state"] != "running" {
			snapshot := core.Document{}
			for key, value := range operation {
				snapshot[key] = value
			}
			n.mu.Unlock()
			return snapshot
		}
		n.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("operation did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAccountRemoveClearsStaleActivation(t *testing.T) {
	f := newAccountManagerFixture(t)
	f.active, f.token = "second", "second-token"
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	// The saved account must not be the current one, or the manager refuses the
	// removal before anything is cleared.
	f.active, f.token = "first", "first-token"
	if err := f.action(t, "account-capture", ""); err != nil {
		t.Fatal(err)
	}
	n := activationNative(t, f)
	if n.accountsState()["activation"].(core.Document)["requiresLogin"] != true {
		t.Fatal("the fixture did not expose the activation the test needs")
	}
	if _, err := n.Action(context.Background(), core.Document{"action": "account-remove", "accountId": accountID("second")}); err != nil {
		t.Fatal(err)
	}
	operation := waitActivationOperation(t, n)
	if operation["state"] == "failed" {
		t.Fatalf("remove failed: %+v", operation)
	}
	rows, _, err := f.vault.summaries()
	if err != nil || len(rows) != 1 {
		t.Fatalf("account was not removed: %+v %v", rows, err)
	}
	if activation := n.accountsState()["activation"].(core.Document); len(activation) != 0 {
		t.Fatalf("a removed account kept its activation record: %+v", activation)
	}
}

func TestAccountCaptureClearsStaleActivation(t *testing.T) {
	f := newAccountManagerFixture(t)
	n := activationNative(t, f)
	if _, err := n.Action(context.Background(), core.Document{"action": "account-capture"}); err != nil {
		t.Fatal(err)
	}
	if operation := waitActivationOperation(t, n); operation["state"] == "failed" {
		t.Fatalf("capture failed: %+v", operation)
	}
	if activation := n.accountsState()["activation"].(core.Document); len(activation) != 0 {
		t.Fatalf("a re-captured account kept its activation record: %+v", activation)
	}
}

func TestAccountCompleteAddClearsStaleActivation(t *testing.T) {
	f := newAccountManagerFixture(t)
	if err := f.action(t, "account-begin-add", ""); err != nil {
		t.Fatal(err)
	}
	f.active, f.token = "second", "second-token"
	n := activationNative(t, f)
	if _, err := n.Action(context.Background(), core.Document{"action": "account-complete-add"}); err != nil {
		t.Fatal(err)
	}
	if operation := waitActivationOperation(t, n); operation["state"] == "failed" {
		t.Fatalf("complete-add failed: %+v", operation)
	}
	if activation := n.accountsState()["activation"].(core.Document); len(activation) != 0 {
		t.Fatalf("a new account kept a stale activation record: %+v", activation)
	}
}

func TestFailedAccountActionKeepsActivation(t *testing.T) {
	f := newAccountManagerFixture(t)
	n := activationNative(t, f)
	// Adding cannot complete without a pending login, so the action fails and the
	// activation still describes the state the user has to resolve.
	if _, err := n.Action(context.Background(), core.Document{"action": "account-complete-add"}); err != nil {
		t.Fatal(err)
	}
	if operation := waitActivationOperation(t, n); operation["state"] != "failed" || operation["reason"] != "account_login_not_pending" {
		t.Fatalf("operation = %+v", operation)
	}
	if activation := n.accountsState()["activation"].(core.Document); len(activation) == 0 {
		t.Fatal("a failed action discarded the activation record")
	}
}

func TestAppliedActivationSurvivesACapture(t *testing.T) {
	f := newAccountManagerFixture(t)
	n := &Native{accounts: f.manager}
	n.accountActivation = core.Document{"state": "applied", "verified": true, "hot": true}
	n.clearStaleAccountActivation()
	activation := n.accountsState()["activation"].(core.Document)
	if activation["state"] != "applied" || activation["verified"] != true {
		t.Fatalf("a working switch was cleared: %+v", activation)
	}
}

func TestClearStaleActivationCoversEachFailureShape(t *testing.T) {
	for name, activation := range map[string]core.Document{
		"restored":        {"state": "restored"},
		"needs-login":     {"state": "needs-login"},
		"failed":          {"state": "failed"},
		"rolled-back":     {"state": "applied", "rolledBack": true},
		"requires-login":  {"state": "applied", "requiresLogin": true},
		"no-state-rolled": {"rolledBack": true},
	} {
		n := &Native{accounts: newAccountManagerFixture(t).manager, accountActivation: activation}
		n.clearStaleAccountActivation()
		n.mu.Lock()
		remaining := n.accountActivation
		n.mu.Unlock()
		if remaining != nil {
			t.Fatalf("%s was kept: %+v", name, remaining)
		}
	}
	for name, activation := range map[string]core.Document{
		"applied":  {"state": "applied"},
		"verified": {"state": "applied", "verified": true, "hot": true},
	} {
		n := &Native{accounts: newAccountManagerFixture(t).manager, accountActivation: activation}
		n.clearStaleAccountActivation()
		n.mu.Lock()
		remaining := n.accountActivation
		n.mu.Unlock()
		if remaining == nil {
			t.Fatalf("%s was cleared", name)
		}
	}
}
