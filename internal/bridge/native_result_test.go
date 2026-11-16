package bridge

import (
	"kimiworkplus.local/migration/internal/kimi"
	"os"
	"path/filepath"
	"testing"
)

func TestRejectedImportIsNotPrepared(t *testing.T) {
	for _, reason := range []string{"native_bridge_waiting_work", "native_import_account_unverified", ""} {
		if err := importResultError(kimi.ImportResult{Reason: reason}); err == nil {
			t.Fatal("rejected import accepted")
		}
	}
	if err := importResultError(kimi.ImportResult{OK: true}); err == nil {
		t.Fatal("accepted without readback treated as applied")
	}
	if err := importResultError(kimi.ImportResult{OK: true, Applied: true}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingWebKeyDoesNotBlockDaemonStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "web-tools-settings.json"), []byte(`{"schemaVersion":1,"provider":"tavily"}`), 0600); err != nil {
		t.Fatal(err)
	}
	policies, err := newIsolatedTestPolicies(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := &Native{service: &Service{StateDir: dir, Keys: CacheKeys(testKeys{})}, policies: policies}
	if err := n.initEnhancements(); err != nil {
		t.Fatal(err)
	}
	defer n.web.Close()
	state := n.web.State(nil)
	if state.Error != "web_adapter_key_missing" || state.KeySet {
		t.Fatalf("unexpected state: %+v", state)
	}
}
