package kimi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Account switching is exercised against a fake official page and a synthetic
// Kimi home. The fake speaks the same method names the production surface uses,
// so the tests fail if the surface stops calling what it claims to call. No real
// credential file is read anywhere in this file.

type fakeAccountPage struct {
	mu sync.Mutex

	// page state, keyed by target id
	storage   map[string]map[string]string
	user      map[string]string // target id -> user id the page would report
	cookies   []AccountCookie
	name      string
	avatarURL string

	// behaviour
	pages []string
	// shellPage names which of pages is the Work shell surface. It answers the
	// official identity read; the others own the cookie jar.
	shellPage        string
	refuseCookies    bool
	refuseStorage    bool
	refuseLogin      bool
	ignoreActivation bool
	// officialIdentity is what the shell page reports through kimiAgentAPI. When
	// empty the shell answers with the id derived from storage, and only when
	// officialMissing is false.
	officialIdentity string
	officialMissing  bool
	// signedOut makes every page report no identity at all, which is what a
	// rejected session looks like after the app throws the tokens away.
	signedOut bool
	// busy is what the conversation status table reports.
	busy bool
	// busyUnknown makes the status read fail, which must not look idle.
	busyUnknown       bool
	runtimeWatch      bool
	runtimeTransition bool
	runtimeChanged    bool
	runtimeWatching   bool
	runtimeUnready    bool
	runtimeBlocking   bool
	onActivation      func()
	onBeginLogin      func()
	reloads           int
	calls             []string
}

func newFakeAccountPage() *fakeAccountPage {
	return &fakeAccountPage{storage: map[string]map[string]string{}, user: map[string]string{},
		pages: []string{"work", "kimi-1"}, shellPage: "work"}
}

func (f *fakeAccountPage) record(call string) {
	f.calls = append(f.calls, call)
}

// accountPages models the two official surfaces the switch has to tell apart:
// the Work shell page, which carries kimiAgentAPI and answers identity, and the
// kimi.com page, which owns the cookie jar. Only the web page is a credential
// surface, exactly like the production classifier.
func (f *fakeAccountPage) accountPages(context.Context) ([]accountPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("pages")
	out := make([]accountPage, 0, len(f.pages))
	for _, id := range f.pages {
		if id == f.shellPage {
			out = append(out, accountPage{targetID: id, sessionID: "session-" + id,
				origin: "app://localhost", credentials: false})
			continue
		}
		out = append(out, accountPage{targetID: id, sessionID: "session-" + id,
			origin: "https://www.kimi.com", credentials: true})
	}
	return out, nil
}

func (f *fakeAccountPage) accountCookies(_ context.Context, page accountPage) ([]AccountCookie, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("cookies:" + page.targetID)
	out := make([]AccountCookie, len(f.cookies))
	copy(out, f.cookies)
	return out, nil
}

func (f *fakeAccountPage) accountSetCookies(_ context.Context, page accountPage, cookies []AccountCookie) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("set-cookies:" + page.targetID)
	if f.refuseCookies {
		return errors.New(AccountErrWriteFailed)
	}
	for _, cookie := range cookies {
		replaced := false
		for i := range f.cookies {
			if cookieKey(f.cookies[i]) == cookieKey(cookie) {
				f.cookies[i] = cookie
				replaced = true
			}
		}
		if !replaced {
			f.cookies = append(f.cookies, cookie)
		}
	}
	return nil
}

func (f *fakeAccountPage) accountRemoveCookies(_ context.Context, page accountPage, cookies []AccountCookie) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove-cookies:" + page.targetID)
	drop := map[string]bool{}
	for _, cookie := range cookies {
		drop[cookieKey(cookie)] = true
	}
	kept := f.cookies[:0]
	for _, cookie := range f.cookies {
		if !drop[cookieKey(cookie)] {
			kept = append(kept, cookie)
		}
	}
	f.cookies = kept
	return nil
}

func (f *fakeAccountPage) accountStorage(_ context.Context, page accountPage) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("storage:" + page.targetID)
	out := map[string]string{}
	for key, value := range f.storage[page.targetID] {
		out[key] = value
	}
	return out, nil
}

func (f *fakeAccountPage) accountWriteStorage(_ context.Context, page accountPage, set map[string]string, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("write-storage:" + page.targetID)
	if f.refuseStorage {
		return errors.New(AccountErrWriteFailed)
	}
	current := f.storage[page.targetID]
	if current == nil {
		current = map[string]string{}
		f.storage[page.targetID] = current
	}
	for key, value := range set {
		current[key] = value
	}
	for _, key := range remove {
		delete(current, key)
	}
	// The desktop re-derives the identity it reports from the web page's token
	// store: the shell page answers kimiAgentAPI from the profile it bound, which
	// follows the signed-in web session. So a write on the credential surface
	// changes what the shell page reports, and a write on the shell surface does
	// not (it owns no token store) — the same asymmetry production has.
	if !f.ignoreActivation && page.credentials {
		if user := current["msh_user_id"]; user != "" {
			f.user[f.shellPage] = user
			f.user[page.targetID] = user
		} else {
			delete(f.user, f.shellPage)
			delete(f.user, page.targetID)
		}
		if f.runtimeWatching && f.runtimeTransition {
			f.runtimeChanged = true
		}
		if f.onActivation != nil {
			f.onActivation()
		}
	}
	return nil
}

func (f *fakeAccountPage) accountWatchRuntime(_ context.Context, page accountPage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("runtime-watch:" + page.targetID)
	if !f.runtimeWatch || page.credentials {
		return errors.New(AccountErrUnsupported)
	}
	f.runtimeWatching, f.runtimeChanged = true, false
	return nil
}

func (f *fakeAccountPage) accountRuntimeReady(ctx context.Context, page accountPage, transition bool) (bool, error) {
	f.mu.Lock()
	f.record("runtime-ready:" + page.targetID)
	blocking := transition && f.runtimeBlocking
	ready := !page.credentials && !f.runtimeUnready && (!transition || (f.runtimeWatching && f.runtimeChanged))
	f.mu.Unlock()
	if blocking {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return ready, nil
}

func (f *fakeAccountPage) accountStopRuntimeWatch(_ context.Context, page accountPage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("runtime-stop:" + page.targetID)
	f.runtimeWatching = false
	return nil
}

func (f *fakeAccountPage) accountBeginLogin(context.Context, accountPage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("reauthenticate")
	if f.refuseLogin {
		return errors.New(AccountErrUnsupported)
	}
	for _, store := range f.storage {
		for key := range store {
			if isAccountStorageKey(key) {
				delete(store, key)
			}
		}
	}
	f.user = map[string]string{}
	if f.onBeginLogin != nil {
		f.onBeginLogin()
	}
	return nil
}

func (f *fakeAccountPage) accountReload(_ context.Context, _ []accountPage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("reload")
	f.reloads++
	return nil
}

// accountIdentity models the real split: only the Work shell page answers the
// official read (`kimiAgentAPI.getUserProfile`), and the kimi.com page can only
// report the token store this process writes.
func (f *fakeAccountPage) accountIdentity(_ context.Context, page accountPage) (accountPageIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("identity:" + page.targetID)
	if f.signedOut {
		return accountPageIdentity{}, nil
	}
	identity := accountPageIdentity{Name: f.name, AvatarURL: f.avatarURL}
	store := f.storage[page.targetID]
	if store["access_token"] != "" {
		identity.AccessToken = true
	}
	if store["refresh_token"] != "" {
		identity.Refresh = true
	}
	if page.targetID != f.shellPage || f.officialMissing {
		identity.UserID = f.user[page.targetID]
		return identity, nil
	}
	identity.Official = true
	if f.officialIdentity != "" {
		identity.UserID = f.officialIdentity
		return identity, nil
	}
	// The shell answers from the profile bound to the view. Once the web session
	// is gone the bound profile is anonymous: an empty id with isAnonymous set.
	if user := f.user[page.targetID]; user != "" {
		identity.UserID = user
		return identity, nil
	}
	identity.Anonymous = true
	return identity, nil
}

func (f *fakeAccountPage) accountWorkBusy(_ context.Context, page accountPage) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("work-busy:" + page.targetID)
	if page.credentials {
		return true, nil
	}
	if f.busyUnknown {
		return true, errors.New(AccountErrUnavailable)
	}
	return f.busy, nil
}

func (f *fakeAccountPage) put(target string, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storage[target] == nil {
		f.storage[target] = map[string]string{}
	}
	f.storage[target][key] = value
	if key != "msh_user_id" {
		return
	}
	f.user[target] = value
	// A token store on the credential surface is what the shell page's bound
	// profile follows, so a fixture that seeds the web page also seeds the
	// identity the shell reports.
	if target != f.shellPage {
		f.user[f.shellPage] = value
	}
}

func (f *fakeAccountPage) callsFor(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, item := range f.calls {
		if item == call {
			count++
		}
	}
	return count
}

// accountActions is the production shape in one value: PageActions (so it can sit
// in Controller.actions) plus the accountSurface the switch needs.
type accountActions struct {
	*fakeAccountPage
	*fakeActions
}

// accountHarness is a controller wired to a fake page and a synthetic home.
type accountHarness struct {
	controller *Controller
	page       *fakeAccountPage
	home       string
	stateDir   string
}

func newAccountHarness(t *testing.T) *accountHarness {
	t.Helper()
	base := t.TempDir()
	profile := filepath.Join(base, "profile")
	home := filepath.Join(profile, "daimon-share", "daimon")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(ControllerSettings{StateDir: base, UserDataDir: profile,
		ProfileMode: ProfileModeIsolated})
	if err != nil {
		t.Fatal(err)
	}
	page := newFakeAccountPage()
	controller.actions = &accountActions{fakeAccountPage: page, fakeActions: &fakeActions{connected: true}}
	previousRetries, previousStep, previousRuntimeRetries := accountVerifyRetries, accountVerifyStep, accountRuntimeRetries
	accountVerifyRetries, accountVerifyStep, accountRuntimeRetries = 3, time.Millisecond, 3
	t.Cleanup(func() {
		accountVerifyRetries, accountVerifyStep, accountRuntimeRetries = previousRetries, previousStep, previousRuntimeRetries
		restartAccountProcess = func(c *Controller, ctx context.Context) error { return c.restartOfficialLocked(ctx) }
	})
	return &accountHarness{controller: controller, page: page, home: home, stateDir: base}
}

func (h *accountHarness) writeConfig(t *testing.T, document map[string]any) {
	t.Helper()
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.home, NativeWorkConfig), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *accountHarness) readConfig(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.home, NativeWorkConfig))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

// accountConfigFixture is the shape of a real config: credential entries next to
// the model selection and provider rows that this product must not touch.
func accountConfigFixture(userID, access, refresh string) map[string]any {
	return map[string]any{
		"model": map[string]any{"selected": "byok-gw/kimi-k2"},
		"models": map[string]any{
			"providers": map[string]any{
				"byok-gw": map[string]any{"type": "openai", "baseUrl": "http://127.0.0.1:9/v1"},
			},
		},
		"credentials": map[string]any{
			"kimiWeb": map[string]any{
				"accessToken": access, "refreshToken": refresh,
				"userId": userID, "deviceId": "device-" + userID,
				"sessionId": "session-" + userID, "updatedAt": "2026-01-01T00:00:00.000Z",
			},
			"kimiCode": map[string]any{"apiKey": "{{SECRET_w4qgbn2l}}-key", "baseUrl": "https://agent-gw.kimi.com/coding"},
		},
		"tabs": []any{map[string]any{"id": "tab-1", "title": "keep me"}},
	}
}

func TestAccountActivationDoesNotOverwriteRotatedCredentialsAfterRestart(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("original", "old-access", "old-refresh"))
	harness.page.put("kimi-1", "msh_user_id", "original")
	target := AccountSession{SchemaVersion: 1, UserID: "target", KimiWeb: map[string]any{"userId": "target", "accessToken": "target-before-restart"}, LocalStorage: []AccountStorageEntry{{Key: "msh_user_id", Value: "target"}, {Key: "access_token", Value: "target-before-restart"}}}
	restartAccountProcess = func(*Controller, context.Context) error {
		harness.writeConfig(t, accountConfigFixture("target", "rotated-after-start", "rotated-refresh"))
		return nil
	}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil || !result.Verified {
		t.Fatal("restarted identity was not verified", err)
	}
	credentials := harness.readConfig(t)["credentials"].(map[string]any)["kimiWeb"].(map[string]any)
	if credentials["accessToken"] != "rotated-after-start" || credentials["refreshToken"] != "rotated-refresh" {
		t.Fatal("activation overwrote freshly rotated credentials")
	}
}

func TestAccountIdentityUsesTheOfficialShellInEitherPageOrder(t *testing.T) {
	for _, pages := range [][]string{{"work", "kimi-1"}, {"kimi-1", "work"}} {
		t.Run(strings.Join(pages, "-"), func(t *testing.T) {
			harness := newAccountHarness(t)
			harness.writeConfig(t, accountConfigFixture("user-1", "access", "refresh"))
			harness.page.pages = pages
			harness.page.officialIdentity = "user-1"
			harness.page.user["kimi-1"] = "stale-web-user"
			identity, err := harness.controller.AccountIdentity(context.Background())
			if err != nil || identity.UserID != "user-1" || !identity.PageVerified || identity.Source != "page+config" || identity.Reason != "" {
				t.Fatalf("credential page displaced the official identity: %+v %v", identity, err)
			}
		})
	}
}

func TestCaptureAccountUsesTheOfficialShellInEitherPageOrder(t *testing.T) {
	for _, pages := range [][]string{{"work", "kimi-1"}, {"kimi-1", "work"}} {
		t.Run(strings.Join(pages, "-"), func(t *testing.T) {
			harness := newAccountHarness(t)
			harness.writeConfig(t, accountConfigFixture("user-1", "access", "refresh"))
			harness.page.pages = pages
			harness.page.officialIdentity = "user-1"
			harness.page.user["kimi-1"] = "stale-web-user"
			session, err := harness.controller.CaptureAccount(context.Background())
			if err != nil || session.UserID != "user-1" || session.CapturedVia != AccountSourcePage {
				t.Fatalf("credential page displaced the captured identity: %+v %v", session, err)
			}
		})
	}
}

func TestCaptureAccountKeepsTheOfficialDisplayIdentity(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access", "refresh"))
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	harness.page.name, harness.page.avatarURL = "Display name", "https://www.kimi.com/avatar.png"
	session, err := harness.controller.CaptureAccount(context.Background())
	if err != nil || session.Name != harness.page.name || session.AvatarURL != harness.page.avatarURL {
		t.Fatal("official display identity was omitted", err)
	}
}

func TestCaptureAccountReadsTheLivePageAndKeepsOnlyCredentials(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "stale-access", "stale-refresh"))
	if err := os.WriteFile(filepath.Join(harness.home, AccountCodeKey), []byte(`{"apiKey":"code-key-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The live page has rotated both tokens, which is exactly the case a config
	// snapshot alone would get wrong.
	harness.page.put("kimi-1", "access_token", "live-access")
	harness.page.put("kimi-1", "refresh_token", "live-refresh")
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	harness.page.put("kimi-1", "kimi-work-draft", "conversation text that must not travel")
	harness.page.put("kimi-1", "theme", "dark")
	harness.page.cookies = []AccountCookie{
		{Name: "kimi_session", Value: "cookie-secret", Domain: ".kimi.com", Path: "/", Secure: true, HTTPOnly: true},
		{Name: "_ga", Value: "analytics-id", Domain: ".kimi.com", Path: "/"},
	}

	session, err := harness.controller.CaptureAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if session.UserID != "user-1" || session.CapturedVia != AccountSourcePage {
		t.Fatalf("identity: %q via %q", session.UserID, session.CapturedVia)
	}
	// The page's rotated tokens win over the stale config copy.
	access, refresh := "", ""
	for _, entry := range session.LocalStorage {
		switch entry.Key {
		case "access_token":
			access = entry.Value
		case "refresh_token":
			refresh = entry.Value
		case "kimi-work-draft", "theme":
			t.Fatalf("captured unrelated storage key %q", entry.Key)
		}
	}
	if access != "live-access" || refresh != "live-refresh" {
		t.Fatalf("tokens: %q %q", access, refresh)
	}
	// Only the login cookie is kept; the measurement cookie is not copied.
	if len(session.Cookies) != 1 || session.Cookies[0].Name != "kimi_session" {
		t.Fatalf("cookies: %+v", session.Cookies)
	}
	if session.KimiWeb["deviceId"] != "device-user-1" || session.KimiWeb["sessionId"] != "session-user-1" {
		t.Fatalf("kimiWeb entry lost its own fields: %+v", session.KimiWeb)
	}
	if string(session.CodeKey) != `{"apiKey":"code-key-1"}` {
		t.Fatalf("code key: %s", session.CodeKey)
	}
}

func TestCaptureAccountRefusesAMixedIdentity(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "a", "r"))
	if err := os.WriteFile(filepath.Join(harness.home, AccountCodeKey), []byte(`{"apiKey":"key-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The page is signed in as another account: the config's kernel key belongs to
	// user-1 and must never be stored under user-2.
	harness.page.put("kimi-1", "access_token", "access-2")
	harness.page.put("kimi-1", "msh_user_id", "user-2")
	harness.page.cookies = []AccountCookie{{Name: "kimi_session", Value: "s", Domain: ".kimi.com", Path: "/"}}

	if _, err := harness.controller.CaptureAccount(context.Background()); err == nil ||
		err.Error() != AccountErrIdentityMismatch {
		t.Fatalf("mixed identity: %v", err)
	}
}

func TestCaptureAccountFallsBackToConfigIdentityWithoutAPage(t *testing.T) {
	harness := newAccountHarness(t)
	harness.page.pages = nil
	harness.writeConfig(t, accountConfigFixture("user-config", "a", "r"))

	session, err := harness.controller.CaptureAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if session.UserID != "user-config" || session.CapturedVia != AccountSourceConfig {
		t.Fatalf("identity: %q via %q", session.UserID, session.CapturedVia)
	}

	// With neither a page nor a config identity there is nothing to capture, and
	// the answer is a fixed code rather than a guess.
	empty := newAccountHarness(t)
	empty.page.pages = nil
	if _, err := empty.controller.CaptureAccount(context.Background()); err == nil ||
		err.Error() != AccountErrIdentityMissing {
		t.Fatalf("missing identity: %v", err)
	}
}

func TestActivateAccountHotSwitchKeepsOfficiallyRotatedCredentials(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("original", "original-access", "original-refresh"))
	harness.page.put("kimi-1", "msh_user_id", "original")
	harness.page.runtimeWatch, harness.page.runtimeTransition = true, true
	harness.page.onActivation = func() {
		harness.writeConfig(t, accountConfigFixture("target", "rotated-access", "rotated-refresh"))
		if err := os.WriteFile(filepath.Join(harness.home, AccountCodeKey), []byte(`{"apiKey":"rotated-code"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restarts := 0
	restartAccountProcess = func(*Controller, context.Context) error { restarts++; return nil }
	target := AccountSession{SchemaVersion: 1, UserID: "target",
		KimiWeb: map[string]any{"userId": "target", "accessToken": "snapshot-access"},
		CodeKey: json.RawMessage(`{"apiKey":"snapshot-code"}`)}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil || !result.Hot || !result.Verified || result.Restarted || restarts != 0 {
		t.Fatalf("hot activation: %+v err=%v restarts=%d", result, err, restarts)
	}
	credentials := harness.readConfig(t)["credentials"].(map[string]any)
	if credentials["kimiWeb"].(map[string]any)["accessToken"] != "rotated-access" {
		t.Fatal("officially rotated token was overwritten by the snapshot")
	}
	key, err := os.ReadFile(filepath.Join(harness.home, AccountCodeKey))
	if err != nil || string(key) != `{"apiKey":"rotated-code"}` {
		t.Fatal("officially rotated Code key was overwritten")
	}
	if harness.page.runtimeWatching || harness.page.callsFor("runtime-stop:work") != 1 {
		t.Fatal("runtime watch was not cleaned up")
	}
	watch, write := -1, -1
	for i, call := range harness.page.calls {
		if call == "runtime-watch:work" {
			watch = i
		}
		if call == "write-storage:kimi-1" {
			write = i
		}
	}
	if watch < 0 || write < watch {
		t.Fatal("runtime watch started after the credential change")
	}
}

func TestActivateAccountDoesNotAcceptAStaleReadyRuntime(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("original", "original-access", "original-refresh"))
	harness.page.put("kimi-1", "msh_user_id", "original")
	harness.page.runtimeWatch = true
	harness.page.onActivation = func() {
		harness.writeConfig(t, accountConfigFixture(harness.page.user["work"], "live-access", "live-refresh"))
	}
	restarts := 0
	restartAccountProcess = func(*Controller, context.Context) error { restarts++; return nil }
	result, err := harness.controller.ActivateAccount(context.Background(), AccountSession{
		SchemaVersion: 1, UserID: "target", KimiWeb: map[string]any{"userId": "target", "accessToken": "target-access"},
	})
	if err != nil || result.Hot || !result.Restarted || !result.Verified || restarts != 1 {
		t.Fatalf("stale ready result: %+v err=%v restarts=%d", result, err, restarts)
	}
	if harness.page.runtimeWatching || harness.page.callsFor("runtime-stop:work") != 2 {
		t.Fatal("restart fallback did not clean up both runtime watches")
	}
}

func TestActivateAccountBoundsHotWaitBeforeRestartFallback(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("original", "original-access", "original-refresh"))
	harness.page.put("kimi-1", "msh_user_id", "original")
	harness.page.runtimeWatch, harness.page.runtimeBlocking = true, true
	harness.page.onActivation = func() {
		harness.writeConfig(t, accountConfigFixture(harness.page.user["work"], "live-access", "live-refresh"))
	}
	previousTimeout := accountRuntimeTimeout
	accountRuntimeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { accountRuntimeTimeout = previousTimeout })
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	restarts := 0
	restartAccountProcess = func(_ *Controller, ctx context.Context) error {
		restarts++
		return ctx.Err()
	}
	result, err := harness.controller.ActivateAccount(ctx, AccountSession{
		SchemaVersion: 1, UserID: "target", KimiWeb: map[string]any{"userId": "target", "accessToken": "target-access"},
	})
	if err != nil || result.Hot || !result.Restarted || !result.Verified || restarts != 1 || ctx.Err() != nil {
		t.Fatalf("bounded wait: %+v err=%v restarts=%d parent=%v", result, err, restarts, ctx.Err())
	}
}

func TestActivateAccountCancellationDoesNotReportAWriteFailure(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("original", "original-access", "original-refresh"))
	harness.page.put("kimi-1", "msh_user_id", "original")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness.page.onActivation = cancel
	restarts := 0
	restartAccountProcess = func(*Controller, context.Context) error { restarts++; return nil }
	result, _ := harness.controller.ActivateAccount(ctx, AccountSession{
		SchemaVersion: 1, UserID: "target", KimiWeb: map[string]any{"userId": "target", "accessToken": "target-access"},
	})
	if result.Reason != AccountErrPageTimeout || restarts != 0 {
		t.Fatalf("cancelled activation: %+v restarts=%d", result, restarts)
	}
}

func TestActivateAccountDoesNotVerifyAnUnreadyRuntime(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("original", "original-access", "original-refresh"))
	harness.page.put("kimi-1", "msh_user_id", "original")
	harness.page.runtimeWatch, harness.page.runtimeTransition, harness.page.runtimeUnready = true, true, true
	harness.page.onActivation = func() {
		harness.writeConfig(t, accountConfigFixture(harness.page.user["work"], "live-access", "live-refresh"))
	}
	restarts := 0
	restartAccountProcess = func(*Controller, context.Context) error { restarts++; return nil }
	result, _ := harness.controller.ActivateAccount(context.Background(), AccountSession{
		SchemaVersion: 1, UserID: "target", KimiWeb: map[string]any{"userId": "target", "accessToken": "target-access"},
	})
	if result.Hot || result.Verified || restarts != 1 {
		t.Fatalf("unready runtime was adopted: %+v restarts=%d", result, restarts)
	}
}

func TestActivateAccountRestartsWhenRuntimeTransitionIsUnsupported(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")

	target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "user-2",
		LocalStorage: []AccountStorageEntry{{Key: "access_token", Value: "access-2"},
			{Key: "refresh_token", Value: "refresh-2"}, {Key: "msh_user_id", Value: "user-2"}},
		Cookies:  []AccountCookie{{Name: "kimi_session", Value: "session-2", Domain: ".kimi.com", Path: "/"}},
		KimiWeb:  map[string]any{"deviceId": "device-user-2"},
		KimiCode: map[string]any{"apiKey": "code-2"}}

	restarts := 0
	restartAccountProcess = func(*Controller, context.Context) error {
		restarts++
		return nil
	}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	// A shell identity alone cannot establish that the runtime adopted the account.
	if result.Hot || !result.Restarted || !result.Verified || result.State != AccountStateRestarted {
		t.Fatalf("result: %+v", result)
	}
	if restarts != 1 {
		t.Fatalf("restarts: %d", restarts)
	}
	if harness.page.reloads == 0 {
		t.Fatal("the page was never reloaded")
	}

	// The config kept everything that is not a credential.
	config := harness.readConfig(t)
	if config["tabs"] == nil || config["model"] == nil {
		t.Fatalf("unrelated config was dropped: %+v", config)
	}
	credentials, _ := config["credentials"].(map[string]any)
	web, _ := credentials["kimiWeb"].(map[string]any)
	if web["userId"] != "user-2" || web["accessToken"] != "access-2" || web["refreshToken"] != "refresh-2" {
		t.Fatalf("kimiWeb: %+v", web)
	}
	if web["deviceId"] != "device-user-2" {
		t.Fatalf("device id was not taken from the snapshot: %+v", web)
	}
	code, _ := credentials["kimiCode"].(map[string]any)
	if code["apiKey"] != "code-2" {
		t.Fatalf("kimiCode: %+v", code)
	}
	if AccountScope(harness.home) != sha256Hex("user-2") {
		t.Fatal("the profile identity does not match the target account")
	}
}

func TestActivateAccountDoesNotRestartWithIncompletePageCredentials(t *testing.T) {
	for _, failure := range []string{"cookies", "storage"} {
		t.Run(failure, func(t *testing.T) {
			harness := newAccountHarness(t)
			harness.writeConfig(t, accountConfigFixture("original", "original-access", "original-refresh"))
			harness.page.put("kimi-1", "access_token", "original-access")
			harness.page.put("kimi-1", "msh_user_id", "original")
			harness.page.refuseCookies = failure == "cookies"
			harness.page.refuseStorage = failure == "storage"
			target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "target",
				LocalStorage: []AccountStorageEntry{{Key: "access_token", Value: "target-access"}, {Key: "msh_user_id", Value: "target"}},
				Cookies:      []AccountCookie{{Name: "kimi-auth", Value: "target-cookie", Domain: ".kimi.com", Path: "/"}}}
			restarts := 0
			restartAccountProcess = func(*Controller, context.Context) error {
				restarts++
				harness.page.officialIdentity = "target"
				return nil
			}
			result, _ := harness.controller.ActivateAccount(context.Background(), target)
			if result.Verified || restarts != 0 {
				t.Fatalf("incomplete %s write was adopted: result=%+v restarts=%d", failure, result, restarts)
			}
			if !accountConfigMatches(harness.home, "original") {
				t.Fatal("incomplete page write changed the kernel account")
			}
		})
	}
}

func TestActivateAccountDoesNotVerifyAgainstTheTokenStore(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	// No official profile API on the shell page: the only readable identity would
	// be the token store, which is what this process just wrote.
	harness.page.officialMissing = true
	restartAccountProcess = func(*Controller, context.Context) error { return nil }

	target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "user-2",
		LocalStorage: []AccountStorageEntry{{Key: "msh_user_id", Value: "user-2"},
			{Key: "access_token", Value: "access-2"}}}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	// This build cannot verify either direction, and that is reported as a
	// capability gap rather than as a verified switch or a failed rollback.
	if err == nil || err.Error() != AccountErrUnsupported {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Verified || result.Restarted || result.RolledBack || result.State != AccountStateFailed {
		t.Fatalf("a token-store match was reported as verified: %+v", result)
	}
}

func TestActivateAccountRestoresNativeOnlyTokensToThePage(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("original", "original-access", "original-refresh"))
	target := AccountSession{SchemaVersion: 1, UserID: "target", KimiWeb: map[string]any{
		"userId": "target", "accessToken": "native-access", "refreshToken": "native-refresh",
	}}
	restartAccountProcess = func(*Controller, context.Context) error { return nil }
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil || !result.Verified {
		t.Fatal("native-only account was not restored", err)
	}
	if harness.page.storage["kimi-1"]["access_token"] != "native-access" ||
		harness.page.storage["kimi-1"]["refresh_token"] != "native-refresh" {
		t.Fatal("native credentials were not synchronized to the page")
	}
}

func TestActivateAccountRefusesWhileAConversationRuns(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	harness.page.busy = true
	restarted := false
	restartAccountProcess = func(*Controller, context.Context) error {
		restarted = true
		return nil
	}

	_, err := harness.controller.ActivateAccount(context.Background(), AccountSession{
		SchemaVersion: AccountSessionSchema, UserID: "user-2"})
	if err == nil || err.Error() != AccountErrWorkBusy {
		t.Fatalf("busy switch: %v", err)
	}
	if restarted {
		t.Fatal("a busy application was restarted")
	}
	credentials, _ := harness.readConfig(t)["credentials"].(map[string]any)
	if web, _ := credentials["kimiWeb"].(map[string]any); web["userId"] != "user-1" {
		t.Fatalf("the config was touched: %+v", credentials)
	}
}

func TestAccountWorkBusyNeverTreatsUnknownAsIdle(t *testing.T) {
	harness := newAccountHarness(t)
	busy, err := harness.controller.AccountWorkBusy(context.Background())
	if err != nil || busy {
		t.Fatalf("idle: %v %v", busy, err)
	}
	harness.page.busy = true
	if busy, err := harness.controller.AccountWorkBusy(context.Background()); err != nil || !busy {
		t.Fatalf("busy: %v %v", busy, err)
	}

	// An unreadable status is busy, not idle.
	harness.page.busyUnknown = true
	busy, err = harness.controller.AccountWorkBusy(context.Background())
	if !busy || err == nil {
		t.Fatalf("unknown was reported as idle: %v %v", busy, err)
	}

	harness.page.pages = []string{"kimi-1"}
	busy, err = harness.controller.AccountWorkBusy(context.Background())
	if !busy || err == nil {
		t.Fatalf("no Work shell was reported as idle: %v %v", busy, err)
	}

	// No page at all is the same answer.
	harness.page.pages = nil
	busy, err = harness.controller.AccountWorkBusy(context.Background())
	if !busy || err == nil {
		t.Fatalf("no page was reported as idle: %v %v", busy, err)
	}
}

func TestCaptureAfterRotationPrefersTheLivePageOverTheConfig(t *testing.T) {
	harness := newAccountHarness(t)
	// The official app rotated the tokens in the page and wrote its own copy to
	// config.json. A later capture must still take the page's values.
	harness.writeConfig(t, accountConfigFixture("user-1", "older-config-access", "older-config-refresh"))
	harness.page.put("kimi-1", "access_token", "rotated-access")
	harness.page.put("kimi-1", "refresh_token", "rotated-refresh")
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	restartAccountProcess = func(*Controller, context.Context) error { return nil }

	session, err := harness.controller.CaptureAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	values := accountStorageMap(session.LocalStorage)
	if values["access_token"] != "rotated-access" || values["refresh_token"] != "rotated-refresh" {
		t.Fatalf("captured stale tokens: %+v", values)
	}
	// Switching that snapshot back writes the rotated values, not the config copy.
	result, err := harness.controller.ActivateAccount(context.Background(), session)
	if err != nil || !result.Verified {
		t.Fatalf("activate: %+v %v", result, err)
	}
	credentials, _ := harness.readConfig(t)["credentials"].(map[string]any)
	web, _ := credentials["kimiWeb"].(map[string]any)
	if web["accessToken"] != "rotated-access" || web["refreshToken"] != "rotated-refresh" {
		t.Fatalf("the switch lost the rotation: %+v", web)
	}
}

func TestActivateAccountRejectsADiskOnlyMatch(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")

	// The page never follows, so only the config on disk would name the target.
	// That is not a success: it is the state that needs a restart, and when the
	// restart does not help either the switch has failed.
	harness.page.ignoreActivation = true
	restarts := 0
	restartAccountProcess = func(*Controller, context.Context) error {
		restarts++
		return errors.New(AccountErrRestartFailed)
	}
	target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "user-2",
		LocalStorage: []AccountStorageEntry{{Key: "msh_user_id", Value: "user-2"},
			{Key: "access_token", Value: "access-2"}}}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil {
		t.Fatalf("a recovered switch reported a hard failure: %v", err)
	}
	if result.Verified || result.Hot || result.Restarted || result.State != AccountStateRestored {
		t.Fatalf("result claims an effect: %+v", result)
	}
	if restarts != 1 {
		t.Fatalf("restarts: %d", restarts)
	}
	// The previous account is back, with its own tokens.
	credentials, _ := harness.readConfig(t)["credentials"].(map[string]any)
	web, _ := credentials["kimiWeb"].(map[string]any)
	if web["userId"] != "user-1" || web["accessToken"] != "access-1" {
		t.Fatalf("rollback did not restore the previous account: %+v", web)
	}
}

func TestActivateAccountReportsHardFailureWhenTheAppCannotBeRead(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")

	harness.page.ignoreActivation = true
	restartAccountProcess = func(*Controller, context.Context) error {
		return errors.New(AccountErrRestartFailed)
	}
	target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "user-2",
		LocalStorage: []AccountStorageEntry{{Key: "msh_user_id", Value: "user-2"}}}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.State != AccountStateRestored || result.RequiresLogin || result.Verified {
		t.Fatalf("result: %+v", result)
	}
	if result.Reason != AccountErrRestartFailed {
		t.Fatalf("reason: %q", result.Reason)
	}
}

func TestActivateAccountFallsBackToANormalRestartAndVerifies(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")

	// The page keeps reporting the previous account after the write and the
	// reload: this is the case only a relaunch can resolve.
	harness.page.ignoreActivation = true
	restarts := 0
	restartAccountProcess = func(*Controller, context.Context) error {
		restarts++
		harness.page.mu.Lock()
		harness.page.ignoreActivation = false
		harness.page.user[harness.page.shellPage] = harness.page.storage["kimi-1"]["msh_user_id"]
		harness.page.mu.Unlock()
		return nil
	}

	target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "user-2",
		LocalStorage: []AccountStorageEntry{{Key: "access_token", Value: "access-2"},
			{Key: "msh_user_id", Value: "user-2"}},
		KimiWeb: map[string]any{"deviceId": "device-2"}}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != AccountStateRestarted || !result.Restarted || !result.Verified || result.Hot {
		t.Fatalf("result: %+v", result)
	}
	if restarts != 1 {
		t.Fatalf("restarts: %d", restarts)
	}
	if AccountScope(harness.home) != sha256Hex("user-2") {
		t.Fatal("the config does not hold the target account after the restart")
	}
}

func TestActivateAccountRestoresThePreviousSnapshotWhenNothingVerifies(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "refresh_token", "refresh-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	harness.page.cookies = []AccountCookie{{Name: "kimi_session", Value: "session-1", Domain: ".kimi.com", Path: "/"}}

	harness.page.ignoreActivation = true
	restartAccountProcess = func(*Controller, context.Context) error { return nil }

	target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "user-2",
		LocalStorage: []AccountStorageEntry{{Key: "access_token", Value: "access-2"},
			{Key: "msh_user_id", Value: "user-2"}},
		Cookies: []AccountCookie{{Name: "kimi_session", Value: "session-2", Domain: ".kimi.com", Path: "/"}}}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil {
		if err.Error() != AccountErrRollbackFailed {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if result.State != AccountStateRestored || !result.RolledBack || result.Verified {
		t.Fatalf("result: %+v", result)
	}
	if AccountScope(harness.home) != sha256Hex("user-1") {
		t.Fatal("the previous account was not restored to the config")
	}
	if harness.page.user[harness.page.shellPage] != "user-1" {
		t.Fatalf("page identity after rollback: %q", harness.page.user[harness.page.shellPage])
	}
	credentials, _ := harness.readConfig(t)["credentials"].(map[string]any)
	web, _ := credentials["kimiWeb"].(map[string]any)
	if web["userId"] != "user-1" || web["accessToken"] != "access-1" {
		t.Fatalf("kimiWeb after rollback: %+v", web)
	}
}

func TestActivateAccountReportsNeedsLoginWhenTheTargetCannotSignIn(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	restartAccountProcess = func(*Controller, context.Context) error { return nil }

	// The target's tokens are expired, so the page ends signed out. It never
	// reports an identity again, and the previous snapshot cannot be restored
	// either: this is the "only that account signing in again helps" case.
	target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "user-2"}
	harness.page.mu.Lock()
	harness.page.storage["kimi-1"] = map[string]string{}
	harness.page.user = map[string]string{}
	harness.page.signedOut = true
	harness.page.mu.Unlock()

	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != AccountStateNeedsLogin || !result.RequiresLogin || result.RolledBack {
		t.Fatalf("result: %+v", result)
	}
}

func TestActivateAccountAbortsBeforeConfigAndRestartWhenTargetSignsOut(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "refresh_token", "refresh-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")

	harness.page.onActivation = func() {
		if harness.page.storage["kimi-1"]["msh_user_id"] == "user-2" {
			delete(harness.page.storage["kimi-1"], "access_token")
			delete(harness.page.storage["kimi-1"], "refresh_token")
			delete(harness.page.storage["kimi-1"], "msh_user_id")
			delete(harness.page.user, "work")
			delete(harness.page.user, "kimi-1")
		}
	}
	restarts := 0
	restartAccountProcess = func(*Controller, context.Context) error {
		restarts++
		return nil
	}

	target := AccountSession{
		SchemaVersion: AccountSessionSchema,
		UserID:        "user-2",
		LocalStorage: []AccountStorageEntry{
			{Key: "access_token", Value: "expired-access-2"},
			{Key: "refresh_token", Value: "expired-refresh-2"},
			{Key: "msh_user_id", Value: "user-2"},
		},
	}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil {
		t.Fatalf("expected hot rollback without hard error, got: %v", err)
	}
	if result.State != AccountStateRestored || !result.RolledBack || !result.RequiresLogin || result.Verified {
		t.Fatalf("unexpected activation result: %+v", result)
	}
	if restarts != 0 {
		t.Fatalf("restarts = %d, want 0 when target immediately signed out", restarts)
	}
	if !accountConfigMatches(harness.home, "user-1") {
		t.Fatal("config.json did not remain on user-1")
	}
	if harness.page.user["work"] != "user-1" {
		t.Fatalf("work shell user = %q, want user-1", harness.page.user["work"])
	}
}

func TestBeginAccountLoginClearsBothCredentialEntriesAndRestores(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	if err := os.WriteFile(filepath.Join(harness.home, AccountCodeKey), []byte(`{"apiKey":"code-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	harness.page.put("kimi-1", "kimi-work-draft", "draft text")
	harness.page.cookies = []AccountCookie{{Name: "kimi_session", Value: "session-1", Domain: ".kimi.com", Path: "/"}}

	// Capture first, exactly like the bridge does.
	saved, err := harness.controller.CaptureAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := harness.controller.BeginAccountLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if harness.page.callsFor("reauthenticate") != 1 || harness.page.reloads != 0 {
		t.Fatal("login did not use the official local reauthentication flow")
	}
	if _, ok := harness.page.storage["kimi-1"]["msh_user_id"]; ok {
		t.Fatal("msh_user_id survived the sign-out")
	}
	if _, ok := harness.page.storage["kimi-1"]["access_token"]; ok {
		t.Fatal("access_token survived the sign-out")
	}
	if harness.page.storage["kimi-1"]["kimi-work-draft"] != "draft text" {
		t.Fatal("the sign-out removed unrelated page state")
	}
	if len(harness.page.cookies) != 0 {
		t.Fatalf("cookies survived: %+v", harness.page.cookies)
	}
	credentials, _ := harness.readConfig(t)["credentials"].(map[string]any)
	if credentials != nil {
		if _, ok := credentials["kimiWeb"]; ok {
			t.Fatal("kimiWeb survived the sign-out")
		}
		// The kernel entry belongs to the account that signed out. Leaving it
		// behind keeps the Work kernel billing that account.
		if _, ok := credentials["kimiCode"]; ok {
			t.Fatal("kimiCode survived the sign-out")
		}
	}
	if _, err := os.Stat(filepath.Join(harness.home, AccountCodeKey)); !os.IsNotExist(err) {
		t.Fatal("the kernel key file survived the sign-out")
	}
	if harness.readConfig(t)["tabs"] == nil {
		t.Fatal("the sign-out dropped unrelated config")
	}

	// Cancelling a login is activating the saved snapshot again.
	restartAccountProcess = func(*Controller, context.Context) error { return nil }
	result, err := harness.controller.ActivateAccount(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.Hot {
		t.Fatalf("restore: %+v", result)
	}
	if AccountScope(harness.home) != sha256Hex("user-1") {
		t.Fatal("the saved account was not restored")
	}
}

func TestBeginAccountLoginFailurePreservesCredentials(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	harness.page.cookies = []AccountCookie{{Name: "kimi_session", Value: "session-1", Domain: ".kimi.com", Path: "/"}}
	harness.page.refuseLogin = true

	if err := harness.controller.BeginAccountLogin(context.Background()); err == nil || err.Error() != AccountErrUnsupported {
		t.Fatalf("login result: %v", err)
	}
	if len(harness.page.cookies) != 1 || harness.page.callsFor("remove-cookies:kimi-1") != 0 {
		t.Fatal("failed reauthentication removed the saved web session")
	}
	if harness.page.storage["kimi-1"]["access_token"] != "access-1" || !accountConfigMatches(harness.home, "user-1") {
		t.Fatal("failed reauthentication changed account credentials")
	}
}

func TestAccountActivationNeverLeaksSecretsOrPaths(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access-1", "refresh-1"))
	harness.page.put("kimi-1", "access_token", "access-1")
	harness.page.put("kimi-1", "msh_user_id", "user-1")

	restartAccountProcess = func(*Controller, context.Context) error {
		return errors.New(AccountErrRestartFailed)
	}
	failures := []AccountSession{
		{UserID: "", LocalStorage: []AccountStorageEntry{{Key: "access_token", Value: "x"}}},
		{UserID: "user-2", Cookies: []AccountCookie{{Name: "kimi_session", Value: "x", Domain: "kimi.com.evil.test", Path: "/"}}},
		{UserID: "user-2", Cookies: []AccountCookie{{Name: "_ga", Value: "x", Domain: ".kimi.com", Path: "/"}}},
		{UserID: "user-2", LocalStorage: []AccountStorageEntry{{Key: "kimi-work-draft", Value: "x"}}},
	}
	for _, session := range failures {
		_, err := harness.controller.ActivateAccount(context.Background(), session)
		if err == nil {
			t.Fatalf("an invalid snapshot was accepted: %+v", session)
		}
		if strings.Contains(err.Error(), "user-2") || strings.Contains(err.Error(), "evil") ||
			strings.Contains(err.Error(), harness.home) || strings.Contains(err.Error(), baseName(harness.home)) {
			t.Fatalf("error leaked context: %v", err)
		}
		if err.Error() != AccountErrSnapshotInvalid && err.Error() != AccountErrRestartFailed &&
			err.Error() != AccountErrRollbackFailed {
			t.Fatalf("error is not a fixed code: %v", err)
		}
	}

	// A result value names the account but never the tokens.
	target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "user-2",
		LocalStorage: []AccountStorageEntry{{Key: "access_token", Value: "access-2"}}}
	harness.page.ignoreActivation = true
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err == nil && result.State != "" {
		encoded, _ := json.Marshal(result)
		for _, forbidden := range []string{"access-2", "refresh-1", "session-1", harness.home, "{{SECRET_"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("the activation result leaked %q: %s", forbidden, encoded)
			}
		}
	}
}

func TestAccountStorageAndCookiePolicyIsAnAllowlist(t *testing.T) {
	allowedStorage := []string{"access_token", "refresh_token", "anonymous_access_token",
		"anonymous_refresh_token", "msh_user_id", "msh_user_subscription_data"}
	for _, key := range allowedStorage {
		if !isAccountStorageKey(key) {
			t.Fatalf("%q was refused", key)
		}
	}
	for _, key := range []string{"kimi-work-draft", "theme", "kimi-work-models-cache.json", "", "access-token"} {
		if isAccountStorageKey(key) {
			t.Fatalf("%q was accepted", key)
		}
	}
	for _, domain := range []string{"kimi.com", ".kimi.com", "www.kimi.com", "auth.kimi.com"} {
		if !isAccountCookieDomain(domain) {
			t.Fatalf("%q was refused", domain)
		}
	}
	for _, domain := range []string{"kimi.com.evil.test", "evil.test", "www.kimi.com.evil.test", ".kimi.com.evil.test"} {
		if isAccountCookieDomain(domain) {
			t.Fatalf("%q was accepted", domain)
		}
	}
	if !isAccountCookieName("kimi_session") || !isAccountCookieName("kimi-auth") ||
		!isAccountCookieName("xcsrftoken") {
		t.Fatal("a login cookie was refused")
	}
	if isAccountCookieName("_ga") || isAccountCookieName("_gid") || isAccountCookieName("sensorsdata_x") {
		t.Fatal("a measurement cookie was accepted")
	}
	for raw, want := range map[string]bool{
		"https://www.kimi.com/chat/abc":       true,
		"https://kimi.com/":                   true,
		"https://www.kimi.com.evil.test/chat": false,
		"http://www.kimi.com/chat":            false,
		"https://evil.test/chat":              false,
		"https://www.kimi.com:8443/chat":      false,
		"https://user:pass@www.kimi.com/chat": false,
	} {
		if _, ok := isOfficialAccountOrigin(raw); ok != want {
			t.Fatalf("%s -> %v, want %v", raw, ok, want)
		}
	}
}

func baseName(path string) string {
	pieces := strings.Split(path, string(filepath.Separator))
	return pieces[len(pieces)-1]
}

// The real machine failed this way: the identity lives on the Work shell page
// (app://localhost/kimi-agent.html, where preload-kimi-agent.mjs exposes
// kimiAgentAPI) while the cookie jar and the token store live on the kimi.com
// page. A surface that only knew the web origin could never read an identity,
// and the bridge rejected the capture as account_identity_unverified.
func TestCaptureAccountReadsIdentityFromTheWorkShellPage(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "access", "refresh"))
	// The web page holds the token store and the cookies.
	harness.page.put("kimi-1", "access_token", "access")
	harness.page.put("kimi-1", "refresh_token", "refresh")
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	harness.page.cookies = []AccountCookie{{Name: "kimi_session", Value: "s", Domain: ".kimi.com", Path: "/"}}
	// The shell page is what actually answers the official read.
	harness.page.mu.Lock()
	harness.page.officialIdentity = "user-1"
	harness.page.mu.Unlock()

	session, err := harness.controller.CaptureAccount(context.Background())
	if err != nil {
		t.Fatalf("capture on a Work-only home failed: %v", err)
	}
	if session.UserID != "user-1" || session.CapturedVia != AccountSourcePage {
		t.Fatalf("identity: %q via %q", session.UserID, session.CapturedVia)
	}
	if len(session.Cookies) != 1 || session.Cookies[0].Name != "kimi_session" {
		t.Fatalf("cookies: %+v", session.Cookies)
	}
	if values := accountStorageMap(session.LocalStorage); values["access_token"] != "access" {
		t.Fatalf("token store: %+v", values)
	}

	// AccountIdentity must be page verified so the bridge accepts the account.
	identity, err := harness.controller.AccountIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !identity.PageVerified || !identity.SignedIn || identity.Source != "page+config" {
		t.Fatalf("identity is not acceptable to the bridge: %+v", identity)
	}
}

// A credential write must never touch the shell page: it owns no kimi.com jar,
// and a cookie written there would be lost or, worse, attributed to the wrong
// origin.
func TestActivateAccountWritesCredentialsOnlyOnTheWebSurface(t *testing.T) {
	harness := newAccountHarness(t)
	harness.writeConfig(t, accountConfigFixture("user-1", "a", "r"))
	harness.page.put("kimi-1", "msh_user_id", "user-1")
	restartAccountProcess = func(*Controller, context.Context) error { return nil }

	target := AccountSession{SchemaVersion: AccountSessionSchema, UserID: "user-2",
		LocalStorage: []AccountStorageEntry{{Key: "msh_user_id", Value: "user-2"},
			{Key: "access_token", Value: "access-2"}},
		Cookies: []AccountCookie{{Name: "kimi_session", Value: "s2", Domain: ".kimi.com", Path: "/"}}}
	result, err := harness.controller.ActivateAccount(context.Background(), target)
	if err != nil || !result.Verified {
		t.Fatalf("activate: %+v %v", result, err)
	}
	for _, call := range []string{"set-cookies:work", "remove-cookies:work", "write-storage:work"} {
		if harness.page.callsFor(call) != 0 {
			t.Fatalf("%q ran on the shell page", call)
		}
	}
	if harness.page.callsFor("set-cookies:kimi-1") == 0 {
		t.Fatal("the credential surface was never written")
	}
	// The identity that verified is the one the shell page reports.
	if harness.page.callsFor("identity:work") == 0 {
		t.Fatal("the official identity read never reached the shell page")
	}
}

func makeTestJWT(expUnix int64, extra string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := fmt.Sprintf(`{"sub":"user-secret-sub","exp":%d%s}`, expUnix, extra)
	payload := base64.RawURLEncoding.EncodeToString([]byte(body))
	return header + "." + payload + ".signature-secret-tail"
}

func TestInspectCredentialLifetime(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	t.Run("healthy refresh dominates expired access and omits secrets", func(t *testing.T) {
		accessExp := now.Add(-1 * time.Hour)
		refreshExp := now.Add(100 * time.Hour)
		cookieExp := now.Add(240 * time.Hour)
		accessJWT := makeTestJWT(accessExp.Unix(), "")
		refreshJWT := makeTestJWT(refreshExp.Unix(), "")
		session := AccountSession{
			UserID: "user-1",
			LocalStorage: []AccountStorageEntry{
				{Key: "access_token", Value: accessJWT},
				{Key: "refresh_token", Value: refreshJWT},
			},
			Cookies: []AccountCookie{
				{Name: "kimi_session", Value: "cookie-secret-val", Domain: ".kimi.com", Path: "/", Expires: float64(cookieExp.Unix())},
				{Name: "kimi_session_temp", Value: "temp-val", Domain: ".kimi.com", Path: "/", Expires: -1},
				{Name: "csrf_token", Value: "csrf-val", Domain: ".kimi.com", Path: "/", Expires: float64(now.Add(1000 * time.Hour).Unix())},
			},
		}
		got := InspectCredentialLifetime(session, now)
		if got.AccessExpiresAt != accessExp.Format(time.RFC3339) {
			t.Fatalf("AccessExpiresAt = %q, want %q", got.AccessExpiresAt, accessExp.Format(time.RFC3339))
		}
		if got.RefreshExpiresAt != refreshExp.Format(time.RFC3339) {
			t.Fatalf("RefreshExpiresAt = %q, want %q", got.RefreshExpiresAt, refreshExp.Format(time.RFC3339))
		}
		if got.CookieExpiresAt != cookieExp.Format(time.RFC3339) {
			t.Fatalf("CookieExpiresAt = %q, want %q", got.CookieExpiresAt, cookieExp.Format(time.RFC3339))
		}
		if got.TokenHealth != TokenHealthHealthy {
			t.Fatalf("TokenHealth = %q, want %q", got.TokenHealth, TokenHealthHealthy)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{accessJWT, refreshJWT, "signature-secret-tail", "user-secret-sub", "cookie-secret-val"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("lifetime JSON leaked secret %q: %s", secret, string(encoded))
			}
		}
	})

	t.Run("refresh expiring within 72h", func(t *testing.T) {
		session := AccountSession{
			LocalStorage: []AccountStorageEntry{
				{Key: "access_token", Value: makeTestJWT(now.Add(200*time.Hour).Unix(), "")},
				{Key: "refresh_token", Value: makeTestJWT(now.Add(48*time.Hour).Unix(), "")},
			},
		}
		got := InspectCredentialLifetime(session, now)
		if got.TokenHealth != TokenHealthExpiringSoon {
			t.Fatalf("TokenHealth = %q, want %q", got.TokenHealth, TokenHealthExpiringSoon)
		}
	})

	t.Run("expired refresh overrides valid access", func(t *testing.T) {
		session := AccountSession{
			LocalStorage: []AccountStorageEntry{
				{Key: "access_token", Value: makeTestJWT(now.Add(48*time.Hour).Unix(), "")},
				{Key: "refresh_token", Value: makeTestJWT(now.Add(-5*time.Minute).Unix(), "")},
			},
		}
		got := InspectCredentialLifetime(session, now)
		if got.TokenHealth != TokenHealthExpired {
			t.Fatalf("TokenHealth = %q, want %q", got.TokenHealth, TokenHealthExpired)
		}
	})

	t.Run("access only thresholds", func(t *testing.T) {
		cases := []struct {
			name   string
			offset time.Duration
			want   string
		}{
			{"healthy", 48 * time.Hour, TokenHealthHealthy},
			{"expiring_soon", 12 * time.Hour, TokenHealthExpiringSoon},
			{"expired", -1 * time.Second, TokenHealthExpired},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				session := AccountSession{
					LocalStorage: []AccountStorageEntry{
						{Key: "access_token", Value: makeTestJWT(now.Add(tc.offset).Unix(), "")},
						{Key: "refresh_token", Value: "opaque-non-jwt-refresh"},
					},
				}
				got := InspectCredentialLifetime(session, now)
				if got.RefreshExpiresAt != "" {
					t.Fatalf("opaque refresh produced expiry: %+v", got)
				}
				if got.TokenHealth != tc.want {
					t.Fatalf("TokenHealth = %q, want %q", got.TokenHealth, tc.want)
				}
			})
		}
	})

	t.Run("opaque tokens and session cookies only are unknown", func(t *testing.T) {
		session := AccountSession{
			LocalStorage: []AccountStorageEntry{
				{Key: "access_token", Value: "opaque-access"},
				{Key: "refresh_token", Value: "opaque-refresh"},
			},
			Cookies: []AccountCookie{
				{Name: "kimi_session", Value: "v", Domain: ".kimi.com", Path: "/", Expires: 0},
				{Name: "kimi_auth", Value: "v", Domain: ".kimi.com", Path: "/", Expires: -1},
			},
		}
		got := InspectCredentialLifetime(session, now)
		if got.AccessExpiresAt != "" || got.RefreshExpiresAt != "" || got.CookieExpiresAt != "" || got.TokenHealth != TokenHealthUnknown {
			t.Fatalf("unexpected opaque lifetime: %+v", got)
		}
	})

	t.Run("cookie only health transitions", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			offset time.Duration
			want   string
		}{
			{"healthy", 72 * time.Hour, TokenHealthHealthy},
			{"expiring_soon", 6 * time.Hour, TokenHealthExpiringSoon},
			{"expired", -1 * time.Minute, TokenHealthExpired},
		} {
			t.Run(tc.name, func(t *testing.T) {
				session := AccountSession{
					Cookies: []AccountCookie{
						{Name: "kimi_auth", Value: "v", Domain: ".kimi.com", Path: "/", Expires: float64(now.Add(tc.offset).Unix())},
					},
				}
				got := InspectCredentialLifetime(session, now)
				if got.TokenHealth != tc.want {
					t.Fatalf("TokenHealth = %q, want %q", got.TokenHealth, tc.want)
				}
			})
		}
	})
}
