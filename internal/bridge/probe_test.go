package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

func callPanel(t *testing.T, service *Service, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, service.Endpoint.URL+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+service.Endpoint.Key)
	if method == http.MethodPost {
		r.Header.Set("Origin", service.Endpoint.URL)
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	service.ServeHTTP(w, r)
	return w
}

func probePanel(t *testing.T, service *Service, body string) protocol.ProbeResult {
	t.Helper()
	w := callPanel(t, service, "POST", "/api/connections/probe", body)
	var result protocol.ProbeResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("probe HTTP %d: %s", w.Code, w.Body.String())
	}
	return result
}

func TestProbeSavedConnectionReadOnlyAndCatalogLimit(t *testing.T) {
	const key = "probe-saved-synthetic"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			rows := make([]protocol.ProbeModel, 52)
			for index := range rows {
				id := fmt.Sprintf("sample-%02d", index)
				rows[index] = protocol.ProbeModel{ID: id, Name: id}
			}
			rows[0].Name = key
			_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
		case "/v1/chat/completions":
			if r.Method != "POST" {
				t.Error(r.Method)
			}
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["model"] != "sample-51" || payload["max_tokens"] != float64(16) || payload["stream"] != false {
				t.Errorf("probe payload = %v", payload)
			}
			w.WriteHeader(http.StatusCreated)
		case "/v1/responses":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["model"] != "gpt-5" || payload["input"] != "ping" {
				t.Errorf("Responses probe payload = %v", payload)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	store, err := core.Open(t.TempDir() + "/state")
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err = store.Update(rev, func(d core.Document) error {
		_, err := core.MergeConnections(d, []any{core.Document{"id": "saved", "name": "Saved", "baseURL": upstream.URL + "/v1", "protocols": []any{"chat"}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, Endpoint: Endpoint{URL: "http://127.0.0.1:4242", Key: "panel"}, Keys: CacheKeys(testKeys{core.KeyEnv("saved"): key})}
	result := probePanel(t, service, `{"connectionId":"saved","model":"sample-51"}`)
	if !result.OK || result.HTTPStatus != 201 || result.ConnectionProtocol != "chat" || result.TestedModel != "sample-51" || !result.Catalog.ModelFound || result.Catalog.ModelCount != 52 || len(result.Catalog.Models) != 50 || result.Catalog.Models[0].Name != "[redacted]" {
		t.Fatalf("probe = %+v", result)
	}
	result = probePanel(t, service, `{"connectionId":"saved","model":"gpt-5","protocol":""}`)
	if !result.OK || result.DetectedProtocol != "responses" || !slices.Equal(result.TriedProtocols, []string{"responses"}) {
		t.Fatalf("explicit empty protocol did not auto-detect: %+v", result)
	}
	result = probePanel(t, service, `{"connectionId":"saved","baseUrl":"https://unrelated.invalid/v1"}`)
	if result.OK || result.Reason != "probe_missing_key" || result.Message != "请先填写 API Key。" {
		t.Fatalf("credential was reused for the draft address: %+v", result)
	}
	if _, after, err := store.Read(); err != nil || after != rev {
		t.Fatalf("probe changed configuration revision: %q %v", after, err)
	}
}

func TestProbeAnthropicPresetCanSaveAndForward(t *testing.T) {
	var messages atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "synthetic" || r.Header.Get("anthropic-version") != protocol.AnthropicVersion || r.Header.Get("Authorization") != "" {
			t.Error("invalid Anthropic authentication")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-6","display_name":"Claude Sonnet"}]}`))
		case "/v1/messages":
			messages.Add(1)
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["model"] != "claude-sonnet-4-6" || payload["max_tokens"] != float64(16) {
				t.Errorf("messages = %v", payload)
			}
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	base := upstream.URL + "/v1"
	service := &Service{Endpoint: Endpoint{URL: "http://127.0.0.1:4242", Key: "panel"}}
	result := probePanel(t, service, fmt.Sprintf(`{"baseUrl":%q,"apiKey":"synthetic","presetId":"anthropic","protocol":"auto"}`, base))
	if !result.OK || result.DetectedProtocol != "anthropic" || result.ConnectionProtocol != "chat" || result.UpstreamProtocol != "anthropic" || result.Catalog.Models[0].Name != "Claude Sonnet" || !slices.Equal(result.TriedProtocols, []string{"anthropic"}) {
		t.Fatalf("probe = %+v", result)
	}
	d := core.Document{"models": []any{}}
	if _, err := core.MergeConnections(d, []any{core.Document{"id": "anthropic", "name": "Anthropic", "baseURL": base, "protocols": []any{result.ConnectionProtocol}}}); err != nil {
		t.Fatal(err)
	}
	row := core.Document{"id": "kimi-claude", "protocol": "chat", "upstreamProtocol": result.UpstreamProtocol, "upstreamModelId": result.TestedModel, "connectionId": "anthropic"}
	if err := core.MergeModels(d, []any{row}); err != nil {
		t.Fatal(err)
	}
	if err := core.Validate(d); err != nil {
		t.Fatal(err)
	}
	models, err := protocol.ValidateModelsAny(core.Rows(d["models"]))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := protocol.NewBackend([]protocol.Endpoint{{ID: "anthropic", BaseURL: base, APIKey: "synthetic", Protocols: []string{result.ConnectionProtocol}}}, models)
	if err != nil {
		t.Fatal(err)
	}
	response, err := backend.Request(context.Background(), protocol.ProtocolChat, []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"ping"}],"max_tokens":16}`), protocol.RequestOptions{ConnectionID: models[0].ConnectionID, Model: &models[0]})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.Status != 200 || messages.Load() != 2 {
		t.Fatalf("forwarded response = %d, messages count = %d", response.Status, messages.Load())
	}
}

func TestProbeHTTPValidation(t *testing.T) {
	service := &Service{Endpoint: Endpoint{URL: "http://127.0.0.1:4242", Key: "panel"}}
	for _, body := range []string{`{}`, `{"baseUrl":"http://example.com/v1"}`, `{"baseUrl":"https://user@example.com/v1"}`, `{"baseUrl":"https://example.com?"}`, `{"baseUrl":"https://example.com#"}`} {
		w := callPanel(t, service, "POST", "/api/connections/probe", body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_base_url") {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
	}
	w := callPanel(t, service, "POST", "/api/connections/probe", `{"baseUrl":"https://example.com/v1","protocol":"unknown"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_probe_protocol") {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
}
