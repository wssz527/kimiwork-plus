package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kimiworkplus.local/migration/internal/bridge"
	"kimiworkplus.local/migration/internal/core"
)

func TestAccountTrayUsesFreshEndpointAndAuthenticatedActions(t *testing.T) {
	key := strings.Repeat("a", 64)
	var requestedAction, requestedAccount string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("missing panel authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			w.Write([]byte(`{"accounts":{"state":"ready","currentId":"first","accounts":[{"id":"first","name":"First"}]}}`))
			return
		}
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing same-origin mutation headers")
		}
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		requestedAction, requestedAccount = in["action"], in["accountId"]
		w.WriteHeader(202)
		w.Write([]byte(`{"operation":{"state":"running"}}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	endpointPath := filepath.Join(dir, "desktop-endpoint.json")
	writeEndpoint := func(url string) {
		t.Helper()
		if err := core.AtomicJSON(endpointPath, bridge.Endpoint{URL: url, Key: key, BuildID: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	writeEndpoint("http://127.0.0.1:1")
	client := accountTrayClient{dir: dir, client: server.Client()}
	writeEndpoint(server.URL)
	var response struct {
		Accounts trayAccounts `json:"accounts"`
	}
	if err := client.request(context.Background(), "GET", "/api/accounts", nil, &response); err != nil || response.Accounts.CurrentID != "first" {
		t.Fatal("tray did not read fresh endpoint", err)
	}
	if err := client.request(context.Background(), "POST", "/api/actions", map[string]string{"action": "account-switch", "accountId": "second"}, nil); err != nil {
		t.Fatal(err)
	}
	if requestedAction != "account-switch" || requestedAccount != "second" {
		t.Fatal("wrong account action")
	}
}

func TestAccountTrayRejectsUntrustedEndpoint(t *testing.T) {
	dir := t.TempDir()
	if err := core.AtomicJSON(filepath.Join(dir, "desktop-endpoint.json"), bridge.Endpoint{URL: "https://example.com", Key: strings.Repeat("a", 64), BuildID: "test"}); err != nil {
		t.Fatal(err)
	}
	client := accountTrayClient{dir: dir, client: http.DefaultClient}
	if err := client.request(context.Background(), "GET", "/api/accounts", nil, nil); err == nil || err.Error() != "account_service_unavailable" {
		t.Fatal("untrusted endpoint was accepted", err)
	}
}
