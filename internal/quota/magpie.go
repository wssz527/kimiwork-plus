package quota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Magpie's own quota read. Magpie is the local model router this product already
// supports as an upstream, so its balances belong next to the vendor balances —
// but they are a different kind of statement: Magpie reports what it read from
// each of its own accounts, not what a vendor told this product directly.
//
// The protocol is the one Magpie documents in `magpie quota --help`:
//
//	magpie quota [<provider>…] --json        the same data as the HTTP form
//	GET <gateway>/v1/magpie/quotas           on the port in Settings' Gateway
//	                                          address (127.0.0.1:3425 by default)
//	Authorization: Bearer <key> | x-api-key  a shared-gateway key, when shared
//
// Shape, confirmed against a running 0.1.3960 install (26 entries, both kinds):
//
//	[{provider,name,kind,plan?,user?,windows[],balance?,asOf?,readAt?,
//	  lastServedAt?,last?,alias?}]
//
//	kind "subscription": windows[]{name,used,remaining,resetsAt?,holds?,
//	                      amount?,limit?,unit?,display?,text?,lane?,...}
//	  `used`/`remaining` are percentages. `amount`/`limit` are the vendor's own
//	  absolute figures when the entry has them; `display` is a pre-rendered
//	  string such as "5500 / 5600 · 100 即将过期".
//	kind "balance":      balance is a formatted string such as "$19.18".

// MagpieDefaultBaseURL is where a default install answers.
const MagpieDefaultBaseURL = "http://127.0.0.1:3425"

// MagpieQuotasPath is the documented read-only path.
const MagpieQuotasPath = "/v1/magpie/quotas"

const (
	maxMagpieEntries = 200
	maxMagpieWindows = 16
	maxMagpieBase    = 512
)

// MagpieWindow is one allowance window as Magpie reports it. Exactly one of the
// absolute or the percentage reading is meaningful for any given entry; which one
// is stated in `Basis` so a renderer cannot treat a percentage as money.
type MagpieWindow struct {
	Name      string   `json:"name"`
	Basis     string   `json:"basis"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	// Amount and Limit are the vendor's absolute figures, present only when
	// Magpie itself carried them.
	Amount  *float64 `json:"amount"`
	Limit   *float64 `json:"limit"`
	Unit    *string  `json:"unit"`
	ResetAt *string  `json:"resetsAt"`
	Display *string  `json:"display"`
	Note    *string  `json:"note"`
}

// MagpieWindowBasis values.
const (
	// MagpieBasisAmount means the entry carried absolute figures.
	MagpieBasisAmount = "amount"
	// MagpieBasisPercent means only a 0-100 percentage was carried.
	MagpieBasisPercent = "percent"
	// MagpieBasisText means only a display string was carried; the numbers are
	// unknown, which is not zero.
	MagpieBasisText = "text"
)

// MagpieEntry is one account or provider balance.
type MagpieEntry struct {
	Provider string         `json:"provider"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	Plan     *string        `json:"plan"`
	User     *string        `json:"user"`
	Alias    *string        `json:"alias"`
	Windows  []MagpieWindow `json:"windows"`
	// Balance is the formatted string for `kind: "balance"`, kept verbatim
	// because it may name a currency this package does not know.
	Balance *string `json:"balance"`
	// BalanceCurrency and BalanceAmount are set only when the string parsed
	// cleanly; otherwise they stay nil and `Balance` is all there is.
	BalanceCurrency *string  `json:"balanceCurrency"`
	BalanceAmount   *float64 `json:"balanceAmount"`
	AsOf            *string  `json:"asOf"`
	ReadAt          *string  `json:"readAt"`
	LastServedAt    *string  `json:"lastServedAt"`
}

// MagpieSnapshot is one successful read.
type MagpieSnapshot struct {
	BaseURL   string        `json:"baseUrl"`
	Entries   []MagpieEntry `json:"entries"`
	FetchedAt *string       `json:"fetchedAt"`
	// Notes are facts about the reply that are not errors, such as an entry
	// whose balance string this package does not parse.
	Notes []string `json:"notes"`
}

// MagpieState is the panel payload. Pure: no request.
type MagpieState struct {
	SchemaVersion int             `json:"schemaVersion"`
	BaseURL       string          `json:"baseUrl"`
	Status        string          `json:"status"`
	Stale         bool            `json:"stale"`
	ErrorReason   *string         `json:"errorReason"`
	KeyConfigured bool            `json:"keyConfigured"`
	Snapshot      *MagpieSnapshot `json:"snapshot"`
	LastAttemptAt *string         `json:"lastAttemptAt"`
	Notes         []string        `json:"notes"`
}

// MagpieError is a refusal the bridge can answer with.
type MagpieError struct {
	Status  int
	Code    string
	Message string
}

// Error implements error.
func (e *MagpieError) Error() string { return e.Code }

func magpieError(status int, code string) *MagpieError {
	return &MagpieError{Status: status, Code: code}
}

// MagpieOptions wires one reader. `BaseURL` is the caller's own gateway address;
// it is validated here, not trusted.
type MagpieOptions struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	Now        func() int64
	TimeoutMS  int64
}

// Magpie reads balances from a running Magpie. It never writes to Magpie and
// never asks it for a model response.
type Magpie struct {
	mu sync.Mutex
	// refreshMu serialises reads so two in-flight requests cannot land out of
	// order and leave the older reply installed.
	refreshMu sync.Mutex
	baseURL   string
	apiKey    string
	client    *http.Client
	now       func() int64
	timeoutMS int64
	// snapshot is the last successful read, tagged with the address it came
	// from; it is shown only while that is still the configured address.
	snapshot      *MagpieSnapshot
	snapshotBase  string
	lastAt        *int64
	lastError     string
	lastErrorBase string
}

// NewMagpie builds the reader. An unusable base URL is refused here so a later
// refresh cannot send a key to a host nobody named.
func NewMagpie(options MagpieOptions) (*Magpie, error) {
	reader := &Magpie{apiKey: strings.TrimSpace(options.APIKey), now: options.Now,
		timeoutMS: options.TimeoutMS}
	if reader.now == nil {
		reader.now = func() int64 { return time.Now().UnixMilli() }
	}
	if reader.timeoutMS <= 0 {
		reader.timeoutMS = 5000
	}
	if reader.timeoutMS < minTimeoutMS {
		reader.timeoutMS = minTimeoutMS
	}
	if reader.timeoutMS > maxTimeoutMS {
		reader.timeoutMS = maxTimeoutMS
	}
	if options.HTTPClient != nil {
		reader.client = options.HTTPClient
	} else {
		reader.client = &http.Client{
			Timeout: time.Duration(reader.timeoutMS) * time.Millisecond,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	base := strings.TrimSpace(options.BaseURL)
	if base == "" {
		base = MagpieDefaultBaseURL
	}
	normalized, err := NormalizeMagpieBase(base)
	if err != nil {
		return nil, err
	}
	reader.baseURL = normalized
	return reader, nil
}

// NormalizeMagpieBase validates a gateway address. Magpie listens on loopback by
// default and can be shared on a LAN or over https; plain HTTP to a remote host
// would put a shared gateway key on the wire in clear, so it is refused, exactly
// as a custom quota endpoint is.
func NormalizeMagpieBase(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxMagpieBase || isControl(value) {
		return "", magpieError(400, "invalid_magpie_address")
	}
	check := CheckEndpoint(value)
	if !check.OK {
		return "", magpieError(400, "invalid_magpie_address")
	}
	// A base with its own path would silently change the request target, so only
	// the bare origin is accepted.
	parsed, err := url.Parse(value)
	if err != nil {
		return "", magpieError(400, "invalid_magpie_address")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", magpieError(400, "invalid_magpie_address")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", magpieError(400, "invalid_magpie_address")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// Endpoint is the address this reader requests. It is exported so a caller can
// show it and so a test can assert nothing else is asked.
func (m *Magpie) Endpoint() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baseURL + MagpieQuotasPath
}

// SetBaseURL repoints the reader, validating the new address first. It waits for
// an in-flight read so the switch cannot interleave with the reply that read is
// about to install.
func (m *Magpie) SetBaseURL(raw string) error {
	normalized, err := NormalizeMagpieBase(raw)
	if err != nil {
		return err
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.mu.Lock()
	m.baseURL = normalized
	m.mu.Unlock()
	return nil
}

// Refresh makes the one request this type makes. It is serialised with
// SetBaseURL, so a reply is always installed under the address that produced it
// and a repointed reader never shows the previous gateway's balances.
func (m *Magpie) Refresh(ctx context.Context) (*MagpieSnapshot, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	m.mu.Lock()
	base := m.baseURL
	target := base + MagpieQuotasPath
	key := m.apiKey
	attemptedAt := m.now()
	m.lastAt = &attemptedAt
	m.mu.Unlock()

	request, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return m.fail(base, "network_error", nil)
	}
	request.Header.Set("Accept", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("x-api-key", key)
	}
	reply, err := m.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return m.fail(base, "timeout", nil)
		}
		// The most likely failure here is that Magpie is not running.
		return m.fail(base, "magpie_unreachable", nil)
	}
	defer func() { _ = reply.Body.Close() }()
	if reply.StatusCode < 200 || reply.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(reply.Body, maxErrorBodyBytes))
		status := reply.StatusCode
		switch {
		case status == 401 || status == 403:
			return m.fail(base, "magpie_key_rejected", &status)
		case status == 404:
			return m.fail(base, "magpie_endpoint_unavailable", &status)
		case status == 429:
			return m.fail(base, "rate_limited", &status)
		default:
			return m.fail(base, "magpie_error", &status)
		}
	}
	raw, err := io.ReadAll(io.LimitReader(reply.Body, maxResponseBytes+1))
	if err != nil {
		return m.fail(base, "network_error", nil)
	}
	if len(raw) > maxResponseBytes {
		return m.fail(base, "response_too_large", nil)
	}
	var document any
	if json.Unmarshal(raw, &document) != nil {
		return m.fail(base, "invalid_response", nil)
	}
	snapshot, code := ParseMagpieQuotas(document)
	if code != "" {
		return m.fail(base, code, nil)
	}
	stamp := m.now()
	snapshot.BaseURL = target
	snapshot.FetchedAt = optISO(&stamp)
	m.mu.Lock()
	m.snapshot = snapshot
	m.snapshotBase = base
	m.lastError = ""
	m.lastErrorBase = base
	m.mu.Unlock()
	return snapshot, nil
}

func (m *Magpie) fail(base, code string, status *int) (*MagpieSnapshot, error) {
	m.mu.Lock()
	m.lastError = code
	m.lastErrorBase = base
	m.mu.Unlock()
	return nil, magpieError(statusOr(status, 503), code)
}

func statusOr(status *int, fallback int) int {
	if status != nil && *status >= 400 && *status <= 599 {
		return *status
	}
	return fallback
}

// ParseMagpieQuotas reads the documented reply. Both the bare array and the
// `{data:[…]}` envelope the HTTP form uses are accepted, because the CLI prints
// the array and the gateway wraps it.
func ParseMagpieQuotas(document any) (*MagpieSnapshot, string) {
	rows, ok := document.([]any)
	if !ok {
		object, isObject := document.(map[string]any)
		if !isObject {
			return nil, "invalid_response"
		}
		rows, ok = object["data"].([]any)
		if !ok {
			return nil, "invalid_response"
		}
	}
	snapshot := &MagpieSnapshot{Entries: []MagpieEntry{}, Notes: []string{}}
	for _, row := range rows {
		if len(snapshot.Entries) >= maxMagpieEntries {
			break
		}
		object, ok := row.(map[string]any)
		if !ok {
			continue
		}
		entry := parseMagpieEntry(object, snapshot)
		if entry == nil {
			continue
		}
		snapshot.Entries = append(snapshot.Entries, *entry)
	}
	if len(snapshot.Entries) == 0 {
		// An empty list is a real answer ("nothing configured"), which is
		// different from a reply this package could not read. Only the latter is
		// an error, and it never becomes "0".
		if len(rows) > 0 {
			return nil, "invalid_response"
		}
	}
	return snapshot, ""
}

func parseMagpieEntry(object map[string]any, snapshot *MagpieSnapshot) *MagpieEntry {
	provider := strings.TrimSpace(stringOf(object["provider"]))
	if provider == "" || !identifierPattern.MatchString(provider) {
		return nil
	}
	entry := &MagpieEntry{Provider: provider, Name: boundText(stringOf(object["name"]), 80),
		Kind: boundText(stringOf(object["kind"]), 32), Windows: []MagpieWindow{}}
	if entry.Kind != "subscription" && entry.Kind != "balance" {
		snapshot.Notes = append(snapshot.Notes, "unknown_kind:"+provider)
	}
	entry.Plan = optString(boundText(stringOf(object["plan"]), 64))
	entry.User = optString(boundText(stringOf(object["user"]), 120))
	entry.Alias = optString(boundText(stringOf(object["alias"]), 80))
	entry.AsOf = optISO(ToTimestamp(object["asOf"]))
	entry.ReadAt = optISO(ToTimestamp(object["readAt"]))
	entry.LastServedAt = optISO(ToTimestamp(object["lastServedAt"]))
	if windows, ok := object["windows"].([]any); ok {
		for _, raw := range windows {
			if len(entry.Windows) >= maxMagpieWindows {
				break
			}
			window, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if parsed, code := parseMagpieWindow(window); code == "" {
				entry.Windows = append(entry.Windows, parsed)
			} else {
				snapshot.Notes = append(snapshot.Notes, code+":"+provider)
			}
		}
	}
	if raw, ok := object["balance"].(string); ok {
		text := boundText(raw, 40)
		entry.Balance = optString(text)
		if currency, amount := parseMagpieBalance(text); amount != nil {
			entry.BalanceCurrency = optString(currency)
			entry.BalanceAmount = amount
		} else if text != "" {
			snapshot.Notes = append(snapshot.Notes, "balance_unparsed:"+provider)
		}
	}
	if len(entry.Windows) == 0 && entry.Balance == nil {
		return nil
	}
	return entry
}

func parseMagpieWindow(window map[string]any) (MagpieWindow, string) {
	result := MagpieWindow{Name: boundText(stringOf(window["name"]), 60)}
	if result.Name == "" {
		return MagpieWindow{}, "window_unreadable"
	}
	result.ResetAt = optISO(ToTimestamp(window["resetsAt"]))
	result.Display = optString(boundText(stringOf(window["display"]), 80))
	for _, name := range []string{"holdsWhy", "note"} {
		if text := boundText(stringOf(window[name]), 40); text != "" {
			result.Note = optString(text)
			break
		}
	}
	amount := ToAmount(window["amount"])
	limit := ToAmount(window["limit"])
	used := ToAmount(window["used"])
	remaining := ToAmount(window["remaining"])
	switch {
	case amount != nil || limit != nil:
		// Absolute figures win: a percentage that Magpie derived from them is a
		// second rounding of the same fact.
		result.Basis = MagpieBasisAmount
		result.Amount = amount
		result.Limit = limit
		if unit := boundText(stringOf(window["unit"]), 16); unit != "" {
			result.Unit = optString(unit)
		}
	case used != nil || remaining != nil:
		result.Basis = MagpieBasisPercent
		result.Used = used
		result.Remaining = remaining
	case result.Display != nil:
		// Nothing but a rendered string: the numbers are unknown. Keeping the
		// text and no figure is the only honest rendering.
		result.Basis = MagpieBasisText
	default:
		return MagpieWindow{}, "window_unreadable"
	}
	if result.ResetAt == nil {
		if runsOut := ToTimestamp(window["runsOutAt"]); runsOut != nil {
			result.ResetAt = optISO(runsOut)
		}
	}
	return result, ""
}

// parseMagpieBalance reads Magpie's formatted balance. The currency is the symbol
// the string carries; an unrecognised symbol leaves the amount unset rather than
// guessing which currency it is.
func parseMagpieBalance(text string) (string, *float64) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", nil
	}
	currency := ""
	rest := trimmed
	for symbol, code := range map[string]string{"$": "USD", "¥": "CNY", "€": "EUR", "£": "GBP"} {
		if strings.HasPrefix(trimmed, symbol) {
			currency = code
			rest = strings.TrimSpace(strings.TrimPrefix(trimmed, symbol))
			break
		}
	}
	if currency == "" {
		for code := range customUnits {
			if len(code) != 3 || !currencyPattern.MatchString(code) {
				continue
			}
			if len(trimmed) >= len(code) && strings.EqualFold(trimmed[len(trimmed)-len(code):], code) {
				currency = code
				rest = strings.TrimSpace(trimmed[:len(trimmed)-len(code)])
				break
			}
		}
	}
	if currency == "" {
		return "", nil
	}
	cleaned := strings.ReplaceAll(rest, ",", "")
	parsed, err := strconv.ParseFloat(cleaned, 64)
	if err != nil {
		return "", nil
	}
	amount := round6(parsed)
	if amount != 0 && (amount > maxAmount || amount < -maxAmount) {
		return "", nil
	}
	return currency, &amount
}

func boundText(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if isControl(trimmed) {
		return ""
	}
	runes := []rune(trimmed)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return trimmed
}

// State is the panel payload. Pure: no request, no key read.
//
// A snapshot or a failure recorded against another address is not this address's
// answer, so after a repoint the card reports "not read" instead of the previous
// gateway's balances.
func (m *Magpie) State() MagpieState {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := MagpieState{SchemaVersion: 1, BaseURL: m.baseURL, Status: "not_read",
		KeyConfigured: m.apiKey != "", Notes: []string{}}
	mismatched := false
	if m.lastErrorBase == m.baseURL && m.lastError != "" {
		state.ErrorReason = optString(m.lastError)
		state.LastAttemptAt = optISO(m.lastAt)
		state.Status = "error"
	} else if m.lastError != "" {
		mismatched = true
	}
	if m.snapshot != nil && m.snapshotBase == m.baseURL {
		state.Snapshot = m.snapshot
		state.Status = "ok"
		if m.lastErrorBase == m.baseURL && m.lastError != "" {
			// A failed read after a good one keeps the old numbers and says they
			// are old; it never writes them as 0.
			state.Status = "stale"
			state.Stale = true
		}
	} else if m.snapshot != nil {
		mismatched = true
	}
	if mismatched {
		state.Notes = append(state.Notes, "reading_from_another_address")
	}
	return state
}

// Magpie actions the bridge may pass through.
const (
	MagpieActionState   = "state"
	MagpieActionRefresh = "refresh"
	MagpieActionAddress = "address"
)

// Action answers one panel request:
//
//	state   -> {"state": MagpieState}
//	refresh -> {"state": …} one read
//	address -> {"baseUrl": "http://127.0.0.1:3425"}  repoints the reader
//
// A rejected address answers 400 invalid_magpie_address; a read that could not
// happen answers with the state carrying the reason, never with 0.
func (m *Magpie) Action(ctx context.Context, action string, input map[string]any) (map[string]any, error) {
	switch action {
	case "", MagpieActionState:
		return map[string]any{"state": m.State()}, nil
	case MagpieActionRefresh:
		snapshot, err := m.Refresh(ctx)
		answer := map[string]any{"state": m.State()}
		if err != nil {
			answer["lastRefresh"] = map[string]any{"status": m.State().Status,
				"errorReason": err.Error()}
			return answer, nil
		}
		answer["lastRefresh"] = map[string]any{"status": "ok", "entries": len(snapshot.Entries),
			"notes": snapshot.Notes}
		return answer, nil
	case MagpieActionAddress:
		raw := stringOf(input["baseUrl"])
		if err := m.SetBaseURL(raw); err != nil {
			var refusal *MagpieError
			if typed, ok := err.(*MagpieError); ok {
				refusal = typed
			}
			if refusal != nil {
				return nil, refusal
			}
			return nil, magpieError(400, "invalid_magpie_address")
		}
		return map[string]any{"state": m.State()}, nil
	}
	return nil, magpieError(404, "not_found")
}
