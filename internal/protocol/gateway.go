package protocol

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bounds the legacy gateway enforced. They are exported because the daemon
// reports them and a test asserts the same numbers.
const (
	BodyLimit = 32 * 1024 * 1024
	// DefaultTimeout is the whole-request budget, including the stream.
	DefaultTimeout = 300 * time.Second
	// DefaultCatalogTimeout bounds the catalog read, not the inference.
	DefaultCatalogTimeout = 6 * time.Second
	// drainTimeout is how long a refused body is drained before answering, so
	// the client does not see an empty 200 from a dropped status line.
	drainTimeout = 2 * time.Second
)

// Aliases are the Kimi-side prefixes a model name may carry. They are removed
// before lookup and before the upstream sees the name.
var Aliases = []string{"byok-kth-kimi/", "byok-kth-resp/"}

// DefaultForwardHeaders is the session-header whitelist forwarded upstream. A
// header not named here never leaves this process.
var DefaultForwardHeaders = []string{"x-msh-version", "r-timezone", "x-session-id", "session_id"}

// Side says who refused a request, which is what turns a bare status into
// something a user can act on.
type Side string

const (
	SideLocal      Side = "local"
	SideUpstream   Side = "upstream"
	SideConnection Side = "connection"
)

// Failure is where a request failed and why, in fields that are safe to keep.
type Failure struct {
	Side             Side   `json:"side"`
	Status           int    `json:"status"`
	Reason           string `json:"reason"`
	TransportStatus  *int   `json:"transportStatus,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	UpstreamType     string `json:"upstreamType,omitempty"`
	ConnectionError  string `json:"connectionError,omitempty"`
	Protocol         string `json:"protocol"`
	Model            string `json:"model,omitempty"`
	UpstreamProtocol string `json:"upstreamProtocol,omitempty"`
	At               int64  `json:"at"`
}

// Record is the telemetry one request produces. Nothing here holds a prompt, a
// key or a response body.
type Record struct {
	At                int64    `json:"at"`
	Model             string   `json:"model"`
	Protocol          string   `json:"protocol"`
	ConnectionID      *string  `json:"connectionId,omitempty"`
	UpstreamModelID   *string  `json:"upstreamModelId,omitempty"`
	UpstreamProtocol  *string  `json:"upstreamProtocol,omitempty"`
	Status            int      `json:"status"`
	MS                int64    `json:"ms"`
	FirstByteMS       *int64   `json:"firstByteMs,omitempty"`
	Side              Side     `json:"side,omitempty"`
	Error             string   `json:"error,omitempty"`
	Outcome           string   `json:"outcome,omitempty"`
	TransportStatus   *int     `json:"transportStatus,omitempty"`
	UpstreamErrorType string   `json:"upstreamErrorType,omitempty"`
	ConnectionError   string   `json:"connectionError,omitempty"`
	Usage             *Usage   `json:"usage,omitempty"`
	Context           *Context `json:"context,omitempty"`
	Rate              *Rate    `json:"rate,omitempty"`
	Failure           *Failure `json:"failure,omitempty"`

	// Attempts is how many upstream candidates were tried; 1 means no failover
	// was involved. FailoverFrom names the connection that was abandoned, so a
	// user can see which route is broken without reading a log.
	Attempts     int     `json:"attempts,omitempty"`
	FailoverFrom *string `json:"failoverFrom,omitempty"`

	// PriceSource and EstimatedCost are what the budget table made of this
	// request. Both stay nil when it could not be priced, which is a fact the
	// panel reports instead of showing a zero.
	PriceSource   *string  `json:"priceSource,omitempty"`
	EstimatedCost *float64 `json:"estimatedCost,omitempty"`
}

// LedgerStatus is the ledger's own vocabulary for a finished request. A request
// refused before it left this process is still a failure the user can act on;
// the HTTP status stays in HttpStatus, so nothing is flattened.
func (r *Record) LedgerStatus() string {
	if r == nil {
		return "failed"
	}
	if r.Status == 499 {
		return "cancelled"
	}
	if r.Outcome == string(OutcomeFailed) || r.Status >= 400 {
		return "failed"
	}
	if r.Outcome != string(OutcomeCompleted) {
		return "incomplete"
	}
	if r.Status >= 200 && r.Status < 300 {
		return "completed"
	}
	return "failed"
}

// UsageEntry is what the ledger is told about one request. Only fields that are
// known are set: a guessed identity is worse than a missing one.
type UsageEntry struct {
	OperationID      string   `json:"operationId"`
	Source           string   `json:"source"`
	At               int64    `json:"at"`
	Status           string   `json:"status"`
	Protocol         string   `json:"protocol,omitempty"`
	ModelID          string   `json:"modelId,omitempty"`
	ModelAlias       string   `json:"modelAlias,omitempty"`
	ConnectionID     *string  `json:"connectionId,omitempty"`
	UpstreamProtocol string   `json:"upstreamProtocol,omitempty"`
	LatencyMS        *int64   `json:"latencyMs,omitempty"`
	HTTPStatus       *int     `json:"httpStatus,omitempty"`
	ErrorCode        string   `json:"errorCode,omitempty"`
	Usage            *Usage   `json:"usage,omitempty"`
	ProviderID       *string  `json:"providerId,omitempty"`
	Account          *string  `json:"account,omitempty"`
	APIKeyID         *string  `json:"apiKeyId,omitempty"`
	ConversationID   *string  `json:"conversationId,omitempty"`
	PriceSource      *string  `json:"priceSource,omitempty"`
	EstimatedCost    *float64 `json:"estimatedCost,omitempty"`
}

// UsageSink receives the two receipts of one request: `started` and then a
// terminal status. A sink that cannot write must not cost the user a reply, so
// implementations must not block and must not panic; a returned error is
// ignored by the gateway.
type UsageSink interface {
	Record(entry UsageEntry) error
}

// UsageSinkFunc adapts a function to UsageSink.
type UsageSinkFunc func(entry UsageEntry) error

// Record implements UsageSink.
func (f UsageSinkFunc) Record(entry UsageEntry) error {
	if f == nil {
		return nil
	}
	return f(entry)
}

// UsageContextRequest is what this process really knows about a request. It is
// handed only the forwarded session headers, never the local gateway
// credential, so a resolver cannot leak one into a ledger.
type UsageContextRequest struct {
	Protocol         string
	Model            string
	ModelID          string
	ConnectionID     *string
	UpstreamProtocol *string
	PromptCacheKey   string
	Headers          map[string]string
	At               int64
}

// UsageContext is what a resolver may add to a ledger entry. A field it omits
// stays absent rather than defaulting to something.
type UsageContext struct {
	ProviderID     *string
	ConnectionID   *string
	Account        *string
	APIKeyID       *string
	ConversationID *string
	PriceSource    *string
	EstimatedCost  *float64
}

// UsageContextResolver maps a request onto the identity this process cannot
// know. It must be safe to call concurrently and must not block on I/O.
type UsageContextResolver func(UsageContextRequest) (UsageContext, error)

// Config is everything one gateway needs. A caller that changes the catalog or
// the endpoint table builds a new Gateway and swaps it atomically, so a live
// request never observes a half-applied configuration.
type Config struct {
	Backend             *HTTPBackend
	Key                 string
	RecordRequests      bool
	UsageSink           UsageSink
	ResolveUsageContext UsageContextResolver
	// Instructions is the shared-rules text already read by the caller. Empty
	// means no managed system message is inserted. Ignored when
	// InstructionsProvider is set.
	Instructions string
	// InstructionsProvider returns the shared rules for one request, so a file
	// edited while the gateway runs is picked up without a restart. It is called
	// once per request and its answer replaces Instructions. An empty answer
	// still removes a copy left in the history; an error refuses the request
	// with that error's text as the code.
	InstructionsProvider func() (string, error)
	ForwardHeaders       []string
	Timeout              time.Duration
	CatalogTimeout       time.Duration
	// BeforeRequest may refuse a request before anything is forwarded. A nil
	// hook allows everything.
	BeforeRequest func(route string, payload *Document) *Refusal
	// Budget may refuse a request for spend or price it after it finishes. A nil
	// gate keeps the behaviour of every release before budget support: nothing
	// is refused and no cost is written.
	Budget BudgetGate
	// Now is injectable for tests.
	Now func() time.Time
}

// Refusal is a policy gate's answer.
type Refusal struct {
	Status int
	Code   string
}

type inflightEntry struct{}

// Gateway is the local HTTP endpoint Kimi's own client talks to.
type Gateway struct {
	config      Config
	inflight    map[*inflightEntry]struct{}
	mu          sync.Mutex
	metrics     Metrics
	lastCatalog *CatalogAttempt
	handler     http.Handler
}

// Metrics is the panel's short window over what just happened.
type Metrics struct {
	StartedAt int64     `json:"startedAt"`
	Requests  int       `json:"requests"`
	Failures  int       `json:"failures"`
	Recent    []*Record `json:"recent"`
}

// CatalogAttempt records the last `GET /v1/models` so a diagnostic can tell an
// unauthorized probe from a browser one.
type CatalogAttempt struct {
	At         string `json:"at"`
	Authorized bool   `json:"authorized"`
	HasOrigin  bool   `json:"hasOrigin"`
	FetchSite  string `json:"fetchSite"`
	Status     int    `json:"status"`
}

// NewGateway validates the configuration and returns a ready handler.
func NewGateway(config Config) (*Gateway, error) {
	if config.Backend == nil {
		return nil, errors.New("invalid_backend")
	}
	if config.Key == "" {
		return nil, errors.New("invalid_connection")
	}
	if config.UsageSink == nil {
		config.UsageSink = UsageSinkFunc(nil)
	}
	if len(config.ForwardHeaders) == 0 {
		config.ForwardHeaders = DefaultForwardHeaders
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	if config.CatalogTimeout <= 0 {
		config.CatalogTimeout = DefaultCatalogTimeout
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	g := &Gateway{config: config, inflight: map[*inflightEntry]struct{}{},
		metrics: Metrics{StartedAt: config.Now().UnixMilli(), Recent: []*Record{}}}
	g.handler = http.HandlerFunc(g.serveHTTP)
	return g, nil
}

// trackedWriter remembers whether a status line went out, which is what tells a
// cancelled-before-answer request (499) from a finished one.
type trackedWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackedWriter) WriteHeader(status int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackedWriter) Write(p []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(p)
}

func (t *trackedWriter) Flush() {
	if flusher, ok := t.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.handler.ServeHTTP(&trackedWriter{ResponseWriter: w}, r)
}

// Metrics returns a copy of the short request window.
func (g *Gateway) Metrics() Metrics {
	g.mu.Lock()
	defer g.mu.Unlock()
	recent := make([]*Record, len(g.metrics.Recent))
	copy(recent, g.metrics.Recent)
	return Metrics{StartedAt: g.metrics.StartedAt, Requests: g.metrics.Requests,
		Failures: g.metrics.Failures, Recent: recent}
}

// Inflight is how many requests are still streaming, which is what a port move
// waits for.
func (g *Gateway) Inflight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.inflight)
}

// LastCatalogAttempt returns the last catalog probe, or nil.
func (g *Gateway) LastCatalogAttempt() *CatalogAttempt {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastCatalog == nil {
		return nil
	}
	copied := *g.lastCatalog
	return &copied
}

// Catalog reads the backend's model list under a bounded timeout. The returned
// slice is the validated catalog the routes consult.
func (g *Gateway) Catalog(ctx context.Context) ([]Model, error) {
	if g.config.Backend == nil {
		return nil, errors.New("kimi_not_connected")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout, cancel := context.WithTimeout(ctx, g.config.CatalogTimeout)
	defer cancel()
	if err := timeout.Err(); err != nil {
		return nil, ErrCatalogUnavailable
	}
	models := g.config.Backend.Models()
	if timeout.Err() != nil {
		return nil, ErrCatalogUnavailable
	}
	return models, nil
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	started := g.config.Now()
	record := g.openRecord(r)
	operationID := ""
	if record != nil && g.config.UsageSink != nil {
		operationID = newUUID()
		g.observe(record, r, operationID, "started", nil)
	}
	var usageContext *UsageContext
	var admission budgetAdmission
	defer func() {
		g.finish(record, r, operationID, w, started, usageContext, admission.prices)
		// A serialising budget slot is released only after the request's own cost
		// is in the ledger, which is the whole point of serialising: the next
		// request's admission has to see what this one spent.
		if admission.release != nil {
			admission.release()
		}
	}()
	usageContext = g.forwardWithAdmission(w, r, record, &admission)
}

// budgetAdmission is what the gate handed this request: the price table it was
// admitted under, and the slot to free once its cost is written.
type budgetAdmission struct {
	prices  *PriceTable
	release func()
}

// finish settles one request: it measures it, prices it, publishes it to the
// panel window and writes its last ledger receipt. It runs before a serialising
// slot is released, so the next admission reads a ledger that already holds this
// request's cost.
func (g *Gateway) finish(record *Record, r *http.Request, operationID string, w http.ResponseWriter,
	started time.Time, usageContext *UsageContext, prices *PriceTable) {
	if record == nil {
		return
	}
	finishRecord(record, w, started, g.config.Now())
	if usageContext != nil {
		applyUsageContext(record, usageContext)
	}
	if rate := RateOf(record.Status, record.Error != "", record.Usage, record.MS); rate != nil {
		record.Rate = rate
	}
	// Pricing happens once per finished request, against the model that was asked
	// for, the usage the upstream reported, and the table this request was
	// admitted under. A request that reported no usage stays unpriced rather than
	// becoming a free one.
	if g.config.Budget != nil {
		cost, source := g.config.Budget.Settle(BudgetSettlement{Route: record.Protocol,
			Model: record.Model, At: record.At, Usage: record.Usage, Prices: prices})
		record.EstimatedCost = cost
		record.PriceSource = source
	}
	record.Failure = FailureOf(record)
	if g.config.RecordRequests {
		g.mu.Lock()
		g.metrics.Requests++
		if record.Status >= 400 || record.Outcome == string(OutcomeFailed) {
			g.metrics.Failures++
		}
		g.metrics.Recent = append([]*Record{record}, g.metrics.Recent...)
		if len(g.metrics.Recent) > 20 {
			g.metrics.Recent = g.metrics.Recent[:20]
		}
		g.mu.Unlock()
	}
	if operationID != "" {
		g.observe(record, r, operationID, record.LedgerStatus(), usageContext)
	}
}

func finishRecord(record *Record, w http.ResponseWriter, started, finished time.Time) {
	record.MS = finished.Sub(started).Milliseconds()
	if record.MS < 0 {
		record.MS = 0
	}
	if record.Status != 0 {
		return
	}
	if tracked, ok := w.(*trackedWriter); ok && tracked.wrote {
		record.Status = http.StatusOK
		return
	}
	record.Status = 499
}

// openRecord decides whether this request is one the panel and the ledger see.
// A request from a browser, an unauthenticated one, or a non-inference route is
// not telemetry.
func (g *Gateway) openRecord(r *http.Request) *Record {
	if !g.config.RecordRequests && g.config.UsageSink == nil {
		return nil
	}
	if r.Method != http.MethodPost {
		return nil
	}
	route, ok := routeOf(r.Method, r.URL.Path)
	if !ok {
		return nil
	}
	if !authorized(r.Header.Get("Authorization"), g.config.Key) {
		return nil
	}
	if r.Header.Get("Origin") != "" {
		return nil
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "none" {
		return nil
	}
	return &Record{At: g.config.Now().UnixMilli(), Protocol: route}
}

func (g *Gateway) observe(record *Record, r *http.Request, operationID, status string, context *UsageContext) {
	entry := UsageEntry{OperationID: operationID, Source: "gateway", At: record.At, Status: status}
	if record.Protocol != "" {
		entry.Protocol = record.Protocol
	}
	modelID := record.Model
	if record.UpstreamModelID != nil && *record.UpstreamModelID != "" {
		modelID = *record.UpstreamModelID
	}
	if modelID != "" {
		entry.ModelID = modelID
	}
	if record.Model != "" && record.Model != modelID {
		entry.ModelAlias = record.Model
	}
	entry.ConnectionID = record.ConnectionID
	if record.UpstreamProtocol != nil {
		entry.UpstreamProtocol = *record.UpstreamProtocol
	}
	if status != "started" {
		latency := record.MS
		entry.LatencyMS = &latency
		status := record.Status
		entry.HTTPStatus = &status
		entry.ErrorCode = record.Error
		entry.Usage = record.Usage
		entry.PriceSource = record.PriceSource
		entry.EstimatedCost = record.EstimatedCost
	}
	if context != nil {
		entry.ProviderID = context.ProviderID
		entry.Account = context.Account
		entry.APIKeyID = context.APIKeyID
		entry.ConversationID = context.ConversationID
		if entry.PriceSource == nil {
			entry.PriceSource = context.PriceSource
		}
		if entry.EstimatedCost == nil {
			entry.EstimatedCost = context.EstimatedCost
		}
		if entry.ConnectionID == nil {
			entry.ConnectionID = context.ConnectionID
		}
	}
	// A ledger that cannot write must not cost the user a reply.
	defer func() { _ = recover() }()
	_ = g.config.UsageSink.Record(entry)
}

func applyUsageContext(record *Record, context *UsageContext) {
	// The model row's own connection is authoritative; the resolver only fills
	// what the request itself did not establish.
	if record.ConnectionID == nil {
		record.ConnectionID = context.ConnectionID
	}
}

// routeOf resolves the two inference routes. Only POST is an inference route:
// the legacy gateway answered a GET on the same path with 404 rather than
// treating it as a malformed request.
func routeOf(method, path string) (string, bool) {
	if method != http.MethodPost {
		return "", false
	}
	switch path {
	case "/v1/chat/completions":
		return ProtocolChat, true
	case "/v1/responses":
		return ProtocolResponses, true
	}
	return "", false
}

func isCatalogRoute(method, path string) bool {
	return method == http.MethodGet && (path == "/v1/models" || path == "/v1/models/api.json")
}

// forwardWithAdmission is the whole request path plus the budget admission the
// request took, which the caller settles and releases.
func (g *Gateway) forwardWithAdmission(w http.ResponseWriter, r *http.Request, record *Record,
	admission *budgetAdmission) *UsageContext {
	return g.forward(w, r, record, admission)
}

// forward is the whole request path. It returns the usage context the resolver
// produced, if any, so the deferred ledger write can carry it. When the budget
// gate admits the request, *admission receives the table it was admitted under
// and the slot to free; the caller releases that slot only after this request's
// cost is in the ledger.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, record *Record, admission *budgetAdmission) *UsageContext {
	refuse := func(status int, code string) {
		if record != nil {
			if record.Side == "" {
				record.Side = SideLocal
			}
			if record.Error == "" {
				record.Error = code
			}
			record.Status = status
		}
		writeError(w, status, code)
	}
	route, inference := routeOf(r.Method, r.URL.Path)
	catalog := isCatalogRoute(r.Method, r.URL.Path)
	if !inference && !catalog {
		refuse(http.StatusNotFound, "not_found")
		return nil
	}
	if g.config.Key == "" {
		refuse(http.StatusServiceUnavailable, "kimi_not_connected")
		return nil
	}
	if catalog {
		g.noteCatalogAttempt(r, w)
	}
	if !authorized(r.Header.Get("Authorization"), g.config.Key) {
		refuse(http.StatusUnauthorized, "unauthorized")
		return nil
	}
	if r.Header.Get("Origin") != "" || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "none") {
		refuse(http.StatusForbidden, "browser_request_refused")
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.config.Timeout)
	defer cancel()
	entry := &inflightEntry{}
	g.mu.Lock()
	g.inflight[entry] = struct{}{}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.inflight, entry)
		g.mu.Unlock()
	}()

	models, err := g.Catalog(ctx)
	if err != nil {
		if record != nil {
			record.Status = http.StatusServiceUnavailable
			record.Error = "catalog_unavailable"
			record.Side = SideLocal
		}
		writeError(w, http.StatusServiceUnavailable, "catalog_unavailable")
		return nil
	}
	if catalog {
		var body []byte
		if r.URL.Path == "/v1/models/api.json" {
			body = NativeCatalog(models)
		} else {
			body = OpenAIModels(models)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		if record != nil {
			record.Status = http.StatusOK
		}
		return nil
	}

	body, tooLarge, err := readBody(w, r)
	if err != nil {
		if tooLarge {
			refuse(http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			refuse(http.StatusBadRequest, "invalid_json")
		}
		return nil
	}
	document, err := ParseDocument(body)
	if err != nil {
		refuse(http.StatusBadRequest, "invalid_json")
		return nil
	}
	rawModel, _ := document.Get("model")
	modelName, ok := rawString(rawModel)
	if !ok {
		refuse(http.StatusBadRequest, "invalid_model")
		return nil
	}
	for _, prefix := range Aliases {
		if strings.HasPrefix(modelName, prefix) {
			modelName = modelName[len(prefix):]
			break
		}
	}
	var selected *Model
	for i := range models {
		if models[i].ID == modelName {
			selected = &models[i]
			break
		}
	}
	if selected == nil {
		refuse(http.StatusConflict, "model_not_enabled_for_kimi")
		return nil
	}
	if record != nil {
		record.Model = selected.ID
		record.ConnectionID = selected.ConnectionID
		record.UpstreamModelID = selected.UpstreamModelID
		record.UpstreamProtocol = selected.UpstreamProtocol
	}
	if selected.Protocol != route {
		refuse(http.StatusConflict, "model_protocol_mismatch")
		return nil
	}
	if g.config.BeforeRequest != nil {
		if blocked := g.config.BeforeRequest(route, document); blocked != nil {
			if blocked.Status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", "1")
			}
			refuse(blocked.Status, blocked.Code)
			return nil
		}
	}
	// The budget gate is asked once per request, after the request is known to be
	// one this gateway will serve. A refusal here spends nothing upstream.
	if g.config.Budget != nil {
		decision, unlock := g.config.Budget.Admit(ctx, BudgetRequest{Route: route,
			Model: selected.ID, At: g.config.Now().UnixMilli()})
		if decision.Refusal != nil {
			if unlock != nil {
				unlock()
			}
			if decision.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(decision.RetryAfter))
			}
			if window := budgetRefusalWindow(decision); window != "" {
				w.Header().Set(BudgetWindowHeader, window)
			}
			refuse(decision.Refusal.Status, decision.Refusal.Code)
			return nil
		}
		// The slot is held until this request's cost is written to the ledger, not
		// until its last byte is forwarded: the caller releases it after
		// settlement, which is what makes serialising actually bound the total.
		// The table is captured here so the settlement is priced in the unit this
		// request was admitted under, even if the panel changes the currency while
		// it is in flight.
		admission.prices, admission.release = decision.Prices, unlock
		if warning := budgetWarning(decision); warning != "" {
			w.Header().Set(BudgetWarningHeader, warning)
		}
	}
	// The legacy gateway called this whenever a rules file was configured, so a
	// history left with a managed message is cleaned even when the file is now
	// missing. A nil provider keeps the older static-text behaviour.
	text := g.config.Instructions
	if g.config.InstructionsProvider != nil {
		value, err := g.config.InstructionsProvider()
		if err != nil {
			refuse(http.StatusServiceUnavailable, err.Error())
			return nil
		}
		text = value
	}
	if text != "" || g.config.InstructionsProvider != nil {
		// ApplyInstructions dispatches on the protocol name, not on the URL.
		if err := ApplyInstructions(document, text, route); err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, ErrInvalidMessages) || errors.Is(err, ErrInvalidInput) {
				status = http.StatusBadRequest
			}
			refuse(status, err.Error())
			return nil
		}
	}
	// The local id is Kimi's route; each candidate receives the exact model name
	// it declared, and tryCandidates writes that name per attempt.
	headers := map[string]string{}
	for _, name := range g.config.ForwardHeaders {
		if value := r.Header.Get(name); value != "" {
			headers[name] = value
		}
	}
	promptCacheKey := ""
	if raw, ok := document.Get("prompt_cache_key"); ok {
		_ = json.Unmarshal(raw, &promptCacheKey)
	}
	chosen := g.tryCandidates(ctx, route, document, selected, headers, promptCacheKey)
	usageContext := chosen.usage
	if record != nil {
		record.Attempts = chosen.tried
		if chosen.tried > 0 {
			record.ConnectionID = chosen.connectionID
			record.UpstreamModelID = optionalString(chosen.upstreamID)
			record.UpstreamProtocol = chosen.upstreamWire
		}
		record.FailoverFrom = chosen.failoverFrom
	}
	if chosen.response == nil {
		failure := chosen.failure
		if failure == nil {
			failure = ErrConnectionUnavailable
		}
		switch {
		case errors.Is(failure, ErrInvalidProtocol), errors.Is(failure, ErrInvalidRequest),
			errors.Is(failure, ErrInvalidThinkingBudget), errors.Is(failure, ErrThinkingToolRoundtrip):
			refuse(http.StatusBadRequest, failure.Error())
		case chosen.unavailable:
			refuse(http.StatusServiceUnavailable, "connection_unavailable")
		default:
			g.connectionFailure(w, record, failure, ctx)
		}
		// A request that lost every candidate is reported as that outcome, not as
		// the last candidate's own error, so the panel can say what happened.
		if record != nil && chosen.tried > 1 {
			record.Error = "all_connections_failed"
		}
		return usageContext
	}
	upstream := chosen.response
	defer func() { _ = upstream.Body.Close() }()
	if record != nil {
		record.Side = SideUpstream
		record.Status = upstream.Status
		if chosen.exhausted {
			record.Error = "all_connections_failed"
		}
	}
	for _, name := range []string{"Content-Type", "Retry-After", "X-Request-Id", "Request-Id"} {
		if value := upstream.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(upstream.Status)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	firstByteAt := g.config.Now()
	first := true
	kind := ScanNone
	if upstream.Status >= 200 && upstream.Status < 300 {
		kind = ScannerFor(w.Header().Get("Content-Type"))
	}
	scanner := NewUsageScanner(kind)
	var failureScan *ErrorTypeScan
	if upstream.Status < 200 || upstream.Status >= 300 {
		failureScan = &ErrorTypeScan{}
	}
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := upstream.Body.Read(buffer)
		if n > 0 {
			if first {
				first = false
				elapsed := g.config.Now().Sub(firstByteAt).Milliseconds()
				if record != nil {
					record.FirstByteMS = &elapsed
				}
			}
			scanner.Push(buffer[:n])
			if failureScan != nil {
				failureScan.Push(buffer[:n])
			}
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				if record != nil {
					record.Status = 499
					record.Error = "request_cancelled"
				}
				return usageContext
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				if record != nil {
					if ctx.Err() != nil {
						record.Status = 499
						record.Error = "request_cancelled"
					} else {
						status := upstream.Status
						record.TransportStatus = &status
						record.Status = http.StatusBadGateway
						record.Error = "response_stream_interrupted"
						record.Side = SideConnection
						record.Outcome = string(OutcomeFailed)
					}
				}
				return usageContext
			}
			break
		}
	}
	if failureScan != nil {
		if typ := failureScan.Finish(); typ != "" && record != nil {
			record.UpstreamErrorType = typ
		}
	}
	if ctx.Err() != nil {
		if record != nil {
			record.Status = 499
			record.Error = "request_cancelled"
		}
		return usageContext
	}
	usage := scanner.Finish()
	if record != nil {
		status := upstream.Status
		record.TransportStatus = &status
		record.Outcome = string(scanner.Outcome())
		if scanner.Outcome() == OutcomeFailed {
			record.Error = "upstream_stream_failed"
		}
	}
	if usage != nil && record != nil {
		record.Usage = usage
		record.Context = ContextOf(usage, selected.ContextWindow)
	}
	return usageContext
}

func (g *Gateway) noteCatalogAttempt(r *http.Request, w http.ResponseWriter) {
	site := r.Header.Get("Sec-Fetch-Site")
	switch site {
	case "none", "same-origin", "same-site", "cross-site":
	case "":
		site = "absent"
	default:
		site = "other"
	}
	attempt := &CatalogAttempt{At: g.config.Now().UTC().Format(time.RFC3339),
		Authorized: authorized(r.Header.Get("Authorization"), g.config.Key),
		HasOrigin:  r.Header.Get("Origin") != "", FetchSite: site}
	g.mu.Lock()
	g.lastCatalog = attempt
	g.mu.Unlock()
}

// connectionFailure reports a request that never reached an upstream answer.
// "Upstream returned 503" and "we never reached the upstream" are different
// problems with different fixes, so they must not be reported as the same one.
func (g *Gateway) connectionFailure(w http.ResponseWriter, record *Record, failure error, ctx context.Context) {
	cancelled := ctx.Err() != nil
	code := "upstream_connection_failed"
	status := http.StatusServiceUnavailable
	if cancelled {
		code = "request_cancelled"
		status = 499
	} else if errors.Is(failure, ErrCatalogUnavailable) {
		code = "catalog_unavailable"
	} else if errors.Is(failure, ErrInvalidUpstreamResponse) {
		code = "upstream_invalid_response"
	}
	if record != nil {
		record.Status = status
		record.Error = code
		if record.Side == "" {
			if cancelled {
				record.Side = SideLocal
			} else {
				record.Side = SideConnection
			}
		}
		if record.Side == SideConnection {
			record.ConnectionError = SafeReasonText(failure.Error())
		}
	}
	writeError(w, status, code)
}

// FailureOf builds the safe classification of a failed request, or nil when the
// request did not fail.
func FailureOf(record *Record) *Failure {
	if record == nil {
		return nil
	}
	if record.Status < 400 && record.Outcome != string(OutcomeFailed) {
		return nil
	}
	side := record.Side
	switch side {
	case SideUpstream, SideConnection:
	default:
		side = SideLocal
	}
	reason := record.Error
	if reason == "" {
		switch {
		case record.Outcome == string(OutcomeFailed):
			reason = "upstream_stream_failed"
		case side == SideUpstream:
			if mapped, ok := UpstreamReasons[record.Status]; ok {
				reason = mapped
			} else {
				reason = "upstream_error"
			}
		case side == SideConnection:
			reason = "upstream_connection_failed"
		case record.Status == 401:
			reason = "local_unauthorized"
		case record.Status == 403:
			reason = "local_browser_request_refused"
		case record.Status == 409:
			reason = "local_request_refused"
		case record.Status == 413:
			reason = "local_request_too_large"
		case record.Status == 503:
			reason = "local_upstream_unavailable"
		default:
			reason = "local_error"
		}
	}
	failure := &Failure{Side: side, Status: record.Status, Reason: reason,
		Protocol: record.Protocol, At: record.At}
	if record.TransportStatus != nil {
		failure.TransportStatus = record.TransportStatus
	}
	if record.Outcome != "" {
		failure.Outcome = record.Outcome
	}
	if typ := VendorType(record.UpstreamErrorType); typ != "" {
		failure.UpstreamType = typ
	}
	if record.ConnectionError != "" {
		failure.ConnectionError = record.ConnectionError
	}
	if record.Model != "" {
		failure.Model = record.Model
	}
	if record.UpstreamProtocol != nil {
		failure.UpstreamProtocol = *record.UpstreamProtocol
	}
	return failure
}

func authorized(actual, key string) bool {
	if key == "" {
		return false
	}
	expected := "Bearer " + key
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func writeError(w http.ResponseWriter, status int, code string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"type": "kimi_adapter_error", "code": code, "message": code}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// readBody buffers at most BodyLimit. A declared length over the limit is
// refused before any byte is buffered, but the body is still drained first:
// answering while the client is mid-write makes some servers drop the status
// line and the caller would see an empty 200 instead of the rejection.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool, error) {
	if declared := r.ContentLength; declared > BodyLimit {
		drain(r)
		return nil, true, errors.New("request_too_large")
	}
	limited := io.LimitReader(r.Body, BodyLimit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if len(body) > BodyLimit {
		drain(r)
		return nil, true, errors.New("request_too_large")
	}
	return body, false, nil
}

func drain(r *http.Request) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64*1024*1024))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainTimeout):
	}
}

func newUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	buf[6] = buf[6]&0x0f | 0x40
	buf[8] = buf[8]&0x3f | 0x80
	encoded := hex.EncodeToString(buf[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

// Listen binds the gateway to loopback on the requested port and returns the
// base URL Kimi's client should use. Port 0 picks a free one.
func (g *Gateway) Listen(port int) (*http.Server, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return nil, "", errors.New("native_byok_port_unavailable")
	}
	server := &http.Server{Handler: g, ReadHeaderTimeout: 15 * time.Second}
	go func() { _ = server.Serve(listener) }()
	address := listener.Addr().(*net.TCPAddr)
	return server, "http://127.0.0.1:" + strconv.Itoa(address.Port) + "/v1", nil
}
