package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/ledger"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

type testKeys map[string]string

func (k testKeys) Get(name string) (string, error) { return k[name], nil }
func (k testKeys) Set(name, value string) error    { k[name] = value; return nil }
func (k testKeys) Delete(name string) error        { delete(k, name); return nil }

func TestControlGatewayLedgerRestart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Error("upstream key missing")
		}
		var d map[string]any
		if json.NewDecoder(r.Body).Decode(&d) != nil {
			t.Error("bad upstream body")
		}
		w.Header().Set("Content-Type", "application/json")
		usage := `"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":20}`
		if d["model"] == "beta" {
			usage = `"prompt_tokens":80,"completion_tokens":20,"total_tokens":100,"prompt_tokens_details":{"cached_tokens":10}`
		}
		if d["model"] == "unknown" {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{%s}}`, usage)
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "state")
	store, e := core.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	keys := CacheKeys(testKeys{})
	runtime, e := NewRuntime(dir, store, keys)
	if e != nil {
		t.Fatal(e)
	}
	s := &Service{Store: store, Keys: keys, StateDir: dir, Gateway: runtime, Extra: runtime.Action, OnConfig: runtime.Apply}
	if e = s.Start("0"); e != nil {
		t.Fatal(e)
	}
	call := func(method, path string, body any, key string, panel bool) (int, core.Document) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, s.Endpoint.URL+path, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		if panel {
			req.Header.Set("Origin", s.Endpoint.URL)
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		var d core.Document
		if json.Unmarshal(b, &d) != nil {
			t.Fatalf("invalid JSON %s", b)
		}
		return res.StatusCode, d
	}
	_, rev, _ := store.Read()
	code, result := call("POST", "/api/connections", core.Document{"revision": rev, "connections": []any{core.Document{"id": "mock", "name": "Local fixture", "baseURL": upstream.URL, "apiKey": "synthetic-key", "protocols": []any{"chat"}}}}, s.Endpoint.Key, true)
	if code != 200 {
		t.Fatal(code, result)
	}
	_, rev, _ = store.Read()
	models := []any{}
	for _, id := range []string{"alpha", "beta", "unknown"} {
		models = append(models, core.Document{"id": "kimi-" + id, "protocol": "chat", "contextWindow": 32000, "connectionId": "mock", "upstreamModelId": id})
	}
	code, result = call("POST", "/api/models", core.Document{"revision": rev, "models": models}, s.Endpoint.Key, true)
	if code != 200 {
		t.Fatal(code, result)
	}
	for _, id := range []string{"alpha", "beta", "unknown"} {
		body := core.Document{"model": "kimi-" + id, "messages": []any{core.Document{"role": "user", "content": "synthetic"}}}
		if id == "alpha" {
			body["prompt_cache_key"] = "conv-runtime"
		}
		code, result = call("POST", "/v1/chat/completions", body, runtime.Token(), false)
		if code != 200 {
			t.Fatal(code, result)
		}
	}
	if code, _ = call("GET", "/v1/models", nil, s.Endpoint.Key, false); code != 401 {
		t.Fatal("panel token authorized inference", code)
	}
	code, result = call("POST", "/api/usage/query", core.Document{"preset": "today", "timeZone": "Asia/Shanghai"}, s.Endpoint.Key, true)
	if code != 200 {
		t.Fatal(code, result)
	}
	summary := core.Object(result["summary"])
	for key, want := range map[string]float64{"requests": 3, "requestsWithUsage": 2, "totalTokens": 250, "inputTokens": 200, "outputTokens": 50, "cacheReadTokens": 30} {
		if summary[key] != want {
			t.Fatalf("%s: got %v want %v", key, summary[key], want)
		}
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "synthetic-key") {
		t.Fatal("key leaked")
	}
	records := core.Rows(result["records"])
	if len(records) != 3 {
		t.Fatalf("records = %d", len(records))
	}
	for _, value := range records {
		record := core.Object(value)
		if record["account"] != ledger.RedactIdentifier("mock") || record["apiKeyId"] != ledger.RedactIdentifier("synthetic-key") || record["account"] == record["apiKeyId"] {
			t.Fatalf("request identity = %#v", record)
		}
		if record["modelAlias"] == "kimi-alpha" && record["conversationId"] != "conv-runtime" {
			t.Fatalf("cache-key conversation = %#v", record["conversationId"])
		}
		if record["modelAlias"] != "kimi-alpha" && record["conversationId"] != nil {
			t.Fatalf("unexpected conversation = %#v", record["conversationId"])
		}
	}
	if code, _ = call("GET", "/api/usage/clear", nil, s.Endpoint.Key, false); code != 404 {
		t.Fatal("GET mutated ledger", code)
	}
	if e = s.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	restarted, e := NewRuntime(dir, store, keys)
	if e != nil {
		t.Fatal(e)
	}
	if restarted.Token() != runtime.Token() {
		t.Fatal("gateway token changed")
	}
	s = &Service{Store: store, Keys: keys, StateDir: dir, Gateway: restarted, Extra: restarted.Action, OnConfig: restarted.Apply}
	if e = s.Start("0"); e != nil {
		t.Fatal(e)
	}
	defer s.Close(context.Background())
	code, result = call("POST", "/api/usage/query", core.Document{"preset": "today", "timeZone": "Asia/Shanghai", "filters": core.Document{"modelId": "kimi-alpha"}}, s.Endpoint.Key, true)
	if code != 200 || core.Object(result["summary"])["totalTokens"] != float64(150) {
		t.Fatal(code, result)
	}
	code, result = call("GET", "/api/usage/export", nil, s.Endpoint.Key, false)
	if code != 200 || core.Object(result["summary"])["totalTokens"] != float64(250) {
		t.Fatal(code, result)
	}
}

// The single-protocol rule must hold through the real save path and the live
// gateway: a connection saved as `responses` receives a Kimi Chat request on
// `/responses`, with the reply bridged back to the Chat shape Kimi parses.
func TestRuntimeServesAChatRequestOnTheConnectionsDeclaredWire(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","model":"vendor-model","status":"completed",`+
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"bridged"}]}]}`)
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{"KIMI_WORK_LOCAL_KEY": "synthetic-key"})
	runtime, err := NewRuntime(dir, store, keys)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, Keys: keys, StateDir: dir, Gateway: runtime, OnConfig: runtime.Apply}
	if err = s.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	_, revision, _ := store.Read()
	if _, err = s.configure("/api/connections", core.Document{"revision": revision, "connections": []any{
		core.Document{"id": "local", "name": "Local", "baseURL": upstream.URL,
			"apiKey": "synthetic-key", "protocols": []any{"responses"}},
	}}); err != nil {
		t.Fatal(err)
	}
	_, revision, _ = store.Read()
	if _, err = s.configure("/api/models", core.Document{"revision": revision, "models": []any{
		core.Document{"id": "kimi-local", "protocol": "chat", "contextWindow": 32000,
			"connectionId": "local", "upstreamModelId": "vendor-model"},
	}}); err != nil {
		t.Fatal(err)
	}

	request, _ := http.NewRequest("POST", s.Endpoint.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"kimi-local","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+runtime.Token())
	request.Header.Set("Content-Type", "application/json")
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	body, _ := io.ReadAll(reply.Body)
	if reply.StatusCode != 200 || !strings.Contains(string(body), "bridged") {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	mu.Lock()
	captured := append([]string(nil), paths...)
	mu.Unlock()
	if len(captured) != 1 || captured[0] != "/responses" {
		t.Fatalf("upstream paths = %v", captured)
	}
}

// A document written before a connection named one protocol may list both. It
// must load, keep working exactly as it did, and never be rewritten in the
// background: the user is asked to choose only when they save that connection.
func TestRuntimeKeepsALegacyMultiProtocolConnection(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/responses" {
			_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","model":"vendor-gpt","status":"completed",`+
				`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"bridged"}]}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"chat-answered"}}]}`)
	}))
	defer upstream.Close()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := core.Document{
		"connections": []any{core.Document{"id": "local", "name": "Local", "baseURL": upstream.URL,
			"apiKeyEnv": "KIMI_WORK_LOCAL_KEY", "protocols": []any{"chat", "responses"}}},
		"models": []any{
			core.Document{"id": "kimi-local", "protocol": "chat", "contextWindow": 32000,
				"connectionId": "local", "upstreamModelId": "vendor-chat"},
			core.Document{"id": "vendor/gpt", "protocol": "responses", "contextWindow": 32000,
				"connectionId": "local", "upstreamModelId": "vendor-gpt"},
		},
	}
	if err = core.AtomicJSON(filepath.Join(dir, "gateway.json"), legacy); err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{"KIMI_WORK_LOCAL_KEY": "synthetic-key"})
	runtime, err := NewRuntime(dir, store, keys)
	if err != nil {
		t.Fatalf("a legacy document did not load: %v", err)
	}
	s := &Service{Store: store, Keys: keys, StateDir: dir, Gateway: runtime, OnConfig: runtime.Apply}
	if err = s.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	call := func(path, body string) (int, string) {
		t.Helper()
		request, _ := http.NewRequest("POST", s.Endpoint.URL+path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+runtime.Token())
		request.Header.Set("Content-Type", "application/json")
		reply, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer reply.Body.Close()
		raw, _ := io.ReadAll(reply.Body)
		return reply.StatusCode, string(raw)
	}
	if status, body := call("/v1/chat/completions",
		`{"model":"kimi-local","messages":[{"role":"user","content":"hello"}]}`); status != 200 || !strings.Contains(body, "chat-answered") {
		t.Fatalf("chat model: %d %s", status, body)
	}
	compatible := `{"model":"` + protocol.KimiCompatibleModelID("vendor/gpt") + `","messages":[{"role":"user","content":"hello"}]}`
	if status, body := call("/v1/chat/completions", compatible); status != 200 || !strings.Contains(body, "bridged") {
		t.Fatalf("responses model through the Chat compatibility route: %d %s", status, body)
	}
	if status, body := call("/v1/responses",
		`{"model":"vendor/gpt","input":"hello"}`); status != 200 || !strings.Contains(body, "bridged") {
		t.Fatalf("responses route: %d %s", status, body)
	}
	mu.Lock()
	captured := append([]string(nil), paths...)
	mu.Unlock()
	want := []string{"/chat/completions", "/responses", "/responses"}
	if len(captured) != len(want) {
		t.Fatalf("upstream paths = %v", captured)
	}
	for index, path := range want {
		if captured[index] != path {
			t.Fatalf("upstream paths = %v, want %v", captured, want)
		}
	}
	stored, _, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(core.Rows(core.Object(core.Rows(stored["connections"])[0])["protocols"])) != 2 {
		t.Fatalf("the legacy declaration was rewritten: %#v", stored["connections"])
	}
}
