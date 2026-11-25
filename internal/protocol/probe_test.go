package protocol

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
	"time"
)

func TestProbeFallsBackAndClosesStreamingAfterFirstFrame(t *testing.T) {
	closed := make(chan struct{})
	var chatAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("missing bearer authentication")
		}
		switch r.URL.Path {
		case "/models":
			http.NotFound(w, r)
		case "/responses":
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request["input"] != "ping" || request["max_output_tokens"] != float64(16) {
				t.Errorf("Responses request = %v", request)
			}
			http.NotFound(w, r)
		case "/chat/completions":
			chatAttempts.Add(1)
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request["stream"] != true {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"error":{"message":"stream must be true"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, ": keepalive\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(closed)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := Probe(ctx, ProbeRequest{BaseURL: server.URL, APIKey: "synthetic", Model: "openai/gpt-5-codex", Protocol: "auto"})
	if !result.OK || result.Catalog.Supported || result.DetectedProtocol != "chat" || chatAttempts.Load() != 2 || !slices.Equal(result.TriedProtocols, []string{"responses", "chat"}) {
		t.Fatalf("result = %+v, chat attempts = %d", result, chatAttempts.Load())
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("probe left the successful SSE response open")
	}
}

func TestProbeStopsOnAuthorizationAndRedactsDetails(t *testing.T) {
	const key = "secret-synthetic-123"
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"error":{"message":%q}}`, key+" "+strings.Repeat("错误", 150))
	}))
	defer server.Close()
	result := Probe(context.Background(), ProbeRequest{BaseURL: server.URL, APIKey: key, Model: "gpt-5"})
	body, _ := json.Marshal(result)
	if result.OK || result.Reason != "probe_unauthorized" || result.HTTPStatus != 403 || attempts.Load() != 1 || len([]rune(result.Detail)) != 200 || strings.Contains(string(body), key) {
		t.Fatalf("result = %+v, attempts = %d", result, attempts.Load())
	}
}

func TestProbeRefusesRedirectsWithoutReplayingCredentials(t *testing.T) {
	var called atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	result := Probe(context.Background(), ProbeRequest{BaseURL: redirector.URL, APIKey: "synthetic", Model: "custom", Protocol: "chat"})
	if result.OK || result.HTTPStatus != 307 || called.Load() || !slices.Equal(result.TriedProtocols, []string{"chat"}) {
		t.Fatalf("result = %+v, redirected = %v", result, called.Load())
	}
}

func TestProbeEmptyDirectoryAndDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Error(r.URL.Path)
		}
		if r.Header.Get("Authorization") == "Bearer fast" {
			_, _ = fmt.Fprint(w, `{"data":[]}`)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	result := Probe(context.Background(), ProbeRequest{BaseURL: server.URL, APIKey: "fast", PresetID: "openai"})
	if !result.OK || !result.Catalog.Supported || result.DetectedProtocol != "responses" || len(result.TriedProtocols) != 0 {
		t.Fatalf("directory-only result = %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result = Probe(ctx, ProbeRequest{BaseURL: server.URL, APIKey: "slow", Model: "sample"})
	if result.OK || result.Reason != "probe_timeout" {
		t.Fatalf("timeout result = %+v", result)
	}
}
