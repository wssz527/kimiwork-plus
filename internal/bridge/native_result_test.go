package bridge

import (
	"github.com/wssz527/kimiwork-plus/internal/kimi"
	"os"
	"path/filepath"
	"testing"
)

func TestRefreshKimiInstallationAfterInitialMissing(t *testing.T) {
	install := kimi.Installation{Application: `C:\Apps\Kimi\Kimi.exe`, Archive: `C:\Apps\Kimi\resources\app.asar`}
	settings := refreshKimiInstallation(kimi.ControllerSettings{}, func() []kimi.Installation { return []kimi.Installation{install} })
	if settings.Installation != install {
		t.Fatalf("installation was not refreshed: %+v", settings.Installation)
	}
	calls := 0
	settings = refreshKimiInstallation(settings, func() []kimi.Installation { calls++; return nil })
	if calls != 0 || settings.Installation != install {
		t.Fatal("an existing installation was rediscovered")
	}
}

func TestNativeDesktopStateExposesInstallationReason(t *testing.T) {
	desktop := nativeDesktopState(kimi.ControllerStatus{Native: &kimi.NativeState{Reason: "not_installed"}})
	if desktop["reason"] != "not_installed" {
		t.Fatalf("installation reason = %v", desktop["reason"])
	}
	desktop = nativeDesktopState(kimi.ControllerStatus{Reason: "kimi_application_missing", Native: &kimi.NativeState{Reason: "not_installed"}})
	if desktop["reason"] != "kimi_application_missing" {
		t.Fatalf("controller reason was replaced: %v", desktop["reason"])
	}
}

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
