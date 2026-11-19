package platform

import (
	"runtime"
	"testing"
)

func TestDefaultStateDirUsesFormalIdentity(t *testing.T) {
	dir, err := DefaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		t.Fatal("empty state dir")
	}
	if contains(dir, "kimi-work-gateway") || contains(dir, "preview") {
		t.Fatalf("state dir should use formal kimi-work-plus identity: %s", dir)
	}
	if !contains(dir, StateDirName) {
		t.Fatalf("state dir missing %s: %s", StateDirName, dir)
	}
	legacy, err := LegacyPreviewStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(legacy, LegacyPreviewStateDirName) || legacy == dir {
		t.Fatalf("legacy preview dir unexpected: %s vs %s", legacy, dir)
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
