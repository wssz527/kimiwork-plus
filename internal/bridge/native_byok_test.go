package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"kimiworkplus.local/migration/internal/byokdns"
	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
)

type fakeBYOKDNS struct {
	port     int
	err      error
	closeErr error
}

func (f *fakeBYOKDNS) Start(port int) error {
	if f.err != nil {
		return f.err
	}
	f.port = port
	return nil
}
func (f *fakeBYOKDNS) Close() error { f.port = 0; return f.closeErr }
func (f *fakeBYOKDNS) Port() int    { return f.port }

func newBYOKTestNative(t *testing.T) *Native {
	t.Helper()
	directory := t.TempDir()
	profile := filepath.Join(directory, "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	controller := &kimi.Controller{}
	if err := controller.Configure(kimi.ControllerSettings{StateDir: directory, UserDataDir: profile}); err != nil {
		t.Fatal(err)
	}
	return &Native{
		controller: controller,
		dns:        &fakeBYOKDNS{},
		probe: &byokdns.Probe{Lookup: func(context.Context, string) (string, error) {
			return "10.1.2.3", nil
		}},
		admin: func(context.Context, string, string) error { return nil },
	}
}

func waitBYOKOperation(t *testing.T, n *Native) core.Document {
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

func TestNativePrepareAndRemoveDNS(t *testing.T) {
	n := newBYOKTestNative(t)
	if _, err := n.Action(context.Background(), core.Document{"action": "prepare-native"}); err != nil {
		t.Fatal(err)
	}
	operation := waitBYOKOperation(t, n)
	if operation["state"] != "failed" || operation["reason"] != "native_bridge_unavailable" || operation["result"] == nil {
		t.Fatalf("preparation did not reach the native import boundary: %+v", operation)
	}
	state := n.byokState()
	if core.Object(state)["revealed"] != true || core.Object(core.Object(state)["intranet"])["passed"] != true || core.Object(core.Object(state)["dns"])["state"] != "ready" {
		t.Fatalf("setup state = %+v", state)
	}
	if _, err := n.Action(context.Background(), core.Document{"action": "remove-dns"}); err != nil {
		t.Fatal(err)
	}
	if operation := waitBYOKOperation(t, n); operation["state"] != "removed" {
		t.Fatalf("remove operation = %+v", operation)
	}
	if n.dns.Port() != 0 {
		t.Fatal("DNS listener remained after removal")
	}
}

func TestNativePrepareRejectsFailedPrerequisites(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Native)
		reason string
	}{
		{name: "dns", mutate: func(n *Native) { n.dns = &fakeBYOKDNS{err: byokdns.ErrUnavailable} }, reason: "native_byok_dns_unavailable"},
		{name: "public address", mutate: func(n *Native) {
			n.probe = &byokdns.Probe{Lookup: func(context.Context, string) (string, error) { return "8.8.8.8", nil }}
		}, reason: "native_byok_intranet_unavailable"},
		{name: "admin", mutate: func(n *Native) {
			n.admin = func(context.Context, string, string) error { return errors.New("exit status 73") }
		}, reason: "resolver_conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			n := newBYOKTestNative(t)
			test.mutate(n)
			if _, err := n.Action(context.Background(), core.Document{"action": "prepare-native"}); err != nil {
				t.Fatal(err)
			}
			operation := waitBYOKOperation(t, n)
			if operation["state"] != "failed" || operation["reason"] != test.reason {
				t.Fatalf("operation = %+v", operation)
			}
		})
	}
}

func TestNativeStateDoesNotHoldGlobalLockDuringDNSProbe(t *testing.T) {
	n := newBYOKTestNative(t)
	settings := n.controller.Settings()
	stateDir := filepath.Join(settings.StateDir, "state")
	store, err := core.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := newNativePolicies(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	n.service = &Service{Store: store, StateDir: stateDir, Keys: CacheKeys(testKeys{})}
	n.policies = policies
	if err := n.initEnhancements(); err != nil {
		t.Fatal(err)
	}
	defer n.web.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	n.probe = &byokdns.Probe{Lookup: func(context.Context, string) (string, error) {
		close(started)
		<-release
		return "10.1.2.3", nil
	}}
	states := make(chan core.Document, 1)
	go func() { states <- n.State() }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("DNS probe did not start")
	}
	unlocked := make(chan struct{})
	go func() {
		n.mu.Lock()
		n.mu.Unlock()
		close(unlocked)
	}()
	select {
	case <-unlocked:
	case <-time.After(200 * time.Millisecond):
		close(release)
		<-states
		t.Fatal("state poll held the native lock during DNS lookup")
	}
	close(release)
	select {
	case state := <-states:
		if core.Object(core.Object(state["setup"])["intranet"])["passed"] != true {
			t.Fatalf("state = %+v", state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("state poll did not finish")
	}
}

func TestNativeRemoveDNSReportsListenerCloseFailure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("DNS listener close is platform-specific")
	}
	n := newBYOKTestNative(t)
	n.dns = &fakeBYOKDNS{closeErr: errors.New("close failed")}
	if err := n.removeBYOK(context.Background()); err == nil || err.Error() != "native_byok_dns_stop_failed" {
		t.Fatalf("remove error = %v", err)
	}
}

func TestNativeBYOKFlagFollowsAuthorizedLoginProfile(t *testing.T) {
	n := newBYOKTestNative(t)
	settings := n.controller.Settings()
	login := filepath.Join(t.TempDir(), "login")
	if err := os.MkdirAll(filepath.Join(login, "kimi-agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(login, "kimi-agent", "byok-availability.json"), []byte(`{"revealed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings.ProfileMode = kimi.ProfileModeExistingAuthorized
	settings.LoginDataDir = login
	if err := n.controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	if state := n.byokState(); state["revealed"] != true {
		t.Fatalf("authorized profile flag was not read: %+v", state)
	}
}
