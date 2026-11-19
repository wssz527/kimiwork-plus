package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func buildSyntheticSFX(t *testing.T, stub []byte, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(stub)
	zw := zip.NewWriter(&buf)
	metaRaw, _ := json.Marshal(installerMeta{
		Version:  "1.0.0",
		BuildID:  "1.0.0-test",
		StubSize: int64(len(stub)),
	})
	w, err := zw.Create(installerMetaEntry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(metaRaw); err != nil {
		t.Fatal(err)
	}
	for name, content := range entries {
		ew, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ew.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "setup.exe")
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractPayloadAndCopyUninstallerStub(t *testing.T) {
	stub := []byte("MZ-synthetic-pe-stub-bytes-0123456789")
	sfx := buildSyntheticSFX(t, stub, map[string]string{
		"KimiWorkPlus.exe":   "desktop-binary-v1",
		"gateway-daemon.exe": "daemon-binary-v1",
	})

	meta, files, err := inspectPayload(sfx)
	if err != nil {
		t.Fatalf("inspectPayload failed: %v", err)
	}
	if meta.Version != "1.0.0" || meta.BuildID != "1.0.0-test" || meta.StubSize != int64(len(stub)) {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 payload files, got %v", files)
	}

	dest := filepath.Join(t.TempDir(), "Kimi Work+")
	meta2, extracted, err := extractPayload(sfx, dest)
	if err != nil {
		t.Fatalf("extractPayload failed: %v", err)
	}
	if len(extracted) != 2 {
		t.Fatalf("expected 2 extracted files, got %v", extracted)
	}
	if _, err := os.Stat(filepath.Join(dest, installerMetaEntry)); !os.IsNotExist(err) {
		t.Fatalf("meta entry should not be extracted into install dir")
	}
	gotDesktop, _ := os.ReadFile(filepath.Join(dest, "KimiWorkPlus.exe"))
	if string(gotDesktop) != "desktop-binary-v1" {
		t.Fatalf("unexpected desktop content: %q", string(gotDesktop))
	}

	uninst := filepath.Join(dest, "uninstall.exe")
	if err := copyUninstallerStub(sfx, uninst, meta2.StubSize); err != nil {
		t.Fatalf("copyUninstallerStub failed: %v", err)
	}
	gotStub, _ := os.ReadFile(uninst)
	if !bytes.Equal(gotStub, stub) {
		t.Fatalf("uninstaller stub mismatch: got %q want %q", string(gotStub), string(stub))
	}
}

func TestExtractPayloadRejectsZipSlip(t *testing.T) {
	sfx := buildSyntheticSFX(t, []byte("MZ-stub"), map[string]string{
		"../escape.exe": "bad",
	})
	dest := filepath.Join(t.TempDir(), "install")
	if _, _, err := extractPayload(sfx, dest); err == nil {
		t.Fatal("expected zip-slip path to be rejected")
	}
}
