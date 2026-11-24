package quota

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CredentialResolver hands out the secret for one reference. It is the caller's
// in-memory resolver: nothing here reads a file, a cookie jar or an agent's
// sign-in store, and a returned secret never leaves this package.
type CredentialResolver func(ref string) string

// Options configures one manager. There is no timer and no background work: a
// reading changes only inside Refresh.
type Options struct {
	StateDir    string
	Credentials CredentialResolver
	HTTPClient  *http.Client
	Now         func() int64
	FreshnessMS int64
	ThrottleMS  int64
	TimeoutMS   int64
	Seed        []map[string]any
}

// Reading is the last attempt and the last success for one source.
type Reading struct {
	LastAttemptAt *int64   `json:"lastAttemptAt"`
	LastAttemptOK bool     `json:"lastAttemptOk"`
	ErrorReason   *string  `json:"errorReason"`
	LastSuccessAt *int64   `json:"lastSuccessAt"`
	Available     *bool    `json:"available"`
	Parts         []Part   `json:"parts"`
	Records       []Record `json:"records"`
}

// Card is one account's presentation.
type Card struct {
	SourceID         string       `json:"sourceId"`
	ProviderID       string       `json:"providerId"`
	SourceName       string       `json:"sourceName"`
	Kind             string       `json:"kind"`
	Supported        bool         `json:"supported"`
	Status           string       `json:"status"`
	Stale            bool         `json:"stale"`
	StaleReason      *string      `json:"staleReason"`
	AccountRefMasked string       `json:"accountRefMasked"`
	BillingIdentity  string       `json:"billingIdentity"`
	ObservedAt       *string      `json:"observedAt"`
	LastSuccessfulAt *string      `json:"lastSuccessfulAt"`
	LastAttemptAt    *string      `json:"lastAttemptAt"`
	ErrorReason      *string      `json:"errorReason"`
	Available        *bool        `json:"available"`
	Parts            []Part       `json:"parts"`
	CredentialRef    string       `json:"credentialRef"`
	OfficialPage     *string      `json:"officialPage"`
	ConsoleURL       *string      `json:"consoleUrl"`
	Notes            []string     `json:"notes"`
	Records          []RecordView `json:"records"`
}

// RecordView is one flat quota row the panel renders.
type RecordView struct {
	SourceID         string   `json:"sourceId"`
	ProviderID       string   `json:"providerId"`
	AccountRefMasked string   `json:"accountRefMasked"`
	BillingIdentity  string   `json:"billingIdentity"`
	Kind             string   `json:"kind"`
	WindowLabel      *string  `json:"windowLabel"`
	Unit             *string  `json:"unit"`
	Limit            *float64 `json:"limit"`
	Used             *float64 `json:"used"`
	Remaining        *float64 `json:"remaining"`
	PeriodStart      *string  `json:"periodStart"`
	ResetAt          *string  `json:"resetAt"`
	ObservedAt       *string  `json:"observedAt"`
	LastSuccessfulAt *string  `json:"lastSuccessfulAt"`
	Stale            bool     `json:"stale"`
	Status           string   `json:"status"`
	ErrorReason      *string  `json:"errorReason"`
	SourceName       string   `json:"sourceName"`
}

// CacheState reports whether readings are being persisted.
type CacheState struct {
	Enabled     bool    `json:"enabled"`
	LastReadAt  *int64  `json:"lastReadAt"`
	LastWriteAt *int64  `json:"lastWriteAt"`
	Error       *string `json:"error"`
}

// State is the whole panel payload. It is pure: no request, no write, no timer.
type State struct {
	SchemaVersion int          `json:"schemaVersion"`
	Cards         []Card       `json:"cards"`
	Records       []RecordView `json:"records"`
	Sources       []SourceView `json:"sources"`
	Presets       []Preset     `json:"presets"`
	RefreshedAt   *string      `json:"refreshedAt"`
	FreshnessMS   int64        `json:"freshnessMs"`
	Cache         CacheState   `json:"cache"`
	// Combining is stated in the payload so a renderer cannot quietly invent a
	// total.
	Combining string `json:"combining"`
}

// RefreshResult reports one refresh.
type RefreshResult struct {
	SourceID    string  `json:"sourceId"`
	Status      string  `json:"status"`
	ErrorReason *string `json:"errorReason"`
	HTTPStatus  *int    `json:"httpStatus,omitempty"`
	Skipped     *string `json:"skipped,omitempty"`
}

// RefreshReport is the whole answer of one refresh call.
type RefreshReport struct {
	RefreshedAt *string         `json:"refreshedAt"`
	Results     []RefreshResult `json:"results"`
	Cache       CacheState      `json:"cache"`
}

// Accounts owns the stored source list, the last reading per source, and
// nothing else.
type Accounts struct {
	mu          sync.Mutex
	stateDir    string
	credentials CredentialResolver
	client      *http.Client
	now         func() int64
	freshnessMS int64
	throttleMS  int64
	timeoutMS   int64
	sourcesFile string
	cacheFile   string
	sources        []Source
	sourcesCorrupt bool
	readings       map[string]*Reading
	refreshedAt    *int64
	cache          CacheState
	refreshMu      sync.Mutex
}

// New loads the stored sources and readings.
func New(options Options) *Accounts {
	manager := &Accounts{stateDir: options.StateDir, credentials: options.Credentials,
		now: options.Now, freshnessMS: options.FreshnessMS, throttleMS: options.ThrottleMS,
		timeoutMS: options.TimeoutMS, readings: map[string]*Reading{}}
	if manager.now == nil {
		manager.now = func() int64 { return time.Now().UnixMilli() }
	}
	if manager.freshnessMS < 0 {
		manager.freshnessMS = defaultFreshnessMS
	} else if manager.freshnessMS == 0 {
		manager.freshnessMS = defaultFreshnessMS
	}
	// A negative throttle disables it entirely, which is what a test or an
	// explicit "refresh now" needs; zero means the documented default.
	if manager.throttleMS < 0 {
		manager.throttleMS = 0
	} else if manager.throttleMS == 0 {
		manager.throttleMS = defaultThrottleMS
	}
	if manager.timeoutMS <= 0 {
		manager.timeoutMS = defaultTimeoutMS
	}
	if manager.timeoutMS < minTimeoutMS {
		manager.timeoutMS = minTimeoutMS
	}
	if manager.timeoutMS > maxTimeoutMS {
		manager.timeoutMS = maxTimeoutMS
	}
	if options.HTTPClient != nil {
		manager.client = options.HTTPClient
	} else {
		manager.client = &http.Client{
			Timeout: time.Duration(manager.timeoutMS) * time.Millisecond,
			// A redirect would replay the Authorization header at a host that
			// answered 302, so it is refused instead of followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if options.StateDir != "" {
		absolute, err := filepath.Abs(options.StateDir)
		if err == nil {
			manager.stateDir = absolute
			manager.sourcesFile = filepath.Join(absolute, "quota-sources.json")
			manager.cacheFile = filepath.Join(absolute, "quota-cache.json")
		}
	}
	manager.cache = CacheState{Enabled: manager.stateDir != ""}
	manager.load(options.Seed)
	return manager
}

// --- persistence -----------------------------------------------------------

func (a *Accounts) readJSON(file string) (map[string]any, bool, bool) {
	if file == "" {
		return nil, false, false
	}
	stat, err := os.Lstat(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, false
		}
		a.setCacheError("store_unavailable")
		return nil, false, false
	}
	if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() {
		a.setCacheError("store_unavailable")
		return nil, false, false
	}
	if stat.Size() > 4*1024*1024 {
		a.setCacheError("store_unavailable")
		return nil, false, true
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		a.setCacheError("store_unavailable")
		return nil, false, true
	}
	var document map[string]any
	if json.Unmarshal(stripBOM(raw), &document) != nil || document == nil {
		a.setCacheError("store_unavailable")
		return nil, false, true
	}
	return document, true, false
}

func stripBOM(raw []byte) []byte {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		return raw[3:]
	}
	return raw
}

func (a *Accounts) setCacheError(code string) {
	value := code
	a.cache.Error = &value
}

func (a *Accounts) writeJSON(file string, value any) {
	if file == "" {
		return
	}
	fail := func() { a.setCacheError("store_unavailable") }
	directory := filepath.Dir(file)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		fail()
		return
	}
	dirStat, err := os.Lstat(directory)
	if err != nil || dirStat.Mode()&os.ModeSymlink != 0 || !dirStat.IsDir() {
		fail()
		return
	}
	if stat, err := os.Lstat(file); err == nil {
		if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() {
			fail()
			return
		}
	} else if !os.IsNotExist(err) {
		fail()
		return
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fail()
		return
	}
	encoded = append(encoded, '\n')
	temporary := file + "." + randomToken() + ".tmp"
	handle, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		fail()
		return
	}
	_, writeErr := handle.Write(encoded)
	closeErr := handle.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		fail()
		return
	}
	if err := os.Rename(temporary, file); err != nil {
		_ = os.Remove(temporary)
		fail()
		return
	}
	stamp := a.now()
	a.cache.LastWriteAt = &stamp
	a.cache.Error = nil
}

func randomToken() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(buf[:])
}

func (a *Accounts) load(seed []map[string]any) {
	document, present, corrupt := a.readJSON(a.sourcesFile)
	if corrupt {
		a.sourcesCorrupt = true
	}
	stored := []Source{}
	if present {
		schema, _ := document["schemaVersion"].(float64)
		rows, _ := document["sources"].([]any)
		if schema != 1 || rows == nil {
			a.setCacheError("store_unavailable")
			a.sourcesCorrupt = true
		} else {
			for _, row := range rows {
				if len(stored) >= maxSources {
					break
				}
				object, ok := row.(map[string]any)
				if !ok {
					continue
				}
				// A hand-edited entry is dropped rather than trusted.
				if source, err := NormalizeSource(object); err == nil {
					stored = append(stored, source)
				}
			}
		}
		stamp := a.now()
		a.cache.LastReadAt = &stamp
	}
	// Seed sources are the caller's defaults, used only when nothing is stored.
	if len(stored) == 0 && len(seed) > 0 {
		for _, row := range seed {
			if len(stored) >= maxSources {
				break
			}
			if source, err := NormalizeSource(row); err == nil {
				stored = append(stored, source)
			}
		}
	}
	seen := map[string]bool{}
	for _, source := range stored {
		if seen[source.SourceID] {
			continue
		}
		seen[source.SourceID] = true
		a.sources = append(a.sources, source)
	}
	cached, present, _ := a.readJSON(a.cacheFile)
	if present {
		schema, _ := cached["schemaVersion"].(float64)
		if schema == 1 {
			if rows, ok := cached["sources"].(map[string]any); ok {
				for _, source := range a.sources {
					if entry, ok := rows[source.SourceID].(map[string]any); ok {
						if reading := normalizeReading(entry); reading != nil {
							a.readings[source.SourceID] = reading
						}
					}
				}
			}
			if refreshed, ok := numberPtr(cached["refreshedAt"]); ok {
				a.refreshedAt = refreshed
			}
		}
	}
}

// normalizeReading treats the cache file as untrusted input: a record that does
// not carry the shape this package writes is dropped instead of being shown as
// a number.
func normalizeReading(entry map[string]any) *Reading {
	reading := &Reading{Parts: []Part{}, Records: []Record{}}
	reading.LastAttemptAt, _ = numberPtr(entry["lastAttemptAt"])
	reading.LastAttemptOK, _ = entry["lastAttemptOk"].(bool)
	if raw, ok := entry["errorReason"].(string); ok {
		reading.ErrorReason = &raw
	}
	if raw, ok := numberPtr(entry["lastSuccessAt"]); ok && *raw > 0 {
		reading.LastSuccessAt = raw
	}
	if raw, ok := entry["available"].(bool); ok {
		reading.Available = &raw
	}
	if rows, ok := entry["parts"].([]any); ok {
		for _, row := range rows {
			if len(reading.Parts) >= 8 {
				break
			}
			part, ok := row.(map[string]any)
			if !ok {
				continue
			}
			label, ok := part["label"].(string)
			amount := ToAmount(part["amount"])
			if !ok || amount == nil {
				continue
			}
			unit, _ := part["unit"].(string)
			reading.Parts = append(reading.Parts, Part{Label: label, Amount: *amount, Unit: optString(unit)})
		}
	}
	if rows, ok := entry["records"].([]any); ok {
		for _, row := range rows {
			if len(reading.Records) >= maxRecordsPerSource {
				break
			}
			record, ok := row.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := record["kind"].(string)
			if !containsString(Kinds, kind) {
				continue
			}
			unitRaw, unitPresent := record["unit"]
			if unitPresent && unitRaw != nil {
				if _, ok := unitRaw.(string); !ok {
					continue
				}
			}
			// A field that is present but not a number is corrupt, not an
			// unknown value: this package only ever writes numbers or null, so
			// such a record is dropped whole rather than shown with a gap.
			limit, okLimit := amountOrNil(record["limit"])
			used, okUsed := amountOrNil(record["used"])
			remaining, okRemaining := amountOrNil(record["remaining"])
			if !okLimit || !okUsed || !okRemaining {
				continue
			}
			if limit == nil && used == nil && remaining == nil {
				continue
			}
			window, _ := record["windowLabel"].(string)
			if len([]rune(window)) > 40 {
				window = string([]rune(window)[:40])
			}
			periodStart, _ := numberPtr(record["periodStart"])
			resetAt, _ := numberPtr(record["resetAt"])
			unit, _ := unitRaw.(string)
			reading.Records = append(reading.Records, Record{Kind: kind, Unit: optString(unit),
				WindowLabel: optString(window), Limit: limit, Used: used, Remaining: remaining,
				PeriodStart: periodStart, ResetAt: resetAt})
		}
	}
	return reading
}

func amountOrNil(value any) (*float64, bool) {
	if value == nil {
		return nil, true
	}
	amount := ToAmount(value)
	return amount, amount != nil
}

func numberPtr(value any) (*int64, bool) {
	switch item := value.(type) {
	case float64:
		if item != float64(int64(item)) {
			return nil, false
		}
		result := int64(item)
		return &result, true
	case int64:
		result := item
		return &result, true
	case json.Number:
		parsed, err := item.Int64()
		if err != nil {
			return nil, false
		}
		return &parsed, true
	}
	return nil, false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (a *Accounts) quarantineCorruptSources() bool {
	if !a.sourcesCorrupt || a.sourcesFile == "" {
		return true
	}
	stat, err := os.Lstat(a.sourcesFile)
	if err != nil {
		if os.IsNotExist(err) {
			a.sourcesCorrupt = false
			return true
		}
		a.setCacheError("store_unavailable")
		return false
	}
	if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() {
		a.setCacheError("store_unavailable")
		return false
	}
	backup := a.sourcesFile + ".corrupt." + strconv.FormatInt(a.now(), 10)
	if _, err := os.Lstat(backup); err == nil {
		backup = backup + "." + randomToken()[:8]
	}
	if err := os.Rename(a.sourcesFile, backup); err != nil {
		a.setCacheError("store_unavailable")
		return false
	}
	a.sourcesCorrupt = false
	return true
}

func (a *Accounts) saveSources() {
	if !a.quarantineCorruptSources() {
		return
	}
	rows := make([]any, 0, len(a.sources))
	for _, source := range a.sources {
		view := Describe(source)
		encoded, _ := json.Marshal(view)
		var decoded map[string]any
		_ = json.Unmarshal(encoded, &decoded)
		rows = append(rows, decoded)
	}
	a.writeJSON(a.sourcesFile, map[string]any{"schemaVersion": 1, "sources": rows})
}

func (a *Accounts) saveCache() {
	if a.cacheFile == "" {
		return
	}
	sources := map[string]any{}
	for sourceID, reading := range a.readings {
		sources[sourceID] = map[string]any{"lastAttemptAt": reading.LastAttemptAt,
			"lastAttemptOk": reading.LastAttemptOK, "errorReason": reading.ErrorReason,
			"lastSuccessAt": reading.LastSuccessAt, "available": reading.Available,
			"parts": reading.Parts, "records": reading.Records}
	}
	a.writeJSON(a.cacheFile, map[string]any{"schemaVersion": 1, "refreshedAt": a.refreshedAt,
		"sources": sources})
}

// --- source management -----------------------------------------------------

// ListSources returns the stored sources, in order.
func (a *Accounts) ListSources() []SourceView {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]SourceView, 0, len(a.sources))
	for _, source := range a.sources {
		out = append(out, Describe(source))
	}
	return out
}

// AddSource validates and stores one source. It never touches a credential: the
// caller stores secrets in its own secure store and only hands this package a
// reference name.
func (a *Accounts) AddSource(input map[string]any) (SourceView, error) {
	source, err := NormalizeSource(input)
	if err != nil {
		return SourceView{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, existing := range a.sources {
		if existing.SourceID == source.SourceID {
			return SourceView{}, sourceError("duplicate_source", "A source with that id already exists.")
		}
	}
	if len(a.sources) >= maxSources {
		return SourceView{}, sourceError("too_many_sources", "Too many sources.")
	}
	a.sources = append(a.sources, source)
	a.saveSources()
	return Describe(source), nil
}

// RemoveSource deletes one source and its reading.
func (a *Accounts) RemoveSource(sourceID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, source := range a.sources {
		if source.SourceID != sourceID {
			continue
		}
		a.sources = append(a.sources[:i], a.sources[i+1:]...)
		delete(a.readings, sourceID)
		a.saveSources()
		a.saveCache()
		return true
	}
	return false
}

// RelabelSource renames an account. A label is the only mutable part of a
// stored source, and it must never carry a credential.
func (a *Accounts) RelabelSource(sourceID, accountLabel string) (SourceView, error) {
	label, err := SanitizeLabel(accountLabel)
	if err != nil {
		return SourceView{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, source := range a.sources {
		if source.SourceID != sourceID {
			continue
		}
		a.sources[i].AccountLabel = optString(label)
		a.saveSources()
		return Describe(a.sources[i]), nil
	}
	return SourceView{}, sourceError("unknown_source", "No such source.")
}

// --- reading ---------------------------------------------------------------

func (a *Accounts) resolve(ref string) string {
	if a.credentials == nil {
		return ""
	}
	secret := strings.TrimSpace(a.credentials(ref))
	if secret == "" || len(secret) > 4096 || isControl(secret) {
		return ""
	}
	return secret
}

type httpTarget struct {
	url     string
	method  string
	headers map[string]string
	body    []byte
}

func targetOf(source Source, secret string) (*httpTarget, string) {
	headers := map[string]string{"Accept": "application/json"}
	if source.Adapter == "custom" {
		if source.Auth["mode"] == "header" {
			name := source.Auth["headerName"]
			if name == "authorization" {
				name = "Authorization"
			}
			headers[name] = secret
		} else {
			headers["Authorization"] = "Bearer " + secret
		}
		target := &httpTarget{url: source.Endpoint, method: source.Method, headers: headers}
		if source.Method == "POST" {
			headers["Content-Type"] = "application/json"
			body := source.Body
			if body == nil {
				body = map[string]any{}
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				return nil, "invalid_body"
			}
			target.body = encoded
		}
		return target, ""
	}
	endpoint := ""
	switch source.Adapter {
	case "deepseek":
		endpoint = DeepSeekEndpoint
	case "moonshot-cn":
		endpoint = MoonshotCNEndpoint
	case "moonshot-global":
		endpoint = MoonshotGlobalEndpoint
	default:
		return nil, "unsupported_source"
	}
	headers["Authorization"] = "Bearer " + secret
	return &httpTarget{url: endpoint, method: "GET", headers: headers}, ""
}

func (a *Accounts) fetch(ctx context.Context, target *httpTarget) (any, string, *int) {
	if a.client == nil {
		return nil, "network_error", nil
	}
	request, err := http.NewRequestWithContext(ctx, target.method, target.url, strings.NewReader(string(target.body)))
	if err != nil {
		return nil, "network_error", nil
	}
	for name, value := range target.headers {
		request.Header.Set(name, value)
	}
	reply, err := a.client.Do(request)
	if err != nil {
		if isTimeoutError(ctx, err) {
			return nil, "timeout", nil
		}
		return nil, "network_error", nil
	}
	defer func() { _ = reply.Body.Close() }()
	if reply.StatusCode < 200 || reply.StatusCode >= 300 {
		// The body is read only to drain it; nothing from it is kept or shown.
		_, _ = io.Copy(io.Discard, io.LimitReader(reply.Body, maxErrorBodyBytes))
		status := reply.StatusCode
		switch {
		case status == 401 || status == 403:
			return nil, "auth_rejected", &status
		case status == 429:
			return nil, "rate_limited", &status
		default:
			return nil, "upstream_error", &status
		}
	}
	raw, err := io.ReadAll(io.LimitReader(reply.Body, maxResponseBytes+1))
	if err != nil {
		if isTimeoutError(ctx, err) {
			return nil, "timeout", nil
		}
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

func (a *Accounts) parse(source Source, document any) (Parsed, string) {
	if source.Adapter == "custom" {
		return parseCustom(source, document)
	}
	var parsed Parsed
	var code string
	switch source.Adapter {
	case "deepseek":
		parsed, code = ParseDeepSeekBalance(document)
	case "moonshot-cn", "moonshot-global":
		parsed, code = ParseMoonshotBalance(document)
	default:
		return Parsed{}, "unsupported_source"
	}
	if code != "" {
		return Parsed{}, code
	}
	for i := range parsed.Parts {
		if parsed.Parts[i].Unit == nil {
			parsed.Parts[i].Unit = source.Unit
		}
	}
	for i := range parsed.Records {
		if parsed.Records[i].Unit == nil {
			parsed.Records[i].Unit = source.Unit
		}
	}
	return parsed, ""
}

func parseCustom(source Source, document any) (Parsed, string) {
	type fieldRead struct {
		present bool
		value   any
	}
	read := func(name string) fieldRead {
		dotted := source.Fields[name]
		if dotted == "" {
			return fieldRead{}
		}
		value, ok := ReadPath(document, dotted)
		if !ok {
			return fieldRead{present: true}
		}
		return fieldRead{present: true, value: value}
	}
	remainingRead := read("remaining")
	usedRead := read("used")
	limitRead := read("limit")
	resetRead := read("resetAt")
	remaining := ToAmount(remainingRead.value)
	used := ToAmount(usedRead.value)
	limit := ToAmount(limitRead.value)
	// A declared field that is not a number is a failed read, not a zero: the
	// reply no longer answers in the shape the person configured.
	for _, entry := range []fieldRead{remainingRead, usedRead, limitRead} {
		if entry.present && ToAmount(entry.value) == nil {
			return Parsed{}, "value_missing"
		}
	}
	var derived *float64
	switch {
	case remaining != nil:
		derived = remaining
	case limit != nil && used != nil:
		value := round6(*limit - *used)
		derived = &value
	}
	if remaining == nil && used == nil && limit == nil {
		return Parsed{}, "value_missing"
	}
	var resetAt *int64
	if resetRead.present {
		resetAt = ToTimestamp(resetRead.value)
		if resetAt == nil {
			return Parsed{}, "value_missing"
		}
	}
	return Parsed{Parts: []Part{}, Records: []Record{{Unit: source.Unit, WindowLabel: source.WindowLabel,
		Limit: limit, Used: used, Remaining: derived, ResetAt: resetAt}}}, ""
}

func cloneReading(r *Reading) *Reading {
	if r == nil {
		return nil
	}
	copied := *r
	if r.LastAttemptAt != nil {
		v := *r.LastAttemptAt
		copied.LastAttemptAt = &v
	}
	if r.ErrorReason != nil {
		v := *r.ErrorReason
		copied.ErrorReason = &v
	}
	if r.LastSuccessAt != nil {
		v := *r.LastSuccessAt
		copied.LastSuccessAt = &v
	}
	if r.Available != nil {
		v := *r.Available
		copied.Available = &v
	}
	if r.Parts != nil {
		copied.Parts = make([]Part, len(r.Parts))
		copy(copied.Parts, r.Parts)
	}
	if r.Records != nil {
		copied.Records = make([]Record, len(r.Records))
		copy(copied.Records, r.Records)
	}
	return &copied
}

func (a *Accounts) hasSourceLocked(sourceID string) bool {
	for _, source := range a.sources {
		if source.SourceID == sourceID {
			return true
		}
	}
	return false
}

func (a *Accounts) storeReading(sourceID string, reading *Reading) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.hasSourceLocked(sourceID) {
		return
	}
	a.readings[sourceID] = reading
}

func (a *Accounts) refreshOne(ctx context.Context, source Source, force bool) RefreshResult {
	a.mu.Lock()
	previous := cloneReading(a.readings[source.SourceID])
	a.mu.Unlock()
	attemptedAt := a.now()
	if !source.Supported {
		a.storeReading(source.SourceID, &Reading{LastAttemptAt: &attemptedAt,
			ErrorReason: optString("unsupported_source"), Parts: []Part{}, Records: []Record{}})
		return RefreshResult{SourceID: source.SourceID, Status: "unsupported",
			ErrorReason: optString("unsupported_source")}
	}
	if !force && previous != nil && previous.LastAttemptAt != nil &&
		attemptedAt-*previous.LastAttemptAt < a.throttleMS {
		return RefreshResult{SourceID: source.SourceID, Status: "throttled", Skipped: optString("throttled")}
	}
	secret := a.resolve(source.CredentialRef)
	if secret == "" {
		reading := &Reading{LastAttemptAt: &attemptedAt, ErrorReason: optString("credential_missing"),
			Parts: []Part{}, Records: []Record{}}
		if previous != nil {
			reading.LastSuccessAt = previous.LastSuccessAt
			reading.Available = previous.Available
			reading.Parts = previous.Parts
			reading.Records = previous.Records
		}
		a.storeReading(source.SourceID, reading)
		return RefreshResult{SourceID: source.SourceID, Status: "not_configured",
			ErrorReason: optString("credential_missing")}
	}
	// Re-check the stored endpoint before every request: a config file is a file
	// on disk and can be edited after it was validated.
	if source.Adapter == "custom" {
		if check := CheckEndpoint(source.Endpoint); !check.OK {
			return a.failSource(source, attemptedAt, previous, check.Code, nil)
		}
	}
	target, code := targetOf(source, secret)
	if target == nil {
		return a.failSource(source, attemptedAt, previous, code, nil)
	}
	document, code, status := a.fetch(ctx, target)
	if code != "" {
		return a.failSource(source, attemptedAt, previous, code, status)
	}
	parsed, code := a.parse(source, document)
	if code != "" {
		return a.failSource(source, attemptedAt, previous, code, nil)
	}
	records := make([]Record, 0, len(parsed.Records))
	for _, record := range parsed.Records {
		window := record.WindowLabel
		if window == nil {
			window = source.WindowLabel
		}
		records = append(records, Record{Kind: source.Kind, Unit: record.Unit, WindowLabel: window,
			Limit: record.Limit, Used: record.Used, Remaining: record.Remaining,
			PeriodStart: record.PeriodStart, ResetAt: record.ResetAt})
	}
	successAt := attemptedAt
	a.storeReading(source.SourceID, &Reading{LastAttemptAt: &attemptedAt, LastAttemptOK: true,
		LastSuccessAt: &successAt, Available: parsed.Available, Parts: parsed.Parts, Records: records})
	return RefreshResult{SourceID: source.SourceID, Status: "ok"}
}

func (a *Accounts) failSource(source Source, attemptedAt int64, previous *Reading, code string, status *int) RefreshResult {
	reading := &Reading{LastAttemptAt: &attemptedAt, ErrorReason: optString(code),
		Parts: []Part{}, Records: []Record{}}
	if previous != nil {
		reading.LastSuccessAt = previous.LastSuccessAt
		reading.Available = previous.Available
		reading.Parts = previous.Parts
		reading.Records = previous.Records
	}
	a.storeReading(source.SourceID, reading)
	// A failed read keeps what was last read. It is never written as 0 and never
	// as "no limit".
	result := RefreshResult{SourceID: source.SourceID, ErrorReason: optString(code), HTTPStatus: status}
	if len(reading.Records) > 0 {
		result.Status = "stale"
	} else {
		result.Status = "error"
	}
	return result
}

// Refresh is the only method that makes a request. Called by the panel when a
// person clicks refresh, or by the caller on its own explicit schedule — never
// from State and never on a timer inside this package.
//
// A second call while a refresh is running waits for that one and then runs its
// own selection, rather than being answered with another call's results.
func (a *Accounts) Refresh(ctx context.Context, sourceIDs []string, force bool) RefreshReport {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	a.mu.Lock()
	sources := make([]Source, len(a.sources))
	copy(sources, a.sources)
	a.mu.Unlock()
	var selected []Source
	unknown := []string{}
	if sourceIDs == nil {
		selected = sources
	} else {
		known := map[string]bool{}
		for _, source := range sources {
			known[source.SourceID] = true
		}
		for _, id := range sourceIDs {
			if !known[id] {
				unknown = append(unknown, id)
			}
		}
		for _, source := range sources {
			for _, id := range sourceIDs {
				if source.SourceID == id {
					selected = append(selected, source)
					break
				}
			}
		}
	}
	results := []RefreshResult{}
	for _, source := range selected {
		results = append(results, a.refreshOne(ctx, source, force))
	}
	for _, id := range unknown {
		results = append(results, RefreshResult{SourceID: id, Status: "unknown",
			ErrorReason: optString("unknown_source")})
	}
	a.mu.Lock()
	stamp := a.now()
	a.refreshedAt = &stamp
	a.saveCache()
	cache := a.cache
	a.mu.Unlock()
	return RefreshReport{RefreshedAt: optISO(&stamp), Results: results, Cache: cache}
}

func optISO(ms *int64) *string {
	if ms == nil || *ms <= 0 {
		return nil
	}
	value := time.UnixMilli(*ms).UTC().Format("2006-01-02T15:04:05.000Z")
	return &value
}

// --- presentation ----------------------------------------------------------

func (a *Accounts) statusOf(source Source, reading *Reading, hasCredential bool) (string, bool, *string) {
	if !source.Supported {
		return "unsupported", false, nil
	}
	if reading == nil {
		if hasCredential {
			return "not_read", false, nil
		}
		return "not_configured", false, nil
	}
	if reading.LastAttemptOK {
		age := int64(math_MaxInt64)
		if reading.LastSuccessAt != nil {
			age = a.now() - *reading.LastSuccessAt
		}
		if age > a.freshnessMS {
			return "stale", true, optString("age")
		}
		return "ok", false, nil
	}
	if reading.ErrorReason != nil && *reading.ErrorReason == "credential_missing" {
		if hasCredential {
			return "not_read", false, nil
		}
		return "not_configured", false, nil
	}
	if len(reading.Records) > 0 {
		return "stale", true, optString("refresh_failed")
	}
	return "error", false, nil
}

const math_MaxInt64 = int64(^uint64(0) >> 1)

func (a *Accounts) maskedRef(source Source, secret string) string {
	if source.AccountLabel != nil && *source.AccountLabel != "" {
		return *source.AccountLabel
	}
	if tag := AccountTag(secret); tag != "" {
		return tag
	}
	if source.CredentialRef != "" {
		return AccountTag(source.CredentialRef)
	}
	return "acct-unresolved"
}

// State is masked cards plus cache state. Pure: no request, no write, no timer.
func (a *Accounts) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := State{SchemaVersion: 1, Cards: []Card{}, Records: []RecordView{}, Sources: []SourceView{},
		Presets: Presets(), RefreshedAt: optISO(a.refreshedAt), FreshnessMS: a.freshnessMS,
		Cache: a.cache, Combining: "never"}
	for _, source := range a.sources {
		reading := a.readings[source.SourceID]
		secret := ""
		if source.Supported {
			secret = a.resolve(source.CredentialRef)
		}
		status, stale, staleReason := a.statusOf(source, reading, secret != "")
		masked := a.maskedRef(source, secret)
		var records []Record
		if reading != nil {
			records = reading.Records
		}
		if len(records) == 0 {
			records = []Record{{}}
		}
		if len(records) > maxRecordsPerSource {
			records = records[:maxRecordsPerSource]
		}
		rows := make([]RecordView, 0, len(records))
		for _, record := range records {
			kind := record.Kind
			if kind == "" {
				kind = source.Kind
			}
			unit := record.Unit
			if unit == nil {
				unit = source.Unit
			}
			view := RecordView{SourceID: source.SourceID, ProviderID: source.ProviderID,
				AccountRefMasked: masked, BillingIdentity: source.ProviderID + ":" + masked, Kind: kind,
				WindowLabel: record.WindowLabel, Unit: unit, Limit: record.Limit, Used: record.Used,
				Remaining: record.Remaining, PeriodStart: optISO(record.PeriodStart),
				ResetAt: optISO(record.ResetAt), LastSuccessfulAt: optISO(readingOrNil(reading, "success")),
				Stale: status == "stale", Status: status, SourceName: source.SourceName}
			if status == "ok" || status == "stale" {
				view.ObservedAt = optISO(readingOrNil(reading, "success"))
			}
			if status != "ok" {
				if reading != nil && reading.ErrorReason != nil {
					view.ErrorReason = reading.ErrorReason
				} else if status == "unsupported" {
					view.ErrorReason = optString("unsupported_source")
				}
			}
			rows = append(rows, view)
			state.Records = append(state.Records, view)
		}
		view := Describe(source)
		card := Card{SourceID: source.SourceID, ProviderID: source.ProviderID, SourceName: source.SourceName,
			Kind: source.Kind, Supported: source.Supported, Status: status, Stale: stale,
			StaleReason: staleReason, AccountRefMasked: masked,
			BillingIdentity:  source.ProviderID + ":" + masked,
			LastSuccessfulAt: optISO(readingOrNil(reading, "success")),
			LastAttemptAt:    optISO(readingOrNil(reading, "attempt")),
			CredentialRef:    source.CredentialRef, OfficialPage: view.OfficialPage,
			ConsoleURL: view.ConsoleURL, Notes: view.Notes, Records: rows}
		if status == "ok" || status == "stale" {
			card.ObservedAt = card.LastSuccessfulAt
		}
		if status != "ok" {
			if reading != nil && reading.ErrorReason != nil {
				card.ErrorReason = reading.ErrorReason
			} else if status == "unsupported" {
				card.ErrorReason = optString("unsupported_source")
			}
		}
		if reading != nil {
			card.Available = reading.Available
			card.Parts = reading.Parts
		}
		if card.Parts == nil {
			card.Parts = []Part{}
		}
		state.Cards = append(state.Cards, card)
		state.Sources = append(state.Sources, view)
	}
	return state
}

// ConnectionView is one configured connection as the quota page needs it: the
// page has to say whether a credential exists for the source that borrows it,
// and it must never learn the credential itself.
type ConnectionView struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	ProviderPreset *string `json:"providerPresetId,omitempty"`
	HasCredential  bool    `json:"hasCredential"`
}

// PanelState is State decorated with the four facts the panel renders that this
// package cannot know on its own: which connection a source borrows, whether
// that connection has a credential, whether the source may be removed, and
// which page a person can open instead.
//
// It is a decorator rather than part of State because every one of those facts
// comes from the caller's configuration, and this package must keep working
// with no configuration at all. `connections` is the caller's own list; pass nil
// when there is none.
func (a *Accounts) PanelState(connections []ConnectionView) map[string]any {
	state := a.State()
	bySource := map[string]Card{}
	for _, card := range state.Cards {
		bySource[card.SourceID] = card
	}
	hasCredential := map[string]bool{}
	for _, connection := range connections {
		hasCredential["connection:"+connection.ID] = connection.HasCredential
	}

	records := make([]map[string]any, 0, len(state.Records))
	for _, record := range state.Records {
		card := bySource[record.SourceID]
		officialURL := ""
		if card.ConsoleURL != nil {
			officialURL = *card.ConsoleURL
		} else if card.OfficialPage != nil {
			officialURL = *card.OfficialPage
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			continue
		}
		var row map[string]any
		if json.Unmarshal(encoded, &row) != nil {
			continue
		}
		row["parts"] = card.Parts
		row["officialUrl"] = nilIfEmpty(officialURL)
		records = append(records, row)
	}

	sources := make([]map[string]any, 0, len(state.Sources))
	for _, source := range state.Sources {
		card := bySource[source.SourceID]
		encoded, err := json.Marshal(source)
		if err != nil {
			continue
		}
		var row map[string]any
		if json.Unmarshal(encoded, &row) != nil {
			continue
		}
		// A source borrowing a connection is configured exactly when that
		// connection has a key; any other source is configured unless its own
		// card says the credential was missing.
		credentialSet := card.Status != "not_configured"
		if borrowed, ok := hasCredential[source.CredentialRef]; ok {
			credentialSet = borrowed
		}
		row["status"] = card.Status
		row["credentialSet"] = credentialSet
		row["errorReason"] = card.ErrorReason
		// Sources bound to a user connection are removable regardless of adapter.
		row["removable"] = strings.HasPrefix(source.CredentialRef, "connection:")
		officialURL := ""
		if source.ConsoleURL != nil {
			officialURL = *source.ConsoleURL
		} else if source.OfficialPage != nil {
			officialURL = *source.OfficialPage
		}
		row["officialUrl"] = nilIfEmpty(officialURL)
		sources = append(sources, row)
	}

	return map[string]any{"schemaVersion": state.SchemaVersion, "cards": state.Cards,
		"records": records, "sources": sources, "presets": state.Presets,
		"refreshedAt": state.RefreshedAt, "freshnessMs": state.FreshnessMS,
		"cache": state.Cache, "combining": state.Combining, "connections": connections}
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func readingOrNil(reading *Reading, which string) *int64 {
	if reading == nil {
		return nil
	}
	if which == "success" {
		return reading.LastSuccessAt
	}
	return reading.LastAttemptAt
}
