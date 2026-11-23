//go:build windows

package kimi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func createTestWindowsJunction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J %q %q failed: %v\n%s", link, target, err, output)
	}
	t.Cleanup(func() {
		_ = os.Remove(link)
	})
}

func TestWindowsProfileDirectoryJunctionValidation(t *testing.T) {
	base := t.TempDir()
	realTarget := filepath.Join(base, "AppData-Moved", "Kimi")
	if err := os.MkdirAll(realTarget, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("regular directory accepted", func(t *testing.T) {
		if err := validateExistingProfileDirectory(realTarget); err != nil {
			t.Fatalf("regular directory rejected: %v", err)
		}
	})

	t.Run("valid directory junction accepted and official start proceeds", func(t *testing.T) {
		appData := filepath.Join(base, "Roaming-Valid")
		if err := os.MkdirAll(appData, 0o700); err != nil {
			t.Fatal(err)
		}
		profile := filepath.Join(appData, "kimi-desktop")
		createTestWindowsJunction(t, profile, realTarget)

		info, err := os.Lstat(profile)
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() {
			t.Fatalf("expected junction Lstat to report non-dir on Go 1.23+, got mode=%v", info.Mode())
		}

		if err := validateExistingProfileDirectory(profile); err != nil {
			t.Fatalf("valid junction rejected: %v", err)
		}
		if err := validateExistingProfileDirectory(profile + `\`); err != nil {
			t.Fatalf("valid junction with trailing backslash rejected: %v", err)
		}

		t.Setenv("AppData", appData)
		fakeApp := filepath.Join(base, "missing-kimi.exe")
		controller, err := NewController(ControllerSettings{
			StateDir:     filepath.Join(base, "state-valid"),
			Installation: Installation{Application: fakeApp},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := controller.validateOfficialProfile(profile); err != nil || got != profile {
			t.Fatalf("validateOfficialProfile(%q) = %q, %v", profile, got, err)
		}
		if _, err := controller.validateOfficialProfile(realTarget); err == nil || err.Error() != "official_profile_mismatch" {
			t.Fatalf("junction target path should not spoof official profile, got %v", err)
		}
		if status := controller.ProfileStatus(); !status.CanReuseLogin {
			t.Fatalf("ProfileStatus().CanReuseLogin = false for valid junction: %+v", status)
		}
		if err := controller.StartOfficial(context.Background()); err == nil {
			t.Fatal("StartOfficial unexpectedly started a missing executable")
		}
		if status := controller.Status(context.Background()); status.State != StateFailed || status.Reason != "kimi_start_failed" {
			t.Fatalf("StartOfficial should pass profile validation and reach process start, got %+v", status)
		}
		if err := controller.RestartOfficial(context.Background()); err == nil {
			t.Fatal("RestartOfficial unexpectedly started a missing executable")
		}
		if status := controller.Status(context.Background()); status.State != StateFailed || status.Reason != "kimi_start_failed" {
			t.Fatalf("RestartOfficial should pass profile validation and reach process start, got %+v", status)
		}
		home, err := controller.WorkHome()
		if err != nil || home != filepath.Join(profile, "daimon-share", "daimon") {
			t.Fatalf("WorkHome() = %q, %v", home, err)
		}
	})

	t.Run("regular file and missing or relative paths rejected", func(t *testing.T) {
		file := filepath.Join(base, "regular-file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{file, filepath.Join(base, "missing"), "relative"} {
			if err := validateExistingProfileDirectory(path); err == nil || err.Error() != "existing_login_profile_invalid" {
				t.Fatalf("validateExistingProfileDirectory(%q) = %v, want existing_login_profile_invalid", path, err)
			}
		}
	})

	t.Run("dangling junction rejected", func(t *testing.T) {
		appData := filepath.Join(base, "Roaming-Dangling")
		if err := os.MkdirAll(appData, 0o700); err != nil {
			t.Fatal(err)
		}
		gone := filepath.Join(base, "gone-target")
		if err := os.Mkdir(gone, 0o700); err != nil {
			t.Fatal(err)
		}
		profile := filepath.Join(appData, "kimi-desktop")
		createTestWindowsJunction(t, profile, gone)
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}

		if err := validateExistingProfileDirectory(profile); err == nil || err.Error() != "existing_login_profile_invalid" {
			t.Fatalf("dangling junction = %v, want existing_login_profile_invalid", err)
		}
		t.Setenv("AppData", appData)
		fakeApp := filepath.Join(base, "missing-kimi.exe")
		controller, err := NewController(ControllerSettings{
			StateDir:     filepath.Join(base, "state-dangling"),
			Installation: Installation{Application: fakeApp},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := controller.validateOfficialProfile(profile); err == nil || err.Error() != "existing_login_profile_invalid" {
			t.Fatalf("validateOfficialProfile(dangling) = %v, want existing_login_profile_invalid", err)
		}
		if err := controller.RestartOfficial(context.Background()); err == nil || err.Error() != "existing_login_profile_invalid" {
			t.Fatalf("RestartOfficial(dangling) = %v, want existing_login_profile_invalid", err)
		}
	})

	t.Run("junction to regular file rejected", func(t *testing.T) {
		replaced := filepath.Join(base, "replaced-target")
		if err := os.Mkdir(replaced, 0o700); err != nil {
			t.Fatal(err)
		}
		junction := filepath.Join(base, "junction-to-file")
		createTestWindowsJunction(t, junction, replaced)
		if err := os.Remove(replaced); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(replaced, []byte("not-a-dir"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validateExistingProfileDirectory(junction); err == nil || err.Error() != "existing_login_profile_invalid" {
			t.Fatalf("junction to file = %v, want existing_login_profile_invalid", err)
		}
	})

	t.Run("chained junction rejected", func(t *testing.T) {
		hop1 := filepath.Join(base, "junction-hop1")
		hop2 := filepath.Join(base, "junction-hop2")
		createTestWindowsJunction(t, hop1, realTarget)
		createTestWindowsJunction(t, hop2, hop1)
		if err := validateExistingProfileDirectory(hop2); err == nil || err.Error() != "existing_login_profile_invalid" {
			t.Fatalf("chained junction = %v, want existing_login_profile_invalid", err)
		}
	})

	t.Run("windows symlinks rejected", func(t *testing.T) {
		dirLink := filepath.Join(base, "dir-symlink")
		if err := os.Symlink(realTarget, dirLink); err != nil {
			t.Skipf("windows symlink privilege unavailable: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(dirLink) })

		if err := validateExistingProfileDirectory(dirLink); err == nil || err.Error() != "existing_login_profile_invalid" {
			t.Fatalf("directory symlink = %v, want existing_login_profile_invalid", err)
		}
		if err := validateExistingProfileDirectory(dirLink + `\`); err == nil || err.Error() != "existing_login_profile_invalid" {
			t.Fatalf("trailing-slash directory symlink = %v, want existing_login_profile_invalid", err)
		}

		fileTarget := filepath.Join(base, "symlink-file-target")
		if err := os.WriteFile(fileTarget, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		fileLink := filepath.Join(base, "file-symlink")
		if err := os.Symlink(fileTarget, fileLink); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(fileLink) })
		if err := validateExistingProfileDirectory(fileLink); err == nil || err.Error() != "existing_login_profile_invalid" {
			t.Fatalf("file symlink = %v, want existing_login_profile_invalid", err)
		}

		junctionToSymlink := filepath.Join(base, "junction-to-symlink")
		createTestWindowsJunction(t, junctionToSymlink, dirLink)
		if err := validateExistingProfileDirectory(junctionToSymlink); err == nil || err.Error() != "existing_login_profile_invalid" {
			t.Fatalf("junction to symlink = %v, want existing_login_profile_invalid", err)
		}
	})
}
