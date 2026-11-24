package webtools_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/webtools"
)

// No test in this file reaches Tavily or any real page. Every vendor answer is a
// loopback handler built from the documented shape.

const vendorKey = "tvly-test-key-0001"

// vendor is a fake Tavily that records exactly what arrived.
type vendor struct {
	mu       sync.Mutex
	requests []captured
	server   *httptest.Server
	reply    func(w http.ResponseWriter, r *http.Request, body []byte)
}

type captured struct {
	Path          string
	Authorization string
	ContentType   string
	Body          string
}

func newVendor(t *testing.T, reply func(w http.ResponseWriter, r *http.Request, body []byte)) *vendor {
	t.Helper()
	fake := &vendor{reply: reply}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.requests = append(fake.requests, captured{Path: r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"), Body: string(body)})
		fake.mu.Unlock()
		fake.reply(w, r, body)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (v *vendor) captured() []captured {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]captured, len(v.requests))
	copy(out, v.requests)
	return out
}

func options(endpoint string) webtools.Options {
	return webtools.Options{APIKey: vendorKey, Endpoint: endpoint, Timeout: 5 * time.Second}
}

// --- search ----------------------------------------------------------------

func TestSearchMapsTavilyOntoTheNativeShape(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"Tavily Docs","url":"https://docs.tavily.test/intro","content":"  How   it works  ",
			 "score":0.99,"published_date":"2026-01-02","raw_content":"ignored","images":["x"]},
			{"title":"","url":"https://www.example.test/page","content":"a snippet"},
			{"title":"No URL","url":"","content":"dropped"},
			{"title":"Bad scheme","url":"ftp://example.test/x","content":"dropped"},
			{"title":"Beyond limit","url":"https://third.test/","content":"dropped"}
		]}`))
	})
	answer, err := webtools.Search(context.Background(),
		webtools.SearchRequest{TextQuery: "how does it work"}, options(fake.server.URL))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// Three usable results survive; the no-URL and bad-scheme rows are dropped,
	// and the limit of 3 stops before the fifth.
	if len(answer.SearchResults) != 3 {
		t.Fatalf("results %d", len(answer.SearchResults))
	}
	first := answer.SearchResults[0]
	if first.Title != "Tavily Docs" || first.URL != "https://docs.tavily.test/intro" ||
		first.Snippet != "How it works" || first.Date != "2026-01-02" || first.SiteName != "docs.tavily.test" {
		t.Fatalf("first %+v", first)
	}
	// A blank title falls back to the site label, with `www.` removed.
	second := answer.SearchResults[1]
	if second.Title != "example.test" || second.SiteName != "example.test" {
		t.Fatalf("second %+v", second)
	}
	// Vendor-only fields never reach the native answer.
	encoded, _ := json.Marshal(answer)
	for _, forbidden := range []string{"score", "raw_content", "images", "0.99"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("vendor field %q leaked: %s", forbidden, encoded)
		}
	}

	requests := fake.captured()
	if len(requests) != 1 {
		t.Fatalf("requests %d", len(requests))
	}
	if requests[0].Authorization != "Bearer "+vendorKey {
		t.Fatalf("authorization %q", requests[0].Authorization)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(requests[0].Body), &sent); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if sent["query"] != "how does it work" || sent["search_depth"] != "basic" ||
		sent["include_answer"] != false || sent["include_raw_content"] != false {
		t.Fatalf("payload %v", sent)
	}
	if sent["max_results"] != float64(webtools.DefaultResults) {
		t.Fatalf("max_results %v", sent["max_results"])
	}
}

func TestSearchBoundsAndRefusals(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	cases := []struct {
		name   string
		req    webtools.SearchRequest
		opts   webtools.Options
		code   string
		status int
	}{
		{"empty query", webtools.SearchRequest{TextQuery: "  "}, options(fake.server.URL),
			"web_adapter_invalid_request", 400},
		{"query too long", webtools.SearchRequest{TextQuery: strings.Repeat("a", 2001)}, options(fake.server.URL),
			"web_adapter_invalid_request", 400},
		{"missing key", webtools.SearchRequest{TextQuery: "x"},
			webtools.Options{Endpoint: fake.server.URL}, "web_adapter_key_missing", 500},
		{"key with space", webtools.SearchRequest{TextQuery: "x"},
			webtools.Options{APIKey: " key", Endpoint: fake.server.URL}, "web_adapter_key_missing", 500},
		{"plain http remote", webtools.SearchRequest{TextQuery: "x"},
			webtools.Options{APIKey: vendorKey, Endpoint: "http://vendor.example.test/search"},
			"web_adapter_endpoint_invalid", 500},
		{"endpoint with query", webtools.SearchRequest{TextQuery: "x"},
			webtools.Options{APIKey: vendorKey, Endpoint: fake.server.URL + "/search?k=1"},
			"web_adapter_endpoint_invalid", 500},
		{"bad depth", webtools.SearchRequest{TextQuery: "x"},
			webtools.Options{APIKey: vendorKey, Endpoint: fake.server.URL, SearchDepth: "deep"},
			"web_adapter_invalid_request", 400},
	}
	for _, test := range cases {
		_, err := webtools.Search(context.Background(), test.req, test.opts)
		var typed *webtools.Error
		if !errors.As(err, &typed) {
			t.Fatalf("%s: error %v", test.name, err)
		}
		if typed.Code != test.code || typed.Status != test.status {
			t.Fatalf("%s: %s/%d want %s/%d", test.name, typed.Code, typed.Status, test.code, test.status)
		}
	}
	// A loopback plain-HTTP endpoint is accepted, which is what a local relay is.
	if _, err := webtools.Search(context.Background(),
		webtools.SearchRequest{TextQuery: "x"}, options(fake.server.URL)); err != nil {
		t.Fatalf("loopback endpoint refused: %v", err)
	}
}

func TestVendorStatusBecomesTheRightRefusal(t *testing.T) {
	cases := []struct {
		status       int
		code         string
		nativeStatus int
	}{
		{401, "web_adapter_auth_failed", 401},
		{403, "web_adapter_auth_failed", 401},
		{429, "web_adapter_rate_limited", 429},
		{400, "web_adapter_invalid_request", 400},
		{500, "web_adapter_upstream_error", 502},
		{503, "web_adapter_upstream_error", 502},
	}
	for _, test := range cases {
		fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.WriteHeader(test.status)
			_, _ = w.Write([]byte(`{"detail":"vendor said no"}`))
		})
		_, err := webtools.Search(context.Background(),
			webtools.SearchRequest{TextQuery: "x"}, options(fake.server.URL))
		var typed *webtools.Error
		if !errors.As(err, &typed) {
			t.Fatalf("status %d: %v", test.status, err)
		}
		if typed.Code != test.code || typed.Status != test.nativeStatus || typed.VendorStatus != test.status {
			t.Fatalf("status %d: %+v", test.status, typed)
		}
		// The vendor's own words are bounded, never published whole.
		if len(typed.Message) > 400 {
			t.Fatalf("status %d: message not bounded: %d", test.status, len(typed.Message))
		}
	}
}

func TestSearchRejectsAnUnexpectedShape(t *testing.T) {
	for _, body := range []string{`not json`, `[]`, `{"results":"nope"}`, `null`} {
		fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
		_, err := webtools.Search(context.Background(),
			webtools.SearchRequest{TextQuery: "x"}, options(fake.server.URL))
		var typed *webtools.Error
		if !errors.As(err, &typed) || typed.Code != "web_adapter_invalid_response" {
			t.Fatalf("%s: %v", body, err)
		}
	}
}

func TestAResponseOverTheLimitIsRefused(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "104857600")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	_, err := webtools.Search(context.Background(),
		webtools.SearchRequest{TextQuery: "x"}, options(fake.server.URL))
	var typed *webtools.Error
	if !errors.As(err, &typed) || typed.Code != "web_adapter_response_too_large" {
		t.Fatalf("error %v", err)
	}
}

func TestATimeoutIsReportedAsATimeout(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	opts := options(fake.server.URL)
	opts.Timeout = webtools.MinTimeout
	// MinTimeout is one second, so a slow handler is not enough; use a cancelled
	// context instead to prove the abort path.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := webtools.Search(ctx, webtools.SearchRequest{TextQuery: "x"}, opts)
	var typed *webtools.Error
	if !errors.As(err, &typed) || typed.Code != "web_adapter_aborted" {
		t.Fatalf("error %v", err)
	}
}

func TestARedirectIsRefusedSoTheKeyIsNotReplayed(t *testing.T) {
	var reached int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(200)
	}))
	defer target.Close()
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(302)
	})
	_, err := webtools.Search(context.Background(),
		webtools.SearchRequest{TextQuery: "x"}, options(fake.server.URL))
	if err == nil {
		t.Fatalf("a redirect was followed")
	}
	if reached != 0 {
		t.Fatalf("the redirect target was reached %d times", reached)
	}
}

// --- fetch -----------------------------------------------------------------

func TestFetchReturnsTheNativeMarkdownBody(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"url":"https://example.test/page","raw_content":"  # Title\n\nBody  "}
		],"failed_results":[]}`))
	})
	page, err := webtools.Fetch(context.Background(),
		webtools.FetchRequest{URL: "https://example.test/page"}, options(fake.server.URL))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if page != "# Title\n\nBody" {
		t.Fatalf("page %q", page)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(fake.captured()[0].Body), &sent)
	urls, _ := sent["urls"].([]any)
	if len(urls) != 1 || urls[0] != "https://example.test/page" ||
		sent["format"] != "markdown" || sent["extract_depth"] != "basic" {
		t.Fatalf("payload %v", sent)
	}
}

func TestExtractFailureIsNeverAnEmptyPage(t *testing.T) {
	// A URL that Tavily reports as failed.
	failed := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"failed_results":[
			{"url":"https://example.test/blocked","error":"robots.txt"}]}`))
	})
	_, err := webtools.Fetch(context.Background(),
		webtools.FetchRequest{URL: "https://example.test/blocked"}, options(failed.server.URL))
	var typed *webtools.Error
	if !errors.As(err, &typed) || typed.Code != "web_adapter_extract_failed" ||
		typed.Status != 502 {
		t.Fatalf("failed_results: %v", err)
	}
	if !strings.Contains(typed.Message, "robots.txt") {
		t.Fatalf("the vendor reason was lost: %q", typed.Message)
	}

	// A 200 whose only result carries empty content.
	empty := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.test/x","raw_content":"   "}]}`))
	})
	_, err = webtools.Fetch(context.Background(),
		webtools.FetchRequest{URL: "https://example.test/x"}, options(empty.server.URL))
	if !errors.As(err, &typed) || typed.Code != "web_adapter_extract_failed" {
		t.Fatalf("empty content: %v", err)
	}
}

func TestOnePagesTextIsNeverReturnedAsAnothers(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		// Two results, neither matching the requested URL.
		_, _ = w.Write([]byte(`{"results":[
			{"url":"https://other.test/a","raw_content":"page A"},
			{"url":"https://other.test/b","raw_content":"page B"}
		]}`))
	})
	_, err := webtools.Fetch(context.Background(),
		webtools.FetchRequest{URL: "https://example.test/wanted"}, options(fake.server.URL))
	if err == nil {
		t.Fatalf("a mismatched page was returned")
	}
	// A single normalised result is accepted, which is what a trailing slash
	// produces.
	single := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.test/page/","raw_content":"ok"}]}`))
	})
	page, err := webtools.Fetch(context.Background(),
		webtools.FetchRequest{URL: "https://example.test/page"}, options(single.server.URL))
	if err != nil || page != "ok" {
		t.Fatalf("normalised single result: %q %v", page, err)
	}
}

func TestLongContentIsTruncatedWithANotice(t *testing.T) {
	long := strings.Repeat("字", webtools.MaxContentChars+10)
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		encoded, _ := json.Marshal(map[string]any{"results": []any{
			map[string]any{"url": "https://example.test/long", "raw_content": long}}})
		_, _ = w.Write(encoded)
	})
	page, err := webtools.Fetch(context.Background(),
		webtools.FetchRequest{URL: "https://example.test/long"}, options(fake.server.URL))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.HasSuffix(page, "[Content truncated at 200000 characters.]") {
		t.Fatalf("no truncation notice: ...%q", page[maxInt(0, len(page)-60):])
	}
	// The cut is on a character boundary, so the text is still valid UTF-8.
	body := strings.TrimSuffix(page, "\n\n[Content truncated at 200000 characters.]")
	if !strings.HasPrefix(body, "字") || strings.ContainsRune(body, '\uFFFD') {
		t.Fatalf("the cut split a character")
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestFetchURLRules(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	for _, target := range []string{"", "   ", "ftp://example.test/x", "not a url",
		"javascript:alert(1)", "https://" + strings.Repeat("a", 4090)} {
		_, err := webtools.Fetch(context.Background(),
			webtools.FetchRequest{URL: target}, options(fake.server.URL))
		var typed *webtools.Error
		if !errors.As(err, &typed) || typed.Code != "web_adapter_invalid_request" {
			t.Fatalf("%q: %v", target, err)
		}
	}
}

// --- probe -----------------------------------------------------------------

func TestProbeReportsEachStepSeparately(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"query"`) {
			_, _ = w.Write([]byte(`{"results":[{"title":"a","url":"https://a.test/","content":"b"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.com/","raw_content":"page"}]}`))
	})
	result := webtools.ProbeService(context.Background(), options(fake.server.URL), "", "")
	if !result.OK || result.Step != "done" || result.Results != 1 || result.PageChars != 4 {
		t.Fatalf("result %+v", result)
	}
	// The sample is the fixed one, never session text.
	if !strings.Contains(fake.captured()[0].Body, webtools.Probe.Query) {
		t.Fatalf("the probe did not use the fixed sample")
	}
}

func TestProbeNamesTheStepThatFailed(t *testing.T) {
	searchOnly := newVendor(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"query"`) {
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	result := webtools.ProbeService(context.Background(), options(searchOnly.server.URL), "", "")
	if result.OK || result.Step != "search" || result.Code != "web_adapter_empty_results" {
		t.Fatalf("empty search: %+v", result)
	}

	readFails := newVendor(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"query"`) {
			_, _ = w.Write([]byte(`{"results":[{"title":"a","url":"https://a.test/","content":"b"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"failed_results":[{"url":"https://example.com/","error":"blocked"}]}`))
	})
	result = webtools.ProbeService(context.Background(), options(readFails.server.URL), "", "")
	if result.OK || result.Step != "read" || result.Code != "web_adapter_extract_failed" {
		t.Fatalf("read failure: %+v", result)
	}
	if result.Results != 1 {
		t.Fatalf("the search step's count was lost: %+v", result)
	}
}

// --- settings --------------------------------------------------------------

type memSettings struct {
	mu   sync.Mutex
	body []byte
}

func (m *memSettings) Read() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.body == nil {
		return nil, os.ErrNotExist
	}
	return m.body, nil
}

func (m *memSettings) Write(body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.body = append([]byte(nil), body...)
	return nil
}

type memCredentials struct {
	mu  sync.Mutex
	key string
	err error
}

func (m *memCredentials) Get() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.key, m.err
}

func (m *memCredentials) Set(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.key = key
	return nil
}

func TestProviderChoiceRoundTripsThroughTheStore(t *testing.T) {
	settings := &memSettings{}
	first := webtools.NewService(webtools.ServiceOptions{Settings: settings,
		Credentials: &memCredentials{}, AuthFile: filepath.Join(t.TempDir(), "auth.json")})
	if first.Provider() != webtools.ProviderOfficial {
		t.Fatalf("default provider %q", first.Provider())
	}
	if err := first.Commit(webtools.ProviderTavily); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// A rebuilt service reads the saved choice.
	second := webtools.NewService(webtools.ServiceOptions{Settings: settings,
		Credentials: &memCredentials{}})
	if second.Provider() != webtools.ProviderTavily {
		t.Fatalf("saved provider %q", second.Provider())
	}
	// A damaged file is reported through the state, not as a panic.
	damaged := &memSettings{body: []byte(`{"schemaVersion":9,"provider":"nope"}`)}
	third := webtools.NewService(webtools.ServiceOptions{Settings: damaged})
	if state := third.State(nil); state.Error != "web_settings_unavailable" {
		t.Fatalf("damaged state %+v", state)
	}
	if err := third.Commit(webtools.ProviderTavily); err == nil {
		t.Fatalf("a damaged file was overwritten")
	}
}

func TestCommitRefusesAnUnknownProvider(t *testing.T) {
	service := webtools.NewService(webtools.ServiceOptions{Settings: &memSettings{}})
	if err := service.Commit("nope"); err == nil {
		t.Fatalf("an unknown provider was accepted")
	}
}

func TestSaveProviderStoresTheKeyAndReturnsTheNativeBlock(t *testing.T) {
	dir := t.TempDir()
	credentials := &memCredentials{}
	service := webtools.NewService(webtools.ServiceOptions{Settings: &memSettings{},
		Credentials: credentials, AuthFile: filepath.Join(dir, "auth.json")})
	block, err := service.SaveProvider(webtools.ProviderTavily, "  bad key  ")
	if err == nil {
		t.Fatalf("a padded key was accepted")
	}
	block, err = service.SaveProvider(webtools.ProviderTavily, "tvly-fresh-key")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	defer func() { _ = service.Close() }()
	if credentials.key != "tvly-fresh-key" {
		t.Fatalf("key not stored: %q", credentials.key)
	}
	search, _ := block["search"].(map[string]any)
	if search["enabled"] != true || !strings.HasSuffix(search["baseURL"].(string), "/search") {
		t.Fatalf("search block %v", block["search"])
	}
	if len(search["apiKey"].(string)) != 64 {
		t.Fatalf("local token %v", search["apiKey"])
	}
	// The provider is not changed by a save alone.
	if service.Provider() != webtools.ProviderOfficial {
		t.Fatalf("a save changed the provider")
	}
	if err := service.Commit(webtools.ProviderTavily); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if service.Provider() != webtools.ProviderTavily {
		t.Fatalf("commit did not persist")
	}
}

func TestSaveProviderWithoutAKeyStoreIsRefused(t *testing.T) {
	service := webtools.NewService(webtools.ServiceOptions{Settings: &memSettings{}})
	if _, err := service.SaveProvider(webtools.ProviderTavily, "tvly-key"); err == nil {
		t.Fatalf("a key was accepted with nowhere to store it")
	}
}

func TestSaveProviderOfficialNeedsNoListener(t *testing.T) {
	service := webtools.NewService(webtools.ServiceOptions{Settings: &memSettings{}})
	block, err := service.SaveProvider(webtools.ProviderOfficial, "")
	if err != nil {
		t.Fatalf("official: %v", err)
	}
	search, _ := block["search"].(map[string]any)
	if search["enabled"] != false {
		t.Fatalf("official block %v", block)
	}
}

// --- the loopback endpoint -------------------------------------------------

// startService brings up a Service whose vendor is the given fake.
func startService(t *testing.T, endpoint string) *webtools.Service {
	t.Helper()
	service := webtools.NewService(webtools.ServiceOptions{Settings: &memSettings{},
		Credentials: &memCredentials{key: vendorKey}, AuthFile: filepath.Join(t.TempDir(), "auth.json"),
		SearchEndpoint: endpoint, FetchEndpoint: endpoint})
	if err := service.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func nativePost(t *testing.T, service *webtools.Service, path, body, token string) (*http.Response, string) {
	t.Helper()
	block, err := service.NativeServices()
	if err != nil {
		t.Fatalf("native services: %v", err)
	}
	entry := block[strings.TrimPrefix(path, "/")].(map[string]any)
	request, err := http.NewRequest(http.MethodPost, entry["baseURL"].(string), strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token == "" {
		token = entry["apiKey"].(string)
	}
	if token != "-" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	raw, _ := io.ReadAll(reply.Body)
	_ = reply.Body.Close()
	return reply, string(raw)
}

func TestTheLocalEndpointServesTheNativeContract(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"query"`) {
			_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://a.test/","content":"S"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.com/","raw_content":"# Page"}]}`))
	})
	service := startService(t, fake.server.URL)

	reply, body := nativePost(t, service, "/search", `{"text_query":"hello"}`, "")
	if reply.StatusCode != 200 {
		t.Fatalf("search status %d", reply.StatusCode)
	}
	if !strings.HasPrefix(reply.Header.Get("Content-Type"), webtools.NativeSearchContentType) {
		t.Fatalf("search content type %q", reply.Header.Get("Content-Type"))
	}
	var answer webtools.SearchResponse
	if err := json.Unmarshal([]byte(body), &answer); err != nil || len(answer.SearchResults) != 1 {
		t.Fatalf("search body %q %v", body, err)
	}

	reply, body = nativePost(t, service, "/fetch", `{"url":"https://example.com/"}`, "")
	if reply.StatusCode != 200 {
		t.Fatalf("fetch status %d", reply.StatusCode)
	}
	if !strings.HasPrefix(reply.Header.Get("Content-Type"), webtools.NativeFetchContentType) {
		t.Fatalf("fetch content type %q", reply.Header.Get("Content-Type"))
	}
	if body != "# Page" {
		t.Fatalf("fetch body %q", body)
	}
	// The vendor key never leaves the process; the local token is what the
	// kernel presents.
	if strings.Contains(body, vendorKey) {
		t.Fatalf("the vendor key leaked into the answer")
	}
}

func TestTheLocalEndpointRefusesEverythingElse(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	service := startService(t, fake.server.URL)

	if reply, _ := nativePost(t, service, "/search", `{"text_query":"x"}`, "wrong-token"); reply.StatusCode != 403 {
		t.Fatalf("wrong token: %d", reply.StatusCode)
	}
	if reply, _ := nativePost(t, service, "/search", `{"text_query":"x"}`, "-"); reply.StatusCode != 403 {
		t.Fatalf("missing token: %d", reply.StatusCode)
	}
	block, _ := service.NativeServices()
	base := block["search"].(map[string]any)["baseURL"].(string)
	token := block["search"].(map[string]any)["apiKey"].(string)

	// A GET is not a native call.
	get, _ := http.NewRequest(http.MethodGet, base, nil)
	get.Header.Set("Authorization", "Bearer "+token)
	if reply, err := http.DefaultClient.Do(get); err != nil || reply.StatusCode != 404 {
		t.Fatalf("GET: %v %v", err, reply)
	} else {
		_ = reply.Body.Close()
	}
	// A browser origin is refused, because the kernel never sends one.
	withOrigin, _ := http.NewRequest(http.MethodPost, base, strings.NewReader(`{"text_query":"x"}`))
	withOrigin.Header.Set("Authorization", "Bearer "+token)
	withOrigin.Header.Set("Content-Type", "application/json")
	withOrigin.Header.Set("Origin", "https://evil.test")
	if reply, err := http.DefaultClient.Do(withOrigin); err != nil || reply.StatusCode != 403 {
		t.Fatalf("origin: %v %v", err, reply)
	} else {
		_ = reply.Body.Close()
	}
	// A non-JSON body is a 415.
	wrongType, _ := http.NewRequest(http.MethodPost, base, strings.NewReader(`text_query=x`))
	wrongType.Header.Set("Authorization", "Bearer "+token)
	wrongType.Header.Set("Content-Type", "text/plain")
	if reply, err := http.DefaultClient.Do(wrongType); err != nil || reply.StatusCode != 415 {
		t.Fatalf("content type: %v %v", err, reply)
	} else {
		_ = reply.Body.Close()
	}
	// An oversized body is a 413.
	big := `{"text_query":"` + strings.Repeat("a", webtools.MaxRequestBytes) + `"}`
	if reply, _ := nativePost(t, service, "/search", big, ""); reply.StatusCode != 413 {
		t.Fatalf("oversized body: %d", reply.StatusCode)
	}
	if len(fake.captured()) != 0 {
		t.Fatalf("a refused request reached the vendor")
	}
}

func TestTheEndpointIsNotServedBeforeStart(t *testing.T) {
	service := webtools.NewService(webtools.ServiceOptions{Credentials: &memCredentials{key: vendorKey}})
	if _, err := service.NativeServices(); err == nil {
		t.Fatalf("a native block was offered before the listener existed")
	}
	if service.BaseURL() != "" {
		t.Fatalf("a base URL was offered before the listener existed")
	}
}

func TestTheLocalTokenSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	first := webtools.NewService(webtools.ServiceOptions{Credentials: &memCredentials{key: vendorKey},
		AuthFile: authFile})
	if err := first.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	before, _ := first.NativeServices()
	_ = first.Close()
	if err := first.Close(); err != nil {
		t.Fatalf("a second close failed: %v", err)
	}

	second := webtools.NewService(webtools.ServiceOptions{Credentials: &memCredentials{key: vendorKey},
		AuthFile: authFile})
	if err := second.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer func() { _ = second.Close() }()
	after, _ := second.NativeServices()
	if before["search"].(map[string]any)["apiKey"] != after["search"].(map[string]any)["apiKey"] {
		t.Fatalf("the local token changed across a restart")
	}
	// A damaged token file is a visible conflict, not a silent replacement.
	if err := os.WriteFile(authFile, []byte(`{"schemaVersion":1,"localToken":"short"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	third := webtools.NewService(webtools.ServiceOptions{Credentials: &memCredentials{key: vendorKey},
		AuthFile: authFile})
	if err := third.Start(); err == nil {
		t.Fatalf("a damaged token file was replaced")
	}
}

func TestConcurrentStartsCreateOneListener(t *testing.T) {
	service := webtools.NewService(webtools.ServiceOptions{Credentials: &memCredentials{key: vendorKey},
		AuthFile: filepath.Join(t.TempDir(), "auth.json")})
	defer func() { _ = service.Close() }()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := service.Start(); err != nil {
				t.Errorf("start: %v", err)
			}
		}()
	}
	wg.Wait()
	if service.BaseURL() == "" {
		t.Fatalf("no listener after concurrent starts")
	}
}

func TestStateDescribesEachProvider(t *testing.T) {
	service := webtools.NewService(webtools.ServiceOptions{Settings: &memSettings{},
		Credentials: &memCredentials{key: vendorKey}})
	official := service.State(nil)
	if official.Provider != webtools.ProviderOfficial || official.PendingRestart {
		t.Fatalf("official %+v", official)
	}
	if !strings.Contains(official.Message, "官方服务") {
		t.Fatalf("official message %q", official.Message)
	}
	if err := service.Commit(webtools.ProviderTavily); err != nil {
		t.Fatalf("commit: %v", err)
	}
	tavily := service.State(map[string]any{"services": map[string]any{
		"search": map[string]any{"status": "connected"},
		"fetch":  map[string]any{"status": "connected"}}})
	if !tavily.PendingRestart || !tavily.KeySet {
		t.Fatalf("tavily %+v", tavily)
	}
	if !strings.Contains(tavily.Message, "connected") {
		t.Fatalf("the native status was not reported: %q", tavily.Message)
	}
}

func TestProbeSkipsEveryProviderButTavily(t *testing.T) {
	service := webtools.NewService(webtools.ServiceOptions{Credentials: &memCredentials{key: vendorKey}})
	official := service.Probe(webtools.ProviderOfficial, "")
	if official["search"].(map[string]any)["state"] != "skipped" {
		t.Fatalf("official probe %v", official)
	}
	native := service.Probe(webtools.ProviderNative, "")
	if native["search"].(map[string]any)["state"] != "skipped" {
		t.Fatalf("native probe %v", native)
	}
}

func TestProbeRecordsTheLastResult(t *testing.T) {
	fake := newVendor(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"query"`) {
			_, _ = w.Write([]byte(`{"results":[{"title":"a","url":"https://a.test/","content":"b"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"url":"https://example.com/","raw_content":"page"}]}`))
	})
	service := webtools.NewService(webtools.ServiceOptions{Credentials: &memCredentials{key: vendorKey},
		SearchEndpoint: fake.server.URL, FetchEndpoint: fake.server.URL})
	answer := service.Probe(webtools.ProviderTavily, "")
	if answer["search"].(map[string]any)["state"] != "passed" ||
		answer["fetch"].(map[string]any)["state"] != "passed" {
		t.Fatalf("probe %v", answer)
	}
	state := service.State(nil)
	if state.LastResult == nil || !state.LastResult.OK {
		t.Fatalf("last result %+v", state.LastResult)
	}
}

func TestProbeWithoutAKeyIsAFailureNotAPanic(t *testing.T) {
	service := webtools.NewService(webtools.ServiceOptions{})
	answer := service.Probe(webtools.ProviderTavily, "")
	if answer["code"] != "web_key_missing" {
		t.Fatalf("answer %v", answer)
	}
}

func TestProbeReportsExactStepRegardlessOfErrorCode(t *testing.T) {
	t.Run("search_invalid_response_stays_in_search", func(t *testing.T) {
		badSearch := newVendor(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"not_results":true}`))
		})
		res := webtools.ProbeService(context.Background(), options(badSearch.server.URL), "", "")
		if res.OK || res.Step != "search" || res.Code != "web_adapter_invalid_response" {
			t.Fatalf("ProbeService = %+v, want step=search code=web_adapter_invalid_response", res)
		}
		service := webtools.NewService(webtools.ServiceOptions{
			Credentials:    &memCredentials{key: vendorKey},
			SearchEndpoint: badSearch.server.URL,
			FetchEndpoint:  badSearch.server.URL,
		})
		answer := service.Probe(webtools.ProviderTavily, "")
		if answer["search"].(map[string]any)["state"] != "failed" ||
			answer["fetch"].(map[string]any)["state"] != "not-run" {
			t.Fatalf("service probe answer = %+v", answer)
		}
	})
	t.Run("fetch_upstream_error_stays_in_read", func(t *testing.T) {
		read503 := newVendor(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(string(body), `"query"`) {
				_, _ = w.Write([]byte(`{"results":[{"title":"a","url":"https://a.test/","content":"b"}]}`))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		res := webtools.ProbeService(context.Background(), options(read503.server.URL), "", "")
		if res.OK || res.Step != "read" || res.Code != "web_adapter_upstream_error" || res.Results != 1 {
			t.Fatalf("ProbeService = %+v, want step=read code=web_adapter_upstream_error results=1", res)
		}
		service := webtools.NewService(webtools.ServiceOptions{
			Credentials:    &memCredentials{key: vendorKey},
			SearchEndpoint: read503.server.URL,
			FetchEndpoint:  read503.server.URL,
		})
		answer := service.Probe(webtools.ProviderTavily, "")
		if answer["search"].(map[string]any)["state"] != "passed" ||
			answer["fetch"].(map[string]any)["state"] != "failed" {
			t.Fatalf("service probe answer = %+v", answer)
		}
	})
}
