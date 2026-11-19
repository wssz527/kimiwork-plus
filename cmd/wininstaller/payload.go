package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	installerMetaEntry = ".installer-meta.json"
	maxPayloadEntries  = 128
	maxPayloadBytes    = 512 << 20 // 512 MiB
)

type installerMeta struct {
	Version  string `json:"version"`
	BuildID  string `json:"buildId"`
	StubSize int64  `json:"stubSize"`
}

func inspectPayload(exePath string) (installerMeta, []string, error) {
	f, err := os.Open(exePath)
	if err != nil {
		return installerMeta{}, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return installerMeta{}, nil, err
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return installerMeta{}, nil, fmt.Errorf("installer payload missing or invalid: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > maxPayloadEntries {
		return installerMeta{}, nil, fmt.Errorf("unexpected payload entry count: %d", len(zr.File))
	}
	var meta installerMeta
	files := make([]string, 0, len(zr.File))
	for _, entry := range zr.File {
		if entry.Name == installerMetaEntry {
			rc, err := entry.Open()
			if err != nil {
				return installerMeta{}, nil, err
			}
			raw, readErr := io.ReadAll(io.LimitReader(rc, 64<<10))
			_ = rc.Close()
			if readErr != nil {
				return installerMeta{}, nil, readErr
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				return installerMeta{}, nil, err
			}
			continue
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		files = append(files, entry.Name)
	}
	return meta, files, nil
}

func extractPayload(exePath, destDir string) (installerMeta, []string, error) {
	f, err := os.Open(exePath)
	if err != nil {
		return installerMeta{}, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return installerMeta{}, nil, err
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return installerMeta{}, nil, fmt.Errorf("open payload archive: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > maxPayloadEntries {
		return installerMeta{}, nil, fmt.Errorf("unexpected payload entry count: %d", len(zr.File))
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return installerMeta{}, nil, err
	}
	cleanRoot := filepath.Clean(destDir)

	var meta installerMeta
	var total int64
	extracted := make([]string, 0, len(zr.File))

	for _, entry := range zr.File {
		if entry.Name == installerMetaEntry {
			rc, err := entry.Open()
			if err != nil {
				return installerMeta{}, nil, err
			}
			raw, readErr := io.ReadAll(io.LimitReader(rc, 64<<10))
			_ = rc.Close()
			if readErr != nil {
				return installerMeta{}, nil, readErr
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				return installerMeta{}, nil, err
			}
			continue
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return installerMeta{}, nil, fmt.Errorf("symlinks not allowed in payload: %s", entry.Name)
		}
		target, err := safePayloadJoin(cleanRoot, entry.Name)
		if err != nil {
			return installerMeta{}, nil, err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return installerMeta{}, nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return installerMeta{}, nil, err
		}

		rc, err := entry.Open()
		if err != nil {
			return installerMeta{}, nil, err
		}
		n, writeErr := writePayloadEntryAtomic(target, rc, maxPayloadBytes-total)
		_ = rc.Close()
		if writeErr != nil {
			return installerMeta{}, nil, writeErr
		}
		total += n
		if total > maxPayloadBytes {
			return installerMeta{}, nil, errors.New("payload exceeds size limit")
		}
		extracted = append(extracted, target)
	}

	if len(extracted) == 0 {
		return installerMeta{}, nil, errors.New("installer payload contained no files")
	}
	return meta, extracted, nil
}

func safePayloadJoin(root, name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) || strings.Contains(name, ":") {
		return "", fmt.Errorf("invalid payload path: %s", name)
	}
	joined := filepath.Clean(filepath.Join(root, filepath.FromSlash(name)))
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("payload path escapes target directory: %s", name)
	}
	return joined, nil
}

func writePayloadEntryAtomic(target string, r io.Reader, remaining int64) (int64, error) {
	if remaining < 0 {
		return 0, errors.New("payload exceeds size limit")
	}
	tmp := fmt.Sprintf("%s.install.%d.tmp", target, time.Now().UnixNano())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, io.LimitReader(r, remaining+1))
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil {
			return 0, copyErr
		}
		if syncErr != nil {
			return 0, syncErr
		}
		return 0, closeErr
	}
	if n > remaining {
		_ = os.Remove(tmp)
		return 0, errors.New("payload exceeds size limit")
	}
	if err := replaceTargetWithAside(target, tmp); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

func replaceTargetWithAside(target, staged string) error {
	sweepStaleAsides(target)
	if err := os.Rename(staged, target); err == nil {
		return nil
	}
	if err := os.Remove(target); err == nil {
		return os.Rename(staged, target)
	}
	aside := fmt.Sprintf("%s.old.%d", target, time.Now().UnixNano())
	if err := os.Rename(target, aside); err != nil {
		return fmt.Errorf("replace locked file %s: %w", filepath.Base(target), err)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(aside, target)
		return err
	}
	_ = os.Remove(aside)
	return nil
}

func sweepStaleAsides(target string) {
	pattern := target + ".old.*"
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, match := range matches {
		_ = os.Remove(match)
	}
}

func copyUninstallerStub(exePath, destPath string, stubSize int64) error {
	in, err := os.Open(exePath)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	copyBytes := info.Size()
	if stubSize > 0 && stubSize <= info.Size() {
		copyBytes = stubSize
	}
	tmp := fmt.Sprintf("%s.install.%d.tmp", destPath, time.Now().UnixNano())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(out, in, copyBytes)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	if err := replaceTargetWithAside(destPath, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
