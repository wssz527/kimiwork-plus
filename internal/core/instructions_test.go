package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRules(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.md")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	return path
}

func TestReadInstructionsReturnsTrimmedText(t *testing.T) {
	path := writeRules(t, []byte("\n  stay in scope  \n"))
	text, err := ReadInstructions(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if text != "stay in scope" {
		t.Fatalf("text %q", text)
	}
}

// A 0644 file is the normal case for something the user wrote by hand, which is
// why this reader does not go through ReadPrivateFile and its 0600 requirement.
func TestReadInstructionsAcceptsGroupAndWorldReadable(t *testing.T) {
	path := writeRules(t, []byte("rules"))
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if text, err := ReadInstructions(path); err != nil || text != "rules" {
		t.Fatalf("text %q err %v", text, err)
	}
}

func TestReadInstructionsStripsOneLeadingBOM(t *testing.T) {
	path := writeRules(t, append([]byte("\xef\xbb\xbf"), []byte("rules")...))
	text, err := ReadInstructions(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if text != "rules" {
		t.Fatalf("text %q", text)
	}
}

// A missing file is not a failure: the caller keeps the cleanup path that strips
// a managed message from a history.
func TestReadInstructionsMissingFileIsEmpty(t *testing.T) {
	text, err := ReadInstructions(filepath.Join(t.TempDir(), "absent.md"))
	if err != nil || text != "" {
		t.Fatalf("text %q err %v", text, err)
	}
}

func TestReadInstructionsRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.md")
	if err := os.WriteFile(target, []byte("rules"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "rules.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	text, err := ReadInstructions(link)
	if !errors.Is(err, ErrInstructionsUnavailable) || text != "" {
		t.Fatalf("text %q err %v", text, err)
	}
}

func TestReadInstructionsRefusesDirectory(t *testing.T) {
	if _, err := ReadInstructions(t.TempDir()); !errors.Is(err, ErrInstructionsUnavailable) {
		t.Fatalf("err %v", err)
	}
}

func TestReadInstructionsRefusesInvalidUTF8(t *testing.T) {
	path := writeRules(t, []byte{0x72, 0x75, 0x6c, 0x65, 0x73, 0xff, 0xfe})
	if _, err := ReadInstructions(path); !errors.Is(err, ErrInstructionsUnavailable) {
		t.Fatalf("err %v", err)
	}
}

func TestReadInstructionsBoundsSize(t *testing.T) {
	exact := writeRules(t, []byte(strings.Repeat("a", InstructionsLimit)))
	if text, err := ReadInstructions(exact); err != nil || len(text) != InstructionsLimit {
		t.Fatalf("at limit: len %d err %v", len(text), err)
	}
	over := writeRules(t, []byte(strings.Repeat("a", InstructionsLimit+1)))
	if _, err := ReadInstructions(over); !errors.Is(err, ErrInstructionsUnavailable) {
		t.Fatalf("over limit: err %v", err)
	}
}

// The provider runs per request, so an edit between two reads has to be visible
// without rebuilding anything.
func TestReadInstructionsSeesLaterEdit(t *testing.T) {
	path := writeRules(t, []byte("first"))
	if text, _ := ReadInstructions(path); text != "first" {
		t.Fatalf("first read %q", text)
	}
	if err := os.WriteFile(path, []byte("second"), 0644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if text, _ := ReadInstructions(path); text != "second" {
		t.Fatalf("second read %q", text)
	}
}
