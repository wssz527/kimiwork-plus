package kimi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account switching moves credentials between the official app's live page, the
// official `config.json` and the Work kernel's own key file.
//
// Two rules are not negotiable, because breaking either bills the wrong account:
// the page's token store is what the running app uses (the config on disk can be
// a rotation behind), and a switch is only verified from a readback of the
// target identity — never from having written it.
//
// The secret fields of AccountSession belong in the caller's keychain. They are
// never rendered by a status value and never appear in an error: every failure
// here is one fixed code with no path, token or response body in it.

const (
	// AccountSessionSchema is the keychain payload version.
	AccountSessionSchema = 1
	// AccountCodeKey is the Work kernel's key file inside the Work data home.
	AccountCodeKey = "kimi-code-key.json"
	// AccountKimiWeb is the official credential entry for the signed-in account.
	AccountKimiWeb = "kimiWeb"
	// AccountKimiCode is the official credential entry the Work kernel reads.
	AccountKimiCode = "kimiCode"
)

const (
	maxAccountCookies        = 32
	maxAccountStorageEntries = 24
	maxAccountSecretBytes    = 192 << 10
	maxAccountStoragePayload = 16 << 10
	maxAccountNameBytes      = 256
	maxAccountAvatarBytes    = 2048
	maxAccountCodeKeyBytes   = 256 << 10
)

// Readback limits are variables so tests can use shorter waits.
var (
	accountVerifyRetries  = 10
	accountVerifyStep     = 300 * time.Millisecond
	accountRuntimeRetries = 40
	accountRuntimeTimeout = 12 * time.Second
)

// Activation states. `hot` and `restarted` claim an effect, and only after a
// readback of the target identity.
const (
	// AccountStateHot means the official shell and committed runtime adopted the
	// target account without restarting the desktop process.
	AccountStateHot = "hot"
	// AccountStateRestarted means the hot path did not verify, the application
	// exited normally and was relaunched, and the readback then passed.
	AccountStateRestarted = "restarted"
	// AccountStateNeedsLogin means the target account's credentials did not take
	// and the page ended signed out.
	AccountStateNeedsLogin = "needs-login"
	// AccountStateRestored means activation did not verify and the previous
	// snapshot was written back and verified.
	AccountStateRestored = "restored"
	// AccountStateFailed means nothing verified and the rollback did not either.
	AccountStateFailed = "failed"
)

// Identity sources.
const (
	AccountSourcePage   = "page"
	AccountSourceConfig = "config"
)

// Fixed failure codes. They are the whole text of an error: no path, no token
// and no page body ever reaches a caller through one.
const (
	AccountErrUnavailable       = "account_session_unavailable"
	AccountErrUnsupported       = "account_session_unsupported"
	AccountErrHomeUnresolved    = "account_session_home_unresolved"
	AccountErrConfigMissing     = "account_session_config_missing"
	AccountErrConfigInvalid     = "account_session_config_invalid"
	AccountErrConfigConflict    = "account_session_config_conflict"
	AccountErrWriteFailed       = "account_session_write_failed"
	AccountErrSnapshotInvalid   = "account_session_snapshot_invalid"
	AccountErrIdentityMissing   = "account_session_identity_missing"
	AccountErrIdentityMismatch  = "account_session_identity_mismatch"
	AccountErrPageTimeout       = "account_session_page_timeout"
	AccountErrRestartFailed     = "account_session_restart_failed"
	AccountErrRollbackFailed    = "account_session_rollback_unverified"
	AccountErrSignOutIncomplete = "account_session_sign_out_incomplete"
	AccountErrWorkBusy          = "account_session_work_busy"
)

// accountSessionMu serialises account operations in this process. One official
// application signs into one account at a time, so two concurrent switches
// cannot both be right.
var accountSessionMu sync.Mutex

// restartAccountProcess is the recovery path: the application's own normal exit
// followed by a managed relaunch. It is a seam so tests exercise the fallback
// without launching anything.
var restartAccountProcess = func(c *Controller, ctx context.Context) error {
	return c.restartOfficialLocked(ctx)
}

// restartOfficialLocked is RestartOfficial's body without taking c.lifecycle,
// for callers that already hold it.
func (c *Controller) restartOfficialLocked(ctx context.Context) error {
	c.mu.Lock()
	application := c.settings.Installation.Application
	c.mu.Unlock()
	if application == "" {
		return errors.New(AccountErrRestartFailed)
	}
	if err := stopOfficialProcesses(ctx, application); err != nil {
		c.setState(StateFailed, AccountErrRestartFailed)
		return errors.New(AccountErrRestartFailed)
	}
	// The child handle is cleared before relaunching: Start reuses a child it
	// still holds, so a handle whose process has just exited must be reaped first
	// or the relaunch would report a dead child as running.
	c.awaitManagedChildExit(ctx)
	c.reapChildLocked()
	if err := c.startOfficial(ctx); err != nil {
		c.setState(StateFailed, AccountErrRestartFailed)
		return errors.New(AccountErrRestartFailed)
	}
	return nil
}

// awaitManagedChildExit waits a bounded time for the managed child's exit to be
// observed. The quit loop has already confirmed the process is gone; this only
// waits for this process to notice, and never signals anything.
func (c *Controller) awaitManagedChildExit(ctx context.Context) {
	c.mu.Lock()
	child := c.child
	c.mu.Unlock()
	if child == nil {
		return
	}
	select {
	case <-child.Exit():
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
}

// AccountCookie is one cookie from the official kimi.com origin. Value is a
// secret.
type AccountCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
}

// AccountStorageEntry is one whitelisted localStorage item. Value is a secret.
type AccountStorageEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// AccountSession is one account's restorable state.
//
// UserID, Name and AvatarURL are metadata. Cookies, LocalStorage, KimiWeb,
// KimiCode and CodeKey are secrets: they belong in the caller's keychain, and
// the caller must never render them.
type AccountSession struct {
	SchemaVersion int    `json:"schemaVersion"`
	UserID        string `json:"userId"`
	Name          string `json:"name,omitempty"`
	AvatarURL     string `json:"avatarURL,omitempty"`
	// CapturedVia says which source supplied UserID: the live page, or the
	// config on disk when no page reported one.
	CapturedVia string `json:"capturedVia,omitempty"`
	CapturedAt  string `json:"capturedAt,omitempty"`

	Cookies      []AccountCookie       `json:"cookies,omitempty"`
	LocalStorage []AccountStorageEntry `json:"localStorage,omitempty"`
	// KimiWeb is the original `credentials.kimiWeb` entry, kept so a switch back
	// restores what that account had (device id, session id) instead of a guess.
	KimiWeb map[string]any `json:"kimiWeb,omitempty"`
	// KimiCode is the original `credentials.kimiCode` entry. It belongs to the
	// same UserID and is never carried across from another account.
	KimiCode map[string]any `json:"kimiCode,omitempty"`
	// CodeKey is the Work kernel's `kimi-code-key.json` content.
	CodeKey json.RawMessage `json:"codeKey,omitempty"`
}

// Token health labels derived purely from expiry metadata.
const (
	TokenHealthHealthy      = "healthy"
	TokenHealthExpiringSoon = "expiring_soon"
	TokenHealthExpired      = "expired"
	TokenHealthUnknown      = "unknown"
)

const (
	accessTokenExpiringWindow  = 24 * time.Hour
	refreshTokenExpiringWindow = 72 * time.Hour
	maxValidTokenUnixSeconds   = 253402300799
)

// AccountTokenLifetime carries read-only expiry metadata extracted from an
// account session. It never contains tokens, cookies or other secrets.
type AccountTokenLifetime struct {
	AccessExpiresAt  string `json:"accessExpiresAt,omitempty"`
	RefreshExpiresAt string `json:"refreshExpiresAt,omitempty"`
	CookieExpiresAt  string `json:"cookieExpiresAt,omitempty"`
	TokenHealth      string `json:"tokenHealth,omitempty"`
}

// InspectCredentialLifetime extracts non-secret expiry timestamps from an
// AccountSession and classifies its token health at now.
func InspectCredentialLifetime(session AccountSession, now time.Time) AccountTokenLifetime {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var lifetime AccountTokenLifetime
	if token := sessionTokenValue(session, "access_token", "accessToken"); token != "" {
		if exp, ok := jwtExpiresAt(token); ok {
			lifetime.AccessExpiresAt = exp.UTC().Format(time.RFC3339)
		}
	}
	if token := sessionTokenValue(session, "refresh_token", "refreshToken"); token != "" {
		if exp, ok := jwtExpiresAt(token); ok {
			lifetime.RefreshExpiresAt = exp.UTC().Format(time.RFC3339)
		}
	}
	if exp, ok := accountCookieExpiresAt(session.Cookies); ok {
		lifetime.CookieExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	lifetime.TokenHealth = ComputeTokenHealth(lifetime, now)
	return lifetime
}

// ComputeTokenHealth derives the token health label from persisted RFC3339
// expiry timestamps without inspecting or decrypting credential secrets.
func ComputeTokenHealth(lifetime AccountTokenLifetime, now time.Time) string {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	refreshAt, hasRefresh := parseTokenExpiryRFC3339(lifetime.RefreshExpiresAt)
	accessAt, hasAccess := parseTokenExpiryRFC3339(lifetime.AccessExpiresAt)
	cookieAt, hasCookie := parseTokenExpiryRFC3339(lifetime.CookieExpiresAt)

	if hasRefresh {
		if !refreshAt.After(now) {
			return TokenHealthExpired
		}
		if refreshAt.Sub(now) <= refreshTokenExpiringWindow {
			return TokenHealthExpiringSoon
		}
		return TokenHealthHealthy
	}
	if hasAccess {
		if !accessAt.After(now) {
			return TokenHealthExpired
		}
		if accessAt.Sub(now) <= accessTokenExpiringWindow {
			return TokenHealthExpiringSoon
		}
		return TokenHealthHealthy
	}
	if hasCookie {
		if !cookieAt.After(now) {
			return TokenHealthExpired
		}
		if cookieAt.Sub(now) <= accessTokenExpiringWindow {
			return TokenHealthExpiringSoon
		}
		return TokenHealthHealthy
	}
	return TokenHealthUnknown
}

func sessionTokenValue(session AccountSession, storageKey, webField string) string {
	for _, entry := range session.LocalStorage {
		if entry.Key == storageKey && strings.TrimSpace(entry.Value) != "" {
			return entry.Value
		}
	}
	if session.KimiWeb != nil {
		if value, ok := session.KimiWeb[webField].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func jwtExpiresAt(token string) (time.Time, bool) {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" || len(trimmed) > maxAccountStoragePayload {
		return time.Time{}, false
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return time.Time{}, false
	}
	header, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[0], "="))
	if err != nil || len(header) == 0 || header[0] != '{' || !json.Valid(header) {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil || len(payload) == 0 {
		return time.Time{}, false
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == nil {
		return time.Time{}, false
	}
	exp := *claims.Exp
	if math.IsNaN(exp) || math.IsInf(exp, 0) || exp <= 0 || exp > maxValidTokenUnixSeconds {
		return time.Time{}, false
	}
	return time.Unix(int64(math.Trunc(exp)), 0).UTC(), true
}

func isPrimaryAuthCookieName(name string) bool {
	if !isAccountCookieName(name) {
		return false
	}
	lower := strings.ToLower(name)
	for _, ignored := range []string{"csrf", "xsrf", "anonymous"} {
		if strings.Contains(lower, ignored) {
			return false
		}
	}
	for _, marker := range []string{"auth", "token", "session", "sid"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isSecondaryAuthCookieName(name string) bool {
	if !isAccountCookieName(name) {
		return false
	}
	lower := strings.ToLower(name)
	for _, ignored := range []string{"csrf", "xsrf", "anonymous"} {
		if strings.Contains(lower, ignored) {
			return false
		}
	}
	return true
}

func accountCookieExpiresAt(cookies []AccountCookie) (time.Time, bool) {
	pick := func(match func(string) bool) (time.Time, bool) {
		var best float64
		found := false
		for _, cookie := range cookies {
			if !match(cookie.Name) {
				continue
			}
			exp := cookie.Expires
			if math.IsNaN(exp) || math.IsInf(exp, 0) || exp <= 0 || exp > maxValidTokenUnixSeconds {
				continue
			}
			if !found || exp > best {
				best = exp
				found = true
			}
		}
		if !found {
			return time.Time{}, false
		}
		return time.Unix(int64(math.Trunc(best)), 0).UTC(), true
	}
	if exp, ok := pick(isPrimaryAuthCookieName); ok {
		return exp, true
	}
	return pick(isSecondaryAuthCookieName)
}

func parseTokenExpiryRFC3339(raw string) (time.Time, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// AccountIdentity is the non-secret answer of an identity read. It carries no
// token, only whether one is present.
type AccountIdentity struct {
	UserID string `json:"userId,omitempty"`
	Digest string `json:"digest,omitempty"`
	// Source is "page+config", "page", "config" or empty.
	Source string `json:"source,omitempty"`
	// SignedIn means an identity was readable at all.
	SignedIn bool `json:"signedIn"`
	// PageVerified means the official page itself reported this identity, through
	// `kimiAgentAPI.getUserProfile()`. A token-store answer is not page verified.
	PageVerified bool `json:"pageVerified"`
	// Reason is a fixed code, or empty when the read was clean.
	Reason string `json:"reason,omitempty"`
}

// AccountActivation is the graded result of a switch. It names no secret.
type AccountActivation struct {
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	ModelsState   string `json:"modelsState,omitempty"`
	ModelsReason  string `json:"modelsReason,omitempty"`
	UserID        string `json:"userId,omitempty"`
	Digest        string `json:"digest,omitempty"`
	// Hot means the page followed without a restart.
	Hot bool `json:"hot"`
	// Restarted means a normal exit and managed relaunch were used.
	Restarted bool `json:"restarted"`
	// Verified means the official page reported the target account and the
	// kernel config was read back as the same account.
	Verified bool `json:"verified"`
	// RolledBack means the previous snapshot was written back.
	RolledBack bool `json:"rolledBack"`
	// RequiresLogin means only the target account signing in again can help.
	RequiresLogin bool `json:"requiresLogin"`
}

// ---------------------------------------------------------------------------
// Controller API

// AccountIdentity reads the current account and changes nothing. The official
// page is preferred; a config-only answer is reported as not page verified.
func (c *Controller) AccountIdentity(ctx context.Context) (AccountIdentity, error) {
	accountSessionMu.Lock()
	defer accountSessionMu.Unlock()
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()

	identity := AccountIdentity{}
	home, err := c.WorkHome()
	if err != nil {
		identity.Reason = AccountErrHomeUnresolved
		return identity, nil
	}
	configUser, err := accountConfigUserID(home)
	if err != nil {
		identity.Reason = AccountErrConfigInvalid
		return identity, nil
	}
	pageUser := ""
	surface, surfaceErr := c.accountSurface()
	if surfaceErr == nil {
		pageUser, identity.PageVerified = accountPageUser(ctx, surface)
	}
	switch {
	case pageUser != "" && configUser != "" && pageUser == configUser:
		identity.Source = "page+config"
	case pageUser != "":
		identity.Source = "page"
	case configUser != "":
		identity.Source = "config"
	}
	identity.UserID = pageUser
	if identity.UserID == "" {
		identity.UserID = configUser
	}
	if identity.UserID != "" {
		identity.Digest = sha256Hex(identity.UserID)
		identity.SignedIn = true
	}
	if surfaceErr != nil {
		identity.Reason = surfaceErr.Error()
	} else if pageUser != "" && configUser != "" && pageUser != configUser {
		identity.Reason = AccountErrIdentityMismatch
	}
	return identity, nil
}

// AccountWorkBusy reports whether the official application has a conversation
// running. A caller that switches accounts while this is true would kill that
// turn, so unknown counts as busy.
func (c *Controller) AccountWorkBusy(ctx context.Context) (bool, error) {
	accountSessionMu.Lock()
	defer accountSessionMu.Unlock()
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()

	surface, err := c.accountSurface()
	if err != nil {
		return true, err
	}
	return accountWorkBusy(ctx, surface)
}

// CaptureAccount reads the account the official application is signed into right
// now: the freshest page tokens, the kimi.com login cookies, the config
// credential entries and the Work kernel key file. It writes nothing.
func (c *Controller) CaptureAccount(ctx context.Context) (AccountSession, error) {
	accountSessionMu.Lock()
	defer accountSessionMu.Unlock()
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()

	home, err := c.WorkHome()
	if err != nil {
		return AccountSession{}, errors.New(AccountErrHomeUnresolved)
	}
	surface, err := c.accountSurface()
	if err != nil {
		return AccountSession{}, err
	}
	return c.captureAccount(ctx, surface, home)
}

// ActivateAccount makes the target account the one the application is signed
// into and the kernel reads.
//
// The official token store initiates the account and kernel transition. A hot
// switch requires its observed runtime transition, a successful runtime query,
// and matching shell and config identities. Otherwise one normal desktop restart
// may recover the switch. A failed attempt restores the previous snapshot.
func (c *Controller) ActivateAccount(ctx context.Context, session AccountSession) (AccountActivation, error) {
	if err := session.validate(); err != nil {
		return AccountActivation{State: AccountStateFailed, Reason: AccountErrSnapshotInvalid},
			errors.New(AccountErrSnapshotInvalid)
	}
	accountSessionMu.Lock()
	defer accountSessionMu.Unlock()
	result, err := c.activateAccount(ctx, session)
	// The attempt may have written another account's credentials and then failed
	// to verify, or rolled back to the previous one. Either way the tree a
	// reconcile was reading is no longer the tree it is now, so the generation is
	// ended before this call returns -- not only on the success path.
	c.beginConfigWrite()
	if err != nil || !result.Verified {
		return result, err
	}
	home, homeErr := c.WorkHome()
	surface, surfaceErr := c.accountSurface()
	if homeErr != nil || surfaceErr != nil {
		result.ModelsState, result.ModelsReason = "pending", "account_models_runtime_unverified"
	} else {
		result.ModelsState, result.ModelsReason = c.syncAccountModels(ctx, surface, home, session.UserID)
	}
	return result, nil
}

func (c *Controller) activateAccount(ctx context.Context, session AccountSession) (AccountActivation, error) {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()

	home, err := c.WorkHome()
	if err != nil {
		return AccountActivation{State: AccountStateFailed, Reason: AccountErrHomeUnresolved},
			errors.New(AccountErrHomeUnresolved)
	}
	surface, err := c.accountSurface()
	if err != nil {
		return AccountActivation{State: AccountStateFailed, Reason: err.Error()}, err
	}
	// A running conversation is the official app's own reason to refuse a
	// credential change; switching under it would kill that turn.
	if busy, busyErr := accountWorkBusy(ctx, surface); busyErr != nil || busy {
		return AccountActivation{State: AccountStateFailed, Reason: AccountErrWorkBusy},
			errors.New(AccountErrWorkBusy)
	}
	// The previous snapshot is taken before anything is written, so a failed
	// attempt has something real to go back to.
	original, originalErr := c.captureAccount(ctx, surface, home)
	result := AccountActivation{UserID: session.UserID, Digest: sha256Hex(session.UserID)}

	runtime, runtimePage, watching := watchAccountRuntime(ctx, surface)
	if watching {
		defer runtime.accountStopRuntimeWatch(context.WithoutCancel(ctx), runtimePage)
	}
	pageErr := c.applyPageState(ctx, surface, session)
	targetSignedOut := pageErr != nil && accountPageSignedOut(ctx, surface)
	pagePrepared := !targetSignedOut && (pageErr == nil || pageErr.Error() == AccountErrIdentityMismatch)
	if pagePrepared && watching && waitAccountRuntime(ctx, surface, runtime, runtimePage, home, session.UserID, true) {
		result.State, result.Hot, result.Verified = AccountStateHot, true, true
		return result, nil
	}
	configOK, restarted := false, false
	if pagePrepared && ctx.Err() == nil {
		configOK = watching && accountConfigMatches(home, session.UserID)
		if !configOK {
			// The config write is the one step that races a reconcile, so it runs
			// with the generation ended and the write serialized. It is a plain
			// file operation; nothing here waits on a page.
			configOK = c.configWrite(func() error {
				return c.applyAccountConfig(home, session)
			}) == nil && accountConfigMatches(home, session.UserID)
		}
		if configOK {
			restarted = restartAccountProcess(c, ctx) == nil
		}
	}
	pageReadable := targetSignedOut
	if restarted {
		pageOK := false
		if fresh, err := c.accountSurface(); err == nil {
			surface = fresh
			pageReadable = true
			restartRetries := accountRuntimeRetries
			if restartRetries < accountVerifyRetries {
				restartRetries = accountVerifyRetries
			}
			pageOK = waitAccountIdentityRetries(ctx, surface, session.UserID, restartRetries)
			if probe, page, ok := accountRuntimeTarget(ctx, surface); ok {
				defer probe.accountStopRuntimeWatch(context.WithoutCancel(ctx), page)
				pageOK = waitAccountRuntime(ctx, surface, probe, page, home, session.UserID, false)
			}
		}
		configOK = accountConfigMatches(home, session.UserID)
		if pageOK && configOK {
			result.State, result.Restarted, result.Verified = AccountStateRestarted, true, true
			return result, nil
		}
		if pageReadable && accountPageSignedOut(ctx, surface) {
			targetSignedOut = true
		}
	}

	reason := AccountErrIdentityMismatch
	switch {
	case ctx.Err() != nil:
		reason = AccountErrPageTimeout
	case targetSignedOut:
		reason = AccountErrIdentityMismatch
	case !pagePrepared:
		reason = pageErr.Error()
	case !configOK:
		reason = AccountErrWriteFailed
	case !restarted:
		reason = AccountErrRestartFailed
	}

	rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancelRollback()

	rolledBack := false
	if originalErr == nil {
		if fresh, err := c.accountSurface(); err == nil {
			surface = fresh
			pageReadable = true
		}
		pageRestored := c.applyPageState(rollbackCtx, surface, original) == nil
		configRestored := c.configWrite(func() error {
			return c.applyAccountConfig(home, original)
		}) == nil && accountConfigMatches(home, original.UserID)
		rolledBack = pageRestored && configRestored
	}
	result.RolledBack = rolledBack
	result.Reason = reason
	result.FailureReason = reason
	if targetSignedOut {
		result.RequiresLogin = true
	}

	if rolledBack {
		// The previous account is back and verified: nothing changed for the user.
		result.State = AccountStateRestored
		return result, nil
	}
	// A page that is readable and reports nobody is the one case where only the
	// target account signing in again can help. An unreadable page (no restart,
	// or the app came back without a bridge) is a plain failure.
	if pageReadable && accountPageSignedOut(rollbackCtx, surface) {
		result.State, result.RequiresLogin = AccountStateNeedsLogin, true
		return result, nil
	}
	// A build whose page does not answer the official profile read cannot verify
	// either direction. That is a capability gap, and reporting it as a failed
	// rollback would send the user after the wrong problem.
	if restarted && !accountOfficialReadable(rollbackCtx, surface) {
		result.State, result.Reason = AccountStateFailed, AccountErrUnsupported
		return result, errors.New(AccountErrUnsupported)
	}
	result.State, result.Reason = AccountStateFailed, AccountErrRollbackFailed
	return result, errors.New(AccountErrRollbackFailed)
}

// BeginAccountLogin clears the signed-in page state so the official window shows
// its login page. The caller captures the current account first (the bridge
// does) and cancels by activating that snapshot again.
func (c *Controller) BeginAccountLogin(ctx context.Context) error {
	accountSessionMu.Lock()
	defer accountSessionMu.Unlock()
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()

	home, err := c.WorkHome()
	if err != nil {
		return errors.New(AccountErrHomeUnresolved)
	}
	surface, err := c.accountSurface()
	if err != nil {
		return err
	}
	pages, err := surface.accountPages(ctx)
	if err != nil || len(pages) == 0 {
		return errors.New(AccountErrUnavailable)
	}
	if busy, busyErr := accountWorkBusy(ctx, surface); busyErr != nil || busy {
		return errors.New(AccountErrWorkBusy)
	}
	var shell *accountPage
	for i := range pages {
		if !pages[i].credentials {
			shell = &pages[i]
			break
		}
	}
	if shell == nil {
		return errors.New(AccountErrUnsupported)
	}
	if err := surface.accountBeginLogin(ctx, *shell); err != nil {
		return err
	}
	if !waitAccountSignedOut(ctx, surface) {
		return errors.New(AccountErrSignOutIncomplete)
	}
	for _, page := range accountCredentialPages(pages) {
		existing, err := surface.accountCookies(ctx, page)
		if err != nil {
			return errors.New(AccountErrUnavailable)
		}
		if len(existing) == 0 {
			continue
		}
		if err := surface.accountRemoveCookies(ctx, page, existing); err != nil {
			return errors.New(AccountErrWriteFailed)
		}
	}
	// The sign-out cleared the page and is about to clear the config. The
	// generation ends first, so a reconcile that read the signed-in tree cannot
	// commit it back as the current state.
	return c.configWrite(func() error { return clearAccountCredentials(home) })
}

// ---------------------------------------------------------------------------
// capture

func (c *Controller) captureAccount(ctx context.Context, surface accountSurface, home string) (AccountSession, error) {
	session := AccountSession{SchemaVersion: AccountSessionSchema, CapturedAt: accountNow()}
	pages, err := surface.accountPages(ctx)
	if err != nil {
		return AccountSession{}, errors.New(AccountErrUnavailable)
	}
	web, code, configUser, err := accountConfigCredentials(home)
	if err != nil && err.Error() != AccountErrConfigMissing {
		return AccountSession{}, err
	}
	// The identity comes from whichever surface reports one; the shell page is
	// the only one that can, because it is where `kimiAgentAPI` lives. The token
	// store and the cookie jar are read from the credential surface, which is
	// where the signed-in web session is.
	credentialPages := accountCredentialPages(pages)
	for _, page := range pages {
		if page.credentials {
			continue
		}
		identity, err := surface.accountIdentity(ctx, page)
		if err != nil || !identity.Official || identity.UserID == "" {
			continue
		}
		// The page and the config naming different accounts means the app is
		// mid-switch or something else wrote the config. The config's kernel key
		// belongs to the account it names, so carrying it into this page's
		// snapshot would bill one account for the other.
		if configUser != "" && identity.UserID != configUser {
			return AccountSession{}, errors.New(AccountErrIdentityMismatch)
		}
		session.UserID, session.CapturedVia = identity.UserID, AccountSourcePage
		session.Name, session.AvatarURL = identity.Name, identity.AvatarURL
		break
	}
	for _, page := range credentialPages {
		items, err := surface.accountStorage(ctx, page)
		if err != nil {
			continue
		}
		session.LocalStorage = filterAccountStorage(items)
		if cookies, err := surface.accountCookies(ctx, page); err == nil {
			for _, cookie := range cookies {
				if isAccountCookieName(cookie.Name) && isAccountCookieDomain(cookie.Domain) {
					session.Cookies = append(session.Cookies, cookie)
				}
			}
		}
		if len(session.Cookies) > 0 {
			break
		}
	}
	if session.UserID == "" {
		if configUser == "" {
			return AccountSession{}, errors.New(AccountErrIdentityMissing)
		}
		session.UserID, session.CapturedVia = configUser, AccountSourceConfig
	}

	session.KimiWeb, session.KimiCode = web, code
	if key, ok := readAccountCodeKey(home); ok {
		session.CodeKey = key
	}
	// A config with no account id cannot say whose kernel key it holds. Carrying
	// it into a page-captured account would be the same mixed-identity bug as a
	// mismatched id, so it is dropped rather than guessed.
	if session.CapturedVia == AccountSourcePage && configUser == "" {
		session.KimiCode = nil
		session.CodeKey = nil
	}
	if err := session.validate(); err != nil {
		return AccountSession{}, errors.New(AccountErrSnapshotInvalid)
	}
	return session, nil
}

// ---------------------------------------------------------------------------
// apply

// applyPageState writes the cookies and the whitelisted storage keys, reloads
// the official pages and waits for them to read the target identity.
func (c *Controller) applyPageState(ctx context.Context, surface accountSurface, session AccountSession) error {
	pages, err := surface.accountPages(ctx)
	if err != nil {
		return errors.New(AccountErrUnavailable)
	}
	// Every write below targets the credential surface. The Work shell page owns
	// no kimi.com cookie jar and its storage is not the account's token store.
	credentialPages := accountCredentialPages(pages)
	if len(credentialPages) == 0 {
		return errors.New(AccountErrUnavailable)
	}
	for _, page := range credentialPages {
		// Cookies the target account does not own are removed first, so the
		// previous account's session cookie cannot shadow the new one.
		existing, err := surface.accountCookies(ctx, page)
		if err != nil {
			return errors.New(AccountErrUnavailable)
		}
		if stale := accountCookieDiff(existing, session.Cookies); len(stale) > 0 {
			if err := surface.accountRemoveCookies(ctx, page, stale); err != nil {
				return errors.New(AccountErrWriteFailed)
			}
		}
		if err := surface.accountSetCookies(ctx, page, session.Cookies); err != nil {
			return errors.New(AccountErrWriteFailed)
		}
	}
	set := accountStorageMap(session.LocalStorage)
	web, _ := accountConfigForSession(session)[AccountKimiWeb].(map[string]any)
	for key, field := range map[string]string{"access_token": "accessToken", "refresh_token": "refreshToken"} {
		if set[key] == "" {
			if value, ok := web[field].(string); ok && value != "" {
				set[key] = value
			}
		}
	}
	set["msh_user_id"] = session.UserID
	for _, page := range credentialPages {
		current, err := surface.accountStorage(ctx, page)
		if err != nil {
			return errors.New(AccountErrWriteFailed)
		}
		var remove []string
		for key := range current {
			if _, ok := set[key]; ok {
				continue
			}
			remove = append(remove, key)
		}
		sort.Strings(remove)
		if err := surface.accountWriteStorage(ctx, page, set, remove); err != nil {
			return errors.New(AccountErrWriteFailed)
		}
	}
	if err := surface.accountReload(ctx, credentialPages); err != nil {
		return errors.New(AccountErrPageTimeout)
	}
	// A page that still reports the previous account after a reload is exactly
	// the evidence that a hot switch did not happen.
	if !waitAccountIdentity(ctx, surface, session.UserID) {
		return errors.New(AccountErrIdentityMismatch)
	}
	return nil
}

// applyAccountConfig writes the credential entries and the kernel key file.
func (c *Controller) applyAccountConfig(home string, session AccountSession) error {
	if err := writeAccountCredentials(home, accountConfigForSession(session)); err != nil {
		return err
	}
	return applyAccountCodeKey(home, session.CodeKey)
}

// accountConfigForSession is the credential entry this session should produce in
// `config.json`. The page's own tokens are merged last: they are the ones Kimi
// rotated to, and writing the stale config copy back would undo a rotation.
func accountConfigForSession(session AccountSession) map[string]any {
	web := cloneJSONMap(session.KimiWeb)
	if web == nil {
		web = map[string]any{}
	}
	for _, entry := range session.LocalStorage {
		switch entry.Key {
		case "access_token":
			if entry.Value != "" {
				web["accessToken"] = entry.Value
			}
		case "refresh_token":
			if entry.Value != "" {
				web["refreshToken"] = entry.Value
			}
		}
	}
	web["userId"] = session.UserID
	web["updatedAt"] = accountNow()
	return map[string]any{AccountKimiWeb: web, AccountKimiCode: cloneJSONMap(session.KimiCode)}
}

// ---------------------------------------------------------------------------
// config and key file

func accountConfigPath(home string) string { return filepath.Join(home, NativeWorkConfig) }

// accountConfigCredentials answers the two credential entries this product owns
// and the account id the official config names. An absent file answers
// AccountErrConfigMissing so a capture can still report page-only state; an
// unreadable one is an error, never a guess.
func accountConfigCredentials(home string) (map[string]any, map[string]any, string, error) {
	file := accountConfigPath(home)
	info, err := os.Lstat(file)
	if err != nil {
		return nil, nil, "", errors.New(AccountErrConfigMissing)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxConfigBytes {
		return nil, nil, "", errors.New(AccountErrConfigInvalid)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, "", errors.New(AccountErrConfigInvalid)
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return nil, nil, "", errors.New(AccountErrConfigInvalid)
	}
	credentials, _ := document["credentials"].(map[string]any)
	var web, code map[string]any
	if credentials != nil {
		web, _ = credentials[AccountKimiWeb].(map[string]any)
		code, _ = credentials[AccountKimiCode].(map[string]any)
	}
	user := ""
	if web != nil {
		if id, ok := web["userId"].(string); ok && validAccountID(id) {
			user = id
		}
	}
	return cloneJSONMap(web), cloneJSONMap(code), user, nil
}

func accountConfigUserID(home string) (string, error) {
	_, _, user, err := accountConfigCredentials(home)
	if err != nil {
		if err.Error() == AccountErrConfigMissing {
			return "", nil
		}
		return "", err
	}
	return user, nil
}

func accountConfigMatches(home string, userID string) bool {
	matches, _ := accountConfigState(home, userID)
	return matches
}

// accountConfigState answers whether the profile's config still holds this
// account, and whether the config was readable at all. The official generator
// rewrites config.json right after an import, so a file that is momentarily
// absent or half-written is not evidence of a different account.
func accountConfigState(home string, userID string) (matches bool, readable bool) {
	if userID == "" {
		return false, true
	}
	current, err := accountConfigUserID(home)
	if err != nil || current == "" {
		return false, false
	}
	if current != userID {
		return false, true
	}
	// The same identity the import path grades against, so the two features can
	// never disagree about which account this profile holds.
	return AccountScope(home) == sha256Hex(userID), true
}

// writeAccountCredentials merges exactly two credential entries into the
// official config. Model selection, tasks, providers and every other entry are
// left as they were.
func writeAccountCredentials(home string, entries map[string]any) error {
	file := accountConfigPath(home)
	raw, info, err := readAccountConfigDocument(file)
	if err != nil {
		return err
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return errors.New(AccountErrConfigInvalid)
	}
	credentials, ok := document["credentials"].(map[string]any)
	if !ok {
		if document["credentials"] != nil {
			return errors.New(AccountErrConfigInvalid)
		}
		credentials = map[string]any{}
		document["credentials"] = credentials
	}
	for _, key := range []string{AccountKimiWeb, AccountKimiCode} {
		value, present := entries[key]
		if !present {
			continue
		}
		if value == nil {
			delete(credentials, key)
			continue
		}
		credentials[key] = value
	}
	return writeAccountConfigDocument(file, raw, info, document)
}

// clearAccountCredentials removes the signed-in account from the config and the
// kernel key file, leaving every other entry alone. Both credential entries go:
// a `kimiCode` left behind keeps the Work kernel billing the account that just
// signed out.
func clearAccountCredentials(home string) error {
	file := accountConfigPath(home)
	raw, info, err := readAccountConfigDocument(file)
	if err != nil {
		if err.Error() == AccountErrConfigMissing {
			return nil
		}
		return err
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return errors.New(AccountErrConfigInvalid)
	}
	if credentials, ok := document["credentials"].(map[string]any); ok {
		delete(credentials, AccountKimiWeb)
		delete(credentials, AccountKimiCode)
	}
	if err := writeAccountConfigDocument(file, raw, info, document); err != nil {
		return err
	}
	if err := os.Remove(accountCodeKeyPath(home)); err != nil && !os.IsNotExist(err) {
		return errors.New(AccountErrWriteFailed)
	}
	return nil
}

// readAccountConfigDocument reads the official config and its mode. A byte
// comparison in writeAccountConfigDocument against exactly these bytes is what
// turns a concurrent official rewrite into a reported conflict.
func readAccountConfigDocument(file string) ([]byte, os.FileInfo, error) {
	info, err := os.Stat(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, errors.New(AccountErrConfigMissing)
		}
		return nil, nil, errors.New(AccountErrConfigInvalid)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxConfigBytes {
		return nil, nil, errors.New(AccountErrConfigInvalid)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, errors.New(AccountErrConfigInvalid)
	}
	return raw, info, nil
}

func writeAccountConfigDocument(file string, raw []byte, info os.FileInfo, document map[string]any) error {
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return errors.New(AccountErrConfigInvalid)
	}
	current, err := os.ReadFile(file)
	if err != nil || string(current) != string(raw) {
		return errors.New(AccountErrConfigConflict)
	}
	if err := replaceFile(file, append(encoded, '\n'), info.Mode().Perm()); err != nil {
		return errors.New(AccountErrWriteFailed)
	}
	return nil
}

// accountCodeKeyPath is the Work kernel's key file inside the Work data home,
// the location the bundled daimon resolves from `shareDir/daimon`.
func accountCodeKeyPath(home string) string { return filepath.Join(home, AccountCodeKey) }

func readAccountCodeKey(home string) (json.RawMessage, bool) {
	info, err := os.Lstat(accountCodeKeyPath(home))
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAccountCodeKeyBytes {
		return nil, false
	}
	raw, err := os.ReadFile(accountCodeKeyPath(home))
	if err != nil || !json.Valid(raw) {
		return nil, false
	}
	return json.RawMessage(raw), true
}

func writeAccountCodeKey(home string, content json.RawMessage) error {
	if len(content) == 0 {
		return nil
	}
	if len(content) > maxAccountCodeKeyBytes || !json.Valid(content) {
		return errors.New(AccountErrSnapshotInvalid)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(accountCodeKeyPath(home)); err == nil {
		mode = info.Mode().Perm()
	}
	if err := replaceFile(accountCodeKeyPath(home), content, mode); err != nil {
		return errors.New(AccountErrWriteFailed)
	}
	return nil
}

// applyAccountCodeKey makes the kernel key file match the snapshot. A snapshot
// with no key file of its own clears the file instead of leaving the previous
// account's key behind, which would keep the kernel billing the wrong account.
func applyAccountCodeKey(home string, content json.RawMessage) error {
	if len(content) > 0 {
		return writeAccountCodeKey(home, content)
	}
	if err := os.Remove(accountCodeKeyPath(home)); err != nil && !os.IsNotExist(err) {
		return errors.New(AccountErrWriteFailed)
	}
	return nil
}

// ---------------------------------------------------------------------------
// verification

type accountRuntimeSurface interface {
	accountWatchRuntime(context.Context, accountPage) error
	accountRuntimeReady(context.Context, accountPage, bool) (bool, error)
	accountStopRuntimeWatch(context.Context, accountPage) error
}

func accountRuntimeTarget(ctx context.Context, surface accountSurface) (accountRuntimeSurface, accountPage, bool) {
	runtime, ok := surface.(accountRuntimeSurface)
	if !ok {
		return nil, accountPage{}, false
	}
	pages, err := surface.accountPages(ctx)
	if err == nil {
		for _, page := range pages {
			if !page.credentials {
				return runtime, page, true
			}
		}
	}
	return nil, accountPage{}, false
}

func watchAccountRuntime(ctx context.Context, surface accountSurface) (accountRuntimeSurface, accountPage, bool) {
	runtime, page, ok := accountRuntimeTarget(ctx, surface)
	if !ok || runtime.accountWatchRuntime(ctx, page) != nil {
		return nil, accountPage{}, false
	}
	return runtime, page, true
}

func waitAccountRuntime(ctx context.Context, surface accountSurface, runtime accountRuntimeSurface, page accountPage, home, want string, transition bool) bool {
	ctx, cancel := context.WithTimeout(ctx, accountRuntimeTimeout)
	defer cancel()
	for attempt := 0; attempt < accountRuntimeRetries; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(accountVerifyStep):
			}
		}
		ready, err := runtime.accountRuntimeReady(ctx, page, transition)
		if err != nil || !ready || !accountConfigMatches(home, want) {
			continue
		}
		if user, official := accountPageUser(ctx, surface); official && user == want {
			return true
		}
	}
	return false
}

// waitAccountIdentity waits for the official page to report want. It accepts
// only a verified read: the token store alone is a copy of what this process
// wrote, not evidence the page bound that account.
func waitAccountIdentity(ctx context.Context, surface accountSurface, want string) bool {
	return waitAccountIdentityRetries(ctx, surface, want, accountVerifyRetries)
}

func waitAccountIdentityRetries(ctx context.Context, surface accountSurface, want string, retries int) bool {
	if want == "" {
		return false
	}
	if retries <= 0 {
		retries = 1
	}
	for attempt := 0; attempt < retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(accountVerifyStep):
			}
		}
		pages, err := surface.accountPages(ctx)
		if err != nil {
			continue
		}
		for _, page := range pages {
			if page.credentials {
				continue
			}
			identity, err := surface.accountIdentity(ctx, page)
			if err != nil || !identity.Official {
				continue
			}
			if identity.UserID == want {
				return true
			}
		}
	}
	return false
}

// waitAccountSignedOut waits for every readable page to report no account. It
// asks the token store as well: the official profile API keeps answering for a
// view whose storage was just cleared until it reloads its own session.
func waitAccountSignedOut(ctx context.Context, surface accountSurface) bool {
	for attempt := 0; attempt < accountVerifyRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(accountVerifyStep):
			}
		}
		if accountPageSignedOut(ctx, surface) {
			return true
		}
	}
	return false
}

// accountPageUser answers the identity the page reports and whether the official
// profile API was the source. Only an official read counts as page verified.
func accountPageUser(ctx context.Context, surface accountSurface) (string, bool) {
	pages, err := surface.accountPages(ctx)
	if err != nil {
		return "", false
	}
	for _, page := range pages {
		if page.credentials {
			continue
		}
		identity, err := surface.accountIdentity(ctx, page)
		if err == nil && identity.Official && identity.UserID != "" {
			return identity.UserID, true
		}
	}
	return "", false
}

// accountOfficialReadable reports whether any readable page answers the official
// profile API. Without it no identity in this build can be verified at all.
func accountOfficialReadable(ctx context.Context, surface accountSurface) bool {
	pages, err := surface.accountPages(ctx)
	if err != nil {
		return false
	}
	for _, page := range pages {
		identity, err := surface.accountIdentity(ctx, page)
		if err != nil {
			continue
		}
		if identity.Official {
			return true
		}
	}
	return false
}

// accountPageSignedOut reports whether every readable page says nobody is signed
// in. A page that cannot be read is not evidence of a sign-out, so it answers
// false.
func accountPageSignedOut(ctx context.Context, surface accountSurface) bool {
	pages, err := surface.accountPages(ctx)
	if err != nil || len(pages) == 0 {
		return false
	}
	readable := 0
	for _, page := range pages {
		identity, err := surface.accountIdentity(ctx, page)
		if err != nil {
			continue
		}
		readable++
		if identity.UserID != "" || identity.AccessToken {
			return false
		}
	}
	return readable > 0
}

// accountWorkBusy asks the official page whether a conversation is running.
// Unknown answers busy: a switch under an unreadable state could still kill a
// turn.
func accountWorkBusy(ctx context.Context, surface accountSurface) (bool, error) {
	pages, err := surface.accountPages(ctx)
	if err != nil || len(pages) == 0 {
		return true, errors.New(AccountErrUnavailable)
	}
	answered := false
	for _, page := range pages {
		if page.credentials {
			continue
		}
		busy, err := surface.accountWorkBusy(ctx, page)
		if err != nil {
			continue
		}
		answered = true
		if busy {
			return true, nil
		}
	}
	if !answered {
		return true, errors.New(AccountErrUnavailable)
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// policy: which data belongs to an account

// accountStorageKeys are the token store keys the official client itself reads
// and writes. `msh_user_id` is the identity the desktop mirrors from the token.
var accountStorageKeys = map[string]bool{
	"access_token":               true,
	"refresh_token":              true,
	"anonymous_access_token":     true,
	"anonymous_refresh_token":    true,
	"msh_user_id":                true,
	"msh_user_subscription_data": true,
}

// isAccountStorageKey accepts the known keys plus the two shapes Kimi has used
// for per-user data. Conversation state and feature flags are not credentials.
func isAccountStorageKey(key string) bool {
	if accountStorageKeys[key] {
		return true
	}
	if len(key) == 0 || len(key) > 64 {
		return false
	}
	if strings.HasPrefix(key, "msh_user_") {
		return true
	}
	return strings.HasSuffix(key, "_token")
}

// accountCookieAnalytics are the measurement cookies that share the kimi.com
// domain. They are not credentials and are never copied.
var accountCookieAnalytics = []string{"_ga", "_gid", "_gat", "_clck", "_clsk", "sensorsdata", "__cf", "_fbp"}

// isAccountCookieName keeps the cookies that carry a login and drops the ones
// that measure it. The domain is already restricted to kimi.com by the caller.
func isAccountCookieName(name string) bool {
	lower := strings.ToLower(name)
	if lower == "" || len(lower) > 128 {
		return false
	}
	for _, prefix := range accountCookieAnalytics {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	for _, marker := range []string{"auth", "token", "session", "csrf", "xsrf", "sid", "user", "kimi"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// isAccountCookieDomain accepts only the official kimi.com tree. A suffix test
// that also accepted "kimi.com.evil.test" would be a leak, so the domain is
// compared label by label.
func isAccountCookieDomain(domain string) bool {
	value := strings.ToLower(strings.TrimPrefix(domain, "."))
	if value == "kimi.com" || value == "www.kimi.com" || value == "auth.kimi.com" {
		return true
	}
	return strings.HasSuffix(value, ".kimi.com")
}

// isOfficialAccountOrigin accepts the two official web origins, exactly. It is
// the only place a non-app:// target becomes visible to account switching.
func isOfficialAccountOrigin(raw string) (origin string, ok bool) {
	parsed, err := parseURL(raw)
	if err != nil || parsed.User != nil || parsed.Scheme != "https" {
		return "", false
	}
	switch strings.ToLower(parsed.Host) {
	case "www.kimi.com", "kimi.com":
		return parsed.Scheme + "://" + strings.ToLower(parsed.Host), true
	}
	return "", false
}

func filterAccountStorage(items map[string]string) []AccountStorageEntry {
	entries := make([]AccountStorageEntry, 0, len(items))
	for key, value := range items {
		if !isAccountStorageKey(key) || value == "" {
			continue
		}
		entries = append(entries, AccountStorageEntry{Key: key, Value: value})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries
}

func accountStorageMap(entries []AccountStorageEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if isAccountStorageKey(entry.Key) && entry.Value != "" {
			out[entry.Key] = entry.Value
		}
	}
	return out
}

// accountCookieDiff answers the cookies the live browser holds but the target
// account does not own.
func accountCookieDiff(existing, wanted []AccountCookie) []AccountCookie {
	keep := map[string]bool{}
	for _, cookie := range wanted {
		keep[cookieKey(cookie)] = true
	}
	var stale []AccountCookie
	for _, cookie := range existing {
		if !keep[cookieKey(cookie)] {
			stale = append(stale, cookie)
		}
	}
	return stale
}

func cookieKey(cookie AccountCookie) string {
	return strings.ToLower(cookie.Name) + "|" + strings.ToLower(cookie.Domain) + "|" + cookie.Path
}

// ---------------------------------------------------------------------------
// validation and small helpers

func (s AccountSession) validate() error {
	if s.UserID == "" || !validAccountID(s.UserID) || len(s.UserID) > 1024 {
		return errors.New(AccountErrSnapshotInvalid)
	}
	if len(s.Name) > maxAccountNameBytes || strings.ContainsAny(s.Name, "\x00\r\n") {
		return errors.New(AccountErrSnapshotInvalid)
	}
	if len(s.AvatarURL) > maxAccountAvatarBytes || strings.ContainsAny(s.AvatarURL, "\x00\r\n") {
		return errors.New(AccountErrSnapshotInvalid)
	}
	if len(s.Cookies) > maxAccountCookies || len(s.LocalStorage) > maxAccountStorageEntries {
		return errors.New(AccountErrSnapshotInvalid)
	}
	total := 0
	for _, cookie := range s.Cookies {
		if !isAccountCookieName(cookie.Name) || !isAccountCookieDomain(cookie.Domain) {
			return errors.New(AccountErrSnapshotInvalid)
		}
		if cookie.Path == "" || strings.ContainsAny(cookie.Path, "\x00\r\n") {
			return errors.New(AccountErrSnapshotInvalid)
		}
		total += len(cookie.Value)
	}
	for _, entry := range s.LocalStorage {
		if !isAccountStorageKey(entry.Key) {
			return errors.New(AccountErrSnapshotInvalid)
		}
		total += len(entry.Value)
	}
	if total > maxAccountStoragePayload {
		return errors.New(AccountErrSnapshotInvalid)
	}
	total += len(s.CodeKey)
	for _, entry := range []map[string]any{s.KimiWeb, s.KimiCode} {
		for _, value := range entry {
			if text, ok := value.(string); ok {
				total += len(text)
			}
		}
	}
	if total > maxAccountSecretBytes {
		return errors.New(AccountErrSnapshotInvalid)
	}
	return nil
}

func cloneJSONMap(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(encoded, &out) != nil {
		return nil
	}
	return out
}

func accountNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// accountSurface is the page surface account switching needs. It is a separate
// interface from PageActions so the existing contract is unchanged, and an
// implementation that cannot move credentials is reported as unsupported rather
// than called with a guess.
type accountSurface interface {
	accountPages(ctx context.Context) ([]accountPage, error)
	accountCookies(ctx context.Context, page accountPage) ([]AccountCookie, error)
	accountSetCookies(ctx context.Context, page accountPage, cookies []AccountCookie) error
	accountRemoveCookies(ctx context.Context, page accountPage, cookies []AccountCookie) error
	accountStorage(ctx context.Context, page accountPage) (map[string]string, error)
	accountWriteStorage(ctx context.Context, page accountPage, set map[string]string, remove []string) error
	accountReload(ctx context.Context, pages []accountPage) error
	accountIdentity(ctx context.Context, page accountPage) (accountPageIdentity, error)
	accountWorkBusy(ctx context.Context, page accountPage) (bool, error)
	accountBeginLogin(ctx context.Context, page accountPage) error
}

// accountPage is one official surface this process may act on. The full URL is
// deliberately not kept: a URL can carry a token in its query. Credentials is
// false for the Work shell page, which answers identity but owns no cookie jar.
type accountPage struct {
	targetID    string
	sessionID   string
	origin      string
	credentials bool
}

// accountPageIdentity is what the page reports about the account. It carries
// presence flags for the tokens, never the token values. Official is true when
// the answer came from `kimiAgentAPI.getUserProfile()` rather than the token
// store this process can write; Anonymous is the official "signed out" answer.
type accountPageIdentity struct {
	UserID      string
	Name        string
	AvatarURL   string
	AccessToken bool
	Refresh     bool
	Official    bool
	Anonymous   bool
}

func (c *Controller) accountSurface() (accountSurface, error) {
	c.mu.Lock()
	actions := c.actions
	child := c.child
	c.mu.Unlock()
	if actions == nil {
		return nil, errors.New(AccountErrUnavailable)
	}
	if child != nil {
		select {
		case <-child.Exit():
			return nil, errors.New(AccountErrUnavailable)
		default:
		}
	}
	surface, ok := actions.(accountSurface)
	if !ok {
		return nil, errors.New(AccountErrUnsupported)
	}
	return surface, nil
}
