package bridge

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/appupdate"
	"github.com/wssz527/kimiwork-plus/internal/core"
)

func TestUpdatesHTTPAPI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := core.PrivateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemaVersion": 1,
			"version":       "1.0.5",
			"channel":       "stable",
			"name":          "Kimi Work+ 1.0.5",
			"notes":         "HTTP API check test",
			"publishedAt":   time.Now().UTC().Format(time.RFC3339),
			"artifacts": []map[string]any{{
				"url": "http://127.0.0.1/pkg.zip", "filename": "pkg.zip", "filetype": "zip",
				"size": 123, "platform": runtime.GOOS, "arch": runtime.GOARCH,
				"digestAlgo": "sha256", "digest": "AAAA", "signatureAlgo": "ed25519", "signature": "AAAA",
			}},
		})
	}))
	defer feed.Close()

	mgr, err := appupdate.NewManager(appupdate.Options{
		StateDir:       dir,
		CurrentVersion: "1.0.0",
		BuildID:        "test-build",
		FeedURL:        feed.URL,
		PublicKey:      pub,
		HTTPClient:     feed.Client(),
		DisableAutoRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	svc := &Service{Store: store, StateDir: dir, Updates: mgr}
	if err := svc.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer svc.Close(t.Context())

	doReq := func(method, path string, body any) (*http.Response, core.Document) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, err := http.NewRequest(method, svc.Endpoint.URL+path, &buf)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+svc.Endpoint.Key)
		if method == http.MethodPost {
			req.Header.Set("Origin", svc.Endpoint.URL)
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out core.Document
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	// GET /api/updates
	resp, doc := doReq(http.MethodGet, "/api/updates", nil)
	if resp.StatusCode != 200 || doc["state"] != "idle" {
		t.Fatalf("GET /api/updates: status=%d doc=%#v", resp.StatusCode, doc)
	}

	// POST /api/updates/download before check -> 409 update_not_available
	resp, doc = doReq(http.MethodPost, "/api/updates/download", core.Document{})
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 before check, got %d (%#v)", resp.StatusCode, doc)
	}

	// POST /api/updates/rollback without backup -> 409 rollback_unavailable
	resp, doc = doReq(http.MethodPost, "/api/updates/rollback", core.Document{})
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 without rollback backup, got %d (%#v)", resp.StatusCode, doc)
	}

	// POST /api/updates/settings invalid channel -> 400
	resp, _ = doReq(http.MethodPost, "/api/updates/settings", core.Document{"channel": "nightly"})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for invalid channel, got %d", resp.StatusCode)
	}

	// POST /api/updates/settings valid
	resp, doc = doReq(http.MethodPost, "/api/updates/settings", core.Document{"autoCheck": false, "channel": "stable"})
	if resp.StatusCode != 200 {
		t.Fatalf("settings save failed: status=%d doc=%#v", resp.StatusCode, doc)
	}

	// POST /api/updates/check -> available 1.0.5
	resp, doc = doReq(http.MethodPost, "/api/updates/check", core.Document{})
	if resp.StatusCode != 200 || doc["ok"] != true {
		t.Fatalf("check failed: status=%d doc=%#v", resp.StatusCode, doc)
	}
	u := core.Object(doc["updates"])
	if u["state"] != "available" {
		t.Fatalf("expected available after check, got %#v", u)
	}

	// GET /api/state reflects updates
	resp, stateDoc := doReq(http.MethodGet, "/api/state", nil)
	if resp.StatusCode != 200 || core.Object(stateDoc["updates"])["state"] != "available" {
		t.Fatalf("state updates mismatch: %#v", stateDoc["updates"])
	}

	// POST /api/updates/open-release without releaseUrl returns 404
	resp, doc = doReq(http.MethodPost, "/api/updates/open-release", core.Document{})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 when no releaseUrl is present, got %d (%#v)", resp.StatusCode, doc)
	}
}

func TestUpdatesOpenReleaseEndpointUsesTrustedServerURL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := core.PrivateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := appupdate.NewManager(appupdate.Options{
		StateDir:       dir,
		CurrentVersion: "1.1.3",
		BuildID:        "test-build",
		MirrorBase:     "https://cnb.cool",
		GitHubRepo:     "wssz527/kimiwork-plus",
		PublicKey:      pub,
		DisableAutoRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	var opened []string
	svc := &Service{
		Store:    store,
		StateDir: dir,
		Updates:  mgr,
		OpenURL: func(raw string) error {
			opened = append(opened, raw)
			return nil
		},
	}
	if err := svc.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer svc.Close(t.Context())

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(core.Document{"url": "https://evil.example/ignored"})
	req, err := http.NewRequest(http.MethodPost, svc.Endpoint.URL+"/api/updates/open-release", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+svc.Endpoint.Key)
	req.Header.Set("Origin", svc.Endpoint.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out core.Document
	_ = json.NewDecoder(resp.Body).Decode(&out)
	want := "https://cnb.cool/wssz527/kimiwork-plus/-/releases"
	if resp.StatusCode != 200 || out["ok"] != true || out["url"] != want {
		t.Fatalf("open-release = status=%d doc=%#v want=%q", resp.StatusCode, out, want)
	}
	if len(opened) != 1 || opened[0] != want {
		t.Fatalf("opened URLs = %v, want [%q]", opened, want)
	}
}
