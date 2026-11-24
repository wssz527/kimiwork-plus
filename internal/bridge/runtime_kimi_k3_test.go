package bridge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

// The document switch has to reach the live gateway: the runtime is the only
// place that builds one, and a catalog read is the observable effect.
func TestRuntimeAppliesKimiK3Compatibility(t *testing.T) {
	const upstreamKey = "vendor-key"
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		seen = append(seen, payload.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	document := core.Document{
		"connections": []any{core.Document{"id": "local", "name": "Local", "baseURL": upstream.URL,
			"apiKeyEnv": "KIMI_WORK_LOCAL_KEY", "protocols": []any{"chat"}}},
		"models": []any{core.Document{"id": "kimi-alpha", "protocol": "chat", "contextWindow": float64(32000),
			"connectionId": "local", "upstreamModelId": "alpha-real"}},
		core.KimiK3CompatibilityKey: true,
	}
	if _, _, err = store.Update(rev, func(d core.Document) error {
		for key, value := range document {
			d[key] = value
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{"KIMI_WORK_LOCAL_KEY": upstreamKey})
	runtime, err := NewRuntime(dir, store, keys)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(runtime)
	defer server.Close()

	call := func(method, path, body string) (int, string) {
		t.Helper()
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
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

	generated := protocol.KimiCompatibleModelID("kimi-alpha")
	status, catalog := call("GET", "/v1/models/api.json", "")
	if status != http.StatusOK || !strings.Contains(catalog, `"`+generated+`"`) ||
		!strings.Contains(catalog, `"name":"kimi-alpha"`) {
		t.Fatalf("enabled catalog: %d %s", status, catalog)
	}

	status, body := call("POST", "/v1/chat/completions",
		`{"model":"`+generated+`","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("generated id: %d %s", status, body)
	}
	if len(seen) != 1 || seen[0] != "alpha-real" {
		t.Fatalf("upstream models = %v", seen)
	}

	// Disabling the opt-in stops the catalog rename but the id stays routable.
	stored, _, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	delete(stored, core.KimiK3CompatibilityKey)
	if err = runtime.Apply(stored); err != nil {
		t.Fatal(err)
	}
	status, catalog = call("GET", "/v1/models/api.json", "")
	if status != http.StatusOK || strings.Contains(catalog, generated) ||
		!strings.Contains(catalog, `"kimi-alpha"`) {
		t.Fatalf("disabled catalog: %d %s", status, catalog)
	}
	status, body = call("POST", "/v1/chat/completions",
		`{"model":"`+generated+`","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK || len(seen) != 2 || seen[1] != "alpha-real" {
		t.Fatalf("after disable: %d %s %v", status, body, seen)
	}
}
