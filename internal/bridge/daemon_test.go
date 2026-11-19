package bridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wssz527/kimi-work-plus/internal/core"
)

func TestServiceStopReusesEndpoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, StateDir: dir}
	if err := s.Start("0"); err != nil {
		t.Fatal(err)
	}
	s.SetStop(func() { go s.Close(context.Background()) })
	first := s.Endpoint
	if err := StopDaemon(dir); err != nil {
		t.Fatal(err)
	}
	second := &Service{Store: store, StateDir: dir}
	if err := second.Start(""); err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	if second.Endpoint.URL != first.URL || second.Endpoint.Key != first.Key {
		t.Fatal("normal restart did not preserve the endpoint")
	}
	if !Healthy(second.Endpoint) {
		t.Fatal("new service not healthy")
	}
}

func TestStopDaemonReportsTransportFailure(t *testing.T) {
	dir := t.TempDir()
	endpoint := Endpoint{URL: "http://127.0.0.1:1", Key: strings.Repeat("a", 64), PID: 1, Version: Version, BuildID: BuildID}
	if err := core.AtomicJSON(filepath.Join(dir, "desktop-endpoint.json"), endpoint); err != nil {
		t.Fatal(err)
	}
	if err := StopDaemon(dir); !errors.Is(err, errStopDaemon) {
		t.Fatalf("expected a readable stop failure, got %v", err)
	}
}

func TestStopDaemonRejectsReplacement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/stop" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	dir := t.TempDir()
	endpoint := Endpoint{URL: server.URL, Key: strings.Repeat("a", 64), PID: 1, Version: Version, BuildID: BuildID}
	if err := core.AtomicJSON(filepath.Join(dir, "desktop-endpoint.json"), endpoint); err != nil {
		t.Fatal(err)
	}
	if err := StopDaemon(dir); err == nil {
		t.Fatal("a different service on the port must not count as stopped")
	}
}
