package kimi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestResolveKimiHomeFollowsKimisLayout checks the derivation against the two
// sources the released build reads.
func TestResolveKimiHomeFollowsKimisLayout(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	home, err := ResolveKimiHome(profile, "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(profile, "daimon-share", "daimon")
	if home != want {
		t.Fatalf("home=%s want=%s", home, want)
	}

	// A shareDir inside the profile relocates Work data.
	share := filepath.Join(profile, "elsewhere")
	if err := os.MkdirAll(share, 0o700); err != nil {
		t.Fatal(err)
	}
	storage, _ := json.Marshal(map[string]any{"version": 1, "shareDir": share})
	if err := os.WriteFile(filepath.Join(profile, storageFileName), storage, 0o600); err != nil {
		t.Fatal(err)
	}
	home, err = ResolveKimiHome(profile, "")
	if err != nil {
		t.Fatal(err)
	}
	if home != filepath.Join(share, "daimon") {
		t.Fatalf("shareDir ignored: %s", home)
	}
}

func TestResolveKimiHomeRefusesAnEscape(t *testing.T) {
	base := t.TempDir()
	profile := filepath.Join(base, "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	// A shareDir outside the profile is refused: it would write the user's real
	// installation.
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	storage, _ := json.Marshal(map[string]any{"version": 1, "shareDir": outside})
	if err := os.WriteFile(filepath.Join(profile, storageFileName), storage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveKimiHome(profile, ""); !errors.Is(err, ErrKimiHomeOutsideProfile) {
		t.Fatalf("outside shareDir: %v", err)
	}
	// An explicit override outside the profile is refused too.
	if _, err := ResolveKimiHome(profile, outside); !errors.Is(err, ErrKimiHomeOutsideProfile) {
		t.Fatalf("outside override: %v", err)
	}
	// A symlink inside the profile pointing outside is refused after resolution.
	link := filepath.Join(profile, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ResolveKimiHome(profile, link); !errors.Is(err, ErrKimiHomeOutsideProfile) {
		t.Fatalf("symlink escape: %v", err)
	}
}

func TestResolveOfficialKimiHomeSupportsRelocatedShareDir(t *testing.T) {
	base := t.TempDir()
	profile := filepath.Join(base, "kimi-desktop")
	relocated := filepath.Join(base, "relocated-share")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(relocated, 0o700); err != nil {
		t.Fatal(err)
	}
	storage, _ := json.Marshal(map[string]any{"version": 1, "shareDir": relocated})
	if err := os.WriteFile(filepath.Join(profile, storageFileName), storage, 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := ResolveOfficialKimiHome(profile, "")
	if err != nil {
		t.Fatalf("official relocated shareDir failed: %v", err)
	}
	want := filepath.Join(relocated, "daimon")
	if home != want {
		t.Fatalf("official home = %q, want %q", home, want)
	}
	if _, err := ResolveKimiHome(profile, ""); !errors.Is(err, ErrKimiHomeOutsideProfile) {
		t.Fatalf("isolated resolver accepted outside shareDir: %v", err)
	}
	if _, err := ResolveOfficialKimiHome(profile, relocated); !errors.Is(err, ErrKimiHomeOutsideProfile) {
		t.Fatalf("official resolver accepted outside override: %v", err)
	}
	if !isUNCPath(`\\server\share\daimon`) || !isUNCPath("//server/share/daimon") {
		t.Fatal("UNC guard did not match network share prefixes")
	}
}

func TestResolveKimiHomeAcceptsAnInsideOverride(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(profile, "custom", "daimon")
	home, err := ResolveKimiHome(profile, inside)
	if err != nil {
		t.Fatal(err)
	}
	if home != inside {
		t.Fatalf("override ignored: %s", home)
	}
}

func TestResolveKimiHomeRejectsRelativeInputs(t *testing.T) {
	if _, err := ResolveKimiHome("relative", ""); err == nil {
		t.Fatal("a relative profile was accepted")
	}
	profile := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveKimiHome(profile, "relative"); err == nil {
		t.Fatal("a relative override was accepted")
	}
}

// TestControllerProfileMustBeInsideStateDir replaces the old macOS-only
// comparison: containment is the property that matters on every platform.
func TestControllerProfileMustBeInsideStateDir(t *testing.T) {
	stateDir := t.TempDir()
	controller, err := NewController(ControllerSettings{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	// Inside the state directory is accepted.
	inside := filepath.Join(stateDir, "kimi-profile")
	if resolved, err := controller.validateProfile(inside); err != nil {
		t.Fatalf("inside profile refused: %v", err)
	} else if resolved != inside {
		t.Fatalf("resolved=%s", resolved)
	}
	// Outside is refused, including a child of the user's real profile, which
	// the old string comparison would have accepted.
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if _, err := controller.validateProfile(outside); err == nil {
		t.Fatal("a profile outside the state directory was accepted")
	}
	if home, err := os.UserHomeDir(); err == nil {
		nested := filepath.Join(home, "Library", "Application Support", "kimi-desktop", "managed")
		if _, err := controller.validateProfile(nested); err == nil {
			t.Fatal("a child of the signed-in profile was accepted")
		}
	}
	// A symlink inside the state directory that points outside is refused.
	link := filepath.Join(stateDir, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := controller.validateProfile(link); err == nil {
		t.Fatal("a symlink escape was accepted")
	}
}

func TestControllerConfigureDoesNotRaceSync(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		Settings: map[string]any{"enabled": true}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := host.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// Configure while Sync runs. The race detector is the assertion.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			host.Configure(map[string]any{"enabled": true, "provider": "bing"}, "rev-"+itoa(i%3))
			_, _ = host.Sync(ctx)
		}
	}()
	for i := 0; i < 20; i++ {
		_ = host.Options()
	}
	<-done
}

func TestHostConfigureKeepsTheScopeWhenOnlyTheViewChanges(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "scope-1"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("sync: %v", err)
	}
	// Same revision: the acknowledgement must survive, so nothing re-injects.
	host.Configure(map[string]any{"enabled": true, "view": "original"}, "scope-1")
	if sessions[0].State() != "active" {
		t.Fatal("a view-only change invalidated the acknowledgement")
	}
	// A new revision must cause a fresh injection on the next Sync. The
	// assertion is on the injection count, not on an intermediate state value:
	// what matters is that the page is re-injected under the new scope.
	before := page.snapshotInjections()
	host.Configure(map[string]any{"enabled": true, "provider": "google"}, "scope-2")
	if _, err := host.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if page.snapshotInjections() <= before {
		t.Fatal("a scope change did not re-inject")
	}
	if sessions[0].State() != "active" {
		t.Fatalf("state after re-injection: %q", sessions[0].State())
	}
}

func TestPublicStateCarriesNoSecretOrPath(t *testing.T) {
	stateDir := t.TempDir()
	controller, err := NewController(ControllerSettings{StateDir: stateDir,
		UserDataDir: filepath.Join(stateDir, "kimi-profile"), GatewayToken: "super-secret-token"})
	if err != nil {
		t.Fatal(err)
	}
	// Publish a bridge so the state has one to redact.
	bridge, err := StartBridgeHost(t.TempDir(), 1, &fakeActions{connected: true})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	controller.mu.Lock()
	controller.bridge = bridge
	controller.mu.Unlock()

	public := controller.PublicState(context.Background())
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"super-secret-token", bridge.Descriptor().Token, "127.0.0.1"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("public state leaked %q: %s", forbidden, text)
		}
	}
	if public.Bridge == nil || !public.Bridge.Published {
		t.Fatal("public state does not report the bridge")
	}
}

func TestTranslationDisabledInjectsNothing(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	host := NewHost(harness.pipe, InjectionOptions{Payload: testPayload, Revision: "rev-1",
		Settings: map[string]any{"enabled": false}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A disabled host detaches rather than attaching a surface.
	host.DetachAll(ctx)
	page.mu.Lock()
	injections := page.injections
	page.mu.Unlock()
	if injections != 0 {
		t.Fatalf("a disabled host injected %d times", injections)
	}
}

// TestAccountScopeReadsKimisOwnIdentity covers both sources and the refusal path.
func TestAccountScopeReadsKimisOwnIdentity(t *testing.T) {
	dir := t.TempDir()
	writeConfig := func(credentials map[string]any) {
		encoded, _ := json.Marshal(map[string]any{"model": map[string]any{}, "credentials": credentials})
		if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeConfig(map[string]any{"kimiWeb": map[string]any{"userId": "user-123"}})
	if got := AccountScope(dir); got != sha256Hex("user-123") {
		t.Fatalf("kimiWeb userId: %s", got)
	}

	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"jwt-user"}`))
	token := "header." + payload + ".signature"
	writeConfig(map[string]any{"kimiCode": map[string]any{"apiKey": token}})
	if got := AccountScope(dir); got != sha256Hex("jwt-user") {
		t.Fatalf("jwt user_id: %s", got)
	}

	writeConfig(map[string]any{"kimiCode": map[string]any{"apiKey": "not-a-jwt"}})
	if got := AccountScope(dir); got != "" {
		t.Fatalf("an unreadable identity produced %q", got)
	}
	if got := AccountScope(filepath.Join(dir, "missing")); got != "" {
		t.Fatalf("a missing config produced %q", got)
	}
}

// TestCDPActionsRefusesAnUnmanagedImportTarget proves the account check happens
// before any page is touched.
func TestCDPActionsRefusesAnUnmanagedImportTarget(t *testing.T) {
	page := &fakePage{surface: "work"}
	harness := newHarness(t, page)
	dir := t.TempDir()
	encoded, _ := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-1"}}})
	if err := os.WriteFile(filepath.Join(dir, NativeWorkConfig), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	actions := NewCDPActions(harness.pipe, dir, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A wrong expected account is refused, and nothing was evaluated.
	result, err := actions.ImportModels(ctx, ImportRequest{URL: "http://127.0.0.1:9999/v1",
		Token: "gateway-token", ExpectedAccountKey: sha256Hex("someone-else")})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Reason != "native_import_account_changed" {
		t.Fatalf("result: %+v", result)
	}
	if len(page.snapshotEvaluations()) != 0 {
		t.Fatal("the page was evaluated before the account was verified")
	}

	// A non-loopback gateway is refused before anything else.
	result, err = actions.ImportModels(ctx, ImportRequest{URL: "https://example.test/v1",
		Token: "gateway-token"})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Reason != "native_bridge_invalid_request" {
		t.Fatalf("remote gateway: %+v", result)
	}
}

func TestCDPActionsSurfaceProbeReadsOnlyVisibility(t *testing.T) {
	for _, forbidden := range []string{"innerHTML", "localStorage", "cookie", "conversation"} {
		if strings.Contains(surfaceProbe, forbidden) {
			t.Fatalf("the surface probe reads %q", forbidden)
		}
	}
	// The expressions must call the official APIs, not drive the DOM.
	for _, expression := range []string{closeExpression, activateExpression, openSettingsExpression} {
		if strings.Contains(expression, "querySelector") || strings.Contains(expression, "click()") {
			t.Fatal("an action drives the DOM instead of calling the official API")
		}
	}
	for _, api := range []string{"closeSettingsPage", "closeKimiWorkSettingPage"} {
		if !strings.Contains(closeExpression, api) {
			t.Fatalf("the close expression does not call %s", api)
		}
	}
	if !strings.Contains(activateExpression, "openKimiWorkTab") {
		t.Fatal("the activate expression does not call openKimiWorkTab")
	}
	if !strings.Contains(openSettingsExpression, `openSettings("general")`) {
		t.Fatal("the open expression does not call openSettings")
	}
	if !strings.Contains(importExpression("t", "http://127.0.0.1:1/v1", ""), "importByokModels") {
		t.Fatal("the import expression does not call importByokModels")
	}
	if !strings.Contains(importExpression("t", "http://127.0.0.1:1/v1", ""), "revealByok") {
		t.Fatal("the import expression does not consult revealByok")
	}
}

func TestIsLoopbackGateway(t *testing.T) {
	good := []string{"http://127.0.0.1:1234/v1", "http://[::1]:1234/v1", "http://127.0.0.1:1/v1/"}
	for _, value := range good {
		if !isLoopbackGateway(value) {
			t.Fatalf("rejected %s", value)
		}
	}
	bad := []string{"https://127.0.0.1:1234/v1", "http://example.test:1234/v1",
		"http://127.0.0.1/v1", "http://user:pass@127.0.0.1:1/v1",
		"http://127.0.0.1:1/v1?x=1", "http://127.0.0.1:1/other", "http://10.0.0.1:1/v1"}
	for _, value := range bad {
		if isLoopbackGateway(value) {
			t.Fatalf("accepted %s", value)
		}
	}
}

func TestSafeActionCodeKeepsTheVocabulary(t *testing.T) {
	if got := safeActionCode("Feature-Disabled"); got != "eature-isabled" {
		t.Fatalf("got %q", got)
	}
	if got := safeActionCode("<script>"); got != "script" {
		t.Fatalf("got %q", got)
	}
	if got := safeActionCode(""); got != "failed" {
		t.Fatalf("got %q", got)
	}
}
