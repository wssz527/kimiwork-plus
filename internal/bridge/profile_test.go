package bridge

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
)

func TestSyncWorkHomeSwitchesPoliciesAndMCP(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	profileA := filepath.Join(dir, "profile-a")
	profileB := filepath.Join(dir, "profile-b")
	for _, profile := range []string{profileA, profileB} {
		if err := os.MkdirAll(profile, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	controller := &kimi.Controller{}
	settings := kimi.ControllerSettings{StateDir: dir, UserDataDir: profileA, ProfileMode: kimi.ProfileModeIsolated}
	if err := controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	homeA, err := controller.WorkHome()
	if err != nil {
		t.Fatal(err)
	}
	policies, err := newNativePolicies(dir, homeA)
	if err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := &Native{service: &Service{StateDir: dir, Store: store, Keys: CacheKeys(testKeys{})}, controller: controller, policies: policies}
	if err := n.initEnhancements(); err != nil {
		t.Fatal(err)
	}
	defer n.web.Close()
	defer n.mcp.Close(context.Background())

	settings.UserDataDir = profileB
	if err := controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	if err := n.syncWorkHome(); err != nil {
		t.Fatal(err)
	}
	homeB, err := controller.WorkHome()
	if err != nil {
		t.Fatal(err)
	}
	kernelHome := filepath.Join(homeB, "runtime", "kimi-code", "home")
	if policies.home != homeB || policies.subagents.HomeDir() != kernelHome {
		t.Fatalf("policy home = %q, subagent home = %q, want %q and %q", policies.home, policies.subagents.HomeDir(), homeB, kernelHome)
	}
	if _, err := n.toolsAction(context.Background(), core.Document{"action": "add", "server": core.Document{
		"name": "profile-switch", "transport": "stdio", "command": "/usr/bin/false",
	}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(homeB, "runtime", "kimi-code", "home", "mcp.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("MCP was not written to the switched home: %v", err)
	}

	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for range 30 {
			n.toolsState(context.Background())
			n.reconcile(nil)
		}
	}()
	for i := range 30 {
		settings.UserDataDir = []string{profileA, profileB}[i%2]
		if err := controller.Configure(settings); err != nil {
			t.Error(err)
			break
		}
		if err := n.syncWorkHome(); err != nil {
			t.Error(err)
			break
		}
	}
	readers.Wait()

	settings.KimiHome = dir
	if err := controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	if err := n.syncWorkHome(); err == nil {
		t.Fatal("an outside home was accepted")
	}
	n.mu.Lock()
	reason := n.profileReason
	n.mu.Unlock()
	if reason != "kimi_home_outside_profile" {
		t.Fatalf("profile diagnostic = %q", reason)
	}
}
