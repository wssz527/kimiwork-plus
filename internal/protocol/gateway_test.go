package protocol_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

const gatewayKey = "synthetic-review-key"

// upstream is a fake vendor that records exactly what arrived.
type upstream struct {
	mu       sync.Mutex
	requests []captured
	server   *httptest.Server
	reply    func(w http.ResponseWriter, r *http.Request, body []byte)
}

type captured struct {
	URL     string
	Method  string
	Headers http.Header
	Body    string
}

func newUpstream(t *testing.T, reply func(w http.ResponseWriter, r *http.Request, body []byte)) *upstream {
	t.Helper()
	fake := &upstream{reply: reply}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.requests = append(fake.requests, captured{URL: r.URL.RequestURI(), Method: r.Method,
			Headers: r.Header.Clone(), Body: string(body)})
		fake.mu.Unlock()
		fake.reply(w, r, body)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (u *upstream) captured() []captured {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]captured, len(u.requests))
	copy(out, u.requests)
	return out
}

func chatModel(id string, extra ...func(*protocol.Model)) protocol.Model {
	model := protocol.Model{ID: id, Protocol: protocol.ProtocolChat, ContextWindow: 1000}
	for _, apply := range extra {
		apply(&model)
	}
	return model
}

func responsesModel(id string) protocol.Model {
	return protocol.Model{ID: id, Protocol: protocol.ProtocolResponses, ContextWindow: 1000}
}

// startGateway wires one fake upstream behind a real loopback listener, so every
// assertion below is about bytes that actually crossed a socket.
func startGateway(t *testing.T, base string, models []protocol.Model, config func(*protocol.Config)) (*protocol.Gateway, string) {
	t.Helper()
	backend, err := protocol.NewBackend([]protocol.Endpoint{{BaseURL: base, APIKey: "vendor-key"}}, models)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	cfg := protocol.Config{Backend: backend, Key: gatewayKey, RecordRequests: true}
	if config != nil {
		config(&cfg)
	}
	gateway, err := protocol.NewGateway(cfg)
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	server, url, err := gateway.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return gateway, url
}

func post(t *testing.T, url, path string, payload any, headers map[string]string) (*http.Response, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+gatewayKey)
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	raw, _ := io.ReadAll(reply.Body)
	_ = reply.Body.Close()
	return reply, string(raw)
}

func waitForRecentRecord(t *testing.T, gateway *protocol.Gateway) *protocol.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if recent := gateway.Metrics().Recent; len(recent) > 0 {
			return recent[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("no record was written")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func errorCode(t *testing.T, body string) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return payload.Error.Code
}

// --- routing and refusals --------------------------------------------------

func TestOnlyFourRoutesExist(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)
	cases := []struct {
		method string
		path   string
		status int
		code   string
	}{
		{http.MethodGet, "/models", 200, ""},
		{http.MethodGet, "/models/api.json", 200, ""},
		{http.MethodGet, "/chat/completions", 404, "not_found"},
		{http.MethodPost, "/models", 404, "not_found"},
		{http.MethodGet, "/not-a-route", 404, "not_found"},
	}
	for _, test := range cases {
		request, _ := http.NewRequest(test.method, url+test.path, nil)
		request.Header.Set("Authorization", "Bearer "+gatewayKey)
		reply, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", test.method, test.path, err)
		}
		body, _ := io.ReadAll(reply.Body)
		_ = reply.Body.Close()
		if reply.StatusCode != test.status {
			t.Fatalf("%s %s: status %d want %d", test.method, test.path, reply.StatusCode, test.status)
		}
		if test.code != "" && errorCode(t, string(body)) != test.code {
			t.Fatalf("%s %s: code %q want %q", test.method, test.path, errorCode(t, string(body)), test.code)
		}
	}
}

func TestAuthenticationAndBrowserGuards(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)

	for _, key := range []string{"wrong-key", ""} {
		request, _ := http.NewRequest(http.MethodGet, url+"/models", nil)
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		reply, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		body, _ := io.ReadAll(reply.Body)
		_ = reply.Body.Close()
		if reply.StatusCode != 401 || errorCode(t, string(body)) != "unauthorized" {
			t.Fatalf("key %q: status %d code %s", key, reply.StatusCode, errorCode(t, string(body)))
		}
	}

	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, map[string]string{"Origin": "https://example.com"})
	if reply.StatusCode != 403 || errorCode(t, body) != "browser_request_refused" {
		t.Fatalf("origin: status %d code %s", reply.StatusCode, errorCode(t, body))
	}
	for _, site := range []string{"same-origin", "same-site", "cross-site", "other"} {
		siteReply, siteBody := post(t, url, "/chat/completions",
			map[string]any{"model": "kimi-probe", "messages": []any{}},
			map[string]string{"Sec-Fetch-Site": site})
		if siteReply.StatusCode != 403 || errorCode(t, siteBody) != "browser_request_refused" {
			t.Fatalf("sec-fetch-site %s: status %d", site, siteReply.StatusCode)
		}
	}
	reply, _ = post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}},
		map[string]string{"Sec-Fetch-Site": "none"})
	if reply.StatusCode != 200 {
		t.Fatalf("sec-fetch-site none: status %d", reply.StatusCode)
	}
	if len(fake.captured()) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(fake.captured()))
	}
}

func TestModelRoutingRefusals(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	models := []protocol.Model{chatModel("kimi-probe"), responsesModel("vendor/model")}
	_, url := startGateway(t, fake.server.URL, models, nil)

	reply, body := post(t, url, "/chat/completions", map[string]any{"model": "kimi-missing"}, nil)
	if reply.StatusCode != 409 || errorCode(t, body) != "model_not_enabled_for_kimi" {
		t.Fatalf("unknown model: %d %s", reply.StatusCode, errorCode(t, body))
	}
	reply, body = post(t, url, "/responses", map[string]any{"model": "kimi-probe", "input": "hi"}, nil)
	if reply.StatusCode != 409 || errorCode(t, body) != "model_protocol_mismatch" {
		t.Fatalf("protocol mismatch: %d %s", reply.StatusCode, errorCode(t, body))
	}
	reply, body = post(t, url, "/chat/completions", map[string]any{"model": 42}, nil)
	if reply.StatusCode != 400 || errorCode(t, body) != "invalid_model" {
		t.Fatalf("invalid model: %d %s", reply.StatusCode, errorCode(t, body))
	}
	if len(fake.captured()) != 0 {
		t.Fatalf("upstream saw %d requests, want 0", len(fake.captured()))
	}
}

func TestMalformedJSONIsRefused(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(200)
	})
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)
	request, _ := http.NewRequest(http.MethodPost, url+"/chat/completions", strings.NewReader("{"))
	request.Header.Set("Authorization", "Bearer "+gatewayKey)
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	body, _ := io.ReadAll(reply.Body)
	_ = reply.Body.Close()
	if reply.StatusCode != 400 || errorCode(t, string(body)) != "invalid_json" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, string(body)))
	}
}

// The official client now names a BYOK provider by UUID. Both the fixed legacy
// aliases and a well-formed UUID provider id must resolve to the configured model,
// while a hand-written "byok-custom/" prefix must not be stripped.
func TestModelAliasPrefixesResolveTheConfiguredModel(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	models := []protocol.Model{chatModel("kimi-probe"), responsesModel("vendor/model")}
	_, url := startGateway(t, fake.server.URL, models, nil)
	const uuid = "byok-3d8250d1-cbbc-4f1f-991e-be98b216f34f"
	for _, test := range []struct {
		name      string
		route     string
		model     string
		payload   map[string]any
		status    int
		wantModel string
	}{
		{name: "legacy chat prefix", route: "/chat/completions", model: "byok-kth-kimi/kimi-probe",
			payload: map[string]any{"messages": []any{}}, status: 200, wantModel: "kimi-probe"},
		{name: "legacy responses prefix", route: "/responses", model: "byok-kth-resp/vendor/model",
			payload: map[string]any{"input": "hi"}, status: 200, wantModel: "vendor/model"},
		{name: "uuid chat prefix", route: "/chat/completions", model: uuid + "/kimi-probe",
			payload: map[string]any{"messages": []any{}}, status: 200, wantModel: "kimi-probe"},
		{name: "uppercase uuid responses prefix", route: "/responses",
			model:   "byok-3D8250D1-CBBC-4F1F-991E-BE98B216F34F/vendor/model",
			payload: map[string]any{"input": "hi"}, status: 200, wantModel: "vendor/model"},
		{name: "unknown byok prefix", route: "/chat/completions", model: "byok-custom/kimi-probe",
			payload: map[string]any{"messages": []any{}}, status: 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.payload["model"] = test.model
			before := len(fake.captured())
			reply, body := post(t, url, test.route, test.payload, nil)
			if reply.StatusCode != test.status {
				t.Fatalf("status %d, want %d: %s", reply.StatusCode, test.status, body)
			}
			requests := fake.captured()
			if test.status != 200 {
				if len(requests) != before {
					t.Fatalf("a refused request reached the upstream")
				}
				if code := errorCode(t, body); code != "model_not_enabled_for_kimi" {
					t.Fatalf("error code %q", code)
				}
				return
			}
			if len(requests) != before+1 {
				t.Fatalf("upstream saw %d requests, want %d", len(requests), before+1)
			}
			var sent map[string]any
			if err := json.Unmarshal([]byte(requests[len(requests)-1].Body), &sent); err != nil {
				t.Fatalf("upstream body is not JSON: %v", err)
			}
			if sent["model"] != test.wantModel {
				t.Fatalf("upstream model = %v, want %s", sent["model"], test.wantModel)
			}
		})
	}
}

// --- payload fidelity ------------------------------------------------------

func TestForwardedBodyKeepsEveryByteExceptModel(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-plain-model")}, nil)
	payload := map[string]any{
		"model": "byok-kth-kimi/kimi-plain-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "Keep every field."},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Weather in Paris?"}}},
			map[string]any{"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "weather", "arguments": `{"city":"Paris"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": `{"temperature":20}`},
		},
		"tools":            []any{map[string]any{"type": "function", "function": map[string]any{"name": "weather", "parameters": map[string]any{"type": "object"}}}},
		"tool_choice":      "auto",
		"reasoning_effort": "high",
		"temperature":      0.25,
	}
	reply, body := post(t, url, "/chat/completions", payload, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	requests := fake.captured()
	if len(requests) != 1 {
		t.Fatalf("upstream saw %d requests", len(requests))
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal([]byte(requests[0].Body), &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(sent["model"]) != `"kimi-plain-model"` {
		t.Fatalf("upstream model = %s", sent["model"])
	}
	// Everything else must be byte-identical to what the client sent.
	var original map[string]json.RawMessage
	encoded, _ := json.Marshal(payload)
	_ = json.Unmarshal(encoded, &original)
	for key, value := range original {
		if key == "model" {
			continue
		}
		if string(sent[key]) != string(value) {
			t.Fatalf("field %q changed:\n sent %s\n want %s", key, sent[key], value)
		}
	}
	if requests[0].URL != "/chat/completions" {
		t.Fatalf("upstream path %q", requests[0].URL)
	}
	if requests[0].Headers.Get("Authorization") != "Bearer vendor-key" {
		t.Fatalf("upstream auth %q", requests[0].Headers.Get("Authorization"))
	}
}

func TestUpstreamModelIDIsWhatTheUpstreamSees(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	upstreamID := "deepseek-chat"
	model := chatModel("byok-deepseek")
	model.UpstreamModelID = &upstreamID
	_, url := startGateway(t, fake.server.URL, []protocol.Model{model}, nil)
	post(t, url, "/chat/completions",
		map[string]any{"model": "byok-deepseek", "messages": []any{}}, nil)
	var sent map[string]any
	_ = json.Unmarshal([]byte(fake.captured()[0].Body), &sent)
	if sent["model"] != upstreamID {
		t.Fatalf("upstream model = %v", sent["model"])
	}
}

func TestSessionHeadersAreForwardedAndNothingElse(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)
	post(t, url, "/chat/completions", map[string]any{"model": "kimi-probe", "messages": []any{}},
		map[string]string{"X-Session-Id": "session-1", "R-Timezone": "UTC", "X-Not-Whitelisted": "secret"})
	headers := fake.captured()[0].Headers
	if headers.Get("X-Session-Id") != "session-1" {
		t.Fatalf("session header missing: %v", headers)
	}
	if headers.Get("R-Timezone") != "UTC" {
		t.Fatalf("timezone header missing: %v", headers)
	}
	if headers.Get("X-Not-Whitelisted") != "" {
		t.Fatalf("a non-whitelisted header was forwarded: %v", headers)
	}
	// The local gateway credential must never reach the upstream.
	if strings.Contains(headers.Get("Authorization"), gatewayKey) {
		t.Fatalf("local gateway key leaked upstream")
	}
}

// --- catalogs --------------------------------------------------------------

func TestCatalogShapesAndModelOrder(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(200)
	})
	withImage := []string{"image", "text"}
	efforts := []string{"low", "high"}
	defaultEffort := "high"
	tools := true
	reasoning := true
	output := int64(2048)
	models := []protocol.Model{
		chatModel("kimi-chat-no-slash", func(m *protocol.Model) { m.Tools = &tools }),
		chatModel("vendor/kimi-chat", func(m *protocol.Model) { m.ContextWindow = 2000; m.InputModalities = &withImage }),
		responsesModel("responses-no-slash"),
		responsesModel("vendor/model"),
	}
	models[2].ContextWindow = 3000
	models[2].Reasoning = &reasoning
	models[3].ContextWindow = 4000
	models[3].MaxOutputTokens = &output
	models[1].ReasoningEfforts = &efforts
	models[1].DefaultEffort = &defaultEffort
	_, url := startGateway(t, fake.server.URL, models, nil)

	request, _ := http.NewRequest(http.MethodGet, url+"/models/api.json", nil)
	request.Host = "untrusted.example:12345"
	request.Header.Set("Authorization", "Bearer "+gatewayKey)
	request.Header.Set("Sec-Fetch-Site", "none")
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	raw, _ := io.ReadAll(reply.Body)
	_ = reply.Body.Close()
	if reply.StatusCode != 200 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	// The model order is what Kimi's own menu shows, so it is asserted on the
	// raw bytes rather than through a map that would reorder it.
	if !strings.Contains(string(raw), `"kimi-chat-no-slash"`) {
		t.Fatalf("catalog %s", raw)
	}
	firstChat := strings.Index(string(raw), `"kimi-chat-no-slash"`)
	secondChat := strings.Index(string(raw), `"vendor/kimi-chat"`)
	if firstChat < 0 || secondChat < 0 || firstChat > secondChat {
		t.Fatalf("chat model order lost: %s", raw)
	}
	firstResponses := strings.Index(string(raw), `"responses-no-slash"`)
	secondResponses := strings.Index(string(raw), `"vendor/model"`)
	if firstResponses < 0 || secondResponses < 0 || firstResponses > secondResponses {
		t.Fatalf("responses model order lost: %s", raw)
	}
	var catalog map[string]struct {
		API string `json:"api"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, group := range []string{"kimi", "responses"} {
		if got := catalog[group].API; got != url {
			t.Errorf("catalog group %q api = %q, want listening endpoint %q", group, got, url)
		}
	}

	request, _ = http.NewRequest(http.MethodGet, url+"/models", nil)
	request.Header.Set("Authorization", "Bearer "+gatewayKey)
	reply, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	raw, _ = io.ReadAll(reply.Body)
	_ = reply.Body.Close()
	var list struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Object != "list" || len(list.Data) != 4 {
		t.Fatalf("openai list %s", raw)
	}
	if list.Data[0]["id"] != "kimi-chat-no-slash" || list.Data[0]["supports_tool_call"] != true {
		t.Fatalf("first row %v", list.Data[0])
	}
	if list.Data[1]["supports_image_in"] != true {
		t.Fatalf("image capability lost: %v", list.Data[1])
	}
	if list.Data[3]["context_length"] != float64(4000) {
		t.Fatalf("context length %v", list.Data[3]["context_length"])
	}
}

// --- usage and streaming ---------------------------------------------------

func TestUsageIsScannedFromAJSONReply(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"content":"ok"}}],
			"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"cache_write_tokens":2}}`))
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)
	reply, _ := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	metrics := gateway.Metrics()
	if len(metrics.Recent) != 1 {
		t.Fatalf("recent %d", len(metrics.Recent))
	}
	record := metrics.Recent[0]
	if record.Usage == nil || *record.Usage.InputTokens != 12 || *record.Usage.OutputTokens != 3 ||
		*record.Usage.TotalTokens != 15 || *record.Usage.CacheWriteTokens != 2 {
		t.Fatalf("usage %+v", record.Usage)
	}
	if record.Context == nil || record.Context.UsedTokens != 12 || record.Context.LimitTokens != 1000 {
		t.Fatalf("context %+v", record.Context)
	}
	if record.LedgerStatus() != "completed" {
		t.Fatalf("ledger status %q", record.LedgerStatus())
	}
}

func TestUsageIsScannedFromAnSSEStreamWithoutAlteringIt(t *testing.T) {
	chunks := []string{
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":" +
			"{\"prompt_tokens\":30,\"completion_tokens\":4,\"total_tokens\":34}}\n\n",
		// The terminal usage arriving twice is exactly the shape that used to be
		// counted as two requests.
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":" +
			"{\"prompt_tokens\":30,\"completion_tokens\":4,\"total_tokens\":34}}\n\n",
		"data: [DONE]\n\n",
	}
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			_, _ = w.Write([]byte(chunk))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}, "stream": true}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	// Every forwarded byte is the upstream's own: the scanner reads a copy.
	if body != strings.Join(chunks, "") {
		t.Fatalf("forwarded body changed:\n%q\nwant\n%q", body, strings.Join(chunks, ""))
	}
	record := gateway.Metrics().Recent[0]
	if record.Usage == nil || *record.Usage.InputTokens != 30 || *record.Usage.TotalTokens != 34 {
		t.Fatalf("usage %+v", record.Usage)
	}
	if record.Outcome != "completed" {
		t.Fatalf("outcome %q", record.Outcome)
	}
}

func TestUpstreamErrorKeepsStatusHeadersAndVendorType(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "17")
		w.Header().Set("X-Request-Id", "request-429")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != 429 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	if reply.Header.Get("Retry-After") != "17" || reply.Header.Get("X-Request-Id") != "request-429" {
		t.Fatalf("headers %v", reply.Header)
	}
	if !strings.Contains(body, "rate_limit_error") {
		t.Fatalf("body %s", body)
	}
	record := gateway.Metrics().Recent[0]
	if record.Failure == nil {
		t.Fatalf("no failure classification")
	}
	if record.Failure.Side != protocol.SideUpstream || record.Failure.Reason != "upstream_rate_limited" ||
		record.Failure.UpstreamType != "rate_limit_error" {
		t.Fatalf("failure %+v", record.Failure)
	}
	if record.Usage != nil {
		t.Fatalf("a failed reply must not carry usage: %+v", record.Usage)
	}
}

func TestConnectionFailureIsNotReportedAsAnUpstreamAnswer(t *testing.T) {
	// Port 1 is not listening: the dial itself fails.
	gateway, url := startGateway(t, "http://127.0.0.1:1", []protocol.Model{chatModel("kimi-probe")}, nil)
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != 503 || errorCode(t, body) != "upstream_connection_failed" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, body))
	}
	record := gateway.Metrics().Recent[0]
	if record.Failure == nil || record.Failure.Side != protocol.SideConnection {
		t.Fatalf("failure %+v", record.Failure)
	}
	if strings.Contains(record.Failure.ConnectionError, "127.0.0.1") {
		t.Fatalf("connection error leaked a host: %q", record.Failure.ConnectionError)
	}
}

func TestLocalPolicyRefusalIsNeverRecordedAsUpstream(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(200)
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")},
		func(c *protocol.Config) {
			c.BeforeRequest = func(route string, payload *protocol.Document) *protocol.Refusal {
				return &protocol.Refusal{Status: 503, Code: "subagent_policy_initializing"}
			}
		})
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != 503 || errorCode(t, body) != "subagent_policy_initializing" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, body))
	}
	if reply.Header.Get("Retry-After") != "1" {
		t.Fatalf("retry-after %q", reply.Header.Get("Retry-After"))
	}
	if len(fake.captured()) != 0 {
		t.Fatalf("a local refusal reached the upstream")
	}
	record := gateway.Metrics().Recent[0]
	if record.Failure == nil || record.Failure.Side != protocol.SideLocal ||
		record.Failure.Reason != "subagent_policy_initializing" {
		t.Fatalf("failure %+v", record.Failure)
	}
}

func TestClientDisconnectIsRecordedAsCancelled(t *testing.T) {
	started := make(chan struct{})
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: waiting\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)
	request, _ := http.NewRequest(http.MethodPost, url+"/chat/completions",
		strings.NewReader(`{"model":"kimi-probe","messages":[],"stream":true}`))
	request.Header.Set("Authorization", "Bearer "+gatewayKey)
	request.Header.Set("Content-Type", "application/json")
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	buffer := make([]byte, 32)
	_, _ = reply.Body.Read(buffer)
	<-started
	_ = reply.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for gateway.Inflight() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("inflight slot was never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	record := waitForRecentRecord(t, gateway)
	if record.Status != 499 || record.LedgerStatus() != "cancelled" {
		t.Fatalf("status %d ledger %q", record.Status, record.LedgerStatus())
	}
}

func TestFirstByteIncludesTheUpstreamHeadDelay(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		// The vendor takes this long to produce its head. Starting the
		// first-byte clock after the head was forwarded counted none of it.
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)
	reply, _ := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	record := waitForRecentRecord(t, gateway)
	if record.FirstByteMS == nil {
		t.Fatal("first byte was not measured")
	}
	if *record.FirstByteMS < 100 {
		t.Fatalf("first byte %d ms does not include the upstream head delay", *record.FirstByteMS)
	}
	if *record.FirstByteMS > record.MS {
		t.Fatalf("first byte %d ms exceeds the total %d ms", *record.FirstByteMS, record.MS)
	}
}

func TestFirstByteIsMeasuredForABodylessResponse(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, nil)
	reply, _ := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", reply.StatusCode)
	}
	record := waitForRecentRecord(t, gateway)
	if record.FirstByteMS == nil {
		t.Fatal("a bodyless response left the first byte unmeasured")
	}
	if *record.FirstByteMS < 100 {
		t.Fatalf("first byte %d ms does not include the upstream head delay", *record.FirstByteMS)
	}
}

func TestLargeResponsesCompletedEventExtractsUsage(t *testing.T) {
	largeText := strings.Repeat("a", 128*1024)
	completedLine := `event: response.completed` + "\n" +
		`data: {"type":"response.completed","response":{"id":"resp_big","object":"response","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + largeText + `"}]}],"usage":{"input_tokens":321,"output_tokens":654,"total_tokens":975}}}` + "\n\n"
	fake := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(completedLine))
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{responsesModel("kimi-big")}, nil)
	reply, _ := post(t, url, "/responses", map[string]any{"model": "kimi-big", "input": "hi", "stream": true}, nil)
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("status %d", reply.StatusCode)
	}
	record := waitForRecentRecord(t, gateway)
	if record.Outcome != string(protocol.OutcomeCompleted) {
		t.Fatalf("outcome = %q, want completed", record.Outcome)
	}
	if record.Usage == nil || record.Usage.InputTokens == nil || *record.Usage.InputTokens != 321 ||
		record.Usage.OutputTokens == nil || *record.Usage.OutputTokens != 654 ||
		record.Usage.TotalTokens == nil || *record.Usage.TotalTokens != 975 {
		t.Fatalf("large response.completed usage lost: %+v", record.Usage)
	}
}

func TestGatewayUpstreamTimeoutReportedAs504NotCancelled(t *testing.T) {
	t.Run("before_headers", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-slow")}, func(cfg *protocol.Config) {
			cfg.Timeout = 60 * time.Millisecond
		})
		reply, body := post(t, url, "/chat/completions", map[string]any{"model": "kimi-slow", "messages": []any{}}, nil)
		if reply.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want 504, body = %s", reply.StatusCode, body)
		}
		if code := errorCode(t, body); code != "upstream_timeout" {
			t.Fatalf("code = %q, want upstream_timeout", code)
		}
		record := waitForRecentRecord(t, gateway)
		if record.Status != http.StatusGatewayTimeout || record.Error != "upstream_timeout" ||
			record.Side != protocol.SideConnection || record.LedgerStatus() != "failed" {
			t.Fatalf("record = %+v, ledgerStatus = %q", record, record.LedgerStatus())
		}
	})
	t.Run("mid_stream", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-slow-stream")}, func(cfg *protocol.Config) {
			cfg.Timeout = 60 * time.Millisecond
		})
		reply, _ := post(t, url, "/chat/completions", map[string]any{"model": "kimi-slow-stream", "messages": []any{}, "stream": true}, nil)
		if reply.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", reply.StatusCode)
		}
		record := waitForRecentRecord(t, gateway)
		if record.Status != http.StatusGatewayTimeout || record.Error != "upstream_timeout" ||
			record.Side != protocol.SideConnection || record.Outcome != string(protocol.OutcomeFailed) ||
			record.LedgerStatus() != "failed" {
			t.Fatalf("mid-stream timeout record = %+v, ledgerStatus = %q", record, record.LedgerStatus())
		}
	})
}
