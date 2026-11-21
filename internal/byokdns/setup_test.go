package byokdns

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolverLifecycle(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "resolver", Domain)
	source := filepath.Join(directory, "source")
	configuration, err := ResolverConfiguration(DNSPort, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(action string) error {
		return exec.Command("/bin/sh", "-c", ResolverScript(action, source, target, false)).Run()
	}
	if err := run("prepare"); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != configuration {
		t.Fatalf("resolver was not installed: %q %v", actual, err)
	}
	if err := run("prepare"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run("prepare"); err == nil {
		t.Fatal("foreign resolver was overwritten")
	}
	if err := run("remove-dns"); err == nil {
		t.Fatal("foreign resolver was removed")
	}
	actual, _ = os.ReadFile(target)
	if string(actual) != "foreign\n" {
		t.Fatalf("foreign resolver changed: %q", actual)
	}
	if err := os.WriteFile(target, []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run("remove-dns"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolver still exists: %v", err)
	}
	if err := run("remove-dns"); err != nil {
		t.Fatal(err)
	}
}

func TestSetupRevealsFlagWithoutReplacingOtherValues(t *testing.T) {
	profile := t.TempDir()
	flag := filepath.Join(profile, "kimi-agent", "byok-availability.json")
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flag, []byte(`{"other":"retained","revealed":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	hosts := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setup := Setup{FlagFile: flag, Platform: "darwin", ResolverPath: filepath.Join(t.TempDir(), Domain),
		HostsPath: hosts, Admin: func(context.Context, string, string) error {
			calls++
			return nil
		}}
	if err := setup.Run(context.Background(), "prepare"); err != nil {
		t.Fatal(err)
	}
	value, state, err := readFlag(flag)
	if err != nil || state != "revealed" || value["other"] != "retained" || calls != 1 {
		t.Fatalf("flag = %#v state=%s calls=%d err=%v", value, state, calls, err)
	}
	if err := setup.Run(context.Background(), "remove-dns"); err != nil {
		t.Fatal(err)
	}
	_, state, _ = readFlag(flag)
	if state != "revealed" {
		t.Fatal("removing DNS must not retract the official reveal flag")
	}
}

func TestSetupMapsFailures(t *testing.T) {
	profile := t.TempDir()
	hosts := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		failure error
		want    string
	}{
		{errors.New("exit status 73"), "resolver_conflict"},
		{errors.New("User canceled (-128)"), "setup_cancelled"},
		{errors.New("OPERATION WAS CANCELED"), "setup_cancelled"},
		{errors.New("Cancelled By The User"), "setup_cancelled"},
		{context.DeadlineExceeded, "setup_timeout"},
		{errors.New("administrator name or password was incorrect (-60007)"), "setup_authorization_failed"},
		{errors.New("other"), "native_setup_failed"},
	} {
		setup := Setup{FlagFile: filepath.Join(profile, "kimi-agent", "byok-availability.json"), Platform: "darwin", HostsPath: hosts, Admin: func(context.Context, string, string) error { return test.failure }}
		if err := setup.Run(context.Background(), "prepare"); err == nil || err.Error() != test.want {
			t.Fatalf("error = %v, want %s", err, test.want)
		}
	}
}

func TestDarwinAdministratorPreservesShellOperators(t *testing.T) {
	command := darwinAdministratorCommand("printf %s resolver_conflict >&2\n")
	if !strings.Contains(command, `>&2`) || strings.Contains(command, `\u003e`) || strings.Contains(command, `\u0026`) {
		t.Fatalf("shell operators were escaped: %s", command)
	}
}

func TestProbeChecksFlagAndCachesDNS(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "byok-availability.json")
	if err := os.WriteFile(flag, []byte(`{"revealed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	probe := Probe{Lookup: func(context.Context, string) (string, error) {
		calls++
		return "10.1.2.3", nil
	}}
	status := probe.Check(context.Background(), flag)
	if !status.Revealed || !status.Intranet.Passed || status.Flag != "revealed" {
		t.Fatalf("unexpected status: %#v", status)
	}
	_ = probe.Check(context.Background(), flag)
	if calls != 1 {
		t.Fatalf("lookup count = %d", calls)
	}
	probe.Invalidate()
	_ = probe.Check(context.Background(), flag)
	if calls != 2 {
		t.Fatalf("invalidated lookup count = %d", calls)
	}
	if err := os.WriteFile(flag, []byte(`not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	status = probe.Check(context.Background(), flag)
	if status.Revealed || status.Flag != "unavailable" {
		t.Fatalf("invalid flag status: %#v", status)
	}
}

func TestProbeReportsPublicAndUnavailableDNS(t *testing.T) {
	for _, test := range []struct {
		address string
		err     error
		passed  bool
		code    string
	}{
		{address: "8.8.8.8"},
		{err: errors.New("timeout"), code: "dns_unavailable"},
	} {
		probe := Probe{Lookup: func(context.Context, string) (string, error) { return test.address, test.err }}
		result := probe.DNS(context.Background())
		if result.Passed != test.passed || result.Error != test.code {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestProbeCoalescesConcurrentLookups(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	probe := Probe{Lookup: func(context.Context, string) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return "10.1.2.3", nil
	}}
	results := make(chan ProbeResult, 8)
	for range 8 {
		go func() { results <- probe.DNS(context.Background()) }()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("lookup did not start")
	}
	close(release)
	for range 8 {
		select {
		case result := <-results:
			if !result.Passed || result.Address != "10.1.2.3" {
				t.Fatalf("result = %#v", result)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent lookup did not finish")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("lookup count = %d", calls.Load())
	}
}

func TestProbeInvalidateRejectsInflightCacheWrite(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	probe := Probe{Lookup: func(context.Context, string) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return "8.8.8.8", nil
		}
		return "10.1.2.3", nil
	}}
	first := make(chan ProbeResult, 1)
	go func() { first <- probe.DNS(context.Background()) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("lookup did not start")
	}
	probe.Invalidate()
	close(release)
	select {
	case result := <-first:
		if result.Passed {
			t.Fatal("public fixture unexpectedly passed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first lookup did not finish")
	}
	result := probe.DNS(context.Background())
	if !result.Passed || result.Address != "10.1.2.3" || calls.Load() != 2 {
		t.Fatalf("stale lookup survived invalidation: result=%#v calls=%d", result, calls.Load())
	}
}

func TestUpdateHostsIsReversibleAndConflictSafe(t *testing.T) {
	original := []byte("\xef\xbb\xbf# retained\r\n127.0.0.1 localhost\r\n")
	prepared, changed, err := UpdateHosts(original, "prepare")
	if err != nil || !changed || !strings.HasPrefix(string(prepared), "\xef\xbb\xbf"+HostsMarker+"\r\n"+HostsEntry()+"\r\n") {
		t.Fatalf("prepared = %q changed=%v err=%v", prepared, changed, err)
	}
	again, changed, err := UpdateHosts(prepared, "prepare")
	if err != nil || changed || string(again) != string(prepared) {
		t.Fatalf("idempotence failed: changed=%v err=%v", changed, err)
	}
	removed, changed, err := UpdateHosts(prepared, "remove-dns")
	if err != nil || !changed || string(removed) != string(original) {
		t.Fatalf("removed = %q changed=%v err=%v", removed, changed, err)
	}
	foreign := []byte("127.0.0.1 " + Domain + " alias\n")
	if _, _, err := UpdateHosts(foreign, "prepare"); err == nil || err.Error() != "resolver_conflict" {
		t.Fatalf("foreign hosts conflict = %v", err)
	}
	broken := []byte(HostsMarker + "\nwrong\n")
	if _, _, err := UpdateHosts(broken, "remove-dns"); err == nil || err.Error() != "resolver_conflict" {
		t.Fatalf("broken owned pair conflict = %v", err)
	}
	if _, _, err := UpdateHosts([]byte{'a', 0, 'b'}, "prepare"); err == nil || err.Error() != "unsupported_hosts_encoding" {
		t.Fatalf("NUL hosts error = %v", err)
	}
}

func TestWindowsScriptsCarryVerificationAndElevation(t *testing.T) {
	script := HostsScript("prepare", `C:\Windows\hosts`, true, `C:\Temp\report`)
	for _, marker := range []string{"ReadAllBytes", "WriteAllBytes", "resolver_conflict", "hosts_verify_failed", "ipconfig /flushdns"} {
		if !strings.Contains(script, marker) {
			t.Fatalf("hosts script lacks %s", marker)
		}
	}
	elevated := elevationCommand("Write-Output 'done'")
	if !strings.Contains(elevated, "-Verb RunAs") || !strings.Contains(elevated, "setup_cancelled") {
		t.Fatalf("unexpected elevation script: %s", elevated)
	}
}

func TestSetupReusesReadyResolverWithoutAdministrator(t *testing.T) {
	profile := t.TempDir()
	flag := filepath.Join(profile, "kimi-agent", "byok-availability.json")
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		t.Fatal(err)
	}
	resolver := filepath.Join(t.TempDir(), Domain)
	configuration, err := ResolverConfiguration(DNSPort, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolver, []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adminCalls := 0
	probeCalls := 0
	setup := Setup{FlagFile: flag, Platform: "darwin", ResolverPath: resolver, HostsPath: hosts,
		Admin: func(context.Context, string, string) error { adminCalls++; return nil },
		Probe: &Probe{Lookup: func(context.Context, string) (string, error) {
			probeCalls++
			return "10.1.2.3", nil
		}}}
	if err := setup.Run(context.Background(), "prepare"); err != nil {
		t.Fatal(err)
	}
	_, state, err := readFlag(flag)
	if err != nil || state != "revealed" || adminCalls != 0 || probeCalls != 1 {
		t.Fatalf("state=%s admin=%d probe=%d err=%v", state, adminCalls, probeCalls, err)
	}
}

func TestSetupDoesNotReuseResolverWithoutPrivateProbe(t *testing.T) {
	profile := t.TempDir()
	flag := filepath.Join(profile, "kimi-agent", "byok-availability.json")
	resolver := filepath.Join(t.TempDir(), Domain)
	configuration, err := ResolverConfiguration(DNSPort, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolver, []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adminCalls := 0
	setup := Setup{FlagFile: flag, Platform: "darwin", ResolverPath: resolver, HostsPath: hosts,
		Admin: func(context.Context, string, string) error { adminCalls++; return nil },
		Probe: &Probe{Lookup: func(context.Context, string) (string, error) { return "8.8.8.8", nil }}}
	if err := setup.Run(context.Background(), "prepare"); err != nil {
		t.Fatal(err)
	}
	if adminCalls != 1 {
		t.Fatalf("admin calls = %d", adminCalls)
	}
}
