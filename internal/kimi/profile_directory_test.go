package kimi

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOfficialWorkHomeResolvesBeforeProfileCreation(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("official profile is not supported on this platform")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)
	profile, err := OfficialProfileDirectory()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(ControllerSettings{StateDir: filepath.Join(dir, "state"), UserDataDir: profile})
	if err != nil {
		t.Fatal(err)
	}
	home, err := controller.WorkHome()
	if err != nil || home != filepath.Join(profile, "daimon-share", "daimon") {
		t.Fatalf("home = %q, err = %v", home, err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("the missing profile was created: %v", err)
	}
}

func TestOfficialStartWithAnUninitializedProfile(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("official profile is not supported on this platform")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)
	profile, err := OfficialProfileDirectory()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(ControllerSettings{StateDir: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := controller.validateOfficialProfile(profile); err != nil || got != profile {
		t.Fatalf("cold profile validation: %q, %v", got, err)
	}
	if err := controller.StartOfficial(context.Background()); err == nil || err.Error() != "kimi_application_missing" {
		t.Fatalf("start should proceed to the installation check, got %v", err)
	}
	if _, err := os.Lstat(profile); !os.IsNotExist(err) {
		t.Fatalf("a refused launch created the profile: %v", err)
	}
}

func TestProfileDirectoryValidation(t *testing.T) {
	dir := t.TempDir()
	if err := validateExistingProfileDirectory(dir); err != nil {
		t.Fatalf("valid directory: %v", err)
	}
	for _, path := range []string{"relative", filepath.Join(dir, "missing")} {
		if err := validateExistingProfileDirectory(path); err == nil {
			t.Fatalf("invalid directory accepted: %q", path)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := validateExistingProfileDirectory(link); err == nil {
		t.Fatal("a symlinked directory was accepted")
	}
}
