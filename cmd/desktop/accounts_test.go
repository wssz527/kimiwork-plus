package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

func TestWindowNavControllerSameOriginAndCrossOrigin(t *testing.T) {
	var mu sync.Mutex
	var urls []string
	var scripts []string
	nav := newWindowNavController(
		true,
		"http://127.0.0.1:41001",
		"key-initial",
		func(u string) {
			mu.Lock()
			urls = append(urls, u)
			mu.Unlock()
		},
		func(js string) {
			mu.Lock()
			scripts = append(scripts, js)
			mu.Unlock()
		},
	)

	// Before initial navigation finishes, RequestOpenAccounts queues until OnNavigationFinished.
	nav.RequestOpenAccounts()
	if len(scripts) != 0 {
		t.Fatalf("expected accounts JS to wait before initial navigation finishes")
	}
	nav.OnNavigationFinished()
	if len(scripts) != 1 || scripts[0] != openAccountsJS {
		t.Fatalf("expected queued accounts JS to flush once on initial navigation finish, got %d", len(scripts))
	}

	// Same-origin restart with identical key (PID change only): keeps navLoaded=true and avoids SetURL.
	nav.ApplyEndpoint(bridge.Endpoint{URL: "http://127.0.0.1:41001", Key: "key-initial", PID: 2002})
	if len(urls) != 0 {
		t.Fatalf("same-origin PID-only update must not trigger SetURL, got %v", urls)
	}
	nav.RequestOpenAccounts()
	if len(scripts) != 2 {
		t.Fatalf("expected immediate ExecJS after same-origin PID-only update, got %d", len(scripts))
	}

	// Same-origin restart with rotated key: updates hash via SetURL but keeps navLoaded=true.
	nav.ApplyEndpoint(bridge.Endpoint{URL: "http://127.0.0.1:41001", Key: "key-rotated", PID: 2003})
	if len(urls) != 1 || urls[0] != "http://127.0.0.1:41001/#key=key-rotated" {
		t.Fatalf("expected SetURL with rotated hash, got %v", urls)
	}
	nav.RequestOpenAccounts()
	if len(scripts) != 3 {
		t.Fatalf("same-origin key rotation must not stall RequestOpenAccounts waiting for didFinishNavigation, got %d", len(scripts))
	}

	// Cross-origin port change: resets navLoaded=false, navigates via SetURL, queues RequestOpenAccounts until finish.
	nav.ApplyEndpoint(bridge.Endpoint{URL: "http://127.0.0.1:41002", Key: "key-newport", PID: 2004})
	if len(urls) != 2 || urls[1] != "http://127.0.0.1:41002/#key=key-newport" {
		t.Fatalf("expected cross-origin SetURL, got %v", urls)
	}
	nav.RequestOpenAccounts()
	if len(scripts) != 3 {
		t.Fatalf("cross-origin navigation must defer RequestOpenAccounts until OnNavigationFinished")
	}
	nav.OnNavigationFinished()
	if len(scripts) != 4 {
		t.Fatalf("expected deferred RequestOpenAccounts to run after cross-origin OnNavigationFinished, got %d", len(scripts))
	}

	// Concurrent stress under -race.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); nav.RequestOpenAccounts() }()
		go func() { defer wg.Done(); nav.OnNavigationFinished() }()
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				nav.ApplyEndpoint(bridge.Endpoint{URL: "http://127.0.0.1:41002", Key: "key-concurrent"})
			} else {
				nav.ApplyEndpoint(bridge.Endpoint{URL: "http://127.0.0.1:41003", Key: "key-concurrent-2"})
			}
		}(i)
	}
	wg.Wait()
}
