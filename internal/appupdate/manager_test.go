package appupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

func makeSignedTestZip(t *testing.T, priv ed25519.PrivateKey, topDir string, files map[string][]byte) ([]byte, string, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(topDir + "/" + name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	sig := ed25519.Sign(priv, sum[:])
	return raw, base64.StdEncoding.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sig)
}

type testManifestDoc struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Version       string                 `json:"version"`
	Channel       string                 `json:"channel"`
	Name          string                 `json:"name"`
	Notes         string                 `json:"notes"`
	PublishedAt   string                 `json:"publishedAt"`
	Artifacts     []testManifestArtifact `json:"artifacts"`
}

type testManifestArtifact struct {
	URL           string `json:"url"`
	Filename      string `json:"filename"`
	Filetype      string `json:"filetype"`
	Size          int64  `json:"size"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch"`
	DigestAlgo    string `json:"digestAlgo"`
	Digest        string `json:"digest"`
	SignatureAlgo string `json:"signatureAlgo,omitempty"`
	Signature     string `json:"signature,omitempty"`
}

func TestPinnedPublicKeyValid(t *testing.T) {
	pub, err := PinnedPublicKey()
	if err != nil {
		t.Fatalf("PinnedPublicKey failed: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("unexpected key size: %d", len(pub))
	}
}

func TestCheckAndVerifyFailClosed(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	zipBytes, digestB64, validSigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist":           []byte("<plist/>"),
		"Contents/MacOS/KimiWorkPlus":   []byte("stub"),
		"Contents/MacOS/gateway-daemon": []byte("stub"),
	})
	_, _, wrongSigB64 := makeSignedTestZip(t, wrongPriv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist":           []byte("<plist/>"),
		"Contents/MacOS/KimiWorkPlus":   []byte("stub"),
		"Contents/MacOS/gateway-daemon": []byte("stub"),
	})

	cases := []struct {
		name        string
		sig         string
		sigAlgo     string
		tamperBytes bool
		wantCode    string
	}{
		{name: "missing_signature_rejected", sig: "", sigAlgo: "", wantCode: "update_signature_invalid"},
		{name: "wrong_key_signature_rejected", sig: wrongSigB64, sigAlgo: "ed25519", wantCode: "update_signature_invalid"},
		{name: "tampered_payload_rejected", sig: validSigB64, sigAlgo: "ed25519", tamperBytes: true, wantCode: "update_digest_mismatch"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/manifest.json" {
					doc := testManifestDoc{
						SchemaVersion: 1,
						Version:       "1.0.1",
						Channel:       "stable",
						Name:          "Kimi Work+ 1.0.1",
						Notes:         "Security test release",
						PublishedAt:   time.Now().UTC().Format(time.RFC3339),
						Artifacts: []testManifestArtifact{{
							URL:           srv.URL + "/pkg.zip",
							Filename:      "pkg.zip",
							Filetype:      "zip",
							Size:          int64(len(zipBytes)),
							Platform:      runtime.GOOS,
							Arch:          runtime.GOARCH,
							DigestAlgo:    "sha256",
							Digest:        digestB64,
							SignatureAlgo: tc.sigAlgo,
							Signature:     tc.sig,
						}},
					}
					_ = json.NewEncoder(w).Encode(doc)
					return
				}
				if r.URL.Path == "/pkg.zip" {
					payload := append([]byte(nil), zipBytes...)
					if tc.tamperBytes {
						payload[len(payload)-1] ^= 0xff
					}
					_, _ = w.Write(payload)
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()

			mgr, err := NewManager(Options{
				StateDir:       t.TempDir(),
				CurrentVersion: "1.0.0",
				BuildID:        "test-100",
				FeedURL:        srv.URL + "/manifest.json",
				PublicKey:      pub,
				HTTPClient:     srv.Client(),
				DisableAutoRun: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer mgr.Close()

			st, err := mgr.CheckNow(context.Background())
			if err != nil {
				t.Fatalf("CheckNow: %v", err)
			}
			if st["state"] != "available" {
				t.Fatalf("expected available, got %v", st["state"])
			}

			if _, err := mgr.StartDownloadAndApply(); err != nil {
				t.Fatalf("StartDownloadAndApply: %v", err)
			}

			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				cur := mgr.State()
				if cur["state"] == "error" {
					errObj, ok := cur["error"].(*UpdateError)
					if !ok || errObj == nil {
						t.Fatalf("expected *UpdateError, got %#v", cur["error"])
					}
					if errObj.Code != tc.wantCode {
						t.Fatalf("expected error code %s, got %s (detail=%s)", tc.wantCode, errObj.Code, errObj.Detail)
					}
					if errObj.Stage != "verify" {
						t.Fatalf("expected verify stage, got %s", errObj.Stage)
					}
					return
				}
				time.Sleep(30 * time.Millisecond)
			}
			t.Fatalf("timed out waiting for error state; final=%#v", mgr.State())
		})
	}
}

func TestDownloadInterruptionAllowsCleanRetryAndIdleGate(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist":           []byte("<plist/>"),
		"Contents/MacOS/KimiWorkPlus":   []byte("stub"),
		"Contents/MacOS/gateway-daemon": []byte("stub"),
	})

	var attempts atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.json" {
			doc := testManifestDoc{
				SchemaVersion: 1,
				Version:       "1.0.2",
				Channel:       "stable",
				Notes:         "Retry and idle test",
				PublishedAt:   time.Now().UTC().Format(time.RFC3339),
				Artifacts: []testManifestArtifact{{
					URL:           srv.URL + "/pkg.zip",
					Filename:      "pkg.zip",
					Filetype:      "zip",
					Size:          int64(len(zipBytes)),
					Platform:      runtime.GOOS,
					Arch:          runtime.GOARCH,
					DigestAlgo:    "sha256",
					Digest:        digestB64,
					SignatureAlgo: "ed25519",
					Signature:     sigB64,
				}},
			}
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		if r.URL.Path == "/pkg.zip" {
			if attempts.Add(1) == 1 {
				// Simulate mid-download network drop: send partial bytes with larger Content-Length.
				w.Header().Set("Content-Length", "999999")
				_, _ = w.Write(zipBytes[:16])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				hj, ok := w.(http.Hijacker)
				if ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return
			}
			_, _ = w.Write(zipBytes)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var busy atomic.Bool
	busy.Store(true)
	mgr, err := NewManager(Options{
		StateDir:       t.TempDir(),
		CurrentVersion: "1.0.0",
		BuildID:        "test-100",
		TargetPath:     filepath.Join(t.TempDir(), "nonexistent.app"),
		FeedURL:        srv.URL + "/manifest.json",
		PublicKey:      pub,
		HTTPClient:     srv.Client(),
		DisableAutoRun: true,
		ValidateStaged: func(string) error { return nil },
		IdleCheck: func() (bool, string) {
			if busy.Load() {
				return false, "Kimi Work 仍有正在进行的任务"
			}
			return true, ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	if _, err := mgr.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}

	// First download fails due to interrupted stream.
	if _, err := mgr.StartDownloadAndApply(); err != nil {
		t.Fatal(err)
	}
	waitUntilState(t, mgr, "error", 5*time.Second)

	// Retry succeeds download + verify and enters waiting_idle because busy=true.
	if _, err := mgr.StartDownloadAndApply(); err != nil {
		t.Fatal(err)
	}
	waitUntilState(t, mgr, "waiting_idle", 5*time.Second)
	st := mgr.State()
	if !strings.Contains(st["idleReason"].(string), "Kimi Work") {
		t.Fatalf("unexpected idleReason: %v", st["idleReason"])
	}

	// Cancel during waiting_idle returns cleanly to available.
	cancelled := mgr.Cancel()
	if cancelled["state"] != "available" {
		t.Fatalf("expected available after cancel, got %v", cancelled["state"])
	}
}

func TestGitHubManifestProviderChannels(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist": []byte("<plist/>"),
	})

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/kwp/releases/latest":
			_ = json.NewEncoder(w).Encode(githubRelease{
				TagName: "v1.1.0",
				Name:    "1.1.0",
				Assets: []githubAsset{{
					Name:               "update-manifest.json",
					BrowserDownloadURL: srv.URL + "/assets/stable-manifest.json",
				}},
			})
		case "/repos/org/kwp/releases":
			_ = json.NewEncoder(w).Encode([]githubRelease{
				{
					TagName:    "v1.2.0-preview.1",
					Prerelease: true,
					Assets: []githubAsset{{
						Name:               "update-manifest-preview.json",
						BrowserDownloadURL: srv.URL + "/assets/preview-manifest.json",
					}},
				},
			})
		case "/assets/stable-manifest.json":
			_ = json.NewEncoder(w).Encode(testManifestDoc{
				SchemaVersion: 1,
				Version:       "1.1.0",
				Channel:       "stable",
				Notes:         "Stable release",
				PublishedAt:   time.Now().UTC().Format(time.RFC3339),
				Artifacts: []testManifestArtifact{{
					URL: srv.URL + "/pkg.zip", Filename: "pkg.zip", Filetype: "zip",
					Size: int64(len(zipBytes)), Platform: runtime.GOOS, Arch: runtime.GOARCH,
					DigestAlgo: "sha256", Digest: digestB64, SignatureAlgo: "ed25519", Signature: sigB64,
				}},
			})
		case "/assets/preview-manifest.json":
			_ = json.NewEncoder(w).Encode(testManifestDoc{
				SchemaVersion: 1,
				Version:       "1.2.0-preview.1",
				Channel:       "preview",
				Notes:         "Preview release",
				PublishedAt:   time.Now().UTC().Format(time.RFC3339),
				Artifacts: []testManifestArtifact{{
					URL: srv.URL + "/pkg.zip", Filename: "pkg.zip", Filetype: "zip",
					Size: int64(len(zipBytes)), Platform: runtime.GOOS, Arch: runtime.GOARCH,
					DigestAlgo: "sha256", Digest: digestB64, SignatureAlgo: "ed25519", Signature: sigB64,
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	stateDir := t.TempDir()
	mgr, err := NewManager(Options{
		StateDir:       stateDir,
		CurrentVersion: "1.0.0",
		BuildID:        "b1",
		GitHubRepo:     "org/kwp",
		GitHubAPIBase:  srv.URL,
		PublicKey:      pub,
		HTTPClient:     srv.Client(),
		DisableAutoRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	st, err := mgr.CheckNow(context.Background())
	if err != nil {
		t.Fatalf("stable check: %v", err)
	}
	avail := st["available"].(*AvailableRelease)
	if avail.Version != "1.1.0" || avail.Channel != "stable" {
		t.Fatalf("unexpected stable release: %+v", avail)
	}

	autoFalse := false
	st, err = mgr.UpdateSettings(&autoFalse, "preview")
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if st["autoCheck"] != false || st["channel"] != "preview" || st["state"] != "idle" {
		t.Fatalf("unexpected settings state: %#v", st)
	}

	st, err = mgr.CheckNow(context.Background())
	if err != nil {
		t.Fatalf("preview check: %v", err)
	}
	avail = st["available"].(*AvailableRelease)
	if avail.Version != "1.2.0-preview.1" || avail.Channel != "preview" {
		t.Fatalf("unexpected preview release: %+v", avail)
	}

	// Reload from disk to verify settings persistence.
	mgr2, err := NewManager(Options{
		StateDir:       stateDir,
		CurrentVersion: "1.0.0",
		BuildID:        "b1",
		PublicKey:      pub,
		DisableAutoRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr2.Close()
	st2 := mgr2.State()
	if st2["autoCheck"] != false || st2["channel"] != "preview" {
		t.Fatalf("settings did not persist: %#v", st2)
	}
}

type githubTestTransport func(*http.Request) (*http.Response, error)

func (f githubTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGitHubStableManifestWhenAPIRateLimited(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digest, sig := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist": []byte("<plist/>"),
	})
	manifest, err := json.Marshal(testManifestDoc{
		SchemaVersion: 1, Version: "1.1.0", Channel: "stable",
		Artifacts: []testManifestArtifact{{
			URL: "https://github.com/org/kwp/releases/download/v1.1.0/pkg.zip", Filename: "pkg.zip", Filetype: "zip",
			Size: int64(len(zipBytes)), Platform: runtime.GOOS, Arch: runtime.GOARCH,
			DigestAlgo: "sha256", Digest: digest, SignatureAlgo: "ed25519", Signature: sig,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: githubTestTransport(func(req *http.Request) (*http.Response, error) {
		resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req}
		var body []byte
		switch req.URL.Host + req.URL.Path {
		case "api.github.com/repos/org/kwp/releases/latest":
			resp.StatusCode = http.StatusForbidden
			resp.Header.Set("X-RateLimit-Remaining", "0")
		case "github.com/org/kwp/releases/latest/download/update-manifest.json":
			body = manifest
		case "github.com/org/kwp/releases/download/v1.1.0/pkg.zip":
			body = zipBytes
		default:
			resp.StatusCode = http.StatusNotFound
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	})}
	mgr, err := NewManager(Options{
		StateDir: t.TempDir(), CurrentVersion: "1.0.0", GitHubRepo: "org/kwp",
		PublicKey: pub, HTTPClient: client, DisableAutoRun: true,
		ValidateStaged: func(string) error { return nil },
		IdleCheck:      func() (bool, string) { return false, "busy" },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	st, err := mgr.CheckNow(context.Background())
	if err != nil || st["state"] != "available" {
		t.Fatalf("stable check: %v, %v", st, err)
	}
	if _, err := mgr.StartDownloadAndApply(); err != nil {
		t.Fatal(err)
	}
	waitUntilState(t, mgr, "waiting_idle", 5*time.Second)
}

func TestRollbackMetadataAvailabilityGuardsSelfVersion(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	backupDir := filepath.Join(stateDir, rollbackDirName, rollbackPrevName, "Kimi Work+.app")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := RollbackMetadata{
		Version:    "1.0.0",
		BuildID:    "build-100",
		TargetPath: "/Applications/Kimi Work+.app",
		BackupPath: backupDir,
		SavedAt:    time.Now().UTC().Format(time.RFC3339),
		Platform:   runtime.GOOS,
	}
	raw, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(stateDir, rollbackDirName, rollbackPrevName, rollbackMetaFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// When currently running 1.0.1, rollback to 1.0.0 must be available.
	mgr101, err := NewManager(Options{
		StateDir:       stateDir,
		CurrentVersion: "1.0.1",
		BuildID:        "build-101",
		PublicKey:      pub,
		DisableAutoRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr101.Close()
	rb := mgr101.State()["rollback"].(core.Document)
	if rb["available"] != true || rb["version"] != "1.0.0" {
		t.Fatalf("expected rollback available for 1.0.1, got %#v", rb)
	}

	// When already running 1.0.0 with same buildId, rollback should not offer self-rollback.
	mgr100, err := NewManager(Options{
		StateDir:       stateDir,
		CurrentVersion: "1.0.0",
		BuildID:        "build-100",
		PublicKey:      pub,
		DisableAutoRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr100.Close()
	rb2 := mgr100.State()["rollback"].(core.Document)
	if rb2["available"] != false {
		t.Fatalf("expected rollback unavailable for same version/build, got %#v", rb2)
	}
}

func waitUntilState(t *testing.T, m *Manager, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.State()["state"] == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for state=%s; got=%#v", want, m.State())
}

func TestGitHubDownloadDoesNotUseManifestTotalTimeout(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist": []byte("<plist/>"),
	})
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/kwp/releases/latest":
			_ = json.NewEncoder(w).Encode(githubRelease{TagName: "v1.1.2", Assets: []githubAsset{{
				Name: "update-manifest.json", BrowserDownloadURL: srv.URL + "/manifest.json",
			}}})
		case "/manifest.json":
			_ = json.NewEncoder(w).Encode(testManifestDoc{
				SchemaVersion: 1, Version: "1.1.2", Channel: "stable",
				PublishedAt: time.Now().UTC().Format(time.RFC3339),
				Artifacts: []testManifestArtifact{{
					URL: srv.URL + "/pkg.zip", Filename: "pkg.zip", Filetype: "zip",
					Size: int64(len(zipBytes)), Platform: runtime.GOOS, Arch: runtime.GOARCH,
					DigestAlgo: "sha256", Digest: digestB64, SignatureAlgo: "ed25519", Signature: sigB64,
				}},
			})
		case "/pkg.zip":
			w.Header().Set("Content-Length", fmt.Sprint(len(zipBytes)))
			_, _ = w.Write(zipBytes[:16])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(500 * time.Millisecond)
			_, _ = w.Write(zipBytes[16:])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 250 * time.Millisecond
	mgr, err := NewManager(Options{
		StateDir: t.TempDir(), CurrentVersion: "1.1.1", BuildID: "old",
		GitHubRepo: "org/kwp", GitHubAPIBase: srv.URL, PublicKey: pub, HTTPClient: client,
		DisableAutoRun: true, ValidateStaged: func(string) error { return nil },
		IdleCheck: func() (bool, string) { return false, "waiting for the regression test" },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if _, err := mgr.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.StartDownloadAndApply(); err != nil {
		t.Fatal(err)
	}
	waitUntilState(t, mgr, "waiting_idle", 5*time.Second)
	mgr.Cancel()
}
