package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/ledger"
)

func postJSON(t *testing.T, s *Service, method, path string, body any, auth, origin string) (int, core.Document) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.Endpoint.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	doc := core.Document{}
	_ = json.Unmarshal(raw, &doc)
	return resp.StatusCode, doc
}

func TestPostOpenURLValidationAndExecution(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var opened []string
	var openErr error
	s := &Service{
		Store:    store,
		StateDir: dir,
		OpenURL: func(target string) error {
			if openErr != nil {
				return openErr
			}
			opened = append(opened, target)
			return nil
		},
	}
	if err := s.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	code, doc := postJSON(t, s, http.MethodPost, "/api/open-url", core.Document{"url": "https://github.com/wssz527/kimiwork-plus"}, s.Endpoint.Key, s.Endpoint.URL)
	if code != http.StatusOK || doc["ok"] != true || doc["url"] != "https://github.com/wssz527/kimiwork-plus" {
		t.Fatalf("valid https open-url: status=%d doc=%+v", code, doc)
	}
	if len(opened) != 1 || opened[0] != "https://github.com/wssz527/kimiwork-plus" {
		t.Fatalf("opened=%v", opened)
	}

	invalidPayloads := []core.Document{
		{},
		{"url": ""},
		{"url": 123},
		{"url": "http://example.com"},
		{"url": "javascript:alert(1)"},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com/\nbad"},
		{"url": "https://example.com/with space"},
		{"url": "https://user:pass@example.com"},
	}
	for _, payload := range invalidPayloads {
		code, doc = postJSON(t, s, http.MethodPost, "/api/open-url", payload, s.Endpoint.Key, s.Endpoint.URL)
		errCode := core.String(core.Object(doc["error"])["code"])
		if code != http.StatusBadRequest || errCode != "invalid_external_url" {
			t.Fatalf("payload %+v: status=%d doc=%+v, want 400 invalid_external_url", payload, code, doc)
		}
	}
	if len(opened) != 1 {
		t.Fatalf("invalid URLs invoked opener: %v", opened)
	}

	openErr = errors.New("boom")
	code, doc = postJSON(t, s, http.MethodPost, "/api/open-url", core.Document{"url": "https://example.com"}, s.Endpoint.Key, s.Endpoint.URL)
	if code != http.StatusServiceUnavailable || core.String(core.Object(doc["error"])["code"]) != "open_url_failed" {
		t.Fatalf("opener failure: status=%d doc=%+v", code, doc)
	}
}

func TestPostDiagnosticsExportWritesFileAndFallsBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	exportDir := t.TempDir()
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	report := core.Document{
		"schemaVersion": diagnosticsSchemaVersion,
		"summary":       core.Document{"version": Version},
		"events":        []any{},
	}
	s := &Service{
		Store:     store,
		StateDir:  dir,
		ExportDir: exportDir,
		Extra: func(_ context.Context, path string, _ core.Document) (any, error) {
			if path != "/api/diagnostics/export" {
				return nil, errors.New("unexpected_path")
			}
			return report, nil
		},
	}
	if err := s.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	code, getDoc := postJSON(t, s, http.MethodGet, "/api/diagnostics/export", nil, s.Endpoint.Key, "")
	if code != http.StatusOK || getDoc["schemaVersion"] != float64(diagnosticsSchemaVersion) {
		t.Fatalf("GET diagnostics export: status=%d doc=%+v", code, getDoc)
	}

	code, postDoc := postJSON(t, s, http.MethodPost, "/api/diagnostics/export", core.Document{}, s.Endpoint.Key, s.Endpoint.URL)
	if code != http.StatusOK || postDoc["ok"] != true {
		t.Fatalf("POST diagnostics export: status=%d doc=%+v", code, postDoc)
	}
	savedPath := core.String(postDoc["path"])
	if filepath.Dir(savedPath) != exportDir {
		t.Fatalf("savedPath=%q, want under %q", savedPath, exportDir)
	}
	namePattern := regexp.MustCompile(`^kimi-work-plus-diagnostics-\d{8}-\d{6}\.json$`)
	if !namePattern.MatchString(filepath.Base(savedPath)) {
		t.Fatalf("unexpected filename: %q", filepath.Base(savedPath))
	}
	if core.Object(postDoc["report"])["schemaVersion"] != float64(diagnosticsSchemaVersion) {
		t.Fatalf("missing report in response: %+v", postDoc)
	}
	raw, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) || !bytes.Contains(raw, []byte("\n  ")) {
		t.Fatalf("expected indented JSON ending with newline: %q", string(raw))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(savedPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file perm=%o, want 0600", perm)
		}
	}

	// Missing primary export dir falls back to filepath.Join(StateDir, "exports").
	s.ExportDir = filepath.Join(t.TempDir(), "missing-downloads")
	code, fallbackDoc := postJSON(t, s, http.MethodPost, "/api/diagnostics/export", core.Document{}, s.Endpoint.Key, s.Endpoint.URL)
	if code != http.StatusOK || fallbackDoc["ok"] != true {
		t.Fatalf("fallback POST diagnostics export: status=%d doc=%+v", code, fallbackDoc)
	}
	fallbackPath := core.String(fallbackDoc["path"])
	wantFallbackDir := filepath.Join(dir, "exports")
	if filepath.Dir(fallbackPath) != wantFallbackDir {
		t.Fatalf("fallbackPath=%q, want under %q", fallbackPath, wantFallbackDir)
	}
	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(wantFallbackDir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Fatalf("fallback dir perm=%o, want 0700", perm)
		}
	}
}

func TestPostUsageExportSyncsFiltersAndFallsBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	kimiHome := filepath.Join(t.TempDir(), "kimi-home")
	exportDir := t.TempDir()
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := &countingKeys{values: map[string]string{}}
	rt, err := NewRuntime(dir, store, keys)
	if err != nil {
		t.Fatal(err)
	}
	nowMS := time.Now().UTC().UnixMilli()
	collector := newWorkUsageCollector(dir, kimiHome, rt.usage, store, rt.budget)
	collector.now = func() int64 { return nowMS }

	wireFile := filepath.Join(kimiHome, "runtime", "kimi-code", "home", "sessions", "wd_1", "conv-1", "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wireFile), 0o700); err != nil {
		t.Fatal(err)
	}
	alphaLine := `{"type":"usage.record","agentId":"main","model":"alpha-model","usage":{"inputOther":100,"output":25,"inputCacheRead":0,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(nowMS-5000) + `}` + "\n"
	betaLine := `{"type":"usage.record","agentId":"main","model":"beta-model","usage":{"inputOther":200,"output":50,"inputCacheRead":0,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(nowMS-4000) + `}` + "\n"
	if err := os.WriteFile(wireFile, []byte(alphaLine+betaLine), 0o600); err != nil {
		t.Fatal(err)
	}

	syncCalls := 0
	s := &Service{
		Store:     store,
		Keys:      keys,
		StateDir:  dir,
		ExportDir: exportDir,
		Gateway:   rt,
		Extra: func(ctx context.Context, path string, in core.Document) (any, error) {
			if path == "/api/usage/export" {
				syncCalls++
				collector.Sync()
			}
			return rt.Action(ctx, path, in)
		},
	}
	if err := s.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	// GET remains 100% compatible.
	code, getDoc := postJSON(t, s, http.MethodGet, "/api/usage/export", nil, s.Endpoint.Key, "")
	if code != http.StatusOK || core.Object(getDoc["summary"])["totalTokens"] != float64(375) {
		t.Fatalf("GET usage export: status=%d doc=%+v", code, getDoc)
	}

	// POST with empty object exports full synced usage JSON.
	code, postDoc := postJSON(t, s, http.MethodPost, "/api/usage/export", core.Document{}, s.Endpoint.Key, s.Endpoint.URL)
	if code != http.StatusOK || postDoc["ok"] != true {
		t.Fatalf("POST usage export: status=%d doc=%+v", code, postDoc)
	}
	if _, hasReport := postDoc["report"]; hasReport {
		t.Fatalf("POST usage export should only return ok and path: %+v", postDoc)
	}
	if syncCalls < 2 {
		t.Fatalf("expected Sync to run on GET and POST, got %d", syncCalls)
	}
	savedPath := core.String(postDoc["path"])
	if filepath.Dir(savedPath) != exportDir {
		t.Fatalf("savedPath=%q, want under %q", savedPath, exportDir)
	}
	namePattern := regexp.MustCompile(`^kimi-work-plus-usage-\d{8}-\d{6}\.json$`)
	if !namePattern.MatchString(filepath.Base(savedPath)) {
		t.Fatalf("unexpected usage filename: %q", filepath.Base(savedPath))
	}
	raw, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) || !bytes.Contains(raw, []byte("\n  ")) {
		t.Fatalf("expected indented usage JSON ending with newline")
	}
	var exported map[string]any
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	if core.Object(exported["summary"])["totalTokens"] != float64(375) {
		t.Fatalf("exported summary=%+v, want 375 totalTokens", exported["summary"])
	}

	// POST with valid ledger.Query filters only matching model.
	queryBody := core.Document{
		"preset":   "today",
		"timeZone": "UTC",
		"filters":  core.Document{"modelId": "alpha-model"},
	}
	code, filteredDoc := postJSON(t, s, http.MethodPost, "/api/usage/export", queryBody, s.Endpoint.Key, s.Endpoint.URL)
	if code != http.StatusOK || filteredDoc["ok"] != true {
		t.Fatalf("filtered POST usage export: status=%d doc=%+v", code, filteredDoc)
	}
	rawFiltered, err := os.ReadFile(core.String(filteredDoc["path"]))
	if err != nil {
		t.Fatal(err)
	}
	var exportedFiltered map[string]any
	if err := json.Unmarshal(rawFiltered, &exportedFiltered); err != nil {
		t.Fatal(err)
	}
	if core.Object(exportedFiltered["summary"])["totalTokens"] != float64(125) {
		t.Fatalf("filtered summary=%+v, want 125 totalTokens", exportedFiltered["summary"])
	}

	// POST with malformed query field type falls back to default ledger.Query{}.
	code, badTypeDoc := postJSON(t, s, http.MethodPost, "/api/usage/export", core.Document{"preset": 999}, s.Endpoint.Key, s.Endpoint.URL)
	if code != http.StatusOK || badTypeDoc["ok"] != true {
		t.Fatalf("bad query type fallback POST usage export: status=%d doc=%+v", code, badTypeDoc)
	}

	// Fallback to StateDir/exports when ExportDir is absent.
	s.ExportDir = filepath.Join(t.TempDir(), "missing-downloads")
	code, fallbackDoc := postJSON(t, s, http.MethodPost, "/api/usage/export", core.Document{}, s.Endpoint.Key, s.Endpoint.URL)
	if code != http.StatusOK || fallbackDoc["ok"] != true {
		t.Fatalf("fallback POST usage export: status=%d doc=%+v", code, fallbackDoc)
	}
	if filepath.Dir(core.String(fallbackDoc["path"])) != filepath.Join(dir, "exports") {
		t.Fatalf("fallback path=%q", fallbackDoc["path"])
	}
}

func TestWorkUsageExportQueryIgnoresInjectedNow(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(dir, store, &countingKeys{values: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := rt.Action(context.Background(), "/api/usage/export", core.Document{"preset": "7d"})
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := out.(json.RawMessage)
	if !ok || len(raw) == 0 {
		t.Fatalf("unexpected export output: %T", out)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["schemaVersion"] != float64(ledger.SchemaVersion) {
		t.Fatalf("unexpected schemaVersion: %v", parsed["schemaVersion"])
	}
}
