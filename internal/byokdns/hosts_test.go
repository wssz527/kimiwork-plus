package byokdns

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// requireShell skips the script execution tests on hosts that cannot run the
// POSIX scripts at all.
func requireShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the setup scripts are POSIX shell")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
}

func runScript(t *testing.T, script string) error {
	t.Helper()
	return exec.Command("/bin/sh", "-c", script).Run()
}

func scriptExitCode(t *testing.T, err error) int {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("not an exit error: %v", err)
	}
	return exit.ExitCode()
}

// hostsFixture lays out the files DarwinHostsScript works on: the hosts target,
// its before/after images, and a resolver source/target pair.
type hostsFixture struct {
	hosts    string
	before   string
	after    string
	resolver string
	source   string
}

func newHostsFixture(t *testing.T, action, hostsContent string) *hostsFixture {
	t.Helper()
	directory := t.TempDir()
	fixture := &hostsFixture{
		hosts:    filepath.Join(directory, "hosts"),
		before:   filepath.Join(directory, "hosts-before"),
		after:    filepath.Join(directory, "hosts-after"),
		resolver: filepath.Join(directory, "resolver", Domain),
		source:   filepath.Join(directory, "resolver-source"),
	}
	next, _, err := UpdateHosts([]byte(hostsContent), action)
	if err != nil {
		t.Fatalf("fixture hosts: %v", err)
	}
	configuration, err := ResolverConfiguration(DNSPort, fixture.resolver)
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		fixture.hosts: hostsContent, fixture.before: hostsContent,
		fixture.after: string(next), fixture.source: configuration,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func (f *hostsFixture) ownResolver(t *testing.T) {
	t.Helper()
	content, err := os.ReadFile(f.source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.resolver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.resolver, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *hostsFixture) foreignResolver(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.resolver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.resolver, []byte("nameserver 8.8.8.8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *hostsFixture) script(action string, flush bool) string {
	return DarwinHostsScript(action, f.before, f.after, f.hosts, f.source, f.resolver, flush)
}

func (f *hostsFixture) read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func TestHasManagedHostsEntry(t *testing.T) {
	managed := HostsMarker + "\n" + HostsEntry() + "\n"
	for _, test := range []struct {
		name    string
		content string
		want    bool
	}{
		{name: "marker pair", content: managed, want: true},
		{name: "plain exact", content: HostsEntry() + "\n", want: true},
		{name: "case insensitive domain", content: Address + " DeV.MsH.TeAm\n", want: true},
		{name: "with other hosts", content: "127.0.0.1 localhost\n" + managed, want: true},
		{name: "bom", content: "\xef\xbb\xbf" + managed, want: true},
		{name: "unrelated", content: "127.0.0.1 localhost\n", want: false},
		{name: "empty", content: "", want: false},
		{name: "conflicting alias", content: "127.0.0.1 " + Domain + " alias\n", want: false},
		{name: "conflicting address", content: "10.0.0.1 " + Domain + "\n", want: false},
		{name: "marker with wrong pair", content: HostsMarker + "\nwrong\n", want: false},
		{name: "nul bytes", content: "a\x00b\n", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hosts")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := HasManagedHostsEntry(path); got != test.want {
				t.Fatalf("HasManagedHostsEntry(%q) = %v, want %v", test.content, got, test.want)
			}
		})
	}
}

func TestHasManagedHostsEntryRefusesMissingSymlinkAndDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "hosts")
	if HasManagedHostsEntry(missing) {
		t.Fatal("a missing file was reported as a managed entry")
	}
	directory := t.TempDir()
	if HasManagedHostsEntry(directory) {
		t.Fatal("a directory was reported as a managed entry")
	}
	real := filepath.Join(directory, "hosts-real")
	if err := os.WriteFile(real, []byte(HostsEntry()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "hosts")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if HasManagedHostsEntry(link) {
		t.Fatal("a symlinked hosts file was reported as a managed entry")
	}
}

func TestDarwinHostsScriptPrepareInstallsEntryAndRemovesOwnResolver(t *testing.T) {
	requireShell(t)
	original := "127.0.0.1 localhost\n::1 localhost\n"
	fixture := newHostsFixture(t, "prepare", original)
	fixture.ownResolver(t)
	if err := runScript(t, fixture.script("prepare", false)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := fixture.read(t, fixture.hosts); got != fixture.read(t, fixture.after) {
		t.Fatalf("hosts = %q, want %q", got, fixture.read(t, fixture.after))
	}
	if !HasManagedHostsEntry(fixture.hosts) {
		t.Fatal("the hosts entry was not installed")
	}
	if _, err := os.Lstat(fixture.resolver); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("our own resolver survived: %v", err)
	}
	if _, err := os.Lstat(fixture.hosts + ".kwp.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the temporary hosts file was left behind: %v", err)
	}
}

func TestDarwinHostsScriptPrepareIsIdempotent(t *testing.T) {
	requireShell(t)
	managed := HostsMarker + "\n" + HostsEntry() + "\n"
	fixture := newHostsFixture(t, "prepare", managed)
	for range 2 {
		if err := runScript(t, fixture.script("prepare", false)); err != nil {
			t.Fatalf("prepare: %v", err)
		}
	}
	if got := fixture.read(t, fixture.hosts); got != managed {
		t.Fatalf("hosts changed on a second prepare: %q", got)
	}
}

func TestDarwinHostsScriptPrepareRefusesForeignResolver(t *testing.T) {
	requireShell(t)
	original := "127.0.0.1 localhost\n"
	fixture := newHostsFixture(t, "prepare", original)
	fixture.foreignResolver(t)
	err := runScript(t, fixture.script("prepare", false))
	if code := scriptExitCode(t, err); code != 73 {
		t.Fatalf("exit code = %d, want 73", code)
	}
	if got := fixture.read(t, fixture.hosts); got != original {
		t.Fatalf("the foreign resolver still changed hosts: %q", got)
	}
	if got := fixture.read(t, fixture.resolver); got != "nameserver 8.8.8.8\n" {
		t.Fatalf("the foreign resolver changed: %q", got)
	}
}

func TestDarwinHostsScriptRefusesConcurrentHostsEdit(t *testing.T) {
	requireShell(t)
	original := "127.0.0.1 localhost\n"
	fixture := newHostsFixture(t, "prepare", original)
	fixture.ownResolver(t)
	if err := os.WriteFile(fixture.hosts, []byte(original+"10.0.0.1 other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runScript(t, fixture.script("prepare", false))
	if code := scriptExitCode(t, err); code != 73 {
		t.Fatalf("exit code = %d, want 73", code)
	}
	if got := fixture.read(t, fixture.hosts); got != original+"10.0.0.1 other\n" {
		t.Fatalf("the concurrent edit was overwritten: %q", got)
	}
	if got := fixture.read(t, fixture.resolver); got == "" {
		t.Fatal("the resolver was removed despite the conflict")
	}
}

func TestDarwinHostsScriptRefusesSymlinkedHosts(t *testing.T) {
	requireShell(t)
	original := "127.0.0.1 localhost\n"
	fixture := newHostsFixture(t, "prepare", original)
	real := filepath.Join(t.TempDir(), "real-hosts")
	if err := os.WriteFile(real, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.hosts); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, fixture.hosts); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := runScript(t, fixture.script("prepare", false)); err == nil {
		t.Fatal("a symlinked hosts target was accepted")
	}
	if got, err := os.ReadFile(real); err != nil || string(got) != original {
		t.Fatalf("the symlink target changed: %q %v", got, err)
	}
}

func TestDarwinHostsScriptRemoveDNSRemovesEntryAndOwnResolver(t *testing.T) {
	requireShell(t)
	original := "127.0.0.1 localhost\n"
	managed, _, err := UpdateHosts([]byte(original), "prepare")
	if err != nil {
		t.Fatal(err)
	}
	fixture := newHostsFixture(t, "remove-dns", string(managed))
	fixture.ownResolver(t)
	if err := runScript(t, fixture.script("remove-dns", false)); err != nil {
		t.Fatalf("remove-dns: %v", err)
	}
	if got := fixture.read(t, fixture.hosts); got != original {
		t.Fatalf("hosts = %q, want %q", got, original)
	}
	if _, err := os.Lstat(fixture.resolver); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the resolver survived: %v", err)
	}
}

func TestDarwinHostsScriptRemoveDNSRefusesForeignResolver(t *testing.T) {
	requireShell(t)
	managed := HostsMarker + "\n" + HostsEntry() + "\n"
	fixture := newHostsFixture(t, "remove-dns", managed)
	fixture.foreignResolver(t)
	err := runScript(t, fixture.script("remove-dns", false))
	if code := scriptExitCode(t, err); code != 73 {
		t.Fatalf("exit code = %d, want 73", code)
	}
	if got := fixture.read(t, fixture.hosts); got != managed {
		t.Fatalf("the managed hosts entry was removed despite the conflict: %q", got)
	}
	if got := fixture.read(t, fixture.resolver); got != "nameserver 8.8.8.8\n" {
		t.Fatalf("the foreign resolver changed: %q", got)
	}
}

func TestDarwinHostsScriptWithoutResolverInstallsOnlyHosts(t *testing.T) {
	requireShell(t)
	original := "127.0.0.1 localhost\n"
	fixture := newHostsFixture(t, "prepare", original)
	if err := runScript(t, fixture.script("prepare", false)); err != nil {
		t.Fatalf("prepare without a resolver: %v", err)
	}
	if !HasManagedHostsEntry(fixture.hosts) {
		t.Fatal("the hosts entry was not installed")
	}
}

// The elevated script is the only thing that flushes the cache and writes
// atomically; both must be visible in the returned text.
func TestDarwinHostsScriptFlushAndAtomicWrite(t *testing.T) {
	script := DarwinHostsScript("prepare", "/tmp/before", "/tmp/after", "/etc/hosts",
		"/tmp/source", "/etc/resolver/"+Domain, true)
	for _, marker := range []string{
		"/usr/bin/dscacheutil -flushcache",
		"/usr/bin/killall -HUP mDNSResponder",
		"/usr/bin/install -m 644",
		"/bin/mv -f",
		"resolver_conflict",
		"exit 73",
	} {
		if !strings.Contains(script, marker) {
			t.Fatalf("the script lacks %q:\n%s", marker, script)
		}
	}
	withoutFlush := DarwinHostsScript("prepare", "/tmp/before", "/tmp/after", "/etc/hosts",
		"/tmp/source", "/etc/resolver/"+Domain, false)
	if strings.Contains(withoutFlush, "dscacheutil") {
		t.Fatal("flush was requested and the script skipped it")
	}
	if DarwinHostsScript("invalid", "/a", "/b", "/c", "/d", "/e", false) != "" {
		t.Fatal("an invalid action produced a script")
	}
}

// adminRecorder stands in for the administrator prompt. execute makes it run the
// script for real through /bin/sh, minus the cache flush, which needs the root
// the test does not have.
type adminRecorder struct {
	execute bool
	fail    error
	calls   int
	scripts []string
}

func (a *adminRecorder) run(_ context.Context, script, _ string) error {
	a.calls++
	a.scripts = append(a.scripts, script)
	if a.fail != nil {
		return a.fail
	}
	if !a.execute {
		return nil
	}
	stripped := script
	for _, line := range []string{"/usr/bin/dscacheutil -flushcache\n", "/usr/bin/killall -HUP mDNSResponder\n"} {
		stripped = strings.ReplaceAll(stripped, line, "")
	}
	output, err := exec.Command("/bin/sh", "-c", stripped).CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(output)) + " " + err.Error())
	}
	return nil
}

func (a *adminRecorder) administrator() Administrator { return a.run }

func darwinSetupFixture(t *testing.T) (*Setup, string, string, string) {
	t.Helper()
	profile := t.TempDir()
	flag := filepath.Join(profile, "kimi-agent", "byok-availability.json")
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flag, []byte(`{"revealed":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := filepath.Join(t.TempDir(), "resolver", Domain)
	setup := &Setup{
		FlagFile:     flag,
		Platform:     "darwin",
		HostsPath:    hosts,
		ResolverPath: resolver,
		Probe: &Probe{Lookup: func(context.Context, string) (string, error) {
			return Address, nil
		}},
	}
	return setup, flag, hosts, resolver
}

func writeOwnResolver(t *testing.T, setup *Setup, resolver string) {
	t.Helper()
	configuration, err := ResolverConfiguration(DNSPort, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(resolver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolver, []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeManagedHosts(t *testing.T, hosts string) {
	t.Helper()
	managed, _, err := UpdateHosts([]byte("127.0.0.1 localhost\n"), "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hosts, managed, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSetupDarwinRequireStaticHostsUpgradesLegacyResolver(t *testing.T) {
	requireShell(t)
	setup, flag, hosts, resolver := darwinSetupFixture(t)
	setup.RequireStaticHosts = true
	writeOwnResolver(t, setup, resolver)
	recorder := &adminRecorder{execute: true}
	setup.Admin = recorder.administrator()
	if err := setup.Run(context.Background(), "prepare"); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 1 {
		t.Fatalf("admin calls = %d", recorder.calls)
	}
	if !HasManagedHostsEntry(hosts) {
		t.Fatal("the static hosts entry was not installed")
	}
	if _, err := os.Lstat(resolver); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the legacy resolver survived: %v", err)
	}
	if _, state, err := readFlag(flag); err != nil || state != "revealed" {
		t.Fatalf("flag state = %s err=%v", state, err)
	}
}

func TestSetupDarwinGatewayModeInstallsOnlyResolver(t *testing.T) {
	requireShell(t)
	setup, flag, hosts, resolver := darwinSetupFixture(t)
	recorder := &adminRecorder{execute: true}
	setup.Admin = recorder.administrator()
	if err := setup.Run(context.Background(), "prepare"); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 1 {
		t.Fatalf("admin calls = %d", recorder.calls)
	}
	if got, err := os.ReadFile(hosts); err != nil || string(got) != "127.0.0.1 localhost\n" {
		t.Fatalf("gateway mode changed hosts: %q %v", got, err)
	}
	configuration, err := ResolverConfiguration(DNSPort, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(resolver); err != nil || string(got) != configuration {
		t.Fatalf("gateway resolver = %q %v", got, err)
	}
	if _, state, err := readFlag(flag); err != nil || state != "revealed" {
		t.Fatalf("flag state = %s err=%v", state, err)
	}
}

func TestSetupDarwinGatewayModeReusesResolverWithoutAdmin(t *testing.T) {
	setup, flag, hosts, resolver := darwinSetupFixture(t)
	writeOwnResolver(t, setup, resolver)
	recorder := &adminRecorder{}
	setup.Admin = recorder.administrator()
	if err := setup.Run(context.Background(), "prepare"); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 0 {
		t.Fatalf("admin calls = %d", recorder.calls)
	}
	if _, state, err := readFlag(flag); err != nil || state != "revealed" {
		t.Fatalf("flag state = %s err=%v", state, err)
	}
	if got, err := os.ReadFile(hosts); err != nil || string(got) != "127.0.0.1 localhost\n" {
		t.Fatalf("gateway mode changed hosts: %q %v", got, err)
	}
}

func TestSetupDarwinReusesManagedHostsWithoutAdmin(t *testing.T) {
	for _, static := range []bool{true, false} {
		setup, flag, hosts, resolver := darwinSetupFixture(t)
		setup.RequireStaticHosts = static
		writeManagedHosts(t, hosts)
		recorder := &adminRecorder{}
		setup.Admin = recorder.administrator()
		if err := setup.Run(context.Background(), "prepare"); err != nil {
			t.Fatalf("static=%v: %v", static, err)
		}
		if recorder.calls != 0 {
			t.Fatalf("static=%v admin calls = %d", static, recorder.calls)
		}
		if _, state, err := readFlag(flag); err != nil || state != "revealed" {
			t.Fatalf("static=%v flag state = %s err=%v", static, state, err)
		}
		if _, err := os.Lstat(resolver); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("static=%v resolver was created", static)
		}
	}
}

func TestSetupDarwinStaticModeRemovesLegacyResolverWithReadyHosts(t *testing.T) {
	requireShell(t)
	setup, _, hosts, resolver := darwinSetupFixture(t)
	setup.RequireStaticHosts = true
	writeManagedHosts(t, hosts)
	writeOwnResolver(t, setup, resolver)
	before, err := os.ReadFile(hosts)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &adminRecorder{execute: true}
	setup.Admin = recorder.administrator()
	if err := setup.Run(context.Background(), "prepare"); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 1 {
		t.Fatalf("admin calls = %d", recorder.calls)
	}
	after, err := os.ReadFile(hosts)
	if err != nil || string(after) != string(before) {
		t.Fatalf("hosts changed while cleaning the resolver: %q %v", after, err)
	}
	if _, err := os.Lstat(resolver); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the legacy resolver survived: %v", err)
	}
}

func TestSetupDarwinPrepareRefusesForeignResolver(t *testing.T) {
	requireShell(t)
	setup, flag, hosts, resolver := darwinSetupFixture(t)
	setup.RequireStaticHosts = true
	if err := os.MkdirAll(filepath.Dir(resolver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolver, []byte("nameserver 8.8.8.8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := &adminRecorder{execute: true}
	setup.Admin = recorder.administrator()
	err := setup.Run(context.Background(), "prepare")
	if err == nil || err.Error() != "resolver_conflict" {
		t.Fatalf("error = %v", err)
	}
	if got, readErr := os.ReadFile(hosts); readErr != nil || string(got) != "127.0.0.1 localhost\n" {
		t.Fatalf("hosts changed despite the conflict: %q %v", got, readErr)
	}
	if got, readErr := os.ReadFile(resolver); readErr != nil || string(got) != "nameserver 8.8.8.8\n" {
		t.Fatalf("the foreign resolver changed: %q %v", got, readErr)
	}
	_, state, _ := readFlag(flag)
	if state == "revealed" {
		t.Fatal("the flag was revealed despite the conflict")
	}
}

func TestSetupDarwinPrepareRefusesForeignHostsEntry(t *testing.T) {
	setup, _, hosts, _ := darwinSetupFixture(t)
	setup.RequireStaticHosts = true
	if err := os.WriteFile(hosts, []byte("127.0.0.1 "+Domain+" alias\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := &adminRecorder{}
	setup.Admin = recorder.administrator()
	err := setup.Run(context.Background(), "prepare")
	if err == nil || err.Error() != "resolver_conflict" {
		t.Fatalf("error = %v", err)
	}
	if recorder.calls != 0 {
		t.Fatalf("admin was prompted despite the conflict: %d", recorder.calls)
	}
}

func TestSetupDarwinRemoveDNSRemovesHostsAndOwnResolver(t *testing.T) {
	requireShell(t)
	setup, _, hosts, resolver := darwinSetupFixture(t)
	writeManagedHosts(t, hosts)
	writeOwnResolver(t, setup, resolver)
	recorder := &adminRecorder{execute: true}
	setup.Admin = recorder.administrator()
	if err := setup.Run(context.Background(), "remove-dns"); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 1 {
		t.Fatalf("admin calls = %d", recorder.calls)
	}
	if got, err := os.ReadFile(hosts); err != nil || string(got) != "127.0.0.1 localhost\n" {
		t.Fatalf("hosts = %q err=%v", got, err)
	}
	if _, err := os.Lstat(resolver); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the resolver survived: %v", err)
	}
}

func TestSetupDarwinRemoveDNSWithoutAnythingSkipsAdmin(t *testing.T) {
	setup, _, hosts, resolver := darwinSetupFixture(t)
	recorder := &adminRecorder{}
	setup.Admin = recorder.administrator()
	if err := setup.Run(context.Background(), "remove-dns"); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 0 {
		t.Fatalf("admin calls = %d", recorder.calls)
	}
	if got, err := os.ReadFile(hosts); err != nil || string(got) != "127.0.0.1 localhost\n" {
		t.Fatalf("a clean removal changed hosts: %q %v", got, err)
	}
	if _, err := os.Lstat(resolver); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a clean removal created a resolver")
	}
}

func TestSetupDarwinRemoveDNSRefusesForeignResolver(t *testing.T) {
	requireShell(t)
	setup, _, hosts, resolver := darwinSetupFixture(t)
	writeManagedHosts(t, hosts)
	if err := os.MkdirAll(filepath.Dir(resolver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolver, []byte("nameserver 8.8.8.8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := &adminRecorder{execute: true}
	setup.Admin = recorder.administrator()
	err := setup.Run(context.Background(), "remove-dns")
	if err == nil || err.Error() != "resolver_conflict" {
		t.Fatalf("error = %v", err)
	}
	if !HasManagedHostsEntry(hosts) {
		t.Fatal("the managed hosts entry was removed despite the conflict")
	}
	if got, readErr := os.ReadFile(resolver); readErr != nil || string(got) != "nameserver 8.8.8.8\n" {
		t.Fatalf("the foreign resolver changed: %q %v", got, readErr)
	}
}
