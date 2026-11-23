package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/bridge"
	"github.com/wssz527/kimiwork-plus/internal/core"
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

func TestTrayAccountLabelFallback(t *testing.T) {
	if got := trayAccountLabel(bridge.AccountSummary{ID: "acct-abcdef", Name: "Work A"}); got != "Work A" {
		t.Fatalf("unexpected named label: %q", got)
	}
	if got := trayAccountLabel(bridge.AccountSummary{ID: "acct-abcdef", Name: "   "}); got != "\u8d26\u53f7 abcdef\uff08\u9ed8\u8ba4\u540d\uff09" {
		t.Fatalf("unexpected fallback tail label: %q", got)
	}
	if got := trayAccountLabel(bridge.AccountSummary{ID: "---", Name: "", NeedsLogin: true}); got != "\u672a\u547d\u540d\u8d26\u53f7 \u00b7 \u9700\u91cd\u65b0\u767b\u5f55" {
		t.Fatalf("unexpected unnamed needsLogin label: %q", got)
	}
}

func TestSingleInstanceKeyAndPlatformDiagnostics(t *testing.T) {
	dir := t.TempDir()
	k1 := singleInstanceEncryptionKey(dir)
	k2 := singleInstanceEncryptionKey(dir + string(filepath.Separator) + ".")
	if k1 != k2 {
		t.Fatalf("singleInstanceEncryptionKey must be stable across path forms")
	}
	other := t.TempDir()
	if k1 == singleInstanceEncryptionKey(other) {
		t.Fatalf("distinct state directories must derive distinct keys")
	}
	winNormal := windowDiagnosticMessage("windows", "window_created")
	if strings.Contains(winNormal, "macOS") || !strings.Contains(winNormal, "Windows") {
		t.Fatalf("windows diagnostic message unexpected: %q", winNormal)
	}
	winTerm := windowDiagnosticMessage("windows", "web_content_process_terminated")
	if strings.Contains(winTerm, "macOS") || !strings.Contains(winTerm, "Windows") {
		t.Fatalf("windows terminated message unexpected: %q", winTerm)
	}
	macNormal := windowDiagnosticMessage("darwin", "window_created")
	if !strings.Contains(macNormal, "macOS") {
		t.Fatalf("darwin diagnostic message unexpected: %q", macNormal)
	}
	if !strings.Contains(openAccountsJS, "settings-tab-accounts") {
		t.Fatalf("openAccountsJS must switch to accounts subtab")
	}
}
