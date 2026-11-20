package icon

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWindowsICOEntries(t *testing.T) {
	entries, err := ParseICO(WindowsICO)
	if err != nil {
		t.Fatalf("ParseICO failed: %v", err)
	}
	got := make([]int, 0, len(entries))
	for _, e := range entries {
		if e.Width != e.Height {
			t.Fatalf("non-square icon entry: %dx%d", e.Width, e.Height)
		}
		got = append(got, e.Width)
	}
	want := []int{16, 32, 48, 64, 128, 256}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected icon sizes: got %v, want %v", got, want)
	}
}

func TestBuildAMD64ResourceSysoMatchesCommittedTargets(t *testing.T) {
	syso, err := BuildAMD64ResourceSyso(WindowsICO, AppIconResourceID)
	if err != nil {
		t.Fatalf("BuildAMD64ResourceSyso failed: %v", err)
	}
	summary, err := InspectPEIcons(syso)
	if err != nil {
		t.Fatalf("InspectPEIcons(syso) failed: %v", err)
	}
	if !slices.Equal(summary.GroupIconIDs, []uint16{AppIconResourceID}) {
		t.Fatalf("unexpected group icon IDs: got %v, want [%d]", summary.GroupIconIDs, AppIconResourceID)
	}
	if !slices.Equal(summary.IconIDs, []uint16{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("unexpected icon IDs: got %v, want [1 2 3 4 5 6]", summary.IconIDs)
	}

	targets := []string{
		filepath.Join("..", "..", "cmd", "desktop", "rsrc_windows_amd64.syso"),
		filepath.Join("..", "..", "cmd", "wininstaller", "rsrc_windows_amd64.syso"),
		filepath.Join("..", "..", "cmd", "gateway-daemon", "rsrc_windows_amd64.syso"),
	}
	for _, target := range targets {
		disk, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read %s: %v", target, err)
		}
		if !bytes.Equal(disk, syso) {
			t.Fatalf("%s is out of sync with assets/icon/kimi-work-plus.ico", target)
		}
	}
}
