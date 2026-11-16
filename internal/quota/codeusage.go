package quota

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Kimi Code's subscription allowance. The reply shape is not documented; the
// field names below are the ones the official clients parse (Kimi Code.app's
// parseQuotaUsages and ~/.kimi-code/bin/kimi's managed-usage.ts), and the parser
// accepts their camelCase aliases too.
//
// One invariant runs through this file: a reading is stored with the source and
// the credential tag that actually answered. A reading is only presented while
// that identity is still the current selection, and a failed read may only fall
// back to an earlier reading of the same identity. A failed read is never a 0.

const CodeUsageEndpoint = "https://api.kimi.com/coding/v1/usages"

const (
	CodeSourceManual = "manual"
	CodeSourceWork   = "work"
	CodeSourceLocal  = "local"
)

const (
	codeManualRef      = "kimi-code:manual"
	codeUsageFileName  = "code-usage.json"
	codeUsageSchema    = 1
	maxCodeWindows     = 8
	maxCodeKeyChars    = 4096
	codeUsageStateSize = 64 * 1024
)

const (
	CodeAvailable = "available"
	CodeMissing   = "missing"
	// CodeUnadopted means a credential exists (the local Kimi Code sign-in) but
	// the person has not said this product may use it.
	CodeUnadopted   = "unadopted"
	CodeUnavailable = "unavailable"
)

// KeyStore is the caller's secret store: the keychain on macOS, DPAPI on Windows.
// Get answers ("", nil) when nothing is stored; an error means the store itself
// failed, which is a different panel state from "no key saved yet".
type KeyStore interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

// CodeUsageOptions wires one service. A resolver returns only a key: the masked
// account shown to a person is a hash of the key that actually answered, so the
// card can never claim an account the reading did not come from.
//
// WorkKey is called again on every refresh so an account switch in Work is
// followed without restarting anything. LocalKey is consulted for detection; the
// key is used only after the person selected that source.
type CodeUsageOptions struct {
	Keys        KeyStore
	WorkKey     func() string
	LocalKey    func() string
	StateDir    string
	HTTPClient  *http.Client
	Now         func() int64
	TimeoutMS   int64
	FreshnessMS int64
	// ThrottleMS: negative disables, zero means the default.
	ThrottleMS int64
}

type CodeUsageError struct {
	Status  int
	Code    string
	Message string
}

func (e *CodeUsageError) Error() string { return e.Code }

func codeError(status int, code string) *CodeUsageError {
	return &CodeUsageError{Status: status, Code: code}
}

// CodeUsageWindow is one allowance window. `limit_month_code` is a subset of
// `limit_month_total`, so no window is ever added to another.
type CodeUsageWindow struct {
	Key              string  `json:"key"`
	Label            string  `json:"label"`
	UsedRatio        float64 `json:"usedRatio"`
	UsedPercent      float64 `json:"usedPercent"`
	RemainingPercent float64 `json:"remainingPercent"`
	ResetAt          *string `json:"resetAt"`
}

// CodeUsageWallet is the prepaid booster the same reply carries. A field the
// reply did not carry stays nil: a missing balance is not a zero balance.
type CodeUsageWallet struct {
	BalanceCents        *float64 `json:"balanceCents"`
	TotalCents          *float64 `json:"totalCents"`
	Currency            *string  `json:"currency"`
	MonthlyLimitCents   *float64 `json:"monthlyChargeLimitCents"`
	MonthlyUsedCents    *float64 `json:"monthlyUsedCents"`
	MonthlyLimitEnabled *bool    `json:"monthlyChargeLimitEnabled"`
}

type CodeUsagePayload struct {
	Windows []CodeUsageWindow `json:"windows"`
	Wallet  *CodeUsageWallet  `json:"wallet"`
	// Notes name parts of a reply that could not be read. They are not errors.
	Notes []string `json:"notes"`
}

type CodeSourceView struct {
	ID            string  `json:"id"`
	Label         string  `json:"label"`
	Availability  string  `json:"availability"`
	Adopted       bool    `json:"adopted"`
	Selected      bool    `json:"selected"`
	AccountMasked *string `json:"accountMasked"`
	Detail        *string `json:"detail"`
}

// CodeUsageState is the panel payload. Pure: no request, no resolver call.
type CodeUsageState struct {
	SchemaVersion  int               `json:"schemaVersion"`
	Endpoint       string            `json:"endpoint"`
	Status         string            `json:"status"`
	Stale          bool              `json:"stale"`
	ErrorReason    *string           `json:"errorReason"`
	SelectedSource *string           `json:"selectedSource"`
	ReadingSource  *string           `json:"readingSource"`
	AccountMasked  *string           `json:"accountMasked"`
	Sources        []CodeSourceView  `json:"sources"`
	Windows        []CodeUsageWindow `json:"windows"`
	Wallet         *CodeUsageWallet  `json:"wallet"`
	Notes          []string          `json:"notes"`
	ObservedAt     *string           `json:"observedAt"`
	LastAttemptAt  *string           `json:"lastAttemptAt"`
	FreshnessMS    int64             `json:"freshnessMs"`
	Cache          CacheState        `json:"cache"`
}

type CodeUsageRefresh struct {
	RefreshedAt *string  `json:"refreshedAt"`
	Source      *string  `json:"source"`
	Status      string   `json:"status"`
	ErrorReason *string  `json:"errorReason"`
	HTTPStatus  *int     `json:"httpStatus,omitempty"`
	Notes       []string `json:"notes"`
}

// codeIdentity is the pair a reading belongs to. Both halves are needed: the same
// account reached through another source is another selection, and the same
// source under another account is another reading.
type codeIdentity struct {
	source  string
	account string
}

type codeReading struct {
	codeIdentity
	windows []CodeUsageWindow
	wallet  *CodeUsageWallet
	notes   []string
	at      int64
}

type codeAttempt struct {
	codeIdentity
	at  int64
	ok  bool
	err string
}

type CodeUsage struct {
	mu           sync.Mutex
	refreshMu    sync.Mutex
	keys         KeyStore
	workKey      func() string
	localKey     func() string
	client       *http.Client
	now          func() int64
	freshnessMS  int64
	throttleMS   int64
	timeoutMS    int64
	stateFile    string
	cache        CacheState
	selected     string
	localAdopted bool
	availability map[string]string
	accounts     map[string]string
	reading      *codeReading
	attempt      *codeAttempt
}

func NewCodeUsage(options CodeUsageOptions) *CodeUsage {
	service := &CodeUsage{keys: options.Keys, workKey: options.WorkKey, localKey: options.LocalKey,
		now: options.Now, freshnessMS: options.FreshnessMS, throttleMS: options.ThrottleMS,
		timeoutMS: options.TimeoutMS, availability: map[string]string{}, accounts: map[string]string{}}
	if service.now == nil {
		service.now = func() int64 { return time.Now().UnixMilli() }
	}
	if service.freshnessMS <= 0 {
		service.freshnessMS = defaultFreshnessMS
	}
	if service.throttleMS < 0 {
		service.throttleMS = 0
	} else if service.throttleMS == 0 {
		service.throttleMS = defaultThrottleMS
	}
	if service.timeoutMS <= 0 {
		service.timeoutMS = 8000
	}
	if service.timeoutMS < minTimeoutMS {
		service.timeoutMS = minTimeoutMS
	}
	if service.timeoutMS > maxTimeoutMS {
		service.timeoutMS = maxTimeoutMS
	}
	if options.HTTPClient != nil {
		service.client = options.HTTPClient
	} else {
		service.client = &http.Client{
			Timeout: time.Duration(service.timeoutMS) * time.Millisecond,
			// A redirect would replay the bearer at whatever host answered 302.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if options.StateDir != "" {
		if absolute, err := filepath.Abs(options.StateDir); err == nil {
			service.stateFile = filepath.Join(absolute, codeUsageFileName)
			service.cache.Enabled = true
		}
	}
	service.loadSelection()
	service.mu.Lock()
	service.detectLocked()
	service.mu.Unlock()
	return service
}

// --- selection persistence -------------------------------------------------
//
// Only the choice is written: which source the person picked and whether they let
// this product use the local sign-in. No key, no window, no number.

func (c *CodeUsage) loadSelection() {
	if c.stateFile == "" {
		return
	}
	stat, err := os.Lstat(c.stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			c.setCacheError("store_unavailable")
		}
		return
	}
	if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() || stat.Size() > codeUsageStateSize {
		c.setCacheError("store_unavailable")
		return
	}
	raw, err := os.ReadFile(c.stateFile)
	if err != nil {
		c.setCacheError("store_unavailable")
		return
	}
	var document map[string]any
	if json.Unmarshal(stripBOM(raw), &document) != nil {
		c.setCacheError("store_unavailable")
		return
	}
	if schema, _ := document["schemaVersion"].(float64); int(schema) != codeUsageSchema {
		c.setCacheError("store_unavailable")
		return
	}
	switch value := document["selected"].(type) {
	case string:
		if value == CodeSourceManual || value == CodeSourceWork || value == CodeSourceLocal {
			c.selected = value
		}
	}
	if adopted, ok := document["localAdopted"].(bool); ok {
		c.localAdopted = adopted
	}
	stamp := c.now()
	c.cache.LastReadAt = &stamp
}

func (c *CodeUsage) saveSelectionLocked() {
	if c.stateFile == "" {
		return
	}
	directory := filepath.Dir(c.stateFile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		c.setCacheError("store_unavailable")
		return
	}
	if dirStat, err := os.Lstat(directory); err != nil || dirStat.Mode()&os.ModeSymlink != 0 ||
		!dirStat.IsDir() {
		c.setCacheError("store_unavailable")
		return
	}
	if stat, err := os.Lstat(c.stateFile); err == nil {
		if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() {
			c.setCacheError("store_unavailable")
			return
		}
	} else if !os.IsNotExist(err) {
		c.setCacheError("store_unavailable")
		return
	}
	encoded, err := json.MarshalIndent(map[string]any{"schemaVersion": codeUsageSchema,
		"selected": c.selected, "localAdopted": c.localAdopted}, "", "  ")
	if err != nil {
		c.setCacheError("store_unavailable")
		return
	}
	encoded = append(encoded, '\n')
	temporary := c.stateFile + "." + randomToken() + ".tmp"
	handle, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		c.setCacheError("store_unavailable")
		return
	}
	_, writeErr := handle.Write(encoded)
	closeErr := handle.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		c.setCacheError("store_unavailable")
		return
	}
	if err := os.Rename(temporary, c.stateFile); err != nil {
		_ = os.Remove(temporary)
		c.setCacheError("store_unavailable")
		return
	}
	stamp := c.now()
	c.cache.LastWriteAt = &stamp
	c.cache.Error = nil
}

func (c *CodeUsage) setCacheError(code string) {
	value := code
	c.cache.Error = &value
}

// --- sources ---------------------------------------------------------------

// readSecret rejects anything that cannot be a bearer value. A resolver that
// answers whitespace or a control character is a missing credential, not a
// malformed header.
func readSecret(key string) (string, bool) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" || len(trimmed) > maxCodeKeyChars || isControl(trimmed) {
		return "", false
	}
	for _, character := range trimmed {
		if character <= 0x20 {
			return "", false
		}
	}
	return trimmed, true
}

func (c *CodeUsage) workCredential() (string, bool) {
	if c.workKey == nil {
		return "", false
	}
	return readSecret(c.workKey())
}

func (c *CodeUsage) localCredential() (string, bool) {
	if c.localKey == nil {
		return "", false
	}
	return readSecret(c.localKey())
}

// detectLocked re-reads which sources hold a credential. The Work resolver is
// called here and again at the start of every refresh, which is what makes the
// card follow an account switched in Work.
func (c *CodeUsage) detectLocked() {
	for _, source := range []string{CodeSourceManual, CodeSourceWork, CodeSourceLocal} {
		c.availability[source] = CodeMissing
		c.accounts[source] = ""
	}
	if c.keys == nil {
		c.availability[CodeSourceManual] = CodeUnavailable
	} else if value, err := c.keys.Get(codeManualRef); err != nil {
		c.availability[CodeSourceManual] = CodeUnavailable
	} else if secret, ok := readSecret(value); ok {
		c.availability[CodeSourceManual] = CodeAvailable
		c.accounts[CodeSourceManual] = AccountTag(secret)
	}
	if c.workKey == nil {
		c.availability[CodeSourceWork] = CodeUnavailable
	} else if secret, ok := c.workCredential(); ok {
		c.availability[CodeSourceWork] = CodeAvailable
		c.accounts[CodeSourceWork] = AccountTag(secret)
	}
	if c.localKey == nil {
		c.availability[CodeSourceLocal] = CodeUnavailable
	} else if secret, ok := c.localCredential(); ok {
		c.accounts[CodeSourceLocal] = AccountTag(secret)
		// Detection never adopts: this sign-in belongs to another product, and
		// using it is the person's decision.
		if c.localAdopted {
			c.availability[CodeSourceLocal] = CodeAvailable
		} else {
			c.availability[CodeSourceLocal] = CodeUnadopted
		}
	}
}

// resolveLocked returns the source a refresh should use and its credential. An
// explicit selection is honoured even when it cannot answer, so a person who
// picked a source is told that source is missing rather than silently read on
// another one.
func (c *CodeUsage) resolveLocked() (string, string) {
	if c.selected != "" {
		switch c.selected {
		case CodeSourceManual:
			if c.keys != nil {
				if value, err := c.keys.Get(codeManualRef); err == nil {
					if secret, ok := readSecret(value); ok {
						return CodeSourceManual, secret
					}
				}
			}
		case CodeSourceWork:
			if secret, ok := c.workCredential(); ok {
				return CodeSourceWork, secret
			}
		case CodeSourceLocal:
			if c.localAdopted {
				if secret, ok := c.localCredential(); ok {
					return CodeSourceLocal, secret
				}
			}
		}
		return c.selected, ""
	}
	if c.keys != nil {
		if value, err := c.keys.Get(codeManualRef); err == nil {
			if secret, ok := readSecret(value); ok {
				return CodeSourceManual, secret
			}
		}
	}
	if secret, ok := c.workCredential(); ok {
		return CodeSourceWork, secret
	}
	return "", ""
}

func (c *CodeUsage) effectiveSourceLocked() string {
	if c.selected != "" {
		return c.selected
	}
	for _, source := range []string{CodeSourceManual, CodeSourceWork} {
		if c.availability[source] == CodeAvailable {
			return source
		}
	}
	return ""
}

// currentIdentityLocked is the identity a reading must carry to be shown.
func (c *CodeUsage) currentIdentityLocked() codeIdentity {
	source := c.effectiveSourceLocked()
	return codeIdentity{source: source, account: c.accounts[source]}
}

func (c *CodeUsage) ownsLocked(identity codeIdentity) bool {
	current := c.currentIdentityLocked()
	return current.source != "" && current.source == identity.source &&
		current.account != "" && current.account == identity.account
}

// --- parsing ---------------------------------------------------------------

// ParseCodeUsageReply reads the usages reply. A reply with nothing readable is an
// error rather than an empty card, because an empty card reads as "all spent".
func ParseCodeUsageReply(document any) (CodeUsagePayload, string) {
	root, ok := document.(map[string]any)
	if !ok {
		return CodeUsagePayload{}, "invalid_response"
	}
	usages, ok := root["usages"].(map[string]any)
	if !ok {
		if data, present := root["data"].(map[string]any); present {
			usages, ok = data["usages"].(map[string]any)
		}
	}
	if !ok {
		usages = root
	}
	payload := CodeUsagePayload{Windows: []CodeUsageWindow{}, Notes: []string{}}
	for _, entry := range []struct {
		key   string
		alias string
		label string
	}{
		{"limit_5h", "limit5h", "5 小时窗口"},
		{"limit_7d", "limit7d", "每周窗口"},
		{"limit_month_total", "monthTotal", "月度总额"},
		{"limit_month_code", "monthCode", "月度（Kimi Code 部分）"},
	} {
		raw, present := usages[entry.key]
		if !present || raw == nil {
			raw, present = usages[entry.alias]
		}
		if !present || raw == nil {
			continue
		}
		window, code := parseCodeWindow(entry.key, entry.label, raw)
		if code != "" {
			payload.Notes = append(payload.Notes, code+":"+entry.key)
			continue
		}
		payload.Windows = append(payload.Windows, window)
		if len(payload.Windows) >= maxCodeWindows {
			break
		}
	}
	payload.Wallet = parseCodeWallet(root)
	if len(payload.Windows) == 0 && payload.Wallet == nil {
		return CodeUsagePayload{}, "invalid_response"
	}
	return payload, ""
}

func parseCodeWindow(key, label string, raw any) (CodeUsageWindow, string) {
	object, ok := raw.(map[string]any)
	if !ok {
		return CodeUsageWindow{}, "window_unreadable"
	}
	ratio := codeRatio(object["used_ratio"])
	if ratio == nil {
		ratio = codeRatio(object["usedRatio"])
	}
	if ratio == nil {
		ratio = codeRatio(object["ratio"])
	}
	if ratio == nil {
		return CodeUsageWindow{}, "window_unreadable"
	}
	if *ratio < 0 || *ratio > 1 {
		// Outside the documented 0..1 range: reported rather than clamped into a
		// number the vendor never sent.
		return CodeUsageWindow{}, "window_ratio_out_of_range"
	}
	window := CodeUsageWindow{Key: key, Label: label, UsedRatio: *ratio,
		UsedPercent: round6(*ratio * 100)}
	window.RemainingPercent = round6(100 - window.UsedPercent)
	if window.RemainingPercent < 0 {
		window.RemainingPercent = 0
	}
	for _, name := range []string{"reset_time", "resetTime", "resetAt"} {
		if stamp := ToTimestamp(object[name]); stamp != nil {
			window.ResetAt = optISO(stamp)
			break
		}
	}
	return window, ""
}

func codeRatio(value any) *float64 { return ToAmount(value) }

// microToCents is the official reader's own conversion: the wallet carries
// millionths of a cent, and a positive fraction below one cent bills as one cent.
func microToCents(value float64) float64 {
	cents := value / 1e6
	if cents > 0 && cents < 1 {
		return 1
	}
	return round6(math.Round(cents))
}

func parseCodeWallet(root map[string]any) *CodeUsageWallet {
	var wallet map[string]any
	for _, name := range []string{"boosterWallet", "booster_wallet", "wallet"} {
		if object, ok := root[name].(map[string]any); ok {
			wallet = object
			break
		}
	}
	if wallet == nil {
		return nil
	}
	balance, ok := wallet["balance"].(map[string]any)
	if !ok {
		return nil
	}
	if kind, _ := balance["type"].(string); kind != "BOOSTER" {
		return nil
	}
	total := ToAmount(balance["amount"])
	if total == nil || *total <= 0 {
		return nil
	}
	result := &CodeUsageWallet{}
	totalCents := microToCents(*total)
	result.TotalCents = &totalCents
	if left := ToAmount(balance["amountLeft"]); left != nil {
		balanceCents := microToCents(*left)
		result.BalanceCents = &balanceCents
	}
	if limit := parseCodeMoney(wallet["monthlyChargeLimit"]); limit != nil {
		result.MonthlyLimitCents = &limit.cents
		if limit.currency != "" {
			result.Currency = optString(limit.currency)
		}
	}
	if used := parseCodeMoney(wallet["monthlyUsed"]); used != nil {
		result.MonthlyUsedCents = &used.cents
		if result.Currency == nil && used.currency != "" {
			result.Currency = optString(used.currency)
		}
	}
	if enabled, ok := wallet["monthlyChargeLimitEnabled"].(bool); ok {
		result.MonthlyLimitEnabled = &enabled
	}
	return result
}

type codeMoney struct {
	cents    float64
	currency string
}

func parseCodeMoney(value any) *codeMoney {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	cents := ToAmount(object["priceInCents"])
	if cents == nil {
		return nil
	}
	currency, _ := object["currency"].(string)
	return &codeMoney{cents: *cents, currency: strings.ToUpper(strings.TrimSpace(currency))}
}

// --- request ---------------------------------------------------------------

func (c *CodeUsage) request(ctx context.Context, key string) (any, string, *int) {
	if c.client == nil {
		return nil, "network_error", nil
	}
	request, err := http.NewRequestWithContext(ctx, "GET", CodeUsageEndpoint, nil)
	if err != nil {
		return nil, "network_error", nil
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	reply, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "timeout", nil
		}
		return nil, "network_error", nil
	}
	defer func() { _ = reply.Body.Close() }()
	if reply.StatusCode < 200 || reply.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(reply.Body, maxErrorBodyBytes))
		status := reply.StatusCode
		switch {
		case status == 401 || status == 403:
			return nil, "auth_rejected", &status
		case status == 404:
			// The official client's reading: this key is not a Kimi Code key.
			return nil, "usage_endpoint_unavailable", &status
		case status == 429:
			return nil, "rate_limited", &status
		default:
			return nil, "upstream_error", &status
		}
	}
	raw, err := io.ReadAll(io.LimitReader(reply.Body, maxResponseBytes+1))
	if err != nil {
		return nil, "network_error", nil
	}
	if len(raw) > maxResponseBytes {
		return nil, "response_too_large", nil
	}
	var document any
	if json.Unmarshal(raw, &document) != nil {
		return nil, "invalid_response", nil
	}
	return document, "", nil
}

// --- refresh ---------------------------------------------------------------

// Refresh is the only method that makes a request. The identity captured before
// the request is the one recorded with the result, so a selection changed while
// the request was in flight cannot attribute this reply to another account.
func (c *CodeUsage) Refresh(ctx context.Context, force bool) CodeUsageRefresh {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.Lock()
	c.detectLocked()
	expected := c.effectiveSourceLocked()
	source, key := c.resolveLocked()
	if expected != "" && (key == "" || source != expected) {
		c.accounts[expected] = ""
		if c.availability[expected] == CodeAvailable {
			c.availability[expected] = CodeMissing
		}
	}
	if key != "" {
		c.accounts[source] = AccountTag(key)
		c.availability[source] = CodeAvailable
	}
	identity := c.currentIdentityLocked()
	attemptedAt := c.now()
	if !force && c.reading != nil && c.reading.codeIdentity == identity && identity.source != "" &&
		attemptedAt-c.reading.at < c.throttleMS {
		effective := c.effectiveSourceLocked()
		c.mu.Unlock()
		return CodeUsageRefresh{Source: optString(effective), Status: "throttled",
			ErrorReason: optString("throttled"), Notes: []string{}}
	}
	c.attempt = &codeAttempt{codeIdentity: identity, at: attemptedAt}
	c.mu.Unlock()

	if source == "" || key == "" {
		c.mu.Lock()
		c.attempt.err = "credential_missing"
		effective := c.effectiveSourceLocked()
		c.mu.Unlock()
		return CodeUsageRefresh{Source: optString(effective), Status: "not_configured",
			ErrorReason: optString("credential_missing"), Notes: []string{}}
	}

	document, code, status := c.request(ctx, key)
	if code == "" {
		var parsed CodeUsagePayload
		parsed, code = ParseCodeUsageReply(document)
		if code == "" {
			status, reason := c.install(identity, parsed, attemptedAt)
			return CodeUsageRefresh{RefreshedAt: optISO(&attemptedAt), Source: optString(source),
				Status: status, ErrorReason: reason, Notes: parsed.Notes}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempt.err = code
	// The selection may have moved while this request ran, in which case its
	// failure says nothing about what is selected now.
	if !c.ownsLocked(identity) {
		return CodeUsageRefresh{RefreshedAt: optISO(&attemptedAt), Source: optString(source),
			Status: "superseded", ErrorReason: optString("selection_changed"),
			HTTPStatus: status, Notes: []string{}}
	}
	result := CodeUsageRefresh{RefreshedAt: optISO(&attemptedAt), Source: optString(source),
		ErrorReason: optString(code), HTTPStatus: status, Notes: []string{}}
	if c.reading != nil && c.reading.codeIdentity == identity {
		// Only a reading of the same source and the same credential is shown as
		// stale after a failure. Another account's numbers are not this
		// account's numbers.
		result.Status = "stale"
	} else {
		result.Status = "error"
	}
	return result
}

// install records a successful read under the identity that produced it. If the
// selection moved while the request ran, the reading is still stored with its own
// identity and the caller is told it was superseded: reporting "ok" here would
// present another account's numbers as the answer to a read of this one.
func (c *CodeUsage) install(identity codeIdentity, parsed CodeUsagePayload, at int64) (string, *string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reading = &codeReading{codeIdentity: identity, windows: parsed.Windows, wallet: parsed.Wallet,
		notes: parsed.Notes, at: at}
	if c.attempt != nil && c.attempt.codeIdentity == identity {
		c.attempt.ok = true
	}
	if !c.ownsLocked(identity) {
		return "superseded", optString("selection_changed")
	}
	return "ok", nil
}

// --- presentation ----------------------------------------------------------

// State reports the last reading only while it belongs to the current selection.
// Pure: it calls no resolver, so an account switched in Work is picked up by the
// next detect or refresh, and until then its predecessor's reading is not shown.
func (c *CodeUsage) State() CodeUsageState {
	c.mu.Lock()
	defer c.mu.Unlock()
	identity := c.currentIdentityLocked()
	state := CodeUsageState{SchemaVersion: 1, Endpoint: CodeUsageEndpoint, Status: "not_configured",
		SelectedSource: optString(identity.source), Sources: []CodeSourceView{},
		Windows: []CodeUsageWindow{}, Notes: []string{}, FreshnessMS: c.freshnessMS, Cache: c.cache}
	for _, source := range []string{CodeSourceManual, CodeSourceWork, CodeSourceLocal} {
		view := CodeSourceView{ID: source, Label: codeSourceLabel(source),
			Availability: c.availability[source], Adopted: true, Selected: source == identity.source,
			AccountMasked: optString(c.accounts[source])}
		if source == CodeSourceLocal {
			view.Adopted = c.localAdopted
			if view.Availability == CodeUnadopted {
				view.Detail = optString("code_local_needs_adoption")
			}
		}
		state.Sources = append(state.Sources, view)
	}
	if c.attempt != nil {
		state.LastAttemptAt = optISO(&c.attempt.at)
	}
	reading := c.reading
	if reading == nil || !c.ownsLocked(reading.codeIdentity) {
		if reading != nil {
			state.Notes = append(state.Notes, "reading_superseded")
		}
		switch {
		case c.attempt != nil && c.attempt.codeIdentity == identity && !c.attempt.ok && c.attempt.err != "":
			state.Status = "error"
			state.ErrorReason = optString(c.attempt.err)
		case identity.source != "" && identity.account != "" &&
			c.availability[identity.source] == CodeAvailable:
			state.Status = "not_read"
		}
		return state
	}
	state.ReadingSource = optString(reading.source)
	state.AccountMasked = optString(reading.account)
	state.Windows = reading.windows
	state.Wallet = reading.wallet
	state.Notes = append(state.Notes, reading.notes...)
	if state.Windows == nil {
		state.Windows = []CodeUsageWindow{}
	}
	state.ObservedAt = optISO(&reading.at)
	if c.attempt != nil && c.attempt.codeIdentity == identity && !c.attempt.ok && c.attempt.err != "" {
		state.Status = "stale"
		state.Stale = true
		state.ErrorReason = optString(c.attempt.err)
		return state
	}
	if c.now()-reading.at > c.freshnessMS {
		state.Status = "stale"
		state.Stale = true
		state.ErrorReason = optString("age")
		return state
	}
	state.Status = "ok"
	return state
}

func codeSourceLabel(source string) string {
	switch source {
	case CodeSourceManual:
		return "手动填写的 Kimi Code Key"
	case CodeSourceWork:
		return "当前 Work 账号自带的 Key"
	case CodeSourceLocal:
		return "本机 Kimi Code 已登录的 Key"
	}
	return source
}

// --- actions ---------------------------------------------------------------

const (
	CodeActionState       = "state"
	CodeActionDetect      = "detect"
	CodeActionSelect      = "select"
	CodeActionSaveManual  = "saveManual"
	CodeActionClearManual = "clearManual"
	CodeActionRefresh     = "refresh"
)

// Action answers one panel request. The answer always carries `state`; the
// hand-typed key is accepted here and never appears in any answer.
func (c *CodeUsage) Action(ctx context.Context, action string, input map[string]any) (map[string]any, error) {
	switch action {
	case "", CodeActionState:
		return map[string]any{"state": c.State()}, nil
	case CodeActionDetect:
		c.mu.Lock()
		c.detectLocked()
		c.mu.Unlock()
		return map[string]any{"state": c.State()}, nil
	case CodeActionSelect:
		source := stringOf(input["source"])
		if source != CodeSourceManual && source != CodeSourceWork && source != CodeSourceLocal {
			return nil, codeError(400, "invalid_code_source")
		}
		c.mu.Lock()
		if source == CodeSourceLocal {
			if c.availability[CodeSourceLocal] != CodeAvailable &&
				c.availability[CodeSourceLocal] != CodeUnadopted {
				c.mu.Unlock()
				return nil, codeError(409, "code_local_unavailable")
			}
			c.localAdopted = true
			c.availability[CodeSourceLocal] = CodeAvailable
		}
		c.selected = source
		c.saveSelectionLocked()
		c.mu.Unlock()
		return map[string]any{"state": c.State()}, nil
	case CodeActionSaveManual:
		if c.keys == nil {
			return nil, codeError(503, "credential_store_unavailable")
		}
		secret, ok := readSecret(stringOf(input["key"]))
		if !ok {
			return nil, codeError(400, "invalid_code_key")
		}
		if err := c.replaceManual(secret); err != nil {
			return nil, err
		}
		return map[string]any{"state": c.State()}, nil
	case CodeActionClearManual:
		if c.keys != nil {
			// The key is deleted first: a store that refuses must not leave the
			// card claiming a source that no longer exists.
			if err := c.keys.Delete(codeManualRef); err != nil {
				return nil, codeError(503, "credential_delete_failed")
			}
		}
		c.mu.Lock()
		c.availability[CodeSourceManual] = CodeMissing
		c.accounts[CodeSourceManual] = ""
		if c.selected == CodeSourceManual {
			c.selected = ""
		}
		c.saveSelectionLocked()
		c.mu.Unlock()
		return map[string]any{"state": c.State()}, nil
	case CodeActionRefresh:
		force := false
		if raw, present := input["force"]; present && raw != nil {
			value, ok := raw.(bool)
			if !ok {
				return nil, codeError(400, "invalid_code_refresh")
			}
			force = value
		}
		report := c.Refresh(ctx, force)
		return map[string]any{"state": c.State(), "lastRefresh": report}, nil
	}
	return nil, codeError(404, "not_found")
}

// replaceManual writes the hand-typed key and verifies it round-trips. A store
// that accepts a write but answers with something else would fail every later
// request, so the previous value is put back rather than the new one kept; the
// old key is never deleted by a failed replacement.
func (c *CodeUsage) replaceManual(secret string) error {
	previous, hadPrevious := "", false
	if value, err := c.keys.Get(codeManualRef); err == nil {
		previous, hadPrevious = value, value != ""
	}
	if err := c.keys.Set(codeManualRef, secret); err != nil {
		c.restoreManual(previous, hadPrevious)
		return codeError(503, "credential_write_failed")
	}
	if back, err := c.keys.Get(codeManualRef); err != nil || back != secret {
		if code := c.restoreManual(previous, hadPrevious); code != "" {
			return codeError(503, code)
		}
		return codeError(503, "credential_readback_failed")
	}
	c.mu.Lock()
	c.availability[CodeSourceManual] = CodeAvailable
	c.accounts[CodeSourceManual] = AccountTag(secret)
	c.selected = CodeSourceManual
	c.saveSelectionLocked()
	c.mu.Unlock()
	return nil
}

// restoreManual puts back what was stored before a failed replacement. It answers
// the code to report when even that failed, so a caller cannot record a success
// over a store that lost the key.
func (c *CodeUsage) restoreManual(previous string, hadPrevious bool) string {
	var err error
	if hadPrevious {
		err = c.keys.Set(codeManualRef, previous)
	} else {
		err = c.keys.Delete(codeManualRef)
	}
	if err != nil {
		return "credential_restore_failed"
	}
	return ""
}
