package kimi

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// This file is the production accountSurface. It drives two official surfaces
// over the CDP pipe the rest of the native integration uses.
//
// The identity surface is the Work shell page: `kimiAgentView` loads
// `app://localhost/kimi-agent.html` with `preload-kimi-agent.mjs`, which is the
// only preload that exposes `window.kimiAgentAPI`. The desktop's own account
// readback (`GET_TOKEN_SCRIPT` → `collectEvent('getToken')` →
// `pollKimiViewTokenForUser`) runs on that same view, so an identity read that
// skips it can never agree with the desktop.
//
// The credential surface is the signed-in web page on kimi.com. Cookies belong
// there and nowhere else.
//
// `Network` is a page domain: a call must carry the session of the page whose
// jar it means to touch, not the browser root.

const accountActionTimeout = actionCallTimeout

// accountPages lists both surfaces. An entry that is only an identity surface
// reports `credentials: false` and is never written to.
func (a *cdpActions) accountPages(ctx context.Context) ([]accountPage, error) {
	result, err := a.pipe.Send(ctx, "Target.getTargets", map[string]any{}, "", accountActionTimeout)
	if err != nil {
		return nil, err
	}
	rows, _ := result.Result["targetInfos"].([]any)
	var out []accountPage
	for _, row := range rows {
		target, ok := row.(map[string]any)
		if !ok {
			continue
		}
		targetID, _ := target["targetId"].(string)
		kind, _ := target["type"].(string)
		raw, _ := target["url"].(string)
		if targetID == "" || kind != "page" {
			continue
		}
		surface, ok := classifyAccountSurface(raw)
		if !ok {
			continue
		}
		sessionID, err := a.session(ctx, targetID)
		if err != nil {
			continue
		}
		out = append(out, accountPage{targetID: targetID, sessionID: sessionID,
			origin: surface.origin, credentials: surface.credentials})
	}
	return out, nil
}

// accountSurfaceKind is which of the two official surfaces a target is and what
// may be done on it.
type accountSurfaceKind struct {
	origin      string
	credentials bool
}

// classifyAccountSurface accepts exactly two surfaces and nothing else:
//
//   - `app://localhost/kimi-agent.html`, the Work shell. It is where
//     `preload-kimi-agent.mjs` exposes `kimiAgentAPI`, so identity and busy state
//     are read here. It carries no kimi.com cookie jar.
//   - an https page on www.kimi.com or kimi.com by exact origin, which owns the
//     cookie jar the account signs in with.
//
// A lookalike host fails the comparison. No other app:// page is accepted, so a
// second desktop page cannot stand in for the shell.
func classifyAccountSurface(raw string) (accountSurfaceKind, bool) {
	if parsed, err := parseURL(raw); err == nil && parsed.User == nil && parsed.Fragment == "" &&
		parsed.Scheme == "app" && parsed.Host == "localhost" && parsed.Path == "/kimi-agent.html" {
		return accountSurfaceKind{origin: "app://localhost", credentials: false}, true
	}
	origin, ok := isOfficialAccountOrigin(raw)
	if !ok {
		return accountSurfaceKind{}, false
	}
	return accountSurfaceKind{origin: origin, credentials: true}, true
}

// accountCredentialPages drops the identity-only surfaces, so a cookie write can
// never land on the Work shell page.
func accountCredentialPages(pages []accountPage) []accountPage {
	out := make([]accountPage, 0, len(pages))
	for _, page := range pages {
		if page.credentials {
			out = append(out, page)
		}
	}
	return out
}

// accountCookies reads the jar through one page's session and keeps only the
// kimi.com login cookies. Measurement cookies on the same domain are dropped by
// name.
func (a *cdpActions) accountCookies(ctx context.Context, page accountPage) ([]AccountCookie, error) {
	if page.sessionID == "" {
		return nil, errors.New(AccountErrUnavailable)
	}
	result, err := a.pipe.Send(ctx, "Network.getAllCookies", map[string]any{}, page.sessionID, accountActionTimeout)
	if err != nil {
		return nil, err
	}
	rows, _ := result.Result["cookies"].([]any)
	out := make([]AccountCookie, 0, len(rows))
	for _, row := range rows {
		cookie, ok := row.(map[string]any)
		if !ok {
			continue
		}
		name, _ := cookie["name"].(string)
		domain, _ := cookie["domain"].(string)
		if !isAccountCookieName(name) || !isAccountCookieDomain(domain) {
			continue
		}
		value, _ := cookie["value"].(string)
		if value == "" || len(value) > 8192 {
			continue
		}
		path, _ := cookie["path"].(string)
		if path == "" {
			path = "/"
		}
		entry := AccountCookie{Name: name, Value: value, Domain: domain, Path: path}
		if expires, ok := cookie["expires"].(float64); ok && expires > 0 {
			entry.Expires = expires
		}
		entry.Secure, _ = cookie["secure"].(bool)
		entry.HTTPOnly, _ = cookie["httpOnly"].(bool)
		entry.SameSite, _ = cookie["sameSite"].(string)
		out = append(out, entry)
		if len(out) >= maxAccountCookies {
			break
		}
	}
	return out, nil
}

// accountSetCookies writes through the page's own session so the jar that is
// later read back is the same one that was written. `Network.setCookie` reports
// a refused cookie as `success:false` with no protocol error, so a write is only
// counted when that flag is true; every cookie must be accepted.
func (a *cdpActions) accountSetCookies(ctx context.Context, page accountPage, cookies []AccountCookie) error {
	if len(cookies) == 0 {
		return nil
	}
	if page.sessionID == "" {
		return errors.New(AccountErrUnavailable)
	}
	var lastErr error
	written := 0
	for _, cookie := range cookies {
		params := map[string]any{
			"name":     cookie.Name,
			"value":    cookie.Value,
			"path":     cookie.Path,
			"secure":   cookie.Secure,
			"httpOnly": cookie.HTTPOnly,
			"url":      "https://" + strings.TrimPrefix(cookie.Domain, ".") + cookie.Path,
		}
		if strings.HasPrefix(cookie.Domain, ".") {
			params["domain"] = cookie.Domain
		}
		if cookie.Expires > 0 {
			params["expires"] = cookie.Expires
		}
		if site := cookieSameSite(cookie.SameSite); site != "" {
			params["sameSite"] = site
		}
		result, err := a.pipe.Send(ctx, "Network.setCookie", params, page.sessionID, accountActionTimeout)
		if err != nil {
			lastErr = err
			continue
		}
		if success, ok := result.Result["success"].(bool); !ok || !success {
			lastErr = errors.New(AccountErrWriteFailed)
			continue
		}
		written++
	}
	if written != len(cookies) {
		if lastErr == nil {
			lastErr = errors.New(AccountErrWriteFailed)
		}
		return lastErr
	}
	return nil
}

// accountRemoveCookies deletes through the page's session. A cookie the browser
// refuses to delete would otherwise leave the previous account's session alive,
// so any failure here is reported instead of skipped.
func (a *cdpActions) accountRemoveCookies(ctx context.Context, page accountPage, cookies []AccountCookie) error {
	if len(cookies) == 0 {
		return nil
	}
	if page.sessionID == "" {
		return errors.New(AccountErrUnavailable)
	}
	var lastErr error
	removed := 0
	for _, cookie := range cookies {
		_, err := a.pipe.Send(ctx, "Network.deleteCookies",
			map[string]any{"name": cookie.Name, "domain": cookie.Domain, "path": cookie.Path},
			page.sessionID, accountActionTimeout)
		if err != nil {
			lastErr = err
			continue
		}
		removed++
	}
	if removed != len(cookies) {
		if lastErr == nil {
			lastErr = errors.New(AccountErrWriteFailed)
		}
		return lastErr
	}
	return nil
}

// accountStorage reads the page's own localStorage through the page. Only the
// whitelisted keys leave the page.
func (a *cdpActions) accountStorage(ctx context.Context, page accountPage) (map[string]string, error) {
	value, err := a.evaluate(ctx, page.sessionID, accountStorageReadExpression)
	if err != nil {
		return nil, err
	}
	raw, _ := value.(map[string]any)
	out := make(map[string]string, len(raw))
	for key, item := range raw {
		text, ok := item.(string)
		if !ok || !isAccountStorageKey(key) || text == "" {
			continue
		}
		if len(out) >= maxAccountStorageEntries {
			break
		}
		out[key] = text
	}
	return out, nil
}

// accountWriteStorage sets and removes keys in one page evaluation. The payload
// is bounded before it is sent. The expression answers false when localStorage
// was unusable, and that answer is a failure: the tokens were not written.
func (a *cdpActions) accountWriteStorage(ctx context.Context, page accountPage, set map[string]string, remove []string) error {
	allowed := make(map[string]string, len(set))
	total := 0
	for key, value := range set {
		if !isAccountStorageKey(key) {
			continue
		}
		allowed[key] = value
		total += len(key) + len(value)
	}
	for _, key := range remove {
		if isAccountStorageKey(key) {
			total += len(key)
		}
	}
	if total > maxAccountStoragePayload {
		return errors.New(AccountErrSnapshotInvalid)
	}
	if len(allowed) == 0 && len(remove) == 0 {
		return nil
	}
	expression := accountStorageWriteExpression(allowed, remove)
	value, err := a.evaluate(ctx, page.sessionID, expression)
	if err != nil {
		return err
	}
	if ok, isBool := value.(bool); !isBool || !ok {
		return errors.New(AccountErrWriteFailed)
	}
	return nil
}

// accountReload reloads the official pages so they re-derive their session from
// the cookies and storage that were just written.
func (a *cdpActions) accountReload(ctx context.Context, pages []accountPage) error {
	var lastErr error
	reloaded := 0
	for _, page := range pages {
		if _, err := a.pipe.Send(ctx, "Page.reload",
			map[string]any{"ignoreCache": true}, page.sessionID, accountActionTimeout); err != nil {
			lastErr = err
			continue
		}
		reloaded++
	}
	if reloaded != len(pages) {
		if lastErr == nil {
			lastErr = errors.New(AccountErrPageTimeout)
		}
		return lastErr
	}
	return nil
}

// accountIdentity reads the account the official page itself reports.
//
// It prefers `window.kimiAgentAPI.getUserProfile()`, the official read: the
// desktop answers it from the profile it bound to the view, and its `id` is the
// user that view is actually acting as. Only when that API is absent does it
// fall back to the page's own token store, and the fallback is flagged so a
// caller can refuse to call a switch verified on it.
func (a *cdpActions) accountIdentity(ctx context.Context, page accountPage) (accountPageIdentity, error) {
	value, err := a.evaluate(ctx, page.sessionID, accountIdentityExpression)
	if err != nil {
		return accountPageIdentity{}, err
	}
	raw, _ := value.(map[string]any)
	identity := accountPageIdentity{}
	if user, ok := raw["userId"].(string); ok && validAccountID(user) {
		identity.UserID = user
	}
	identity.AccessToken, _ = raw["accessToken"].(bool)
	identity.Refresh, _ = raw["refresh"].(bool)
	identity.Official, _ = raw["official"].(bool)
	identity.Anonymous, _ = raw["anonymous"].(bool)
	identity.Name, _ = raw["name"].(string)
	identity.AvatarURL, _ = raw["avatarURL"].(string)
	return identity, nil
}

func (a *cdpActions) accountBeginLogin(ctx context.Context, page accountPage) error {
	if page.credentials {
		return errors.New(AccountErrUnsupported)
	}
	value, err := a.evaluate(ctx, page.sessionID, accountBeginLoginExpression)
	if err != nil {
		return errors.New(AccountErrPageTimeout)
	}
	if ok, _ := value.(bool); !ok {
		return errors.New(AccountErrUnsupported)
	}
	return nil
}

const accountBeginLoginExpression = `(async function(){
var api = globalThis.kimiAgentAPI;
if (!api || !api.accountV2 || typeof api.accountV2.reauthenticate !== "function") return false;
var result = await api.accountV2.reauthenticate();
return Boolean(result && result.ok === true && result.data && result.data.confirmed === true);
})()`

func cookieSameSite(value string) string {
	switch strings.ToLower(value) {
	case "strict":
		return "Strict"
	case "lax":
		return "Lax"
	case "none", "no_restriction", "unspecified":
		return "None"
	}
	return ""
}

// The identity expressions run on the Work shell page, the only one that has
// `window.kimiAgentAPI`. They never navigate and never call fetch themselves.

// accountIdentityExpression answers the account the shell page is acting as.
//
// It prefers `kimiAgentAPI.getUserProfile()`. That call can answer null while
// the profile is being fetched from the network, which is why the desktop's own
// readback (`GET_TOKEN_SCRIPT` → `collectEvent('getToken')`) is the second
// source: it reports `user_unique_id`, the identity the page reports to the
// analytics SDK, and the desktop compares that same value against
// `credentials.kimiWeb.userId`. Only those two official answers set `official`;
// the token store is reported separately and never upgrades a read.
const accountIdentityExpression = `(function(){
var out = { userId: "", accessToken: false, refresh: false, official: false, anonymous: false };
function flags(){
  try {
    var store = globalThis.localStorage;
    out.accessToken = Boolean(store && store.getItem("access_token"));
    out.refresh = Boolean(store && store.getItem("refresh_token"));
  } catch (error) {}
}
function fromProfile(profile){
  if (!profile || typeof profile !== "object") return false;
  out.official = true;
  out.anonymous = profile.isAnonymous === true;
  if (typeof profile.id === "string" && profile.id && !out.anonymous) {
    out.userId = profile.id;
    if (typeof profile.nickname === "string") out.name = profile.nickname;
    if (typeof profile.avatarUrl === "string") out.avatarURL = profile.avatarUrl;
  }
  return true;
}
function fromTokenInfo(info){
  if (!info || typeof info !== "object") return false;
  var id = typeof info.user_unique_id === "string" ? info.user_unique_id
    : (typeof info.userId === "string" ? info.userId : "");
  if (!id) return false;
  out.userId = id;
  out.official = true;
  return true;
}
function collectToken(){
  return new Promise(function(resolve){
    if (typeof globalThis.collectEvent !== "function") { resolve(false); return; }
    var settled = false;
    var timer = setTimeout(function(){ if (!settled) { settled = true; resolve(false); } }, 3000);
    try {
      globalThis.collectEvent("getToken", function(info){
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve(fromTokenInfo(info));
      });
    } catch (error) {
      if (!settled) { settled = true; clearTimeout(timer); resolve(false); }
    }
  });
}
try {
  flags();
  var api = globalThis.kimiAgentAPI;
  var profileCall = (api && typeof api.getUserProfile === "function")
    ? Promise.resolve(api.getUserProfile()).then(fromProfile, function(){ return false; })
    : Promise.resolve(false);
  return profileCall.then(async function(answered){
    if (api && typeof api.getInitialAuthTokens === "function") {
      var tokens = await api.getInitialAuthTokens();
      if (tokens && typeof tokens === "object" && Object.prototype.hasOwnProperty.call(tokens, "access_token")) {
        out.accessToken = Boolean(tokens.access_token);
        out.refresh = Boolean(tokens.refresh_token);
        if (!out.accessToken) {
          out.userId = "";
          out.official = true;
          out.anonymous = true;
          return out;
        }
      }
    } else if (globalThis.__KIMI_DESKTOP_BRIDGE && typeof globalThis.__KIMI_DESKTOP_BRIDGE.getLocalStorageSnapshot === "function") {
      try {
        var snap = await globalThis.__KIMI_DESKTOP_BRIDGE.getLocalStorageSnapshot();
        if (Array.isArray(snap)) {
          var accessRow = snap.find(function(row){ return row && row.key === "access_token"; });
          var refreshRow = snap.find(function(row){ return row && row.key === "refresh_token"; });
          if (accessRow) out.accessToken = Boolean(accessRow.value);
          if (refreshRow) out.refresh = Boolean(refreshRow.value);
        }
      } catch (error) {}
    }
    if (answered || !out.accessToken) return out;
    return collectToken().then(function(){ return out; });
  });
} catch (error) {}
return out;
})()`

const accountStorageReadExpression = `(async function(){
try {
  var exact = {
    "access_token": 1, "refresh_token": 1, "anonymous_access_token": 1,
    "anonymous_refresh_token": 1, "msh_user_id": 1, "msh_user_subscription_data": 1
  };
  function isAllowed(key){
    return Boolean(key) && (Object.prototype.hasOwnProperty.call(exact, key) ||
      key.indexOf("msh_user_") === 0 || /_token$/.test(key));
  }
  var out = {};
  var store = globalThis.localStorage;
  if (store) {
    for (var index = 0; index < store.length; index++) {
      var key = store.key(index);
      if (!isAllowed(key)) continue;
      var value = store.getItem(key);
      if (typeof value === "string" && value) out[key] = value;
    }
  }
  var api = globalThis.__KIMI_DESKTOP_BRIDGE;
  if (api && typeof api.getLocalStorageSnapshot === "function") {
    try {
      var snapshot = await api.getLocalStorageSnapshot();
      if (Array.isArray(snapshot)) {
        for (var i = 0; i < snapshot.length; i++) {
          var row = snapshot[i];
          if (!row || typeof row !== "object" || !isAllowed(row.key)) continue;
          if (typeof row.value === "string" && row.value) out[row.key] = row.value;
          else if (row.value === null) delete out[row.key];
        }
      }
    } catch (error) {}
  }
  return out;
} catch (error) { return {}; }
})()`

// accountStorageWriteExpression renders one bounded write. Keys and values are
// quoted by quoteJS, so a stored token cannot break out of the literal.
func accountStorageWriteExpression(set map[string]string, remove []string) string {
	var out strings.Builder
	out.WriteString(`(async function(){try{var store=globalThis.localStorage;if(!store)return false;var changes={};var api=globalThis.__KIMI_DESKTOP_BRIDGE;if(!api||typeof api.onLocalStorageChanged!=="function"||typeof api.getLocalStorageSnapshot!=="function")return false;`)
	for _, key := range sortedAccountKeys(set) {
		out.WriteString(`changes[` + quoteJS(key) + `]=` + quoteJS(set[key]) + `;`)
	}
	for _, key := range remove {
		if isAccountStorageKey(key) {
			out.WriteString(`changes[` + quoteJS(key) + `]=null;`)
		}
	}
	out.WriteString(`api.onLocalStorageChanged(changes);var snapshot=await api.getLocalStorageSnapshot();if(!Array.isArray(snapshot))return false;for(var key of ["access_token","refresh_token"]){if(!Object.prototype.hasOwnProperty.call(changes,key))continue;var item=snapshot.find(function(row){return row.key===key;});if((item?item.value:null)!==changes[key])return false;}`)
	for _, key := range sortedAccountKeys(set) {
		out.WriteString(`store.setItem(` + quoteJS(key) + `,` + quoteJS(set[key]) + `);`)
	}
	for _, key := range remove {
		if isAccountStorageKey(key) {
			out.WriteString(`store.removeItem(` + quoteJS(key) + `);`)
		}
	}
	out.WriteString(`return true;}catch(error){return false;}})()`)
	return out.String()
}

func sortedAccountKeys(set map[string]string) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (a *cdpActions) accountWorkBusy(ctx context.Context, page accountPage) (bool, error) {
	value, err := a.evaluate(ctx, page.sessionID, accountWorkBusyExpression)
	if err != nil {
		return true, err
	}
	raw, _ := value.(map[string]any)
	if raw == nil {
		return true, errors.New(AccountErrUnavailable)
	}
	running, ok := raw["running"].(bool)
	if !ok {
		return true, errors.New(AccountErrUnavailable)
	}
	known, _ := raw["known"].(bool)
	if !known {
		return true, nil
	}
	return running, nil
}

// accountWorkBusyExpression reads the conversation status table and counts the
// entries that report an active turn. A table this code cannot read answers
// known:false, which the caller treats as busy.
const accountWorkBusyExpression = `(function(){
var out = { running: false, known: false };
try {
  var api = globalThis.kimiAgentAPI;
  if (!api || typeof api.getConversationStatuses !== "function") return Promise.resolve(out);
  return Promise.resolve(api.getConversationStatuses()).then(function(table){
    if (!table || typeof table !== "object") return out;
    if (Object.prototype.hasOwnProperty.call(table, "statuses")) table = table.statuses;
    if (!table || typeof table !== "object") return out;
    var entries = Array.isArray(table) ? table : Object.keys(table).map(function(key){ return table[key]; });
    for (var index = 0; index < entries.length; index++) {
      var entry = entries[index];
      var status = typeof entry === "string" ? entry.toLowerCase() : "";
      var recognized = false;
      if (entry && typeof entry === "object") {
        for (var name of ["running", "isRunning", "busy"]) {
          if (entry[name] === true) return { running: true, known: true };
          recognized = recognized || entry[name] === false;
        }
        status = typeof entry.status === "string" ? entry.status.toLowerCase() : "";
      }
      if (["running", "streaming", "compacting", "busy"].includes(status)) return { running: true, known: true };
      recognized = recognized || ["idle", "completed", "blocked", "failed", "cancelled", "done", "stopped"].includes(status);
      if (!recognized) return out;
    }
    return { running: false, known: true };
  }, function(){ return out; });
} catch (error) { return out; }
})()`
