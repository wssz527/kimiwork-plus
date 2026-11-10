package core

import (
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// InstructionsLimit is the largest shared-rules file the legacy gateway read.
const InstructionsLimit = 1024 * 1024

// ErrInstructionsUnavailable is the single code every rejection reports. The
// legacy implementation collapsed a directory, an oversized file and a
// decoding failure into it, and the gateway turns it into a 503.
var ErrInstructionsUnavailable = errors.New("instructions_unavailable")

// ReadInstructions returns the shared-rules text from one local file. A missing
// file is empty text and no error: the legacy gateway reported it that way so
// the caller can still strip a managed message left in the history.
//
// Anything that is not one readable, bounded, UTF-8 regular file is refused
// with ErrInstructionsUnavailable. A symlink is refused too, which is stricter
// than the legacy fs.statSync check: the rules file is a fixed local path, and
// following a link there lets whatever created it point the gateway at any file
// on the machine.
func ReadInstructions(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", ErrInstructionsUnavailable
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > InstructionsLimit {
		return "", ErrInstructionsUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return "", ErrInstructionsUnavailable
	}
	defer file.Close()
	// The path could have been replaced between Lstat and Open; the second
	// identity check makes the mode and size above describe the bytes read.
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", ErrInstructionsUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(file, InstructionsLimit+1))
	if err != nil || int64(len(raw)) > InstructionsLimit {
		return "", ErrInstructionsUnavailable
	}
	if !utf8.Valid(raw) {
		return "", ErrInstructionsUnavailable
	}
	return strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")), nil
}
