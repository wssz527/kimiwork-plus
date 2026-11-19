package bridge

import (
	"github.com/wssz527/kimiwork-plus/internal/core"
	"os"
	"testing"
)

func TestDeferredConfigReconcilesWhenCreated(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newIsolatedTestPolicies(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{StateDir: dir, Store: store}
	_, err = s.saveNativeContext(p, core.Document{"policy": core.Document{"mode": "custom", "triggerRatio": 0.85, "reservedTokens": float64(0)}})
	if err != nil {
		t.Fatal(err)
	}
	n := &Native{service: s, policies: p}
	if n.reconcile(nil) != nil {
		t.Fatal("missing file claimed present")
	}
	if err = os.MkdirAll(p.subagents.HomeDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.subagents.ConfigPath(), []byte("# newly created official configuration\n"), 0600); err != nil {
		t.Fatal(err)
	}
	observed := n.reconcile(nil)
	if observed == nil || p.context.State().State != "file-applied" {
		t.Fatal(p.context.State())
	}
	if next := n.reconcile(observed); next != observed {
		t.Fatal("unchanged file caused another reconcile")
	}
	if err = os.WriteFile(p.subagents.ConfigPath(), []byte("[loop_control]\ncompaction_trigger_ratio = 0.5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	n.reconcile(observed)
	raw, err := os.ReadFile(p.subagents.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[loop_control]\ncompaction_trigger_ratio = 0.5\n" {
		t.Fatalf("foreign field overwritten: %s", raw)
	}
}
