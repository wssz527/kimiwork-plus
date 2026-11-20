package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoveryDeclarationsAndMetadata(t *testing.T) {
	p := map[string]any{"protocols": []any{"chat", "responses"}}
	for _, tc := range []struct {
		raw  string
		want int
	}{{`{}`, 2}, {`{"protocols":[]}`, 0}, {`{"api_capabilities":{}}`, 0}, {`{"api_capabilities":{"chat":false,"responses":true}}`, 1}, {`{"api_capabilities":{"chat":"unknown","responses":null}}`, 0}, {`{"protocols":["chat"],"api_capabilities":{"chat":false}}`, 1}} {
		var row map[string]any
		_ = json.Unmarshal([]byte(tc.raw), &row)
		if got := catalogProtocols(row, p); len(got) != tc.want {
			t.Errorf("%s: %v", tc.raw, got)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/models" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"vendor-model","api_capabilities":{"chat":false,"responses":true},"context_window":100000,"max_output_tokens":5000,"tools":false,"reasoning":true,"input_modalities":["text","image"],"reasoning_efforts":["low","high"],"aliases":["nickname"]},{"id":"magpie-model","reasoning":true,"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"max"}]}]}`))
	}))
	defer server.Close()
	result, e := Discover(context.Background(), server.URL, "synthetic", "openai")
	if e != nil {
		t.Fatal(e)
	}
	if result["state"] != "passed" {
		t.Fatal(result)
	}
	model := Object(Rows(result["models"])[0])
	if model["tools"] != false || len(Rows(model["protocols"])) != 1 || len(Rows(model["inputModalities"])) != 2 {
		t.Fatal(model)
	}
	magpie := Object(Rows(result["models"])[1])
	efforts := Rows(magpie["reasoningEfforts"])
	if len(efforts) != 3 || efforts[0] != "low" || efforts[1] != "high" || efforts[2] != "max" {
		t.Fatalf("Magpie reasoning levels were not preserved: %v", magpie)
	}
	searched := SearchCatalog(Preset("openai"), result, "nickname")
	if Object(Rows(searched["models"])[0])["id"] != "vendor-model" {
		t.Fatal(searched)
	}
}
func TestSearchPreservesExplicitNegatives(t *testing.T) {
	p := map[string]any{"models": []any{map[string]any{"id": "alpha", "tools": true, "contextWindow": 100000, "protocols": []any{"chat"}, "aliases": []any{"first"}}}, "source": map[string]any{"url": "https://example.invalid/docs", "checkedAt": "2026-11-10"}}
	result := SearchCatalog(p, Document{"models": []any{map[string]any{"id": "alpha", "tools": false, "protocols": []any{}, "aliases": []any{"second"}}}, "sourceURL": "https://example.invalid/v1/models"}, "second")
	m := Object(Rows(result["models"])[0])
	if m["tools"] != false || len(Rows(m["protocols"])) != 0 || Object(m["metadataSource"])["kind"] != "preset" {
		t.Fatal(m)
	}
}
