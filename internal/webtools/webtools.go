// Package webtools is the native search and page-reader adapter.
//
// Kimi's own kernel calls two plain HTTP endpoints:
//
//	WebSearch  POST <base_url>  {text_query}  Authorization: Bearer <api_key>
//	           -> 200 {"search_results":[{"title","url","snippet","date?","site_name?"}]}
//	FetchURL   POST <base_url>  {url}         Authorization: Bearer <api_key>
//	           -> 200 text/markdown (the body itself is the page text)
//
// Anything other than 200 is an error for the kernel, and the FetchURL provider
// falls back to its own local fetcher when the call fails. A third-party service
// therefore only takes over if it answers in exactly those two shapes. This
// package maps one vendor (Tavily) onto them and serves them from a private
// loopback listener.
//
// What it deliberately does not do:
//
//   - It never reads or forwards the Kimi account token. The vendor key is
//     supplied by the caller through the Credentials interface and only ever
//     travels to the vendor endpoint as `Authorization: Bearer`.
//   - It copies no inbound header to the vendor.
//   - It sends no inference request. Search and extract are the vendor's own
//     documented read-only endpoints.
//   - It does not own the vendor key's storage: the caller decides where a key
//     lives and how it is protected.
package webtools

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wssz527/kimi-work-plus/internal/core"
)

// The vendor endpoints this adapter talks to.
const (
	TavilySearchEndpoint  = "https://api.tavily.com/search"
	TavilyExtractEndpoint = "https://api.tavily.com/extract"
)

// What the kernel expects back. Search is JSON, FetchURL is the raw text body.
const (
	NativeSearchContentType = "application/json"
	NativeFetchContentType  = "text/markdown"
)

// Probe is the fixed sample a service test uses: one cheap search and one
// stable page. It proves the configured account answers; it does not prove that
// a running Kimi session has reloaded the endpoint.
var Probe = struct {
	Query string
	URL   string
}{Query: "Tavily API documentation", URL: "https://example.com/"}

// Bounds. Request and response size, URL shape, query length, timeout and
// cancellation are all bounded here so the local endpoint that calls this stays
// a thin shell.
const (
	MaxQueryChars     = 2000
	MaxTargetURLChars = 4096
	MaxKeyChars       = 4096
	MaxResults        = 20
	DefaultResults    = 8
	MaxSnippetChars   = 1200
	MaxContentChars   = 200000
	MaxSearchBytes    = 4 * 1024 * 1024
	MaxExtractBytes   = 8 * 1024 * 1024
	MaxErrorDetail    = 300
	MaxErrorBodyBytes = 4096
	DefaultTimeout    = 20 * time.Second
	MinTimeout        = 1 * time.Second
	MaxTimeout        = 120 * time.Second
	// MaxRequestBytes is the bound on one inbound native request body.
	MaxRequestBytes = 65536
)

// Error is a failure the local endpoint can branch on. Status is the HTTP
// status it should answer with: a non-200 makes the kernel treat the call as
// failed, which is the only way to refuse an Extract whose 200 carries
// `failed_results`.
type Error struct {
	Code         string
	Message      string
	Status       int
	VendorStatus int
}

// Error implements error.
func (e *Error) Error() string { return e.Code }

func failure(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

func vendorFailure(label string, status int, detail string) *Error {
	suffix := ""
	if detail != "" {
		suffix = ". " + detail
	}
	prefix := label + " failed: "
	switch {
	case status == 401 || status == 403:
		return &Error{Code: "web_adapter_auth_failed",
			Message: prefix + "the search service rejected its API key (HTTP " + itoa(status) + ")" + suffix,
			Status:  401, VendorStatus: status}
	case status == 429:
		return &Error{Code: "web_adapter_rate_limited",
			Message: prefix + "the search service is rate limited (HTTP 429)" + suffix,
			Status:  429, VendorStatus: status}
	case status >= 400 && status < 500:
		return &Error{Code: "web_adapter_invalid_request",
			Message: prefix + "the search service refused the request (HTTP " + itoa(status) + ")" + suffix,
			Status:  400, VendorStatus: status}
	default:
		return &Error{Code: "web_adapter_upstream_error",
			Message: prefix + "the search service answered HTTP " + itoa(status) + suffix,
			Status:  502, VendorStatus: status}
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// --- bounds ----------------------------------------------------------------

var controlPattern = regexp.MustCompile(`[\x00-\x1f\x7f]`)

var loopbackHosts = map[string]bool{"127.0.0.1": true, "::1": true, "[::1]": true, "localhost": true}

var searchDepths = map[string]bool{"basic": true, "advanced": true}

// NormalizeServiceURL is the shape a vendor endpoint must have: a plain
// http(s) address with no credentials, query or fragment. Plain HTTP is only
// allowed to loopback, because anything else would put the vendor key on the
// wire in clear.
func NormalizeServiceURL(raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	if text == "" || len(text) > 2048 || controlPattern.MatchString(text) {
		return "", false
	}
	parsed, err := url.Parse(text)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	if parsed.Scheme == "http" && !loopbackHosts[parsed.Hostname()] {
		return "", false
	}
	return parsed.String(), true
}

func vendorEndpoint(value, fallback string) (string, error) {
	raw := value
	if raw == "" {
		raw = fallback
	}
	normalized, ok := NormalizeServiceURL(raw)
	if !ok {
		return "", failure("web_adapter_endpoint_invalid",
			"The search service endpoint must be a plain HTTPS address without credentials, query or fragment.", 500)
	}
	return normalized, nil
}

// vendorKey rejects a key that cannot be sent safely. The legacy pattern is
// `^[\x21-\x7e]{1,4096}$`: printable ASCII with no space, which is what a header
// value can carry without being altered. Go's regexp caps a repeat at 1000, so
// the bound is checked by hand.
func vendorKey(value string) (string, error) {
	if !printableKey(value) {
		return "", failure("web_adapter_key_missing", "The search service has no usable API key.", 500)
	}
	return value, nil
}

func printableKey(value string) bool {
	if len(value) == 0 || len(value) > MaxKeyChars || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func timeoutOf(value time.Duration) time.Duration {
	if value <= 0 {
		return DefaultTimeout
	}
	if value < MinTimeout {
		return MinTimeout
	}
	if value > MaxTimeout {
		return MaxTimeout
	}
	return value
}

func resultCount(value int) int {
	if value <= 0 {
		return DefaultResults
	}
	if value > MaxResults {
		return MaxResults
	}
	return value
}

func searchDepth(value string) (string, error) {
	if value == "" {
		return "basic", nil
	}
	if !searchDepths[value] {
		return "", failure("web_adapter_invalid_request", "Unsupported search depth.", 400)
	}
	return value, nil
}

// truncateChars cuts on a character boundary without splitting a surrogate
// pair, so Chinese text and emoji survive a cut. The bound is in UTF-16 code
// units because that is what the legacy JavaScript measured.
func truncateChars(value string, limit int) string {
	if utf16Len(value) <= limit {
		return value
	}
	units := 0
	for index, r := range value {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			return value[:index]
		}
		units += width
	}
	return value
}

func utf16Len(value string) int {
	units := 0
	for _, r := range value {
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
	}
	return units
}

func truncationNotice(limit int) string {
	return "\n\n[Content truncated at " + itoa(limit) + " characters.]"
}

// --- requests --------------------------------------------------------------

// SearchRequest is the parsed Kimi WebSearch body.
type SearchRequest struct {
	TextQuery string `json:"text_query"`
}

// FetchRequest is the parsed Kimi FetchURL body.
type FetchRequest struct {
	URL string `json:"url"`
}

func queryText(request SearchRequest) (string, error) {
	query := strings.TrimSpace(request.TextQuery)
	if query == "" || utf16Len(query) > MaxQueryChars {
		return "", failure("web_adapter_invalid_request",
			"The search request must carry a non-empty \"text_query\" of at most 2000 characters.", 400)
	}
	return query, nil
}

func targetURL(request FetchRequest) (string, error) {
	raw := strings.TrimSpace(request.URL)
	if raw == "" || len(raw) > MaxTargetURLChars {
		return "", failure("web_adapter_invalid_request",
			"The fetch request must carry a public http(s) URL of at most 4096 characters.", 400)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", failure("web_adapter_invalid_request", "The fetch request must carry a valid URL.", 400)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", failure("web_adapter_invalid_request", "Only http(s) URLs can be read.", 400)
	}
	return parsed.String(), nil
}

func hostLabel(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
}

func snippetText(value string) string {
	plain := strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if plain == "" {
		return ""
	}
	if utf16Len(plain) > MaxSnippetChars {
		return truncateChars(plain, MaxSnippetChars) + "..."
	}
	return plain
}

func resultURL(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" || len(raw) > MaxTargetURLChars {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.String()
}

// --- options ---------------------------------------------------------------

// Options is everything one adapter call needs. A zero Options is usable: the
// vendor endpoint defaults to Tavily and the timeout to DefaultTimeout.
type Options struct {
	// APIKey is the vendor credential, supplied by the caller.
	APIKey string
	// Endpoint overrides the vendor address. Empty means the Tavily default.
	Endpoint string
	// MaxResults bounds the search answer.
	MaxResults int
	// SearchDepth is "basic" or "advanced".
	SearchDepth string
	// Timeout bounds one vendor call.
	Timeout time.Duration
	// Client is the HTTP client. A nil client uses a client that refuses
	// redirects, so the Authorization header is never replayed at another host.
	Client *http.Client
	// Now is injectable for tests.
	Now func() time.Time
}

func (o Options) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// --- transport -------------------------------------------------------------

// readLimited reads at most limit bytes, refusing a declared length over the
// limit before any of it is buffered.
func readLimited(response *http.Response, limit int64) ([]byte, error) {
	if response.ContentLength > limit {
		return nil, failure("web_adapter_response_too_large", "The search service response was too large.", 502)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, failure("web_adapter_network_error",
			"The search service could not be reached.", 502)
	}
	if int64(len(body)) > limit {
		return nil, failure("web_adapter_response_too_large", "The search service response was too large.", 502)
	}
	return body, nil
}

// postJSON is one POST with one bounded JSON answer. The vendor key goes to the
// vendor endpoint only; a redirect is refused rather than followed.
func postJSON(request postRequest) (map[string]any, error) {
	if request.context != nil && request.context.Err() != nil {
		return nil, failure("web_adapter_aborted", request.label+" was cancelled.", 499)
	}
	payload, err := json.Marshal(request.payload)
	if err != nil {
		return nil, failure("web_adapter_invalid_request", "The request could not be encoded.", 400)
	}
	ctx := request.context
	if ctx == nil {
		ctx = contextBackground()
	}
	ctx, cancel := contextWithTimeout(ctx, request.timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, request.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, failure("web_adapter_endpoint_invalid", "The search service endpoint is unusable.", 500)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+request.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := request.client.Do(httpRequest)
	if err != nil {
		if request.context != nil && request.context.Err() != nil {
			return nil, failure("web_adapter_aborted", request.label+" was cancelled.", 499)
		}
		if ctx.Err() != nil {
			return nil, failure("web_adapter_timeout", request.label+" timed out.", 504)
		}
		return nil, failure("web_adapter_network_error",
			request.label+" failed: the search service could not be reached.", 502)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := ""
		if body, err := readLimited(response, MaxErrorBodyBytes); err == nil {
			detail = truncateChars(strings.Join(strings.Fields(string(body)), " "), MaxErrorDetail)
		}
		return nil, vendorFailure(request.label, response.StatusCode, detail)
	}
	body, err := readLimited(response, request.limit)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, failure("web_adapter_timeout", request.label+" timed out.", 504)
	}
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		return nil, failure("web_adapter_invalid_response",
			request.label+" failed: the response was not JSON.", 502)
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, failure("web_adapter_invalid_response",
			request.label+" failed: unexpected response shape.", 502)
	}
	return object, nil
}

type postRequest struct {
	client   *http.Client
	endpoint string
	apiKey   string
	label    string
	limit    int64
	timeout  time.Duration
	payload  map[string]any
	context  Context
}

// --- mapping ---------------------------------------------------------------

// SearchResult is one native search hit.
type SearchResult struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet"`
	Date     string `json:"date,omitempty"`
	SiteName string `json:"site_name,omitempty"`
}

// SearchResponse is the exact native 200 body.
type SearchResponse struct {
	SearchResults []SearchResult `json:"search_results"`
}

// mapSearchResults drops everything the kernel does not read. A result without
// a usable URL is dropped because the kernel cites by URL.
func mapSearchResults(document map[string]any, limit int) ([]SearchResult, error) {
	rows, ok := document["results"].([]any)
	if !ok {
		return nil, failure("web_adapter_invalid_response",
			"Search failed: the response carried no results list.", 502)
	}
	out := make([]SearchResult, 0, len(rows))
	for _, raw := range rows {
		if len(out) >= limit {
			break
		}
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		link := resultURL(stringOf(entry["url"]))
		if link == "" {
			continue
		}
		site := hostLabel(link)
		title := strings.TrimSpace(stringOf(entry["title"]))
		if title == "" {
			if site != "" {
				title = site
			} else {
				title = link
			}
		}
		result := SearchResult{Title: title, URL: link, Snippet: snippetText(stringOf(entry["content"]))}
		if date := strings.TrimSpace(stringOf(entry["published_date"])); date != "" {
			result.Date = date
		}
		if site != "" {
			result.SiteName = site
		}
		out = append(out, result)
	}
	return out, nil
}

func stringOf(value any) string {
	text, _ := value.(string)
	return text
}

// extractContent turns one Tavily extract answer into the page text.
//
// A 200 is not a success on its own: a URL can land in `failed_results` with an
// empty `results`, and an entry can carry empty `raw_content`. Both are reported
// as failures, so the kernel shows an error instead of an empty page. A single
// result is accepted when Tavily normalised the URL; anything else must match the
// URL that was asked for, so one page's text is never returned as another's.
func extractContent(document map[string]any, target string) (string, error) {
	results := rowsOf(document["results"])
	failed := rowsOf(document["failed_results"])
	for _, entry := range failed {
		if resultURL(stringOf(entry["url"])) == target {
			reason := strings.TrimSpace(stringOf(entry["error"]))
			message := "The page could not be read"
			if reason != "" {
				message += ": " + truncateChars(reason, MaxErrorDetail)
			}
			return "", failure("web_adapter_extract_failed", message+".", 502)
		}
	}
	var match map[string]any
	for _, entry := range results {
		if resultURL(stringOf(entry["url"])) == target {
			match = entry
			break
		}
	}
	if match == nil && len(results) == 1 {
		match = results[0]
	}
	content := strings.TrimSpace(stringOf(match["raw_content"]))
	if content == "" {
		message := "The page could not be read"
		for _, entry := range failed {
			if reason := strings.TrimSpace(stringOf(entry["error"])); reason != "" {
				message += ": " + truncateChars(reason, MaxErrorDetail)
				break
			}
		}
		return "", failure("web_adapter_extract_failed", message+".", 502)
	}
	if utf16Len(content) > MaxContentChars {
		return truncateChars(content, MaxContentChars) + truncationNotice(MaxContentChars), nil
	}
	return content, nil
}

func rowsOf(value any) []map[string]any {
	rows, _ := value.([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, raw := range rows {
		if entry, ok := raw.(map[string]any); ok {
			out = append(out, entry)
		}
	}
	return out
}

// --- public adapter surface ------------------------------------------------

// Search answers one native WebSearch request. The result is the exact native
// 200 body, to be written as JSON.
func Search(ctx Context, request SearchRequest, options Options) (SearchResponse, error) {
	query, err := queryText(request)
	if err != nil {
		return SearchResponse{}, err
	}
	endpoint, err := vendorEndpoint(options.Endpoint, TavilySearchEndpoint)
	if err != nil {
		return SearchResponse{}, err
	}
	key, err := vendorKey(options.APIKey)
	if err != nil {
		return SearchResponse{}, err
	}
	depth, err := searchDepth(options.SearchDepth)
	if err != nil {
		return SearchResponse{}, err
	}
	count := resultCount(options.MaxResults)
	document, err := postJSON(postRequest{
		client: options.client(), endpoint: endpoint, apiKey: key, label: "Web search",
		limit: MaxSearchBytes, timeout: timeoutOf(options.Timeout), context: ctx,
		payload: map[string]any{
			"query": query, "max_results": count, "search_depth": depth,
			"topic": "general", "include_answer": false, "include_raw_content": false,
			"include_images": false,
		},
	})
	if err != nil {
		return SearchResponse{}, err
	}
	results, err := mapSearchResults(document, count)
	if err != nil {
		return SearchResponse{}, err
	}
	return SearchResponse{SearchResults: results}, nil
}

// Fetch answers one native FetchURL request. The result is the native 200 body
// itself: markdown text, to be written with NativeFetchContentType.
func Fetch(ctx Context, request FetchRequest, options Options) (string, error) {
	target, err := targetURL(request)
	if err != nil {
		return "", err
	}
	endpoint, err := vendorEndpoint(options.Endpoint, TavilyExtractEndpoint)
	if err != nil {
		return "", err
	}
	key, err := vendorKey(options.APIKey)
	if err != nil {
		return "", err
	}
	document, err := postJSON(postRequest{
		client: options.client(), endpoint: endpoint, apiKey: key, label: "Web page reader",
		limit: MaxExtractBytes, timeout: timeoutOf(options.Timeout), context: ctx,
		payload: map[string]any{"urls": []any{target}, "extract_depth": "basic",
			"format": "markdown", "include_images": false},
	})
	if err != nil {
		return "", err
	}
	return extractContent(document, target)
}

// ProbeResult is what a service test reports.
type ProbeResult struct {
	OK        bool   `json:"ok"`
	Step      string `json:"step"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	Results   int    `json:"results,omitempty"`
	PageChars int    `json:"pageChars,omitempty"`
	ElapsedMS int64  `json:"elapsedMs"`
}

// Probe runs the fixed sample. A pass means the account and endpoint answer in
// the native shape; it says nothing about whether a running Kimi session has
// reloaded its endpoint, and a caller must report those two facts separately.
//
// Search and read may be configured on different endpoints, so each step takes
// its own; an empty value falls back to the vendor default.
func ProbeService(ctx Context, options Options, searchEndpoint, fetchEndpoint string) ProbeResult {
	started := options.now()
	elapsed := func() int64 { return options.now().Sub(started).Milliseconds() }
	searchOptions := options
	if searchEndpoint != "" {
		searchOptions.Endpoint = searchEndpoint
	}
	searchOptions.MaxResults = 3
	search, err := Search(ctx, SearchRequest{TextQuery: Probe.Query}, searchOptions)
	if err != nil {
		code := errorCode(err)
		return ProbeResult{Step: probeStep(code), Code: code, Message: errorMessage(err), ElapsedMS: elapsed()}
	}
	if len(search.SearchResults) == 0 {
		return ProbeResult{Step: "search", Code: "web_adapter_empty_results",
			Message:   "The search service answered, but returned no results for the sample query.",
			ElapsedMS: elapsed()}
	}
	fetchOptions := options
	if fetchEndpoint != "" {
		fetchOptions.Endpoint = fetchEndpoint
	}
	page, err := Fetch(ctx, FetchRequest{URL: Probe.URL}, fetchOptions)
	if err != nil {
		code := errorCode(err)
		return ProbeResult{Step: probeStep(code), Code: code, Message: errorMessage(err),
			Results: len(search.SearchResults), ElapsedMS: elapsed()}
	}
	return ProbeResult{OK: true, Step: "done", Results: len(search.SearchResults),
		PageChars: utf16Len(page), ElapsedMS: elapsed()}
}

func probeStep(code string) string {
	if code == "web_adapter_extract_failed" || code == "web_adapter_invalid_response" {
		return "read"
	}
	return "search"
}

func errorCode(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return "web_adapter_probe_failed"
}

func errorMessage(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Message
	}
	return "The search service test failed."
}

// --- settings --------------------------------------------------------------

// Providers the panel may select.
const (
	ProviderOfficial = "official"
	ProviderTavily   = "tavily"
	ProviderNative   = "native"
)

var providers = map[string]bool{ProviderOfficial: true, ProviderTavily: true, ProviderNative: true}

// Settings is the saved provider choice.
type Settings struct {
	SchemaVersion int    `json:"schemaVersion"`
	Provider      string `json:"provider"`
}

// Credentials is the caller's vendor-key source. This package never decides
// where a key lives: it asks for one and reports whether one exists.
type Credentials interface {
	// Get returns the vendor key, or "" when none is configured.
	Get() (string, error)
	// Set stores a key. A caller that does not support storage may return an
	// error; the panel then reports the key as unset.
	Set(key string) error
}

// SettingsStore persists the provider choice. core.AtomicJSON and
// core.ReadPrivateFile already implement the atomic write and the permission
// check, so a caller can satisfy this with two lines.
type SettingsStore interface {
	Read() ([]byte, error)
	Write([]byte) error
}

// FileSettings is a SettingsStore backed by one private file.
type FileSettings struct{ Path string }

// Read implements SettingsStore.
func (f FileSettings) Read() ([]byte, error) { return core.ReadPrivateFile(f.Path, 4096) }

// Write implements SettingsStore.
func (f FileSettings) Write(body []byte) error {
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return errors.New("invalid_web_settings")
	}
	return core.AtomicJSON(f.Path, document)
}

// Service owns the saved provider choice and the loopback listener.
type Service struct {
	mu          sync.Mutex
	settings    SettingsStore
	credentials Credentials
	client      *http.Client
	now         func() time.Time
	timeout     time.Duration
	searchURL   string
	fetchURL    string
	provider    string
	providerErr string
	lastResult  *ProbeResult
	server      *http.Server
	listener    net.Listener
	host        string
	baseURL     string
	localToken  string
	authFile    string
}

// ServiceOptions configures one Service.
type ServiceOptions struct {
	// Settings persists the provider choice. Nil keeps the choice in memory only.
	Settings SettingsStore
	// Credentials supplies and stores the vendor key. Nil means no key.
	Credentials Credentials
	// AuthFile is where the loopback token is kept. Empty disables persistence,
	// and a fresh token is minted on every Start.
	AuthFile string
	// Client is the HTTP client used for vendor calls.
	Client *http.Client
	// SearchEndpoint and FetchEndpoint override the vendor addresses.
	SearchEndpoint string
	FetchEndpoint  string
	// Timeout bounds one vendor call.
	Timeout time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// NewService reads the saved provider choice. A damaged settings file is
// reported through the state rather than as an error, because the panel still
// has to render something.
func NewService(options ServiceOptions) *Service {
	service := &Service{settings: options.Settings, credentials: options.Credentials,
		client: options.Client, now: options.Now, timeout: options.Timeout,
		searchURL: options.SearchEndpoint, fetchURL: options.FetchEndpoint,
		authFile: options.AuthFile, provider: ProviderOfficial}
	if service.now == nil {
		service.now = time.Now
	}
	if service.settings == nil {
		return service
	}
	raw, err := service.settings.Read()
	if err != nil {
		if !isNotExist(err) {
			service.providerErr = "web_settings_unavailable"
		}
		return service
	}
	var saved Settings
	if json.Unmarshal(raw, &saved) != nil || saved.SchemaVersion != 1 || !providers[saved.Provider] {
		service.providerErr = "web_settings_unavailable"
		return service
	}
	service.provider = saved.Provider
	return service
}

func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// Provider reports the saved choice.
func (s *Service) Provider() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provider
}

// KeySet reports whether a vendor key is available right now.
func (s *Service) KeySet() bool {
	_, err := s.key()
	return err == nil
}

func (s *Service) key() (string, error) {
	if s.credentials == nil {
		return "", failure("web_key_missing", "The search service has no API key.", 500)
	}
	value, err := s.credentials.Get()
	if err != nil {
		return "", failure("web_key_missing", "The search service has no API key.", 500)
	}
	if _, err := vendorKey(value); err != nil {
		return "", err
	}
	return value, nil
}

// Start binds the loopback listener. It is idempotent: a second call while the
// listener is up does nothing.
func (s *Service) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server != nil {
		return nil
	}
	token, err := s.loadOrCreateToken()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return failure("web_adapter_unavailable", "The local search endpoint could not start.", 503)
	}
	address := listener.Addr().(*net.TCPAddr)
	s.localToken = token
	s.listener = listener
	s.host = "127.0.0.1:" + itoa(address.Port)
	s.baseURL = "http://" + s.host
	server := &http.Server{Handler: http.HandlerFunc(s.serveHTTP),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	s.server = server
	go func() { _ = server.Serve(listener) }()
	return nil
}

func (s *Service) loadOrCreateToken() (string, error) {
	if s.authFile == "" {
		return randomToken()
	}
	raw, err := core.ReadPrivateFile(s.authFile, 4096)
	if err == nil {
		var document struct {
			SchemaVersion int    `json:"schemaVersion"`
			LocalToken    string `json:"localToken"`
		}
		if json.Unmarshal(raw, &document) != nil || document.SchemaVersion != 1 || !tokenPattern.MatchString(document.LocalToken) {
			return "", failure("web_local_auth_conflict", "The local search token file is unusable.", 409)
		}
		return document.LocalToken, nil
	}
	if !isNotExist(err) {
		return "", failure("web_local_auth_conflict", "The local search token file is unusable.", 409)
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	if err := core.AtomicJSON(s.authFile, map[string]any{"schemaVersion": 1, "localToken": token}); err != nil {
		return "", failure("web_local_auth_conflict", "The local search token could not be saved.", 409)
	}
	return token, nil
}

var tokenPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func randomToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", failure("web_adapter_unavailable", "A local token could not be generated.", 503)
	}
	return hex.EncodeToString(buf[:]), nil
}

// Close stops the listener. It is safe to call twice.
func (s *Service) Close() error {
	s.mu.Lock()
	server := s.server
	s.server = nil
	s.listener = nil
	s.baseURL = ""
	s.host = ""
	s.mu.Unlock()
	if server == nil {
		return nil
	}
	return server.Close()
}

// NativeServices is the block Kimi's config needs: two endpoints and the local
// token. It refuses before the listener is up, because a config naming a dead
// endpoint would silently disable search.
func (s *Service) NativeServices() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == nil {
		return nil, failure("web_adapter_unavailable", "The local search endpoint is not running.", 503)
	}
	return map[string]any{
		"search": map[string]any{"enabled": true, "baseURL": s.baseURL + "/search", "apiKey": s.localToken},
		"fetch":  map[string]any{"enabled": true, "baseURL": s.baseURL + "/fetch", "apiKey": s.localToken},
	}, nil
}

// BaseURL reports the listener root, or "" when it is not running.
func (s *Service) BaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseURL
}

// --- the local endpoint ----------------------------------------------------

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	ctx, cancel := contextWithCancel(r.Context())
	defer cancel()
	// A client that goes away aborts the vendor call instead of leaving it to
	// finish for nobody.
	go func() {
		<-ctx.Done()
		cancel()
	}()
	if !s.authorized(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/search" && r.URL.Path != "/fetch") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !contentTypeIsJSON(r.Header.Get("Content-Type")) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBody(r)
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key, keyErr := s.key()
	options := Options{APIKey: key, Client: s.client, Timeout: s.timeout, Now: s.now}
	search := r.URL.Path == "/search"
	if search {
		options.Endpoint = s.searchURL
	} else {
		options.Endpoint = s.fetchURL
	}
	if keyErr != nil {
		s.writeError(w, keyErr)
		return
	}
	if search {
		request := SearchRequest{TextQuery: stringOf(object["text_query"])}
		answer, err := Search(ctx, request, options)
		if err != nil {
			s.writeError(w, err)
			return
		}
		encoded, err := json.Marshal(answer)
		if err != nil {
			s.writeError(w, failure("web_adapter_invalid_response", "The answer could not be encoded.", 502))
			return
		}
		w.Header().Set("Content-Type", NativeSearchContentType+"; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
		return
	}
	request := FetchRequest{URL: stringOf(object["url"])}
	page, err := Fetch(ctx, request, options)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", NativeFetchContentType+"; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
}

// contentTypeIsJSON accepts `application/json` with any parameters, which is
// what the kernel sends.
func contentTypeIsJSON(media string) bool {
	lowered := strings.ToLower(strings.TrimSpace(media))
	return lowered == "application/json" || strings.HasPrefix(lowered, "application/json;")
}

func (s *Service) authorized(r *http.Request) bool {
	s.mu.Lock()
	host, token := s.host, s.localToken
	s.mu.Unlock()
	if host == "" {
		return false
	}
	if r.Host != host || r.Header.Get("Origin") != "" {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
}

func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxRequestBytes {
		return nil, errors.New("too_large")
	}
	return body, nil
}

func (s *Service) writeError(w http.ResponseWriter, err error) {
	code := "web_adapter_invalid_request"
	status := 400
	var typed *Error
	if errors.As(err, &typed) {
		if regexp.MustCompile(`^web_adapter_[a-z_]+$`).MatchString(typed.Code) {
			code = typed.Code
		}
		if typed.Status >= 400 && typed.Status <= 504 {
			status = typed.Status
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `"}}`))
}

// --- panel surface ---------------------------------------------------------

// State is the block the panel renders.
type State struct {
	Provider       string       `json:"provider"`
	KeySet         bool         `json:"keySet"`
	Message        string       `json:"message"`
	PendingRestart bool         `json:"pendingRestart"`
	LastResult     *ProbeResult `json:"lastResult"`
	Error          string       `json:"error,omitempty"`
}

// State renders the current provider choice. It makes no request.
func (s *Service) State(nativeStatus map[string]any) State {
	s.mu.Lock()
	provider, providerErr, last := s.provider, s.providerErr, s.lastResult
	s.mu.Unlock()
	state := State{Provider: provider, KeySet: s.KeySet(), LastResult: last,
		PendingRestart: provider != ProviderOfficial}
	if providerErr != "" {
		state.Error = providerErr
		state.Message = providerErr
		return state
	}
	searchStatus, fetchStatus := "未确认", "未确认"
	if services, ok := nativeStatus["services"].(map[string]any); ok {
		if entry, ok := services["search"].(map[string]any); ok {
			if text := stringOf(entry["status"]); text != "" {
				searchStatus = text
			}
		}
		if entry, ok := services["fetch"].(map[string]any); ok {
			if text := stringOf(entry["status"]); text != "" {
				fetchStatus = text
			}
		}
	}
	switch provider {
	case ProviderTavily:
		if !state.KeySet {
			state.Error = "web_adapter_key_missing"
			state.Message = "Tavily 密钥不可用，请重新保存密钥后启用。"
			return state
		}
		state.Message = "Tavily 本机适配已选择。搜索配置：" + searchStatus +
			"；正文读取配置：" + fetchStatus + "。需要重新加载 Kimi 会话后使用。"
	case ProviderNative:
		state.Message = "使用下方高级表单保存原生兼容端点；不能填写模型 Base URL。"
	default:
		state.Message = "沿用 Kimi 官方服务，本机 Tavily 适配不会接管请求。"
	}
	return state
}

// SaveProvider validates a choice. For `tavily` it stores the key through the
// caller's Credentials and starts the listener, returning the block Kimi's
// config needs.
//
// The key is written before the listener starts, so a save that fails at the
// listener still has a key the caller can reuse; nothing here leaves a
// half-applied provider, because the provider itself is only changed by Commit.
func (s *Service) SaveProvider(provider, apiKey string) (map[string]any, error) {
	if !providers[provider] {
		return nil, failure("invalid_services", "Unknown service provider.", 400)
	}
	if provider == ProviderTavily {
		if apiKey != "" {
			if _, err := vendorKey(apiKey); err != nil {
				return nil, err
			}
			if s.credentials == nil {
				return nil, failure("credentials_file_unavailable",
					"No credential store is available for the search service key.", 500)
			}
			if err := s.credentials.Set(apiKey); err != nil {
				return nil, failure("credentials_unavailable",
					"The search service key could not be stored.", 503)
			}
		}
		if _, err := s.key(); err != nil {
			return nil, err
		}
		if err := s.Start(); err != nil {
			return nil, err
		}
		return s.NativeServices()
	}
	if provider == ProviderOfficial {
		return map[string]any{"search": map[string]any{"enabled": false},
			"fetch": map[string]any{"enabled": false}}, nil
	}
	return nil, nil
}

// Commit persists the provider choice. It is separate from SaveProvider so a
// caller can validate and start the adapter first, then commit, then stop the
// listener if the commit failed.
func (s *Service) Commit(provider string) error {
	if !providers[provider] {
		return failure("invalid_services", "Unknown service provider.", 400)
	}
	s.mu.Lock()
	providerErr := s.providerErr
	s.mu.Unlock()
	if providerErr != "" {
		return failure("web_settings_unavailable", "The saved service settings are unusable.", 503)
	}
	if s.settings != nil {
		body, err := json.Marshal(Settings{SchemaVersion: 1, Provider: provider})
		if err != nil {
			return failure("web_settings_unavailable", "The service settings could not be encoded.", 503)
		}
		if err := s.settings.Write(body); err != nil {
			return failure("web_settings_unavailable", "The service settings could not be saved.", 503)
		}
	}
	s.mu.Lock()
	s.provider = provider
	s.providerErr = ""
	s.mu.Unlock()
	return nil
}

// Probe tests the configured vendor account with the fixed sample. Only the
// `tavily` provider is tested: the official service is untouched, and a custom
// native endpoint can only be verified inside a real Kimi session.
func (s *Service) Probe(provider, apiKey string) map[string]any {
	skipped := func(message string) map[string]any {
		return map[string]any{"search": map[string]any{"state": "skipped"},
			"fetch": map[string]any{"state": "skipped"}, "message": message}
	}
	if provider != ProviderTavily {
		if provider == ProviderOfficial {
			return skipped("官方服务未改动；不发送 Tavily 测试请求。")
		}
		return skipped("自定义原生服务请在实际 Kimi 新会话中验证；此处不把模型端点当搜索服务。")
	}
	key := apiKey
	if key == "" {
		value, err := s.key()
		if err != nil {
			return map[string]any{"search": map[string]any{"state": "failed"},
				"fetch": map[string]any{"state": "not-run"},
				"code":  "web_key_missing", "message": "无法开始适配测试：web_key_missing。"}
		}
		key = value
	}
	result := ProbeService(contextBackground(), Options{APIKey: key, Client: s.client,
		Timeout: s.timeout, Now: s.now}, s.searchURL, s.fetchURL)
	s.mu.Lock()
	s.lastResult = &result
	s.mu.Unlock()
	searchState := "failed"
	if result.OK || result.Step == "read" {
		searchState = "passed"
	}
	fetchState := "not-run"
	if result.OK {
		fetchState = "passed"
	} else if result.Step == "read" {
		fetchState = "failed"
	}
	answer := map[string]any{
		"search": map[string]any{"state": searchState, "count": result.Results},
		"fetch":  map[string]any{"state": fetchState, "pageChars": result.PageChars},
	}
	if result.OK {
		answer["message"] = "本机 Tavily 搜索与正文读取均返回了原生契约结果；尚未证明当前 Kimi 会话已重新加载。"
	} else {
		answer["code"] = result.Code
		answer["message"] = "本机适配测试未完成：" + result.Code + "。"
	}
	return answer
}
