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
	"errors"
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

	"github.com/wailsapp/wails/v3/pkg/updater"
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

func TestCancelledRollbackCannotCancelImmediateRetry(t *testing.T) {
	stateDir := t.TempDir()
	backupPath := filepath.Join(stateDir, "backup.app")
	if err := os.WriteFile(backupPath, []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, rollbackDirName, rollbackPrevName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := core.AtomicJSON(filepath.Join(stateDir, rollbackDirName, rollbackPrevName, rollbackMetaFile), RollbackMetadata{
		Version:    "1.1.3",
		BuildID:    "1.1.3-test",
		TargetPath: backupPath,
		BackupPath: backupPath,
	}); err != nil {
		t.Fatal(err)
	}

	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	mgr, err := NewManager(Options{
		StateDir:       stateDir,
		CurrentVersion: "1.1.4",
		BuildID:        "1.1.4-test",
		DisableAutoRun: true,
		IdleCheck: func() (bool, string) {
			switch calls.Add(1) {
			case 1:
				close(firstEntered)
				<-releaseFirst
			case 2:
				close(secondEntered)
			}
			return false, "busy"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	if _, err := mgr.StartRollback(); err != nil {
		t.Fatal(err)
	}
	<-firstEntered
	mgr.Cancel()
	if _, err := mgr.StartRollback(); err != nil {
		t.Fatal(err)
	}
	<-secondEntered
	close(releaseFirst)
	time.Sleep(100 * time.Millisecond)

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.opCancel == nil || mgr.state != "waiting_idle" {
		t.Fatalf("old rollback cleanup cancelled its replacement: state=%s opCancelPresent=%v", mgr.state, mgr.opCancel != nil)
	}
}

func TestStaleUpdaterEventsDoNotOverwriteReplacementOperation(t *testing.T) {
	mgr := &Manager{state: "waiting_idle", operationID: 2}
	host := &managerHost{mgr: mgr, operationID: 1}
	host.Emit(updater.EventDownloadProgress, updater.Progress{Written: 9, Total: 10})
	host.Emit(updater.EventVerifying)
	if mgr.state != "waiting_idle" || mgr.progress != nil {
		t.Fatalf("stale updater events changed replacement operation: state=%s progress=%#v", mgr.state, mgr.progress)
	}
	host.operationID = 2
	host.Emit(updater.EventVerifying)
	if mgr.state != "verifying" {
		t.Fatalf("current updater event was rejected: state=%s", mgr.state)
	}
}

func TestCancelledDownloadCannotCorruptImmediateRetry(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist":           []byte("<plist/>"),
		"Contents/MacOS/KimiWorkPlus":   bytes.Repeat([]byte("a"), 128<<10),
		"Contents/MacOS/gateway-daemon": []byte("stub"),
	})

	var attempts atomic.Int32
	firstChunkSent := make(chan struct{})
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			doc := testManifestDoc{
				SchemaVersion: 1,
				Version:       "1.1.5",
				Channel:       "stable",
				Name:          "KimiWork + 1.1.5",
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
		case "/pkg.zip":
			if attempts.Add(1) == 1 {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
				half := len(zipBytes) / 2
				_, _ = w.Write(zipBytes[:half])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				close(firstChunkSent)
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mgr, err := NewManager(Options{
		StateDir:       t.TempDir(),
		CurrentVersion: "1.1.4",
		BuildID:        "1.1.4-test",
		TargetPath:     filepath.Join(t.TempDir(), "nonexistent.app"),
		FeedURL:        srv.URL + "/manifest.json",
		PublicKey:      pub,
		HTTPClient:     srv.Client(),
		DisableAutoRun: true,
		ValidateStaged: func(string) error { return nil },
		IdleCheck:      func() (bool, string) { return false, "busy" },
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
	<-firstChunkSent

	cancelled := mgr.Cancel()
	if cancelled["state"] != "available" || cancelled["progress"] != nil {
		t.Fatalf("expected clean available state after cancel, got state=%v progress=%v", cancelled["state"], cancelled["progress"])
	}

	if _, err := mgr.StartDownloadAndApply(); err != nil {
		t.Fatal(err)
	}
	waitUntilState(t, mgr, "waiting_idle", 5*time.Second)

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.state != "waiting_idle" || mgr.opCancel == nil || mgr.lastErr != nil {
		t.Fatalf("stale download corrupted retry: state=%s opCancel=%v err=%#v", mgr.state, mgr.opCancel != nil, mgr.lastErr)
	}
}

func TestCancelDuringVerifyingResetsStateAndAllowsRetry(t *testing.T) {
	cases := []struct {
		name           string
		firstErr       error
		immediateRetry bool
	}{
		{name: "old_validate_errors", firstErr: errors.New("stale codesign failure"), immediateRetry: false},
		{name: "old_validate_succeeds", firstErr: nil, immediateRetry: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
				"Contents/Info.plist":           []byte("<plist/>"),
				"Contents/MacOS/KimiWorkPlus":   []byte("stub"),
				"Contents/MacOS/gateway-daemon": []byte("stub"),
			})

			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/manifest.json":
					doc := testManifestDoc{
						SchemaVersion: 1,
						Version:       "1.1.5",
						Channel:       "stable",
						Name:          "KimiWork + 1.1.5",
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
				case "/pkg.zip":
					_, _ = w.Write(zipBytes)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			var validateCalls atomic.Int32
			var firstStagedDir atomic.Value
			firstEntered := make(chan struct{})
			releaseFirst := make(chan struct{})

			mgr, err := NewManager(Options{
				StateDir:       t.TempDir(),
				CurrentVersion: "1.1.4",
				BuildID:        "1.1.4-test",
				TargetPath:     filepath.Join(t.TempDir(), "nonexistent.app"),
				FeedURL:        srv.URL + "/manifest.json",
				PublicKey:      pub,
				HTTPClient:     srv.Client(),
				DisableAutoRun: true,
				ValidateStaged: func(stagedPath string) error {
					if validateCalls.Add(1) == 1 {
						firstStagedDir.Store(filepath.Dir(stagedPath))
						close(firstEntered)
						<-releaseFirst
						return tc.firstErr
					}
					return nil
				},
				IdleCheck: func() (bool, string) { return false, "busy" },
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
			<-firstEntered

			if got := mgr.State()["state"]; got != "verifying" {
				t.Fatalf("expected verifying state during validation, got %v", got)
			}

			cancelled := mgr.Cancel()
			if cancelled["state"] != "available" || cancelled["progress"] != nil {
				t.Fatalf("expected verifying cancel to reset to available, got state=%v progress=%v", cancelled["state"], cancelled["progress"])
			}

			if tc.immediateRetry {
				if _, err := mgr.StartDownloadAndApply(); err != nil {
					t.Fatal(err)
				}
				waitUntilState(t, mgr, "waiting_idle", 5*time.Second)
			}

			close(releaseFirst)
			stagedDir, _ := firstStagedDir.Load().(string)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(stagedDir); os.IsNotExist(err) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := os.Stat(stagedDir); !os.IsNotExist(err) {
				t.Fatalf("stale verifying staged directory was not cleaned up: %s", stagedDir)
			}
			time.Sleep(50 * time.Millisecond)

			if tc.immediateRetry {
				mgr.mu.Lock()
				state, opCancel, lastErr := mgr.state, mgr.opCancel, mgr.lastErr
				mgr.mu.Unlock()
				if state != "waiting_idle" || opCancel == nil || lastErr != nil {
					t.Fatalf("stale verify completion corrupted retry: state=%s opCancel=%v err=%#v", state, opCancel != nil, lastErr)
				}
				return
			}

			st := mgr.State()
			if st["state"] != "available" || st["error"] != nil {
				t.Fatalf("stale verify error overwrote cancelled state: state=%v err=%v", st["state"], st["error"])
			}
			if _, err := mgr.StartDownloadAndApply(); err != nil {
				t.Fatal(err)
			}
			waitUntilState(t, mgr, "waiting_idle", 5*time.Second)
		})
	}
}

func TestCancelDuringApplyingDoesNotSwallowError(t *testing.T) {
	mgr, err := NewManager(Options{
		StateDir:       t.TempDir(),
		CurrentVersion: "1.1.4",
		BuildID:        "1.1.4-test",
		DisableAutoRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	opCtx, opCancel := context.WithCancel(context.Background())
	defer opCancel()

	mgr.mu.Lock()
	mgr.state = "applying"
	mgr.operationID = 7
	mgr.opCancel = opCancel
	mgr.mu.Unlock()

	st := mgr.Cancel()
	if st["state"] != "applying" {
		t.Fatalf("expected applying state to remain uncancelled, got %v", st["state"])
	}

	mgr.mu.Lock()
	gotOpID := mgr.operationID
	gotCancel := mgr.opCancel
	mgr.mu.Unlock()
	if gotOpID != 7 || gotCancel == nil || opCtx.Err() != nil {
		t.Fatalf("Cancel mutated applying operation: operationID=%d opCancelPresent=%v ctxErr=%v", gotOpID, gotCancel != nil, opCtx.Err())
	}

	mgr.setError(7, "install", errors.New("update_backup_failed: disk full"))
	after := mgr.State()
	if after["state"] != "error" || after["error"] == nil {
		t.Fatalf("expected applying error to be preserved after Cancel, got state=%v err=%v", after["state"], after["error"])
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

func TestPreviewManifestCheckUsesMirrorFallback(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist": []byte("<plist/>"),
	})

	for _, tc := range []struct {
		name       string
		failSource string
		wantSource string
	}{
		{name: "github_manifest_timeout", failSource: "github", wantSource: "cnb"},
		{name: "cnb_manifest_timeout", failSource: "cnb", wantSource: "github"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ghAPIHits atomic.Int32
			var ghManifestHits atomic.Int32
			var cnbManifestHits atomic.Int32
			var ghSrv *httptest.Server
			manifest := func(artifactURL string) testManifestDoc {
				return testManifestDoc{
					SchemaVersion: 1,
					Version:       "1.1.6-preview.2",
					Channel:       "preview",
					PublishedAt:   time.Now().UTC().Format(time.RFC3339),
					Artifacts: []testManifestArtifact{{
						URL: artifactURL, Filename: "pkg.zip", Filetype: "zip",
						Size: int64(len(zipBytes)), Platform: runtime.GOOS, Arch: runtime.GOARCH,
						DigestAlgo: "sha256", Digest: digestB64, SignatureAlgo: "ed25519", Signature: sigB64,
					}},
				}
			}
			ghSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/org/kwp/releases":
					ghAPIHits.Add(1)
					_ = json.NewEncoder(w).Encode([]githubRelease{{
						TagName: "v1.1.6-preview.2", Prerelease: true,
						Assets: []githubAsset{{
							Name:               "update-manifest-preview.json",
							BrowserDownloadURL: ghSrv.URL + "/org/kwp/releases/download/v1.1.6-preview.2/update-manifest-preview.json",
						}},
					}})
				case "/org/kwp/releases/download/v1.1.6-preview.2/update-manifest-preview.json":
					ghManifestHits.Add(1)
					if tc.failSource == "github" {
						select {
						case <-r.Context().Done():
						case <-time.After(time.Second):
						}
						return
					}
					_ = json.NewEncoder(w).Encode(manifest(ghSrv.URL + "/org/kwp/releases/download/v1.1.6-preview.2/pkg.zip"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer ghSrv.Close()

			cnbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/org/kwp/-/releases/download/v1.1.6-preview.2/update-manifest-preview.json" {
					http.NotFound(w, r)
					return
				}
				cnbManifestHits.Add(1)
				if tc.failSource == "cnb" {
					select {
					case <-r.Context().Done():
					case <-time.After(time.Second):
					}
					return
				}
				_ = json.NewEncoder(w).Encode(manifest("pkg.zip"))
			}))
			defer cnbSrv.Close()

			mgr, err := NewManager(Options{
				StateDir:           t.TempDir(),
				CurrentVersion:     "1.1.6-preview.1",
				BuildID:            "preview-1",
				GitHubRepo:         "org/kwp",
				MirrorBase:         cnbSrv.URL,
				GitHubAPIBase:      ghSrv.URL,
				GitHubWebBase:      ghSrv.URL,
				CheckSourceTimeout: 100 * time.Millisecond,
				PublicKey:          pub,
				HTTPClient:         &http.Client{},
				DisableAutoRun:     true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer mgr.Close()
			if _, err := mgr.UpdateSettings(nil, "preview"); err != nil {
				t.Fatal(err)
			}
			st, err := mgr.CheckNow(context.Background())
			if err != nil {
				t.Fatalf("preview check must recover through the other source: %v", err)
			}
			avail, ok := st["available"].(*AvailableRelease)
			if !ok || avail.Version != "1.1.6-preview.2" || avail.Source != tc.wantSource || st["activeSource"] != tc.wantSource {
				t.Fatalf("unexpected preview fallback result: %#v", st)
			}
			if ghAPIHits.Load() != 1 {
				t.Fatalf("expected one release discovery call, got %d", ghAPIHits.Load())
			}
			if tc.wantSource == "cnb" && (cnbManifestHits.Load() != 1 || ghManifestHits.Load() != 0) {
				t.Fatalf("expected CNB to answer before GitHub, cnb=%d github=%d", cnbManifestHits.Load(), ghManifestHits.Load())
			}
			if tc.wantSource == "github" && (cnbManifestHits.Load() != 1 || ghManifestHits.Load() != 1) {
				t.Fatalf("expected GitHub to recover a CNB failure, cnb=%d github=%d", cnbManifestHits.Load(), ghManifestHits.Load())
			}
		})
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

func TestMirrorPriorityHitsCNBWithoutCallingGitHub(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist": []byte("<plist/>"),
	})

	var ghHits atomic.Int32
	var cnbManifestHits atomic.Int32
	var cnbDownloadHits atomic.Int32
	var ghSrv *httptest.Server
	ghSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ghHits.Add(1)
		http.NotFound(w, r)
	}))
	defer ghSrv.Close()

	var cnbSrv *httptest.Server
	cnbSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/kwp/-/releases/latest/download/update-manifest.json":
			cnbManifestHits.Add(1)
			// Even when the manifest advertises an absolute GitHub artifact URL,
			// the client should still try the CNB tag-pinned artifact URL first.
			_ = json.NewEncoder(w).Encode(testManifestDoc{
				SchemaVersion: 1,
				Version:       "1.2.0",
				Channel:       "stable",
				Name:          "Kimi Work+ 1.2.0",
				PublishedAt:   time.Now().UTC().Format(time.RFC3339),
				Artifacts: []testManifestArtifact{{
					URL:           ghSrv.URL + "/org/kwp/releases/download/v1.2.0/pkg.zip",
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
			})
		case "/org/kwp/-/releases/download/v1.2.0/pkg.zip":
			cnbDownloadHits.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(len(zipBytes)))
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cnbSrv.Close()

	mgr, err := NewManager(Options{
		StateDir:       t.TempDir(),
		CurrentVersion: "1.1.3",
		BuildID:        "b113",
		GitHubRepo:     "org/kwp",
		MirrorBase:     cnbSrv.URL,
		GitHubWebBase:  ghSrv.URL,
		PublicKey:      pub,
		HTTPClient:     cnbSrv.Client(),
		DisableAutoRun: true,
		ValidateStaged: func(string) error { return nil },
		IdleCheck:      func() (bool, string) { return false, "busy" },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	st, err := mgr.CheckNow(context.Background())
	if err != nil {
		t.Fatalf("CheckNow: %v", err)
	}
	wantReleaseURL := cnbSrv.URL + "/org/kwp/-/releases/tag/v1.2.0"
	if st["state"] != "available" || st["preferredSource"] != "cnb" || st["activeSource"] != "cnb" || st["source"] != "cnb:org/kwp" || st["releaseUrl"] != wantReleaseURL {
		t.Fatalf("unexpected state after CNB check: %#v", st)
	}
	avail, ok := st["available"].(*AvailableRelease)
	if !ok || avail.Source != "cnb" || avail.ReleaseURL != wantReleaseURL {
		t.Fatalf("unexpected available release: %#v", st["available"])
	}
	if ghHits.Load() != 0 {
		t.Fatalf("expected 0 GitHub requests on CNB hit, got %d", ghHits.Load())
	}

	if _, err := mgr.StartDownloadAndApply(); err != nil {
		t.Fatal(err)
	}
	waitUntilState(t, mgr, "waiting_idle", 5*time.Second)
	mgr.Cancel()
	if cnbDownloadHits.Load() != 1 || ghHits.Load() != 0 {
		t.Fatalf("expected CNB download=1 and GitHub hits=0, got cnb=%d gh=%d", cnbDownloadHits.Load(), ghHits.Load())
	}

	// Up-to-date check must also stop after CNB without hitting GitHub.
	mgrUpToDate, err := NewManager(Options{
		StateDir:       t.TempDir(),
		CurrentVersion: "1.2.0",
		BuildID:        "b120",
		GitHubRepo:     "org/kwp",
		MirrorBase:     cnbSrv.URL,
		GitHubWebBase:  ghSrv.URL,
		PublicKey:      pub,
		HTTPClient:     cnbSrv.Client(),
		DisableAutoRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgrUpToDate.Close()
	st2, err := mgrUpToDate.CheckNow(context.Background())
	if err != nil {
		t.Fatalf("up-to-date CheckNow: %v", err)
	}
	if st2["state"] != "up_to_date" || st2["activeSource"] != "cnb" || st2["releaseUrl"] != cnbSrv.URL+"/org/kwp/-/releases" {
		t.Fatalf("unexpected up-to-date state: %#v", st2)
	}
	if ghHits.Load() != 0 || cnbManifestHits.Load() != 2 {
		t.Fatalf("expected 0 GitHub requests and 2 CNB manifest checks, got gh=%d cnb=%d", ghHits.Load(), cnbManifestHits.Load())
	}
}

func TestMirrorCheckFallbacksToGitHub(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist": []byte("<plist/>"),
	})

	cases := []struct {
		name       string
		cnbHandler func(w http.ResponseWriter, r *http.Request)
	}{
		{
			name: "cnb_404",
			cnbHandler: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
		},
		{
			name: "cnb_500",
			cnbHandler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "mirror error", http.StatusInternalServerError)
			},
		},
		{
			name: "cnb_invalid_json",
			cnbHandler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("<html>not json</html>"))
			},
		},
		{
			name: "cnb_timeout",
			cnbHandler: func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cnbSrv := httptest.NewServer(http.HandlerFunc(tc.cnbHandler))
			defer cnbSrv.Close()

			var ghSrv *httptest.Server
			ghSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/org/kwp/releases/latest/download/update-manifest.json" {
					_ = json.NewEncoder(w).Encode(testManifestDoc{
						SchemaVersion: 1,
						Version:       "1.2.0",
						Channel:       "stable",
						PublishedAt:   time.Now().UTC().Format(time.RFC3339),
						Artifacts: []testManifestArtifact{{
							URL:           ghSrv.URL + "/org/kwp/releases/download/v1.2.0/pkg.zip",
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
					})
					return
				}
				http.NotFound(w, r)
			}))
			defer ghSrv.Close()

			mgr, err := NewManager(Options{
				StateDir:           t.TempDir(),
				CurrentVersion:     "1.1.3",
				BuildID:            "b113",
				GitHubRepo:         "org/kwp",
				MirrorBase:         cnbSrv.URL,
				GitHubWebBase:      ghSrv.URL,
				CheckSourceTimeout: 150 * time.Millisecond,
				PublicKey:          pub,
				HTTPClient:         &http.Client{},
				DisableAutoRun:     true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer mgr.Close()

			st, err := mgr.CheckNow(context.Background())
			if err != nil {
				t.Fatalf("CheckNow fallback failed: %v", err)
			}
			wantReleaseURL := ghSrv.URL + "/org/kwp/releases/tag/v1.2.0"
			if st["state"] != "available" || st["preferredSource"] != "cnb" || st["activeSource"] != "github" || st["source"] != "github:org/kwp" || st["releaseUrl"] != wantReleaseURL {
				t.Fatalf("unexpected fallback state: %#v", st)
			}
			avail := st["available"].(*AvailableRelease)
			if avail.Source != "github" || avail.ReleaseURL != wantReleaseURL {
				t.Fatalf("unexpected available release: %+v", avail)
			}
		})
	}
}

func TestMirrorDownloadFallbacksToGitHubCleanly(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist":           []byte("<plist/>"),
		"Contents/MacOS/KimiWorkPlus":   bytes.Repeat([]byte("A"), 4096),
		"Contents/MacOS/gateway-daemon": bytes.Repeat([]byte("B"), 4096),
	})

	cases := []struct {
		name            string
		cnbDownloadMode string
	}{
		{name: "cnb_midstream_drop", cnbDownloadMode: "drop"},
		{name: "cnb_stall_timeout", cnbDownloadMode: "stall"},
		{name: "cnb_truncated_eof", cnbDownloadMode: "truncate"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cnbDLHits atomic.Int32
			var ghDLHits atomic.Int32
			stallRelease := make(chan struct{})
			defer close(stallRelease)

			var ghSrv *httptest.Server
			ghSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/org/kwp/releases/download/v1.2.0/pkg.zip" {
					ghDLHits.Add(1)
					w.Header().Set("Content-Length", fmt.Sprint(len(zipBytes)))
					_, _ = w.Write(zipBytes)
					return
				}
				http.NotFound(w, r)
			}))
			defer ghSrv.Close()

			var cnbSrv *httptest.Server
			cnbSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/org/kwp/-/releases/latest/download/update-manifest.json":
					_ = json.NewEncoder(w).Encode(testManifestDoc{
						SchemaVersion: 1,
						Version:       "1.2.0",
						Channel:       "stable",
						PublishedAt:   time.Now().UTC().Format(time.RFC3339),
						Artifacts: []testManifestArtifact{{
							URL:           ghSrv.URL + "/org/kwp/releases/download/v1.2.0/pkg.zip",
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
					})
				case "/org/kwp/-/releases/download/v1.2.0/pkg.zip":
					cnbDLHits.Add(1)
					switch tc.cnbDownloadMode {
					case "drop":
						w.Header().Set("Content-Length", fmt.Sprint(len(zipBytes)))
						_, _ = w.Write(zipBytes[:32])
						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}
						if hj, ok := w.(http.Hijacker); ok {
							if conn, _, err := hj.Hijack(); err == nil {
								_ = conn.Close()
							}
						}
					case "stall":
						w.Header().Set("Content-Length", fmt.Sprint(len(zipBytes)))
						_, _ = w.Write(zipBytes[:32])
						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}
						select {
						case <-r.Context().Done():
						case <-stallRelease:
						case <-time.After(3 * time.Second):
						}
					case "truncate":
						// Flush partial payload and return cleanly without Content-Length.
						_, _ = w.Write(zipBytes[:32])
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer cnbSrv.Close()

			mgr, err := NewManager(Options{
				StateDir:             t.TempDir(),
				CurrentVersion:       "1.1.3",
				BuildID:              "b113",
				GitHubRepo:           "org/kwp",
				MirrorBase:           cnbSrv.URL,
				GitHubWebBase:        ghSrv.URL,
				DownloadStallTimeout: 200 * time.Millisecond,
				PublicKey:            pub,
				HTTPClient:           &http.Client{},
				DisableAutoRun:       true,
				ValidateStaged:       func(string) error { return nil },
				IdleCheck:            func() (bool, string) { return false, "busy" },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer mgr.Close()

			if _, err := mgr.CheckNow(context.Background()); err != nil {
				t.Fatalf("CheckNow: %v", err)
			}
			if _, err := mgr.StartDownloadAndApply(); err != nil {
				t.Fatal(err)
			}
			waitUntilState(t, mgr, "waiting_idle", 5*time.Second)

			st := mgr.State()
			if st["activeSource"] != "github" {
				t.Fatalf("expected activeSource=github after fallback download, got %#v", st["activeSource"])
			}
			if prog, ok := st["progress"].(*DownloadProgress); ok && prog != nil {
				if prog.Total > 0 && prog.Written > prog.Total {
					t.Fatalf("expected progress.Written (%d) <= progress.Total (%d)", prog.Written, prog.Total)
				}
			}
			if cnbDLHits.Load() != 1 || ghDLHits.Load() != 1 {
				t.Fatalf("expected cnbDL=1 and ghDL=1, got cnb=%d gh=%d", cnbDLHits.Load(), ghDLHits.Load())
			}
			mgr.Cancel()
		})
	}
}

func TestRelativeURLManifestWorksWithCNBAndGitHub(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist": []byte("<plist/>"),
	})
	makeRelManifest := func() testManifestDoc {
		return testManifestDoc{
			SchemaVersion: 1,
			Version:       "1.2.1",
			Channel:       "stable",
			PublishedAt:   time.Now().UTC().Format(time.RFC3339),
			Artifacts: []testManifestArtifact{{
				URL:           "pkg.zip",
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
	}

	var cnbAvailable atomic.Bool
	cnbAvailable.Store(true)

	cnbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cnbAvailable.Load() {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/org/kwp/-/releases/latest/download/update-manifest.json":
			_ = json.NewEncoder(w).Encode(makeRelManifest())
		case "/org/kwp/-/releases/latest/download/pkg.zip":
			w.Header().Set("Content-Length", fmt.Sprint(len(zipBytes)))
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cnbSrv.Close()

	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/kwp/releases/latest/download/update-manifest.json":
			_ = json.NewEncoder(w).Encode(makeRelManifest())
		case "/org/kwp/releases/latest/download/pkg.zip":
			w.Header().Set("Content-Length", fmt.Sprint(len(zipBytes)))
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghSrv.Close()

	// CNB relative manifest + relative sibling artifact download.
	mgrCNB, err := NewManager(Options{
		StateDir:       t.TempDir(),
		CurrentVersion: "1.1.3",
		BuildID:        "b113",
		GitHubRepo:     "org/kwp",
		MirrorBase:     cnbSrv.URL,
		GitHubWebBase:  ghSrv.URL,
		PublicKey:      pub,
		HTTPClient:     &http.Client{},
		DisableAutoRun: true,
		ValidateStaged: func(string) error { return nil },
		IdleCheck:      func() (bool, string) { return false, "busy" },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgrCNB.Close()

	if _, err := mgrCNB.CheckNow(context.Background()); err != nil {
		t.Fatalf("CNB relative CheckNow: %v", err)
	}
	if _, err := mgrCNB.StartDownloadAndApply(); err != nil {
		t.Fatal(err)
	}
	waitUntilState(t, mgrCNB, "waiting_idle", 5*time.Second)
	if mgrCNB.State()["activeSource"] != "cnb" {
		t.Fatalf("expected activeSource=cnb, got %#v", mgrCNB.State()["activeSource"])
	}
	mgrCNB.Cancel()

	// Disable CNB so GitHub relative manifest + relative sibling artifact is exercised.
	cnbAvailable.Store(false)
	mgrGH, err := NewManager(Options{
		StateDir:       t.TempDir(),
		CurrentVersion: "1.1.3",
		BuildID:        "b113",
		GitHubRepo:     "org/kwp",
		MirrorBase:     cnbSrv.URL,
		GitHubWebBase:  ghSrv.URL,
		PublicKey:      pub,
		HTTPClient:     &http.Client{},
		DisableAutoRun: true,
		ValidateStaged: func(string) error { return nil },
		IdleCheck:      func() (bool, string) { return false, "busy" },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgrGH.Close()

	if _, err := mgrGH.CheckNow(context.Background()); err != nil {
		t.Fatalf("GitHub relative CheckNow: %v", err)
	}
	if _, err := mgrGH.StartDownloadAndApply(); err != nil {
		t.Fatal(err)
	}
	waitUntilState(t, mgrGH, "waiting_idle", 5*time.Second)
	if mgrGH.State()["activeSource"] != "github" {
		t.Fatalf("expected activeSource=github, got %#v", mgrGH.State()["activeSource"])
	}
	mgrGH.Cancel()
}

func TestMirrorEnvVarDisableAndOverride(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("disable_via_env", func(t *testing.T) {
		t.Setenv("KIMI_WORK_PLUS_UPDATE_MIRROR_BASE", "OFF")
		mgr, err := NewManager(Options{
			StateDir:       t.TempDir(),
			CurrentVersion: "1.1.3",
			PublicKey:      pub,
			DisableAutoRun: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		st := mgr.State()
		if st["preferredSource"] != "github" || st["source"] != "github:"+DefaultGitHubRepo {
			t.Fatalf("expected mirror disabled, got %#v", st)
		}
	})

	t.Run("override_via_env", func(t *testing.T) {
		t.Setenv("KIMI_WORK_PLUS_UPDATE_MIRROR_BASE", "https://mirror.example.test/")
		mgr, err := NewManager(Options{
			StateDir:       t.TempDir(),
			CurrentVersion: "1.1.3",
			PublicKey:      pub,
			DisableAutoRun: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		st := mgr.State()
		if st["preferredSource"] != "cnb" || st["releaseUrl"] != "https://mirror.example.test/"+DefaultGitHubRepo+"/-/releases" {
			t.Fatalf("expected mirror override, got %#v", st)
		}
	})
}

func TestFeedURLDownloadNoTotalTimeoutAndSupportsStallTimeout(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist": []byte("<plist/>"),
	})

	t.Run("slow_steady_stream_exceeds_client_timeout", func(t *testing.T) {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/manifest.json":
				_ = json.NewEncoder(w).Encode(testManifestDoc{
					SchemaVersion: 1,
					Version:       "1.2.2",
					Channel:       "stable",
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
				})
			case "/pkg.zip":
				w.Header().Set("Content-Length", fmt.Sprint(len(zipBytes)))
				chunk := len(zipBytes) / 4
				if chunk == 0 {
					chunk = 1
				}
				for off := 0; off < len(zipBytes); off += chunk {
					end := off + chunk
					if end > len(zipBytes) {
						end = len(zipBytes)
					}
					_, _ = w.Write(zipBytes[off:end])
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					if end < len(zipBytes) {
						time.Sleep(110 * time.Millisecond)
					}
				}
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		client := srv.Client()
		client.Timeout = 200 * time.Millisecond
		mgr, err := NewManager(Options{
			StateDir:             t.TempDir(),
			CurrentVersion:       "1.1.3",
			FeedURL:              srv.URL + "/manifest.json",
			DownloadStallTimeout: 500 * time.Millisecond,
			PublicKey:            pub,
			HTTPClient:           client,
			DisableAutoRun:       true,
			ValidateStaged:       func(string) error { return nil },
			IdleCheck:            func() (bool, string) { return false, "busy" },
		})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		if _, err := mgr.CheckNow(context.Background()); err != nil {
			t.Fatalf("CheckNow: %v", err)
		}
		if _, err := mgr.StartDownloadAndApply(); err != nil {
			t.Fatal(err)
		}
		waitUntilState(t, mgr, "waiting_idle", 5*time.Second)
		if mgr.State()["activeSource"] != "endpoint" {
			t.Fatalf("expected activeSource=endpoint, got %#v", mgr.State()["activeSource"])
		}
		mgr.Cancel()
	})

	t.Run("stalled_stream_aborts_on_stall_timeout", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)

		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/manifest.json":
				_ = json.NewEncoder(w).Encode(testManifestDoc{
					SchemaVersion: 1,
					Version:       "1.2.2",
					Channel:       "stable",
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
				})
			case "/pkg.zip":
				w.Header().Set("Content-Length", fmt.Sprint(len(zipBytes)))
				_, _ = w.Write(zipBytes[:16])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				case <-time.After(3 * time.Second):
				}
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		mgr, err := NewManager(Options{
			StateDir:             t.TempDir(),
			CurrentVersion:       "1.1.3",
			FeedURL:              srv.URL + "/manifest.json",
			DownloadStallTimeout: 180 * time.Millisecond,
			PublicKey:            pub,
			HTTPClient:           srv.Client(),
			DisableAutoRun:       true,
			ValidateStaged:       func(string) error { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()

		if _, err := mgr.CheckNow(context.Background()); err != nil {
			t.Fatalf("CheckNow: %v", err)
		}
		if _, err := mgr.StartDownloadAndApply(); err != nil {
			t.Fatal(err)
		}
		waitUntilState(t, mgr, "error", 5*time.Second)
		errObj, ok := mgr.State()["error"].(*UpdateError)
		if !ok || errObj == nil || errObj.Code != "update_network_failed" {
			t.Fatalf("expected update_network_failed on stall, got %#v", mgr.State()["error"])
		}
	})
}

func TestWindowsWhitelistBackupSkipsUninstallAndTemps(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "backup")

	files := map[string]string{
		"KimiWorkPlus.exe":                "MZ-desktop",
		"gateway-daemon.exe":              "MZ-daemon",
		"LICENSE":                         "MIT",
		"THIRD_PARTY_NOTICES.txt":         "notices",
		"uninstall.exe":                   "MZ-uninstaller",
		"KimiWorkPlus.exe.old.12345":      "stale-old",
		"gateway-daemon.exe.next.999.tmp": "stale-tmp",
		"extra.log":                       "ignore-me",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "subdir", "nested.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyWindowsInstallationWhitelist(srcDir, dstDir); err != nil {
		t.Fatalf("copyWindowsInstallationWhitelist: %v", err)
	}

	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"KimiWorkPlus.exe", "gateway-daemon.exe", "LICENSE", "THIRD_PARTY_NOTICES.txt"} {
		if !got[want] {
			t.Fatalf("expected whitelisted file %s in backup, got %v", want, got)
		}
		raw, err := os.ReadFile(filepath.Join(dstDir, want))
		if err != nil || string(raw) != files[want] {
			t.Fatalf("backup content mismatch for %s: %q, %v", want, string(raw), err)
		}
	}
	for _, forbidden := range []string{"uninstall.exe", "KimiWorkPlus.exe.old.12345", "gateway-daemon.exe.next.999.tmp", "extra.log", "subdir"} {
		if got[forbidden] {
			t.Fatalf("expected %s to be excluded from Windows backup, got %v", forbidden, got)
		}
	}

	// Missing a required binary must fail.
	missingSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(missingSrc, "KimiWorkPlus.exe"), []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyWindowsInstallationWhitelist(missingSrc, filepath.Join(t.TempDir(), "bad")); err == nil {
		t.Fatal("expected error when gateway-daemon.exe is missing from Windows backup source")
	}
}

func TestWrapStripAuthOnMultiHopRedirect(t *testing.T) {
	client := wrapStripAuthOnRedirect(&http.Client{})

	t.Run("https_downgrade_then_same_host_http", func(t *testing.T) {
		r0, _ := http.NewRequest("GET", "https://example.test/1", nil)
		r1, _ := http.NewRequest("GET", "http://example.test/2", nil)
		r2, _ := http.NewRequest("GET", "http://example.test/3", nil)
		r2.Header.Set("Authorization", "Bearer secret")
		if err := client.CheckRedirect(r2, []*http.Request{r0, r1}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := r2.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected Authorization stripped after prior https downgrade, got %q", got)
		}
	})

	t.Run("parent_to_subdomain_then_same_subdomain", func(t *testing.T) {
		r0, _ := http.NewRequest("GET", "https://example.test/1", nil)
		r1, _ := http.NewRequest("GET", "https://sub.example.test/2", nil)
		r2, _ := http.NewRequest("GET", "https://sub.example.test/3", nil)
		r2.Header.Set("Authorization", "Bearer secret")
		if err := client.CheckRedirect(r2, []*http.Request{r0, r1}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := r2.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected Authorization stripped after cross-host hop, got %q", got)
		}
	})
}

func TestDownloadWithStallTimeoutRejectsOversizedPayloadEarly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("A"), 1024))
	}))
	defer srv.Close()

	var dst bytes.Buffer
	err := downloadURLToWriter(t.Context(), srv.Client(), srv.URL, 128, true, time.Second, &dst, nil)
	if err == nil || !strings.Contains(err.Error(), "oversized payload") {
		t.Fatalf("expected oversized payload error, got %v", err)
	}
	if dst.Len() > 128 {
		t.Fatalf("expected dst not to exceed expectedSize, wrote %d bytes", dst.Len())
	}
}

func TestCheckNowDoesNotOverwriteChannelChangeDuringInflightCheck(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, digestB64, sigB64 := makeSignedTestZip(t, priv, "Kimi Work+.app", map[string][]byte{
		"Contents/Info.plist": []byte("<plist/>"),
	})
	manifestBytes, _ := json.Marshal(testManifestDoc{
		SchemaVersion: 1,
		Version:       "1.2.9",
		Channel:       "stable",
		PublishedAt:   time.Now().UTC().Format(time.RFC3339),
		Artifacts: []testManifestArtifact{{
			URL:           "pkg.zip",
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
	})
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write(manifestBytes)
	}))
	defer srv.Close()

	mgr, err := NewManager(Options{
		StateDir:       t.TempDir(),
		CurrentVersion: "1.1.4",
		BuildID:        "1.1.4-test",
		PublicKey:      pub,
		FeedURL:        srv.URL + "/update-manifest.json",
		HTTPClient:     srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = mgr.CheckNow(t.Context())
	}()
	<-started

	// Switch channel while stable check is in flight -> stateEpoch bumps and state resets to idle.
	if _, err := mgr.UpdateSettings(nil, "preview"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	st := mgr.State()
	if st["state"] != "idle" || st["available"] != nil {
		t.Fatalf("stale in-flight CheckNow overwrote post-channel-switch state: %#v", st)
	}
}
