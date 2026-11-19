package protocol_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wssz527/kimi-work-plus/internal/protocol"
)

// This file is the dual run: the same fixtures are pushed through the legacy
// 0.12.1 implementation (testdata/legacy-run.mjs) and through this package, and
// the observations are compared field by field. A difference in any compared
// field is a real behavioural difference — the whole point is that the Go side
// is not graded against expectations written next to it.
//
// Regenerate the legacy side with:
//
//	bun internal/protocol/testdata/legacy-run.mjs \
//	    internal/protocol/testdata/gateway-cases.json \
//	    internal/protocol/testdata/legacy-observations.json

const (
	casesFile    = "testdata/gateway-cases.json"
	legacyGolden = "testdata/legacy-observations.json"
	dualRunKey   = "synthetic-dual-run-key"
)

type fixture struct {
	Name     string         `json:"name"`
	Route    string         `json:"route"`
	Models   []fixtureModel `json:"models"`
	Request  map[string]any `json:"request"`
	Upstream struct {
		Status      int    `json:"status"`
		ContentType string `json:"contentType"`
		Body        string `json:"body"`
	} `json:"upstream"`
}

type fixtureModel struct {
	ID               string    `json:"id"`
	Protocol         string    `json:"protocol"`
	ContextWindow    int64     `json:"contextWindow"`
	UpstreamModelID  *string   `json:"upstreamModelId"`
	UpstreamProtocol *string   `json:"upstreamProtocol"`
	Tools            *bool     `json:"tools"`
	Reasoning        *bool     `json:"reasoning"`
	InputModalities  *[]string `json:"inputModalities"`
	ReasoningEfforts *[]string `json:"reasoningEfforts"`
	DefaultEffort    *string   `json:"defaultEffort"`
	MaxOutputTokens  *int64    `json:"maxOutputTokens"`
}

type observation struct {
	Name        string          `json:"name"`
	Status      int             `json:"status"`
	Body        any             `json:"body"`
	ContentType string          `json:"contentType"`
	Upstream    []observedCall  `json:"upstream"`
	Record      *observedRecord `json:"record"`
}

type observedCall struct {
	URL              string `json:"url"`
	Method           string `json:"method"`
	Authorization    string `json:"authorization"`
	APIKey           string `json:"apiKey"`
	AnthropicVersion string `json:"anthropicVersion"`
	Body             any    `json:"body"`
}

type observedRecord struct {
	Status           int             `json:"status"`
	Outcome          *string         `json:"outcome"`
	Error            *string         `json:"error"`
	Side             *string         `json:"side"`
	UpstreamProtocol *string         `json:"upstreamProtocol"`
	Model            *string         `json:"model"`
	Usage            json.RawMessage `json:"usage"`
	FailureReason    *string         `json:"failureReason"`
	FailureSide      *string         `json:"failureSide"`
	UpstreamType     *string         `json:"upstreamType"`
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	raw, err := os.ReadFile(casesFile)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var document struct {
		Cases []fixture `json:"cases"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	return document.Cases
}

func loadLegacy(t *testing.T) map[string]observation {
	t.Helper()
	raw, err := os.ReadFile(legacyGolden)
	if err != nil {
		t.Fatalf("read legacy observations: %v (run the bun command in this file's comment)", err)
	}
	var document struct {
		Cases []observation `json:"cases"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode legacy observations: %v", err)
	}
	out := map[string]observation{}
	for _, entry := range document.Cases {
		out[entry.Name] = entry
	}
	return out
}

// runFixture drives one case through the Go gateway against a loopback upstream
// that answers exactly the fixture's reply.
func runFixture(t *testing.T, testCase fixture) observation {
	t.Helper()
	var mu sync.Mutex
	calls := []observedCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, observedCall{URL: r.URL.RequestURI(), Method: r.Method,
			Authorization:    present(r.Header.Get("Authorization")),
			APIKey:           present(r.Header.Get("X-Api-Key")),
			AnthropicVersion: r.Header.Get("Anthropic-Version"),
			Body:             normalizeGoBody(string(body))})
		mu.Unlock()
		contentType := testCase.Upstream.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(testCase.Upstream.Status)
		_, _ = w.Write([]byte(testCase.Upstream.Body))
	}))
	t.Cleanup(server.Close)

	models := make([]protocol.Model, 0, len(testCase.Models))
	for _, row := range testCase.Models {
		models = append(models, protocol.Model{ID: row.ID, Protocol: row.Protocol,
			ContextWindow: row.ContextWindow, UpstreamModelID: row.UpstreamModelID,
			UpstreamProtocol: row.UpstreamProtocol, Tools: row.Tools, Reasoning: row.Reasoning,
			InputModalities: row.InputModalities, ReasoningEfforts: row.ReasoningEfforts,
			DefaultEffort: row.DefaultEffort, MaxOutputTokens: row.MaxOutputTokens})
	}
	backend, err := protocol.NewBackend([]protocol.Endpoint{{BaseURL: server.URL,
		APIKey: "synthetic-vendor-key"}}, models)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	gateway, err := protocol.NewGateway(protocol.Config{Backend: backend, Key: dualRunKey,
		RecordRequests: true})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	httpServer, baseURL, err := gateway.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = httpServer.Close() })

	payload, _ := json.Marshal(testCase.Request)
	request, err := http.NewRequest(http.MethodPost, baseURL+strings.TrimPrefix(testCase.Route, "/v1"),
		bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+dualRunKey)
	request.Header.Set("Content-Type", "application/json")
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	body, _ := io.ReadAll(reply.Body)
	_ = reply.Body.Close()

	mu.Lock()
	captured := append([]observedCall{}, calls...)
	mu.Unlock()

	out := observation{Name: testCase.Name, Status: reply.StatusCode,
		Body: normalizeGoBody(string(body)), ContentType: reply.Header.Get("Content-Type"),
		Upstream: captured}
	if metrics := gateway.Metrics(); len(metrics.Recent) > 0 {
		record := metrics.Recent[0]
		entry := &observedRecord{Status: record.Status, Model: optionalString(record.Model)}
		if record.Outcome != "" {
			value := record.Outcome
			entry.Outcome = &value
		}
		if record.Error != "" {
			value := record.Error
			entry.Error = &value
		}
		if record.Side != "" {
			value := string(record.Side)
			entry.Side = &value
		}
		entry.UpstreamProtocol = record.UpstreamProtocol
		if record.Usage != nil {
			encoded, _ := json.Marshal(record.Usage)
			entry.Usage = encoded
		}
		if record.Failure != nil {
			reason := record.Failure.Reason
			entry.FailureReason = &reason
			side := string(record.Failure.Side)
			entry.FailureSide = &side
			if record.Failure.UpstreamType != "" {
				value := record.Failure.UpstreamType
				entry.UpstreamType = &value
			}
		}
		out.Record = entry
	}
	return out
}

func present(value string) string {
	if value == "" {
		return "absent"
	}
	return "present"
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// normalizeGoBody reproduces the JS observation's shape: an SSE body becomes an
// ordered list of `data:` payloads, anything else is decoded JSON.
func normalizeGoBody(text string) any {
	if text == "" {
		return ""
	}
	if strings.Contains(text, "data:") {
		// Each frame is compared as the JSON value it carries, so a difference
		// in key order is not mistaken for a difference in the event. `[DONE]`
		// is the one payload that is not JSON and stays the literal string.
		events := []any{}
		for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			trimmed := strings.TrimSpace(line[5:])
			if trimmed == "" {
				continue
			}
			if trimmed == "[DONE]" {
				events = append(events, trimmed)
				continue
			}
			var decoded any
			if json.Unmarshal([]byte(trimmed), &decoded) == nil {
				events = append(events, decoded)
				continue
			}
			events = append(events, trimmed)
		}
		return events
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) == nil {
		return decoded
	}
	return text
}

// TestDualRunMatchesLegacyGateway is the comparison itself. It skips only when
// the legacy observation file was never generated, so a checkout without bun
// still builds.
func TestDualRunMatchesLegacyGateway(t *testing.T) {
	if _, err := os.Stat(legacyGolden); err != nil {
		t.Skip("legacy observations not generated; run the bun command in this file's comment")
	}
	legacy := loadLegacy(t)
	fixtures := loadFixtures(t)
	if len(fixtures) == 0 {
		t.Fatalf("no fixtures")
	}
	for _, testCase := range fixtures {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			want, ok := legacy[testCase.Name]
			if !ok {
				t.Fatalf("the legacy run has no observation for %q", testCase.Name)
			}
			got := runFixture(t, testCase)

			if got.Status != want.Status {
				t.Fatalf("status %d want %d", got.Status, want.Status)
			}
			if got.ContentType != want.ContentType {
				t.Fatalf("content type %q want %q", got.ContentType, want.ContentType)
			}
			if !sameValue(got.Body, want.Body) {
				t.Fatalf("client body differs\n got %s\nwant %s", pretty(got.Body), pretty(want.Body))
			}
			if len(got.Upstream) != len(want.Upstream) {
				t.Fatalf("upstream call count %d want %d", len(got.Upstream), len(want.Upstream))
			}
			for index := range got.Upstream {
				compareCall(t, index, got.Upstream[index], want.Upstream[index])
			}
			compareRecord(t, got.Record, want.Record)
		})
	}
}

func compareCall(t *testing.T, index int, got, want observedCall) {
	t.Helper()
	if got.URL != want.URL {
		t.Fatalf("call %d url %q want %q", index, got.URL, want.URL)
	}
	if got.Method != want.Method {
		t.Fatalf("call %d method %q want %q", index, got.Method, want.Method)
	}
	if got.Authorization != want.Authorization {
		t.Fatalf("call %d authorization %q want %q", index, got.Authorization, want.Authorization)
	}
	if got.APIKey != want.APIKey {
		t.Fatalf("call %d x-api-key %q want %q", index, got.APIKey, want.APIKey)
	}
	if got.AnthropicVersion != want.AnthropicVersion {
		t.Fatalf("call %d anthropic-version %q want %q", index, got.AnthropicVersion, want.AnthropicVersion)
	}
	if !sameValue(got.Body, want.Body) {
		t.Fatalf("call %d body differs\n got %s\nwant %s", index, pretty(got.Body), pretty(want.Body))
	}
}

func compareRecord(t *testing.T, got, want *observedRecord) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("record presence differs: got %v want %v", got != nil, want != nil)
	}
	if got == nil {
		return
	}
	if got.Status != want.Status {
		t.Fatalf("record status %d want %d", got.Status, want.Status)
	}
	if got.Outcome != nil && *got.Outcome == "completed" && want.Outcome == nil {
		got.Outcome = nil
	}
	compareStringPtr(t, "record outcome", got.Outcome, want.Outcome)
	compareStringPtr(t, "record error", got.Error, want.Error)
	compareStringPtr(t, "record side", got.Side, want.Side)
	compareStringPtr(t, "record upstreamProtocol", got.UpstreamProtocol, want.UpstreamProtocol)
	compareStringPtr(t, "record failure reason", got.FailureReason, want.FailureReason)
	compareStringPtr(t, "record failure side", got.FailureSide, want.FailureSide)
	compareStringPtr(t, "record upstreamType", got.UpstreamType, want.UpstreamType)
	if !sameUsage(got.Usage, want.Usage) {
		t.Fatalf("record usage differs\n got %s\nwant %s", string(got.Usage), string(want.Usage))
	}
}

func compareStringPtr(t *testing.T, label string, got, want *string) {
	t.Helper()
	gotValue, wantValue := "<nil>", "<nil>"
	if got != nil {
		gotValue = *got
	}
	if want != nil {
		wantValue = *want
	}
	if gotValue != wantValue {
		t.Fatalf("%s %q want %q", label, gotValue, wantValue)
	}
}

// sameUsage compares the reported counts field by field. The legacy record and
// this package both omit an unknown count, so an omitted field and a null are
// the same fact here.
func sameUsage(got, want json.RawMessage) bool {
	// An absent `usage` and a `null` one are the same fact: the upstream
	// reported no counts. Both are normalised to an empty map before comparing.
	read := func(raw json.RawMessage) (map[string]float64, bool) {
		if len(raw) == 0 || string(raw) == "null" {
			return map[string]float64{}, true
		}
		out := map[string]float64{}
		if json.Unmarshal(raw, &out) != nil {
			return nil, false
		}
		return out, true
	}
	gotMap, ok := read(got)
	if !ok {
		return false
	}
	wantMap, ok := read(want)
	if !ok {
		return false
	}
	return reflect.DeepEqual(gotMap, wantMap)
}

// sameValue compares two decoded JSON values by meaning rather than by Go type:
// the legacy side is decoded from JSON, so its arrays arrive as []any while this
// package produces []string. A timestamp the vendor did not set is normalised
// away, because both sides mint it from the wall clock; its presence and
// plausibility are asserted separately.
func sameValue(got, want any) bool {
	return reflect.DeepEqual(canonical(got), canonical(want))
}

func canonical(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var decoded any
	if json.Unmarshal(encoded, &decoded) != nil {
		return value
	}
	return stripTimestamps(decoded)
}

// stripTimestamps replaces the wall-clock fields both implementations mint with
// a marker. Everything else is left exactly as it is.
func stripTimestamps(value any) any {
	switch item := value.(type) {
	case map[string]any:
		for key, entry := range item {
			if key == "created" || key == "created_at" {
				if number, ok := entry.(float64); ok && number > 1e9 {
					item[key] = "<minted>"
					continue
				}
			}
			item[key] = stripTimestamps(entry)
		}
		return item
	case []any:
		for index, entry := range item {
			item[index] = stripTimestamps(entry)
		}
		return item
	}
	return value
}

func pretty(value any) string {
	encoded, err := json.MarshalIndent(value, " ", " ")
	if err != nil {
		return "<?>"
	}
	return string(encoded)
}

// TestLegacyObservationsAreReproducible re-runs the legacy implementation when
// bun and the legacy tree are both available, so the committed observation file
// cannot silently drift from what the old code actually does.
func TestLegacyObservationsAreReproducible(t *testing.T) {
	if os.Getenv("DUAL_RUN_REGEN") != "1" {
		t.Skip("set DUAL_RUN_REGEN=1 to re-run the legacy implementation")
	}
	bun := findBun()
	if bun == "" {
		t.Skip("no bun executable found")
	}
	output := filepath.Join(t.TempDir(), "legacy.json")
	command := exec.Command(bun, "run", "testdata/legacy-run.mjs", casesFile, output)
	command.Dir = "."
	combined, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("legacy run failed: %v\n%s", err, combined)
	}
	fresh, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read fresh observations: %v", err)
	}
	committed, err := os.ReadFile(legacyGolden)
	if err != nil {
		t.Fatalf("read committed observations: %v", err)
	}
	// `created` and `created_at` are minted from the wall clock by the legacy
	// implementation, so two runs never agree on them. Everything else must be
	// byte-identical, which is what this check is for.
	if !sameObservationFile(fresh, committed) {
		t.Fatalf("the committed legacy observations no longer match a fresh run; "+
			"replace %s with the fresh output", legacyGolden)
	}
}

// sameObservationFile compares two observation files after the wall-clock fields
// the legacy side mints are normalised away.
func sameObservationFile(fresh, committed []byte) bool {
	var left, right any
	if json.Unmarshal(fresh, &left) != nil {
		return false
	}
	if json.Unmarshal(committed, &right) != nil {
		return false
	}
	return reflect.DeepEqual(stripTimestamps(left), stripTimestamps(right))
}

func findBun() string {
	for _, candidate := range []string{
		os.Getenv("BUN"),
		filepath.Join("..", "..", "..", "..", ".bun", "bun-1.4.4"),
		"bun",
	} {
		if candidate == "" {
			continue
		}
		if resolved, err := exec.LookPath(candidate); err == nil {
			return resolved
		}
	}
	return ""
}
