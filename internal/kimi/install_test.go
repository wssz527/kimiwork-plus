package kimi

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestArchiveCandidatesPerPlatform(t *testing.T) {
	darwin := ArchiveCandidates("darwin", map[string]string{"HOME": "/Users/tester"})
	if len(darwin) != 2 || darwin[0] != "/Applications/Kimi.app/Contents/Resources/app.asar" {
		t.Fatalf("darwin candidates: %v", darwin)
	}
	if darwin[1] != "/Users/tester/Applications/Kimi.app/Contents/Resources/app.asar" {
		t.Fatalf("darwin user candidate: %v", darwin[1])
	}

	windows := ArchiveCandidates("win32", map[string]string{
		"LOCALAPPDATA": `D:\用户\AppData\Local`, "ProgramFiles": `E:\Programs`,
		"KIMI_DISABLE_DISCOVERY": "1",
	})
	joined := strings.Join(windows, "\n")
	for _, expected := range []string{
		`D:\用户\AppData\Local\Programs\Kimi\resources\app.asar`,
		`E:\Programs\Kimi\resources\app.asar`,
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing candidate %s in %v", expected, windows)
		}
	}
	if got := ArchiveCandidates("linux", nil); got != nil {
		t.Fatalf("linux should have no candidates: %v", got)
	}
}

func TestExecutableCandidatesFollowTheBundleLayout(t *testing.T) {
	darwin := ExecutableCandidates("/Applications/Kimi.app/Contents/Resources/app.asar", "darwin")
	if len(darwin) != 1 || darwin[0] != "/Applications/Kimi.app/Contents/MacOS/Kimi" {
		t.Fatalf("darwin executable: %v", darwin)
	}
	windows := ExecutableCandidates(`C:\Program Files\Kimi\resources\app.asar`, "win32")
	if len(windows) == 0 || !strings.HasSuffix(windows[0], "Kimi.exe") {
		t.Fatalf("windows executable: %v", windows)
	}
}

func TestSameInstallationRejectsAForeignExecutable(t *testing.T) {
	archive := "/Applications/Kimi.app/Contents/Resources/app.asar"
	record := Installation{Archive: archive, Application: "/Applications/Kimi.app/Contents/MacOS/Kimi"}
	if !SameInstallation(record, "/Applications/Kimi.app/Contents/MacOS/Kimi", false, "darwin") {
		t.Fatal("the implied executable was refused")
	}
	if SameInstallation(record, "/tmp/impostor", false, "darwin") {
		t.Fatal("a foreign executable was accepted")
	}
	// An explicit path is the caller's decision, not an inference.
	if !SameInstallation(record, "/tmp/impostor", true, "darwin") {
		t.Fatal("an explicit application was refused")
	}
	if SameInstallation(Installation{}, "/x", true, "darwin") {
		t.Fatal("an empty record was accepted")
	}
}

func TestDetectFindsNothingOnAnEmptyHost(t *testing.T) {
	empty := map[string]string{"HOME": t.TempDir()}
	found := Detect("darwin", empty)
	for _, installation := range found {
		if !strings.Contains(installation.Archive, "/Applications/") {
			continue
		}
		// The real machine has Kimi installed; that is expected and fine. The
		// synthetic home must not have contributed anything.
		if strings.HasPrefix(installation.Archive, empty["HOME"]) {
			t.Fatalf("synthetic home produced an install: %s", installation.Archive)
		}
	}
}

func TestDetectStateNeverClaimsNotRunningWhenItCannotTell(t *testing.T) {
	// A platform with no discovery has no installation, which is a definite
	// answer: nothing is installed, so nothing is running.
	state := DetectState(context.Background(), ProbeOptions{Platform: "linux"})
	if state.Application != NotRunning {
		t.Fatalf("linux: %s (%s)", state.Application, state.Reason)
	}
	if state.Installation != nil {
		t.Fatal("linux reported an installation")
	}
}

func TestDetectStateOnARealHostIsReadOnlyAndDefinite(t *testing.T) {
	if os.Getenv("KIMI_SKIP_LIVE") == "1" {
		t.Skip("live probe disabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	state := DetectState(ctx, ProbeOptions{})
	t.Logf("platform=%s application=%s reason=%s pids=%v", state.Platform, state.Application, state.Reason, state.PIDs)
	switch state.Application {
	case Running, NotRunning, RunningStateUnknown:
	default:
		t.Fatalf("unexpected state %q", state.Application)
	}
	// `unknown` must never be silently turned into not_running.
	if state.Application == RunningStateUnknown && state.Reason == "" {
		t.Fatal("unknown state without a reason")
	}
	if state.Application == NotRunning && state.Installation != nil && state.Installation.Application == "" {
		t.Fatal("not_running without an executable to check")
	}
}

func TestProcessIdentityIsNeverAFabricatedMatch(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("identity source is platform specific")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	self := ProcessStartIdentity(ctx, os.Getpid())
	if self.Kind == IdentityUnknown {
		t.Skipf("identity unavailable here: %s", self.Reason)
	}
	if self.Kind == IdentityNotFound {
		t.Fatal("the current process was reported as not found")
	}
	if self.Value == "" {
		t.Fatal("identity has no value")
	}
	if !SameProcess(ctx, os.Getpid(), self) {
		t.Fatal("a process does not match its own identity")
	}
	// An unknown identity never compares equal, so a failed check cannot
	// authorise acting on the process.
	if SameProcess(ctx, os.Getpid(), ProcessIdentity{Kind: IdentityUnknown}) {
		t.Fatal("an unknown identity matched")
	}
	missing := ProcessStartIdentity(ctx, 999999)
	if missing.Kind == IdentityDarwinLStart || missing.Kind == IdentityWindowsFT {
		t.Fatalf("a missing pid produced an identity: %+v", missing)
	}
}

func TestValidFiletimeRejectsImpossibleValues(t *testing.T) {
	if validFiletime("") || validFiletime("0") || validFiletime("abc") || validFiletime("-1") {
		t.Fatal("invalid FILETIME accepted")
	}
	if !validFiletime("133000000000000000") {
		t.Fatal("valid FILETIME rejected")
	}
}

func TestSurfaceClassificationIsAnAllowlist(t *testing.T) {
	cases := map[string]string{
		"app://localhost/kimi-agent.html":                                         "work",
		"app://localhost/kimi-agent.html?theme=dark&locale=zh-CN&platform=darwin": "work",
		"file:///Applications/Kimi.app/kimi-agent.html?theme=dark":                "work",
		"kimi://local/work/kimi-agent.html?theme=dark":                            "work",
		"https://www.kimi.com/chat/abc-123":                                       "chat",
		"https://www.kimi.com/chat/history":                                       "chat",
		"https://www.kimi.com/":                                                   "chat",
		// Everything below must never receive a script or reasoning text.
		"app://localhost/work-settings.html?theme=dark":      "other",
		"app://localhost/desktopclaw.html":                   "other",
		"app://localhost/work/kimi-agent.html":               "other",
		"app://evil/kimi-agent.html":                         "other",
		"app://localhost:123/kimi-agent.html":                "other",
		"app://user@localhost/kimi-agent.html":               "other",
		"app://localhost/kimi-agent.html?theme=dark#work":    "other",
		"file:///Applications/Kimi.app/kimi-agent.html#work": "other",
		"kimi://user@local/work/kimi-agent.html":             "other",
		"https://user@www.kimi.com/chat/abc":                 "other",
		"https://www.kimi.com/chat/abc#chat":                 "other",
		"app://localhost/%zz":                                "other",
		"https://www.kimi.com/settings":                      "other",
		"https://www.kimi.com.evil.test/chat/abc":            "other",
		"https://evil.test/chat/abc":                         "other",
		"https://www.kimi.com/chat/abc?token=leak":           "other",
		"https://www.kimi.com/chat/abc/extra":                "other",
	}
	for raw, want := range cases {
		if got := ClassifySurface("page", raw); got != want {
			t.Fatalf("%s -> %s, want %s", raw, got, want)
		}
	}
	if ClassifySurface("worker", "https://www.kimi.com/chat/abc") != "other" {
		t.Fatal("a worker must never be a surface")
	}
	if ClassifySurface("iframe", "app://localhost/kimi-agent.html") != "other" {
		t.Fatal("a non-page target must never be a surface")
	}
}

func TestSanitizeTargetURLDropsParameters(t *testing.T) {
	got := SanitizeTargetURL("https://www.kimi.com/chat/abc?token=secret#frag")
	if strings.Contains(got, "secret") || strings.Contains(got, "frag") {
		t.Fatalf("parameters survived: %s", got)
	}
	if got != "https://www.kimi.com/chat/abc" {
		t.Fatalf("unexpected sanitized url: %s", got)
	}
}

func TestInstallationDirectoryOfWindowsCandidatesIsDeterministic(t *testing.T) {
	env := map[string]string{
		"USERPROFILE":            `C:\Users\tester`,
		"KIMI_DISABLE_DISCOVERY": "1",
	}
	candidates := ArchiveCandidates("win32", env)
	if len(candidates) == 0 {
		t.Fatal("no windows candidates")
	}
	// The user-profile fallback must appear when LOCALAPPDATA is absent.
	found := false
	for _, candidate := range candidates {
		if strings.Contains(candidate, `C:\Users\tester\AppData\Local`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("USERPROFILE fallback missing: %v", candidates)
	}
}

func TestArchiveReadFailureIsUnavailableNotVerified(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.asar")
	result := Inspect(context.Background(), ProbeOptions{Archive: missing})
	if result.State != StateUnavailable {
		t.Fatalf("missing archive: %s", result.State)
	}
	// A file that exists but is not an asar is also unavailable, never verified.
	broken := filepath.Join(dir, "broken.asar")
	if err := os.WriteFile(broken, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := Inspect(context.Background(), ProbeOptions{Archive: broken}); result.State != StateUnavailable {
		t.Fatalf("broken archive: %s", result.State)
	}
	if result := InspectLaunchContract(context.Background(), ProbeOptions{Archive: broken}); result.State != StateUnavailable {
		t.Fatalf("broken archive launch: %s", result.State)
	}
}

func TestInspectRejectsAnEmptyConfiguredCatalog(t *testing.T) {
	archive := firstExisting(ArchiveCandidates(runtime.GOOS, environMap()))
	if archive == "" {
		t.Skip("no installation")
	}
	result := Inspect(context.Background(), ProbeOptions{Archive: archive, Models: []Model{}})
	if result.State == StateVerified {
		t.Fatal("an empty configured catalog was certified as verified")
	}
}
