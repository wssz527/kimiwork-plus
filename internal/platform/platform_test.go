package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAcquireLockIsExclusiveAcrossHandles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Held() {
		t.Fatal("first lock not held")
	}
	// A second acquisition from this process must also fail: the OS lock is per
	// open file description, and a second handle is a second description.
	if second, err := AcquireLock(dir); err == nil {
		second.Release()
		t.Fatal("second lock was granted while the first was held")
	} else if err != ErrLocked {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	third, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("lock not reusable after release: %v", err)
	}
	third.Release()
	// Release is idempotent and safe on a nil receiver, so a deferred call on an
	// error path cannot panic.
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLockHeldByAnotherProbe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	held, err := LockHeldByAnother(dir)
	if err != nil || held {
		t.Fatalf("fresh directory reported held=%v err=%v", held, err)
	}
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	held, err = LockHeldByAnother(dir)
	if err != nil || !held {
		t.Fatalf("held directory reported held=%v err=%v", held, err)
	}
	lock.Release()
}

func TestEnsurePrivateDirRefusesASymlink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := EnsurePrivateDir(link); err == nil {
		t.Fatal("a symlinked state directory was accepted")
	}
}

func TestEnsurePrivateDirTightensPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not apply")
	}
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("directory still group/other accessible: %v", info.Mode().Perm())
	}
}

func TestServiceDescriptionNeverEnablesItself(t *testing.T) {
	// The paths are built the way the target platform builds them, so the absolute
	// path check is exercised on the platform's own semantics rather than on a
	// separator the host happens to use.
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	options := ServiceOptions{Binary: filepath.Join(root, "usr", "local", "bin", "daemon"),
		StateDir: filepath.Join(root, "tmp", "state")}
	description, err := DescribeService(options)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		if err != nil {
			t.Fatal(err)
		}
	} else if err == nil {
		t.Fatal("unsupported platform returned a service description")
	} else {
		return
	}
	if len(description.Install) == 0 || len(description.Uninstall) == 0 {
		t.Fatal("install/uninstall commands missing")
	}
	switch description.Kind {
	case "launchd":
		if !strings.Contains(description.Content, "<key>RunAtLoad</key>") ||
			!strings.Contains(description.Content, "<false/>") {
			t.Fatal("launchd definition does not pin RunAtLoad false")
		}
		if !strings.Contains(description.Content, "<key>KeepAlive</key>") {
			t.Fatal("launchd definition does not state KeepAlive")
		}
	case "registry":
		install := strings.Join(flatten(description.Install), " ")
		if !strings.Contains(install, "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run") {
			t.Fatal("the Windows definition does not use the per-user Run key")
		}
		if !strings.Contains(install, " delete ") {
			t.Fatal("a registration that does not remove the Run value could start at login")
		}
		if strings.Contains(install, "/d ") {
			t.Fatal("a registration wrote a Run value, which starts it at login")
		}
		if !strings.Contains(description.Content, `"loginStart": false`) {
			t.Fatalf("the definition does not record login start false:\n%s", description.Content)
		}
		if !strings.Contains(description.Content, `"keepAlive": false`) {
			t.Fatalf("the definition does not record keepAlive false:\n%s", description.Content)
		}
		// The command line, not the Install list, is what the Run value holds; that
		// is the place a login start actually starts the daemon.
		if !strings.Contains(description.Content, "--start-background") {
			t.Fatalf("the definition does not start the daemon in the background:\n%s", description.Content)
		}
	}
	// The label must be the preview identity, never the released build's.
	if description.Label != ServiceLabel || !strings.Contains(ServiceLabel, "preview") {
		t.Fatalf("unexpected label %q", description.Label)
	}
}

func TestServiceDescriptionRefusesRelativePaths(t *testing.T) {
	if _, err := DescribeService(ServiceOptions{Binary: "daemon"}); err == nil {
		t.Fatal("a relative binary path was accepted")
	}
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	if _, err := DescribeService(ServiceOptions{Binary: filepath.Join(root, "bin", "d"),
		StateDir: "relative"}); err == nil {
		t.Fatal("a relative state directory was accepted")
	}
}

func flatten(commands [][]string) []string {
	var out []string
	for _, command := range commands {
		out = append(out, command...)
	}
	return out
}

func TestComponentsNeverClaimEquivalence(t *testing.T) {
	components := Components()
	if len(components) == 0 {
		t.Fatal("no components reported")
	}
	seen := map[string]bool{}
	for _, component := range components {
		if component.ID == "" || component.State == "" {
			t.Fatalf("incomplete component: %+v", component)
		}
		seen[component.ID] = true
		if component.State == ComponentNative {
			t.Fatalf("%s claims native equivalence without evidence", component.ID)
		}
		if component.State == ComponentLegacyEntry && component.LegacyEntry == "" {
			t.Fatalf("%s has no legacy entry named", component.ID)
		}
		if component.Detail == "" {
			t.Fatalf("%s has no explanation", component.ID)
		}
	}
	for _, required := range []string{ComponentCredentials, ComponentUpdater, ComponentCompanion} {
		if !seen[required] {
			t.Fatalf("%s missing from the component list", required)
		}
	}
	if _, ok := ComponentByID(ComponentCredentials); !ok {
		t.Fatal("ComponentByID cannot find credentials")
	}
}

func TestCredentialInputValidation(t *testing.T) {
	if err := validateService(""); err != ErrCredentialInvalid {
		t.Fatal("empty service accepted")
	}
	if err := validateService("bad service"); err != ErrCredentialInvalid {
		t.Fatal("service with a space accepted")
	}
	if err := validateService("dev.kimi-work-plus.preview"); err != nil {
		t.Fatalf("valid service refused: %v", err)
	}
	if err := validateKey(""); err != ErrCredentialInvalid {
		t.Fatal("empty key accepted")
	}
	if err := validateKey("bad\nkey"); err != ErrCredentialInvalid {
		t.Fatal("control character accepted")
	}
	if err := validateValue(strings.Repeat("x", maxSecretBytes+1)); err != ErrCredentialInvalid {
		t.Fatal("oversized value accepted")
	}
	if _, err := OpenCredentials("bad service"); err != ErrCredentialInvalid {
		t.Fatalf("OpenCredentials accepted an invalid service: %v", err)
	}
}

// TestCredentialRoundTrip uses a throwaway service name and removes the item
// afterwards. It never touches a real provider key: the value is synthetic and
// the service is unique to this test run.
func TestCredentialRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("touches the OS credential store")
	}
	if !Available() {
		t.Skip("no native credential store on this platform")
	}
	service := "dev.kimi-work-plus.preview.test." + sanitize(t.Name())
	store, err := OpenCredentials(service)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	key := "probe-key"
	const secret = "synthetic-test-value-0123456789"
	t.Cleanup(func() { _ = store.Delete(key) })

	if _, err := store.Get(key); err != ErrCredentialMissing {
		t.Fatalf("fresh key: want ErrCredentialMissing, got %v", err)
	}
	if err := store.Set(key, secret); err != nil {
		t.Fatalf("set: %v", err)
	}
	value, err := store.Get(key)
	if err != nil || value != secret {
		t.Fatalf("get: %q %v", value, err)
	}
	if err := store.Set(key, secret+"-v2"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if value, err := store.Get(key); err != nil || value != secret+"-v2" {
		t.Fatalf("replaced get: %q %v", value, err)
	}
	if err := store.Delete(key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(key); err != ErrCredentialMissing {
		t.Fatalf("after delete: %v", err)
	}
	// Deleting a missing key succeeds: the caller asked for it to be gone.
	if err := store.Delete(key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func sanitize(name string) string {
	var out strings.Builder
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9':
			out.WriteRune(character)
		default:
			out.WriteByte('-')
		}
	}
	return out.String()
}
