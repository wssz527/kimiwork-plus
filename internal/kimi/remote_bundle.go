package kimi

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The remote companion is a copy of the installed application with its own
// app.asar. Building that archive and checking a copied bundle's signature are
// pure data and one documented tool call, so both are implemented here.
//
// What is not implemented, and why:
//
//	the companion's bootstrap       runs inside Electron's main process; it is
//	                                JavaScript by construction and is shipped as
//	                                a versioned asset, not ported
//	the update proxy                replaces Electron objects in place
//	                                (setFeedURL/checkForUpdates/quitAndInstall);
//	                                there is no Go equivalent inside that process
//	the native updater addon        drives Squirrel/Sparkle with code-signature
//	                                checks of ShipIt; that is the addon's contract
//
// So this file covers the archive and the signature check, and leaves the
// in-process pieces to the versioned assets the caller already has.

const (
	remoteAsarBlockSize = 4 * 1024 * 1024
	remoteAsarMaxEntry  = 64 << 20
)

// RemoteAsarResult is the built archive and its header digest.
type RemoteAsarResult struct {
	Data       []byte
	HeaderHash string
}

// BuildRemoteAsar renders an asar archive in the format the official client
// reads: a 16-byte outer pickle, the header pickle, then the payloads in the
// order they were listed, each entry carrying its size, offset, whole-file hash
// and per-block hashes.
//
// The entry order is part of the archive's bytes, so it is an input rather than
// an implementation detail: `order` names the entries in the sequence they are
// written. A name in `files` that is missing from `order` is refused instead of
// being appended silently, because a different order produces a different
// archive and a different header digest.
func BuildRemoteAsar(files map[string][]byte, order []string) (RemoteAsarResult, error) {
	if len(order) != len(files) {
		return RemoteAsarResult{}, errors.New("remote_bundle_order_mismatch")
	}
	seen := make(map[string]bool, len(order))
	for _, name := range order {
		if seen[name] {
			return RemoteAsarResult{}, errors.New("remote_bundle_order_mismatch")
		}
		seen[name] = true
		if _, present := files[name]; !present {
			return RemoteAsarResult{}, errors.New("remote_bundle_order_mismatch")
		}
	}
	names := order
	for name := range files {
		if !validAsarEntryName(name) {
			return RemoteAsarResult{}, errors.New("remote_bundle_entry_invalid")
		}
	}

	// The header JSON is part of the archive's identity, so its member order is
	// fixed: entries in `order`, and each entry's fields in the released build's
	// own sequence. A map would sort the names and produce a different digest.
	type integrity struct {
		Algorithm string   `json:"algorithm"`
		Hash      string   `json:"hash"`
		BlockSize int      `json:"blockSize"`
		Blocks    []string `json:"blocks"`
	}
	type entry struct {
		Size      int       `json:"size"`
		Offset    string    `json:"offset"`
		Integrity integrity `json:"integrity"`
	}
	ordered := make([]struct {
		Name  string
		Entry entry
	}, 0, len(names))
	var payloads [][]byte
	offset := 0
	for _, name := range names {
		data := files[name]
		if len(data) > remoteAsarMaxEntry {
			return RemoteAsarResult{}, errors.New("remote_bundle_entry_invalid")
		}
		blocks := make([]string, 0, len(data)/remoteAsarBlockSize+1)
		for at := 0; at < len(data); at += remoteAsarBlockSize {
			end := at + remoteAsarBlockSize
			if end > len(data) {
				end = len(data)
			}
			blocks = append(blocks, sha256HexBytes(data[at:end]))
		}
		ordered = append(ordered, struct {
			Name  string
			Entry entry
		}{Name: name, Entry: entry{Size: len(data), Offset: strconv.Itoa(offset),
			Integrity: integrity{Algorithm: "SHA256", Hash: sha256HexBytes(data),
				BlockSize: remoteAsarBlockSize, Blocks: blocks}}})
		offset += len(data)
		payloads = append(payloads, data)
	}

	var document bytes.Buffer
	document.WriteString(`{"files":{`)
	for i, item := range ordered {
		if i > 0 {
			document.WriteByte(',')
		}
		name, err := json.Marshal(item.Name)
		if err != nil {
			return RemoteAsarResult{}, errors.New("remote_bundle_entry_invalid")
		}
		encoded, err := json.Marshal(item.Entry)
		if err != nil {
			return RemoteAsarResult{}, errors.New("remote_bundle_entry_invalid")
		}
		document.Write(name)
		document.WriteByte(':')
		document.Write(encoded)
	}
	document.WriteString(`}}`)
	// The header pickle is padded to a four-byte boundary.
	payload := document.Bytes()
	padding := (4 - len(payload)%4) % 4
	header := make([]byte, 8+len(payload)+padding)
	binary.LittleEndian.PutUint32(header[0:], uint32(len(header)-4))
	binary.LittleEndian.PutUint32(header[4:], uint32(len(payload)))
	copy(header[8:], payload)

	outer := make([]byte, 8)
	binary.LittleEndian.PutUint32(outer[0:], 4)
	binary.LittleEndian.PutUint32(outer[4:], uint32(len(header)))

	var buffer bytes.Buffer
	buffer.Write(outer)
	buffer.Write(header)
	for _, payload := range payloads {
		buffer.Write(payload)
	}
	return RemoteAsarResult{Data: buffer.Bytes(), HeaderHash: sha256HexBytes(payload)}, nil
}

func validAsarEntryName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_', character == '.', character == '-':
		default:
			return false
		}
	}
	return true
}

func sha256HexBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// VerifyBundleSignature asks the platform's own verifier whether a copied
// bundle's signature is intact.
//
//	darwin  codesign --verify --strict
//
// A platform without a verifier answers false rather than true: an unverified
// bundle must never be reported as verified.
func VerifyBundleSignature(bundle string) bool {
	switch runtimeGOOS() {
	case "darwin":
		command := exec.Command("/usr/bin/codesign", "--verify", "--strict", bundle)
		command.WaitDelay = 2 * time.Second
		return command.Run() == nil
	}
	return false
}

// VerifyRemoteBundleMetadata checks the fields the companion must carry. It is
// separate from the signature because a correctly signed bundle with the wrong
// identity would still be launched as the wrong app.
func VerifyRemoteBundleMetadata(metadata map[string]any, bundleID, executableName string) bool {
	if bundleID == "" || executableName == "" {
		return false
	}
	if value, _ := metadata["CFBundleIdentifier"].(string); value != bundleID {
		return false
	}
	if value, _ := metadata["CFBundleExecutable"].(string); value != executableName {
		return false
	}
	return true
}

// RemoteBundleIdentityValid accepts only this product's bundle ids, matching the
// released build's rule.
func RemoteBundleIdentityValid(bundleID string) bool {
	const prefix = "dev.kimi-work-gateway."
	if !strings.HasPrefix(bundleID, prefix) {
		return false
	}
	rest := bundleID[len(prefix):]
	if rest == "" || len(rest) > 64 {
		return false
	}
	for _, character := range rest {
		if !(character >= 'a' && character <= 'z' || character == '-') {
			return false
		}
	}
	return true
}

// ReadRemoteAsarEntry reads one file back out of an archive this package built,
// so a caller can prove what it wrote without a second parser.
func ReadRemoteAsarEntry(archive []byte, name string) ([]byte, bool) {
	if len(archive) < 16 {
		return nil, false
	}
	headerSize := int(binary.LittleEndian.Uint32(archive[4:]))
	if headerSize <= 8 || 8+headerSize > len(archive) {
		return nil, false
	}
	header := archive[8 : 8+headerSize]
	jsonSize := int(binary.LittleEndian.Uint32(header[4:]))
	if jsonSize <= 0 || 8+jsonSize > len(header) {
		return nil, false
	}
	var document struct {
		Files map[string]struct {
			Size   int    `json:"size"`
			Offset string `json:"offset"`
		} `json:"files"`
	}
	if json.Unmarshal(header[8:8+jsonSize], &document) != nil {
		return nil, false
	}
	entry, present := document.Files[name]
	if !present || entry.Size < 0 {
		return nil, false
	}
	offsetValue, err := parseSmallInt(entry.Offset)
	if err != nil {
		return nil, false
	}
	offset := offsetValue
	base := 8 + headerSize
	if base+offset+entry.Size > len(archive) {
		return nil, false
	}
	return archive[base+offset : base+offset+entry.Size], true
}

// runtimeGOOS is a seam so a test can pose another platform.
var runtimeGOOS = func() string { return platformGOOS() }
