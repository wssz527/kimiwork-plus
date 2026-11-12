package kimi

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func isolationFixture(t *testing.T) (string, string, string) {
	t.Helper()
	profile := t.TempDir()
	isolated := t.TempDir()
	return profile, isolated, filepath.Join(isolated, defaultSharePath)
}

func TestWorkIsolationRestoresOriginalPointer(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[existing], func(t *testing.T) {
			profile, isolated, share := isolationFixture(t)
			pointer := filepath.Join(profile, storageFileName)
			before := []byte("{\n  \"version\": 1, \"shareDir\": \"/original/work\"\n}\n")
			if existing {
				if err := os.WriteFile(pointer, before, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(profile, "login-marker")
			if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			lease, err := beginWorkIsolation(profile, isolated, share)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lease.Restore() })
			active, _, present, err := readStoragePointer(pointer)
			if err != nil || !present || !bytes.Contains(active, []byte(share)) {
				t.Fatalf("active pointer: %s %v", active, err)
			}
			if err := lease.Restore(); err != nil {
				t.Fatal(err)
			}
			restored, err := os.ReadFile(pointer)
			if existing {
				info, statErr := os.Stat(pointer)
				if err != nil || statErr != nil || !bytes.Equal(restored, before) || info.Mode().Perm() != 0o640 {
					t.Fatalf("original bytes/mode not restored: %s %v", restored, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("original absence not restored: %v", err)
			}
			data, _ := os.ReadFile(marker)
			if string(data) != "unchanged" {
				t.Fatal("login storage was modified")
			}
			if _, err := os.Stat(lease.journal); !os.IsNotExist(err) {
				t.Fatalf("journal remains: %v", err)
			}
		})
	}
}

func TestWorkIsolationExcludesOtherStateDirectories(t *testing.T) {
	profile, isolated, share := isolationFixture(t)
	lease, err := beginWorkIsolation(profile, isolated, share)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Restore()
	other := t.TempDir()
	if second, err := beginWorkIsolation(profile, other, filepath.Join(other, defaultSharePath)); err == nil {
		second.Restore()
		t.Fatal("two states acquired the same official profile")
	}
}

func TestWorkIsolationRecoversInterruptedTransaction(t *testing.T) {
	profile, isolated, share := isolationFixture(t)
	lease, err := beginWorkIsolation(profile, isolated, share)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.lock.Release(); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	recovered, err := beginWorkIsolation(profile, other, filepath.Join(other, defaultSharePath))
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profile, storageFileName)); !os.IsNotExist(err) {
		t.Fatalf("interrupted transaction became the new original: %v", err)
	}
}

func TestWorkIsolationPreservesConcurrentPointerChange(t *testing.T) {
	profile, isolated, share := isolationFixture(t)
	lease, err := beginWorkIsolation(profile, isolated, share)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.lock.Release()
	changed := []byte("{\"version\":1,\"shareDir\":\"/user/changed\"}\n")
	if err := os.WriteFile(filepath.Join(profile, storageFileName), changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lease.Restore(); !errors.Is(err, errWorkStorageConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	current, _ := os.ReadFile(filepath.Join(profile, storageFileName))
	if !bytes.Equal(current, changed) {
		t.Fatal("concurrent pointer change was overwritten")
	}
	if _, err := os.Stat(lease.journal); err != nil {
		t.Fatalf("recovery evidence lost: %v", err)
	}
}

func TestWorkIsolationRejectsSymlinkPointerAndEscapedWorkRoot(t *testing.T) {
	profile, isolated, share := isolationFixture(t)
	outside := filepath.Join(t.TempDir(), "pointer.json")
	before := []byte("{\"version\":1,\"shareDir\":\"/original/work\"}\n")
	os.WriteFile(outside, before, 0o600)
	if err := os.Symlink(outside, filepath.Join(profile, storageFileName)); err != nil {
		t.Fatal(err)
	}
	if lease, err := beginWorkIsolation(profile, isolated, share); err == nil {
		lease.Restore()
		t.Fatal("symlink pointer accepted")
	}
	current, _ := os.ReadFile(outside)
	if !bytes.Equal(current, before) {
		t.Fatal("symlink target changed")
	}
	profile = t.TempDir()
	if lease, err := beginWorkIsolation(profile, isolated, t.TempDir()); err == nil {
		lease.Restore()
		t.Fatal("escaped Work root accepted")
	}
}

func TestWorkIsolationWaitsForManagedChildExit(t *testing.T) {
	profile, isolated, share := isolationFixture(t)
	lease, err := beginWorkIsolation(profile, isolated, share)
	if err != nil {
		t.Fatal(err)
	}
	child := &Child{exitWait: make(chan struct{})}
	controller := &Controller{child: child, isolation: lease}
	if err := controller.restoreWorkIsolation(); err == nil {
		t.Fatal("restored storage while child was alive")
	}
	close(child.exitWait)
	if !controller.reapChild() {
		t.Fatal("child exit was not observed")
	}
	if _, err := os.Stat(filepath.Join(profile, storageFileName)); !os.IsNotExist(err) {
		t.Fatalf("storage not restored after exit: %v", err)
	}
	if controller.Status(context.Background()).State != StateStopped {
		t.Fatal("controller did not stop")
	}
}

func TestWorkIsolationMissingJournalDoesNotClaimRestoration(t *testing.T) {
	profile, isolated, share := isolationFixture(t)
	lease, err := beginWorkIsolation(profile, isolated, share)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.lock.Release()
	os.Remove(lease.journal)
	if err := lease.Restore(); !errors.Is(err, errWorkStorageConflict) {
		t.Fatalf("missing journal claimed restored: %v", err)
	}
}

func TestWorkIsolationRejectsOriginalWorkStorageAsTarget(t *testing.T) {
	profile, isolated, share := isolationFixture(t)
	before := []byte("{\"version\":1,\"shareDir\":" + quoteJS(share) + "}\n")
	pointer := filepath.Join(profile, storageFileName)
	if err := os.WriteFile(pointer, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if lease, err := beginWorkIsolation(profile, isolated, share); err == nil || err.Error() != "work_storage_target_conflict" {
		if lease != nil {
			lease.Restore()
		}
		t.Fatalf("original Work storage accepted as isolated: %v", err)
	}
	current, _ := os.ReadFile(pointer)
	if !bytes.Equal(current, before) {
		t.Fatal("original pointer changed")
	}
	if lease, err := beginWorkIsolation(profile, profile, filepath.Join(profile, "nested")); err == nil {
		lease.Restore()
		t.Fatal("target inside login profile was accepted")
	}
}
