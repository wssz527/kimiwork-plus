package kimi

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// EntryPath is the official main-process bundle inside app.asar. Nothing else
// in the archive is read.
const EntryPath = "out/main/index.js"

const (
	maxHeaderBytes = 64 * 1024 * 1024
	maxEntryBytes  = 512 * 1024 * 1024
)

var ErrAsarUnreadable = errors.New("asar_unreadable")

// ReadAsarEntry extracts one file from an asar archive.
//
// The asar header is a pickle: a 4-byte size, a 4-byte payload size and a
// 4-byte length, then the JSON directory index. File bytes start after the
// whole header, so offsets in the index are relative to that base.
func ReadAsarEntry(file, entryPath string) (string, error) {
	if entryPath == "" {
		entryPath = EntryPath
	}
	handle, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer handle.Close()

	head := make([]byte, 16)
	if _, err := io.ReadFull(handle, head); err != nil {
		return "", ErrAsarUnreadable
	}
	jsonSize := binary.LittleEndian.Uint32(head[12:])
	if jsonSize == 0 || jsonSize > maxHeaderBytes {
		return "", ErrAsarUnreadable
	}
	document := make([]byte, jsonSize)
	if _, err := io.ReadFull(handle, document); err != nil {
		return "", ErrAsarUnreadable
	}
	base := int64(8) + int64(binary.LittleEndian.Uint32(head[4:]))

	var node map[string]any
	if json.Unmarshal(document, &node) != nil {
		return "", ErrAsarUnreadable
	}
	current := node
	for _, part := range strings.Split(entryPath, "/") {
		files, ok := current["files"].(map[string]any)
		if !ok {
			return "", ErrAsarUnreadable
		}
		next, ok := files[part].(map[string]any)
		if !ok {
			return "", ErrAsarUnreadable
		}
		current = next
	}
	size, sizeOK := number(current["size"])
	offset, offsetOK := number(current["offset"])
	if !sizeOK || !offsetOK || size <= 0 || size > maxEntryBytes {
		return "", ErrAsarUnreadable
	}
	body := make([]byte, size)
	if _, err := handle.ReadAt(body, base+offset); err != nil {
		return "", ErrAsarUnreadable
	}
	return string(body), nil
}

// ListAsarEntries lists regular file paths under dirPrefix (slash-separated,
// e.g. "out/preload") inside an asar archive without extracting file bodies.
func ListAsarEntries(file, dirPrefix string) ([]string, error) {
	handle, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	head := make([]byte, 16)
	if _, err := io.ReadFull(handle, head); err != nil {
		return nil, ErrAsarUnreadable
	}
	jsonSize := binary.LittleEndian.Uint32(head[12:])
	if jsonSize == 0 || jsonSize > maxHeaderBytes {
		return nil, ErrAsarUnreadable
	}
	document := make([]byte, jsonSize)
	if _, err := io.ReadFull(handle, document); err != nil {
		return nil, ErrAsarUnreadable
	}
	var node map[string]any
	if json.Unmarshal(document, &node) != nil {
		return nil, ErrAsarUnreadable
	}
	current := node
	prefix := strings.Trim(dirPrefix, "/")
	if prefix != "" {
		for _, part := range strings.Split(prefix, "/") {
			files, ok := current["files"].(map[string]any)
			if !ok {
				return nil, ErrAsarUnreadable
			}
			next, ok := files[part].(map[string]any)
			if !ok {
				return nil, ErrAsarUnreadable
			}
			current = next
		}
	}
	var out []string
	var walk func(n map[string]any, p string)
	walk = func(n map[string]any, p string) {
		files, ok := n["files"].(map[string]any)
		if !ok {
			return
		}
		for name, raw := range files {
			child, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			full := name
			if p != "" {
				full = p + "/" + name
			}
			if _, hasChildren := child["files"]; hasChildren {
				walk(child, full)
				continue
			}
			out = append(out, full)
		}
	}
	walk(current, prefix)
	return SortedIDs(out), nil
}

func number(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	}
	return 0, false
}

// HashFile is the archive digest reported next to a probe result.
func HashFile(file string) (string, error) {
	handle, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, handle); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

var plistVersion = regexp.MustCompile(`<key>CFBundleShortVersionString</key>\s*<string>([^<]+)</string>`)

// InstalledVersion prefers the archive's own package.json, then the macOS
// bundle plist. An unreadable install answers empty, never a guess.
func InstalledVersion(archiveFile string) string {
	if text, err := ReadAsarEntry(archiveFile, "package.json"); err == nil {
		var manifest struct {
			Version string `json:"version"`
		}
		if json.Unmarshal([]byte(text), &manifest) == nil && manifest.Version != "" && len(manifest.Version) <= 128 {
			return manifest.Version
		}
	}
	plist := filepath.Join(filepath.Dir(filepath.Dir(archiveFile)), "Info.plist")
	if data, err := os.ReadFile(plist); err == nil {
		if match := plistVersion.FindSubmatch(data); match != nil {
			return string(match[1])
		}
	}
	return ""
}
