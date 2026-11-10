package platform

import (
	"runtime"
	"testing"
)

func TestDefaultStateDirIsIsolatedFromTheReleasedBuild(t *testing.T) {
	dir, err := DefaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		t.Fatal("empty state dir")
	}
	// The preview must never share a directory with the released build, or the
	// two writers would race on the same config revision journal.
	if contains(dir, "kimi-work-gateway") {
		t.Fatalf("state dir collides with the released layout: %s", dir)
	}
	if !contains(dir, "preview") {
		t.Fatalf("state dir is not identifiable as the preview: %s", dir)
	}
}

func TestDefaultStateDirHonoursTheOverride(t *testing.T) {
	explicit := t.TempDir()
	t.Setenv("KIMI_WORK_PLUS_STATE_DIR", explicit)
	dir, err := DefaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != explicit {
		t.Fatalf("override ignored: %s != %s", dir, explicit)
	}
}

func TestPlatformMatchesTheBuildTarget(t *testing.T) {
	if Platform() != runtime.GOOS {
		t.Fatalf("Platform()=%s GOOS=%s", Platform(), runtime.GOOS)
	}
}

func contains(haystack, needle string) bool {
	for at := 0; at+len(needle) <= len(haystack); at++ {
		if haystack[at:at+len(needle)] == needle {
			return true
		}
	}
	return false
}
