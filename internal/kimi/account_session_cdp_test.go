package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests exercise the production surface through a real CDP pipe, not
// through a Go stub: the method names, the session each call carries and the page
// expressions are the ones a live Kimi would receive.

type cdpRecorder struct {
	mu      sync.Mutex
	methods []string
	params  []map[string]any
	// sessions records the session id each request carried, "" for the browser
	// root.
	sessions []string
	// cookies is the jar the fake browser reports.
	cookies []any
	// evaluated is what every Runtime.evaluate answers.
	evaluated any
	// setCookieSuccess is what Network.setCookie reports for each call.
	setCookieSuccess []bool
	// refuseDelete makes Network.deleteCookies answer with a protocol error.
	refuseDelete bool
}

func (r *cdpRecorder) record(method string, params map[string]any, session string) {
	r.methods = append(r.methods, method)
	r.params = append(r.params, params)
	r.sessions = append(r.sessions, session)
}

func (r *cdpRecorder) count(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, item := range r.methods {
		if item == method {
			total++
		}
	}
	return total
}

func (r *cdpRecorder) find(method string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, item := range r.methods {
		if item == method {
			return r.params[i]
		}
	}
	return nil
}

func (r *cdpRecorder) sessionFor(method string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, item := range r.methods {
		if item == method {
			return r.sessions[i]
		}
	}
	return ""
}

// newCDPRecorderPipe wires a Pipe to a fake browser that answers the calls the
// account surface makes.
func newCDPRecorderPipe(t *testing.T, recorder *cdpRecorder) *Pipe {
	t.Helper()
	toHostReader, toHostWriter := io.Pipe()
	toPageReader, toPageWriter := io.Pipe()
	pipe := NewPipe(toHostReader, toPageWriter)

	handle := func(frame string) {
		var request struct {
			ID        int            `json:"id"`
			Method    string         `json:"method"`
			Params    map[string]any `json:"params"`
			SessionID string         `json:"sessionId"`
		}
		if json.Unmarshal([]byte(frame), &request) != nil {
			return
		}
		recorder.mu.Lock()
		recorder.record(request.Method, request.Params, request.SessionID)
		result := map[string]any{}
		session := request.SessionID
		switch request.Method {
		case "Target.getTargets":
			result["targetInfos"] = []any{
				map[string]any{"targetId": "work", "type": "page", "url": "app://localhost/kimi-agent.html"},
				map[string]any{"targetId": "claw", "type": "page", "url": "app://localhost/desktopclaw.html"},
				map[string]any{"targetId": "settings", "type": "page", "url": "app://localhost/work-settings.html"},
				map[string]any{"targetId": "chat", "type": "page", "url": "https://www.kimi.com/chat/abc"},
				map[string]any{"targetId": "root", "type": "page", "url": "https://kimi.com/"},
				map[string]any{"targetId": "lookalike", "type": "page", "url": "https://www.kimi.com.evil.test/chat"},
				map[string]any{"targetId": "plain", "type": "page", "url": "http://www.kimi.com/chat"},
				map[string]any{"targetId": "worker", "type": "worker", "url": "https://www.kimi.com/chat/abc"},
			}
		case "Target.attachToTarget":
			result["sessionId"] = "session-" + request.Params["targetId"].(string)
		case "Network.getAllCookies":
			result["cookies"] = recorder.cookies
		case "Network.setCookie":
			success := false
			if len(recorder.setCookieSuccess) > 0 {
				success = recorder.setCookieSuccess[0]
				recorder.setCookieSuccess = recorder.setCookieSuccess[1:]
			}
			result["success"] = success
		case "Network.deleteCookies":
			if recorder.refuseDelete {
				recorder.mu.Unlock()
				failure, _ := json.Marshal(map[string]any{"id": request.ID,
					"error": map[string]any{"code": -32601, "message": "Method not found"}})
				_, _ = toHostWriter.Write(append(failure, 0))
				return
			}
		case "Page.reload":
		case "Runtime.evaluate":
			value := recorder.evaluated
			result["result"] = map[string]any{"type": "object", "value": value}
		}
		recorder.mu.Unlock()
		response := map[string]any{"id": request.ID, "result": result}
		if session != "" {
			response["sessionId"] = session
		}
		encoded, _ := json.Marshal(response)
		_, _ = toHostWriter.Write(append(encoded, 0))
	}
	go func() {
		buffer := make([]byte, 0, 64<<10)
		chunk := make([]byte, 4096)
		for {
			n, err := toPageReader.Read(chunk)
			buffer = append(buffer, chunk[:n]...)
			for {
				at := strings.IndexByte(string(buffer), 0)
				if at < 0 {
					break
				}
				handle(string(buffer[:at]))
				buffer = buffer[at+1:]
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { pipe.Dispose() })
	return pipe
}

func TestCDPAccountPagesSplitIdentityAndCredentialSurfaces(t *testing.T) {
	recorder := &cdpRecorder{}
	actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pages, err := actions.accountPages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The Work shell page carries `kimiAgentAPI` and must be reachable; the two
	// kimi.com pages own the cookie jar. The lookalike, the plain-http page, the
	// worker and every other app:// path stay out.
	if len(pages) != 3 {
		t.Fatalf("pages: %+v", pages)
	}
	byOrigin := map[string]accountPage{}
	for _, page := range pages {
		byOrigin[page.origin] = page
		if page.sessionID == "" {
			t.Fatal("a page without a session was accepted")
		}
	}
	shell, ok := byOrigin["app://localhost"]
	if !ok {
		t.Fatal("the Work shell page, the only one carrying kimiAgentAPI, was not accepted")
	}
	if shell.credentials {
		t.Fatal("the Work shell page was treated as a cookie surface")
	}
	if shell.targetID != "work" {
		t.Fatalf("shell target: %q", shell.targetID)
	}
	for _, origin := range []string{"https://www.kimi.com", "https://kimi.com"} {
		page, ok := byOrigin[origin]
		if !ok {
			t.Fatalf("%s was not accepted", origin)
		}
		if !page.credentials {
			t.Fatalf("%s was not treated as a credential surface", origin)
		}
	}
	// Only the shell and the two web pages were attached to.
	if recorder.count("Target.attachToTarget") != 3 {
		t.Fatalf("attached to %d targets", recorder.count("Target.attachToTarget"))
	}
	// A second desktop page is not a stand-in for the shell.
	if _, ok := classifyAccountSurface("app://localhost/desktopclaw.html"); ok {
		t.Fatal("an unrelated desktop page was accepted as an identity surface")
	}
	if _, ok := classifyAccountSurface("app://localhost/kimi-agent.html#token"); ok {
		t.Fatal("a fragment on the shell page was accepted")
	}
}

func TestCDPAccountCookiesKeepTheLoginAndDropTheMeasurement(t *testing.T) {
	recorder := &cdpRecorder{cookies: []any{
		map[string]any{"name": "kimi_session", "value": "secret", "domain": ".kimi.com", "path": "/",
			"secure": true, "httpOnly": true, "sameSite": "Lax", "expires": 1800000000.0},
		map[string]any{"name": "_ga", "value": "analytics", "domain": ".kimi.com", "path": "/"},
		map[string]any{"name": "kimi_session", "value": "other", "domain": "evil.test", "path": "/"},
		map[string]any{"name": "kimi_session", "value": "", "domain": ".kimi.com", "path": "/"},
	}}
	actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	page := accountPage{targetID: "chat", sessionID: "session-chat", origin: "https://www.kimi.com"}

	cookies, err := actions.accountCookies(ctx, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 1 {
		t.Fatalf("cookies: %+v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != "kimi_session" || cookie.Value != "secret" || !cookie.HTTPOnly ||
		cookie.SameSite != "Lax" || cookie.Expires == 0 {
		t.Fatalf("cookie: %+v", cookie)
	}
	// The jar read belongs to the page session, not the browser root.
	if got := recorder.sessionFor("Network.getAllCookies"); got != "session-chat" {
		t.Fatalf("cookies were read on session %q", got)
	}
}

func TestCDPAccountCookieWritesUseThePageSessionAndCheckSuccess(t *testing.T) {
	recorder := &cdpRecorder{setCookieSuccess: []bool{true}}
	actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	page := accountPage{targetID: "chat", sessionID: "session-chat", origin: "https://www.kimi.com"}

	if err := actions.accountSetCookies(ctx, page, []AccountCookie{{Name: "kimi_session", Value: "v",
		Domain: ".kimi.com", Path: "/", Secure: true, HTTPOnly: true, SameSite: "Lax", Expires: 1800000000}}); err != nil {
		t.Fatal(err)
	}
	params := recorder.find("Network.setCookie")
	if params == nil {
		t.Fatal("no cookie was written")
	}
	if params["url"] != "https://kimi.com/" || params["sameSite"] != "Lax" || params["httpOnly"] != true {
		t.Fatalf("params: %+v", params)
	}
	if got := recorder.sessionFor("Network.setCookie"); got != "session-chat" {
		t.Fatalf("the cookie was written on session %q", got)
	}

	// A refused cookie answers success:false with no protocol error. That is a
	// failure: the token the switch depends on is not in the jar.
	refused := &cdpRecorder{setCookieSuccess: []bool{false}}
	refusedActions := NewCDPActions(newCDPRecorderPipe(t, refused), t.TempDir(), t.TempDir()).(*cdpActions)
	if err := refusedActions.accountSetCookies(ctx, page, []AccountCookie{{Name: "kimi_session",
		Value: "v", Domain: ".kimi.com", Path: "/"}}); err == nil {
		t.Fatal("a refused cookie was reported as written")
	}

	// No cookies written at all is also a failure.
	if err := actions.accountSetCookies(ctx, page, []AccountCookie{{Name: "kimi_session",
		Value: "v", Domain: ".kimi.com", Path: "/"}}); err == nil {
		t.Fatal("a jar that accepted nothing was reported as written")
	}

	if err := actions.accountRemoveCookies(ctx, page, []AccountCookie{{Name: "kimi_session", Domain: ".kimi.com", Path: "/"}}); err != nil {
		t.Fatal(err)
	}
	removal := recorder.find("Network.deleteCookies")
	if removal == nil || removal["domain"] != ".kimi.com" || removal["path"] != "/" {
		t.Fatalf("removal: %+v", removal)
	}
	if got := recorder.sessionFor("Network.deleteCookies"); got != "session-chat" {
		t.Fatalf("the cookie was deleted on session %q", got)
	}

	// A delete the browser rejects leaves the previous account's session alive.
	refuser := &cdpRecorder{refuseDelete: true}
	refuseActions := NewCDPActions(newCDPRecorderPipe(t, refuser), t.TempDir(), t.TempDir()).(*cdpActions)
	if err := refuseActions.accountRemoveCookies(ctx, page, []AccountCookie{{Name: "kimi_session",
		Domain: ".kimi.com", Path: "/"}}); err == nil {
		t.Fatal("a rejected delete was reported as done")
	}
}

func TestCDPAccountCookieWritesRejectPartialSuccess(t *testing.T) {
	for _, results := range [][]bool{{true, false}, {false, true}, {false, false}, {true, true}} {
		t.Run(fmt.Sprint(results), func(t *testing.T) {
			recorder := &cdpRecorder{setCookieSuccess: append([]bool(nil), results...)}
			actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			page := accountPage{targetID: "chat", sessionID: "session-chat", credentials: true}
			err := actions.accountSetCookies(ctx, page, []AccountCookie{
				{Name: "kimi-auth", Value: "auth", Domain: ".kimi.com", Path: "/"},
				{Name: "kimi_session", Value: "session", Domain: ".kimi.com", Path: "/"},
			})
			if complete := results[0] && results[1]; (err == nil) != complete {
				t.Fatalf("cookie completion %v returned %v", results, err)
			}
		})
	}
}

func TestCDPAccountCookieWritesPreserveDomainScope(t *testing.T) {
	for _, domain := range []string{".kimi.com", ".www.kimi.com", "www.kimi.com"} {
		t.Run(domain, func(t *testing.T) {
			recorder := &cdpRecorder{setCookieSuccess: []bool{true}}
			actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			page := accountPage{targetID: "chat", sessionID: "session-chat", credentials: true}
			err := actions.accountSetCookies(ctx, page, []AccountCookie{{Name: "kimi-auth", Value: "v", Domain: domain, Path: "/"}})
			if err != nil {
				t.Fatal(err)
			}
			params := recorder.find("Network.setCookie")
			if params["url"] != "https://"+strings.TrimPrefix(domain, ".")+"/" {
				t.Fatalf("source URL: %#v", params)
			}
			if strings.HasPrefix(domain, ".") {
				if params["domain"] != domain {
					t.Fatalf("domain cookie lost its scope: %#v", params)
				}
			} else if _, ok := params["domain"]; ok {
				t.Fatalf("host-only cookie became a domain cookie: %#v", params)
			}
		})
	}
}

func TestCDPAccountStorageExpressionsStayInsideTheAllowlist(t *testing.T) {
	recorder := &cdpRecorder{}
	actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	page := accountPage{targetID: "chat", sessionID: "session-chat", origin: "https://www.kimi.com"}

	recorder.evaluated = map[string]any{
		"access_token":     "a",
		"refresh_token":    "r",
		"msh_user_id":      "user-1",
		"kimi-work-draft":  "draft",
		"theme":            "dark",
		"kimi-work-models": "cache",
	}
	storage, err := actions.accountStorage(ctx, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(storage) != 3 || storage["access_token"] != "a" || storage["msh_user_id"] != "user-1" {
		t.Fatalf("storage: %+v", storage)
	}

	recorder.evaluated = true
	if err := actions.accountWriteStorage(ctx, page, map[string]string{
		"access_token": "a2", "kimi-work-draft": "nope"},
		[]string{"refresh_token", "theme"}); err != nil {
		t.Fatal(err)
	}
	expression, _ := recorder.params[len(recorder.params)-1]["expression"].(string)
	if !strings.Contains(expression, `store.setItem("access_token","a2")`) {
		t.Fatalf("the allowed key was not written: %s", expression)
	}
	for _, forbidden := range []string{"kimi-work-draft", "theme", "nope"} {
		if strings.Contains(expression, forbidden) {
			t.Fatalf("the expression touches %q: %s", forbidden, expression)
		}
	}
	if !strings.Contains(expression, `globalThis.__KIMI_DESKTOP_BRIDGE`) ||
		!strings.Contains(expression, `api.onLocalStorageChanged(changes)`) ||
		!strings.Contains(expression, `await api.getLocalStorageSnapshot()`) {
		t.Fatal("storage write did not synchronize and read back the official token store")
	}
	if !strings.Contains(expression, `store.removeItem("refresh_token")`) {
		t.Fatalf("the stale token was not removed: %s", expression)
	}

	// The write expression answers false when localStorage was unusable; that is
	// a failed write, not a done one.
	recorder.evaluated = false
	if err := actions.accountWriteStorage(ctx, page, map[string]string{"access_token": "a3"}, nil); err == nil {
		t.Fatal("a write that answered false was reported as done")
	}
	recorder.evaluated = map[string]any{"unexpected": true}
	if err := actions.accountWriteStorage(ctx, page, map[string]string{"access_token": "a4"}, nil); err == nil {
		t.Fatal("a write with no boolean answer was reported as done")
	}

	// A token containing a quote cannot break out of the JavaScript literal.
	recorder.evaluated = true
	if err := actions.accountWriteStorage(ctx, page, map[string]string{
		"access_token": `a"b;globalThis.pwned=1//`}, nil); err != nil {
		t.Fatal(err)
	}
	expression, _ = recorder.params[len(recorder.params)-1]["expression"].(string)
	if strings.Contains(expression, `store.setItem("access_token","a"b`) {
		t.Fatalf("the value was not escaped: %s", expression)
	}
	if !strings.Contains(expression, `a\"b`) {
		t.Fatalf("the value is not a quoted literal: %s", expression)
	}
}

func TestCDPAccountLoginUsesLocalReauthentication(t *testing.T) {
	recorder := &cdpRecorder{evaluated: true}
	actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
	page := accountPage{sessionID: "shell", origin: "app://localhost"}
	if err := actions.accountBeginLogin(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	expression, _ := recorder.params[len(recorder.params)-1]["expression"].(string)
	if !strings.Contains(expression, "accountV2.reauthenticate()") || strings.Contains(expression, ".logout(") {
		t.Fatal("login invalidated the saved server session")
	}
	recorder.evaluated = false
	if err := actions.accountBeginLogin(context.Background(), page); err == nil {
		t.Fatal("unconfirmed reauthentication reported success")
	}
}

func TestCDPAccountIdentityPrefersTheOfficialProfileAPI(t *testing.T) {
	recorder := &cdpRecorder{evaluated: map[string]any{"userId": "user-1", "accessToken": true,
		"refresh": false, "official": true}}
	actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	page := accountPage{targetID: "chat", sessionID: "session-chat", origin: "https://www.kimi.com"}

	identity, err := actions.accountIdentity(ctx, page)
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID != "user-1" || !identity.AccessToken || identity.Refresh || !identity.Official {
		t.Fatalf("identity: %+v", identity)
	}
	// The expression reads the official API first and the token store only through
	// the page's own globals: no DOM, no navigation, no fetch.
	expression, _ := recorder.params[len(recorder.params)-1]["expression"].(string)
	if !strings.Contains(expression, "getUserProfile") {
		t.Fatalf("the identity read does not use the official API: %s", expression)
	}
	for _, forbidden := range []string{"document", "querySelector", "location", "fetch("} {
		if strings.Contains(expression, forbidden) {
			t.Fatalf("the identity expression uses %q: %s", forbidden, expression)
		}
	}
	if err := actions.accountReload(ctx, []accountPage{page}); err != nil {
		t.Fatal(err)
	}
	if recorder.count("Page.reload") != 1 {
		t.Fatal("the page was not reloaded")
	}
}

func TestAccountStorageSyncsTokenPairBeforeReactivePageWrites(t *testing.T) {
	expression := accountStorageWriteExpression(map[string]string{"access_token": "expired-access", "refresh_token": "refresh-current"}, []string{"anonymous_access_token"})
	syncAt := strings.Index(expression, "api.onLocalStorageChanged(changes)")
	readbackAt := strings.Index(expression, "await api.getLocalStorageSnapshot()")
	for _, mutation := range []string{"store.setItem(", "store.removeItem("} {
		if at := strings.Index(expression, mutation); syncAt < 0 || readbackAt <= syncAt || at <= readbackAt {
			t.Fatalf("page mutation precedes the complete main-process credential pair: %s", mutation)
		}
	}
	for _, field := range []string{`changes["access_token"]="expired-access"`, `changes["refresh_token"]="refresh-current"`} {
		if at := strings.Index(expression, field); at < 0 || at >= syncAt {
			t.Fatal("main-process snapshot is incomplete")
		}
	}
}

func TestCDPAccountWorkBusyReadsTheOfficialStatusTable(t *testing.T) {
	recorder := &cdpRecorder{evaluated: map[string]any{"running": true, "known": true}}
	actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	page := accountPage{targetID: "chat", sessionID: "session-chat", origin: "https://www.kimi.com"}

	busy, err := actions.accountWorkBusy(ctx, page)
	if err != nil || !busy {
		t.Fatalf("busy: %v %v", busy, err)
	}
	expression, _ := recorder.params[len(recorder.params)-1]["expression"].(string)
	if !strings.Contains(expression, "getConversationStatuses") {
		t.Fatalf("the busy read does not use the official table: %s", expression)
	}

	// A table this code cannot interpret is busy, not idle.
	recorder.evaluated = map[string]any{"running": false, "known": false}
	if busy, err := actions.accountWorkBusy(ctx, page); err != nil || !busy {
		t.Fatalf("unknown was reported as idle: %v %v", busy, err)
	}
	// Nothing at all is the same answer, and it is an error.
	recorder.evaluated = nil
	if busy, err := actions.accountWorkBusy(ctx, page); !busy || err == nil {
		t.Fatalf("no answer was reported as idle: %v %v", busy, err)
	}
	// An idle table is idle.
	recorder.evaluated = map[string]any{"running": false, "known": true}
	if busy, err := actions.accountWorkBusy(ctx, page); err != nil || busy {
		t.Fatalf("idle: %v %v", busy, err)
	}
}

// The shell page is the only surface with kimiAgentAPI, and its profile call can
// answer null while the desktop is still fetching. The identity read therefore
// also asks the official token callback the desktop itself uses, and only those
// two answers count as official.
func TestCDPAccountIdentityUsesOnlyOfficialAnswers(t *testing.T) {
	recorder := &cdpRecorder{evaluated: map[string]any{"userId": "user-1", "accessToken": true,
		"refresh": true, "official": true, "anonymous": false, "name": "Ada", "avatarURL": "https://x/a.png"}}
	actions := NewCDPActions(newCDPRecorderPipe(t, recorder), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	page := accountPage{targetID: "work", sessionID: "session-work", origin: "app://localhost"}

	identity, err := actions.accountIdentity(ctx, page)
	if err != nil {
		t.Fatal(err)
	}
	if identity.UserID != "user-1" || !identity.Official || identity.Anonymous {
		t.Fatalf("identity: %+v", identity)
	}
	if identity.Name != "Ada" || identity.AvatarURL != "https://x/a.png" {
		t.Fatalf("metadata: %+v", identity)
	}

	expression, _ := recorder.params[len(recorder.params)-1]["expression"].(string)
	// Both official sources are present, and neither invents a network call.
	if !strings.Contains(expression, "getUserProfile") {
		t.Fatalf("the profile read is missing: %s", expression)
	}
	if !strings.Contains(expression, "collectEvent") || !strings.Contains(expression, "getToken") {
		t.Fatalf("the desktop's own token readback is missing: %s", expression)
	}
	for _, forbidden := range []string{"fetch(", "XMLHttpRequest", "document.cookie"} {
		if strings.Contains(expression, forbidden) {
			t.Fatalf("the identity expression uses %q: %s", forbidden, expression)
		}
	}

	// A signed-out shell answers anonymous with no id, which is not an identity.
	recorder.evaluated = map[string]any{"userId": "", "official": true, "anonymous": true}
	signedOut, err := actions.accountIdentity(ctx, page)
	if err != nil {
		t.Fatal(err)
	}
	if signedOut.UserID != "" || !signedOut.Anonymous {
		t.Fatalf("signed-out identity: %+v", signedOut)
	}
}

// The desktop builds the shell URL with query parameters, so the classifier must
// accept exactly that shape and still refuse a fragment or a different page.
func TestCDPAccountSurfaceClassificationMatchesTheRealShellURL(t *testing.T) {
	shell := "app://localhost/kimi-agent.html?theme=dark&locale=zh&platform=darwin&oversea=false" +
		"&kimiOrigin=https%3A%2F%2Fwww.kimi.com&titlebarHeight=40&chatFontLevel=0"
	kind, ok := classifyAccountSurface(shell)
	if !ok {
		t.Fatalf("the real shell URL was refused: %s", shell)
	}
	if kind.credentials {
		t.Fatal("the shell URL was treated as a cookie surface")
	}
	if kind.origin != "app://localhost" {
		t.Fatalf("origin: %q", kind.origin)
	}
	for _, raw := range []string{
		"app://localhost/kimi-agent.html#frag",
		"app://localhost/desktopclaw.html",
		"app://localhost/work-settings.html",
		"app://evil.test/kimi-agent.html",
		"https://www.kimi.com.evil.test/kimi-agent.html",
		"http://www.kimi.com/chat",
	} {
		if _, ok := classifyAccountSurface(raw); ok {
			t.Fatalf("%q was accepted", raw)
		}
	}
	// A shell query cannot smuggle a second origin into the credential rule.
	if _, ok := classifyAccountSurface("https://www.kimi.com/kimi-agent.html?x=1"); !ok {
		t.Fatal("a kimi.com page was refused")
	}
}
