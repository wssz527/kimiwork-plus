package kimi

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unicode/utf16"
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
	if err := validateProfileDirectory(dir); err != nil {
		t.Fatalf("validateProfileDirectory valid directory: %v", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", filepath.Join(dir, "missing"), file} {
		if err := validateExistingProfileDirectory(path); err == nil {
			t.Fatalf("invalid directory accepted: %q", path)
		}
		if err := validateProfileDirectory(path); err == nil {
			t.Fatalf("validateProfileDirectory accepted invalid path: %q", path)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := validateExistingProfileDirectory(link); err == nil {
		t.Fatal("a symlinked directory was accepted")
	}
	if err := validateProfileDirectory(link); err == nil {
		t.Fatal("validateProfileDirectory accepted a symlinked directory")
	}
}

func buildTestMountPointBuffer(tag uint32, substitute, printName string) []byte {
	subUnits := utf16.Encode([]rune(substitute))
	printUnits := utf16.Encode([]rune(printName))
	pathUnits := make([]uint16, 0, len(subUnits)+1+len(printUnits)+1)
	pathUnits = append(pathUnits, subUnits...)
	pathUnits = append(pathUnits, 0)
	printOffset := len(pathUnits) * 2
	pathUnits = append(pathUnits, printUnits...)
	pathUnits = append(pathUnits, 0)

	dataLen := windowsMountPointHeaderSize + len(pathUnits)*2
	buf := make([]byte, windowsReparseHeaderSize+dataLen)
	binary.LittleEndian.PutUint32(buf[0:4], tag)
	binary.LittleEndian.PutUint16(buf[4:6], uint16(dataLen))
	binary.LittleEndian.PutUint16(buf[8:10], 0)
	binary.LittleEndian.PutUint16(buf[10:12], uint16(len(subUnits)*2))
	binary.LittleEndian.PutUint16(buf[12:14], uint16(printOffset))
	binary.LittleEndian.PutUint16(buf[14:16], uint16(len(printUnits)*2))
	for i, u := range pathUnits {
		binary.LittleEndian.PutUint16(buf[16+i*2:18+i*2], u)
	}
	return buf
}

func TestParseWindowsJunctionTarget(t *testing.T) {
	valid := []struct {
		name       string
		substitute string
		printName  string
		want       string
	}{
		{
			name:       "standard junction",
			substitute: `\??\D:\AppData-Moved\Kimi`,
			printName:  `D:\AppData-Moved\Kimi`,
			want:       `D:\AppData-Moved\Kimi`,
		},
		{
			name:       "blank print name",
			substitute: `\??\D:\AppData-Moved\Kimi`,
			printName:  "",
			want:       `D:\AppData-Moved\Kimi`,
		},
		{
			name:       "trailing backslash",
			substitute: `\??\D:\AppData-Moved\Kimi\`,
			printName:  `D:\AppData-Moved\Kimi\`,
			want:       `D:\AppData-Moved\Kimi`,
		},
		{
			name:       "lowercase drive and unicode",
			substitute: `\??\d:\` + "\u4e2d\u6587" + `\Kimi Data`,
			printName:  "",
			want:       `d:\` + "\u4e2d\u6587" + `\Kimi Data`,
		},
		{
			name:       "drive root",
			substitute: `\??\D:\`,
			printName:  `D:\`,
			want:       `D:\`,
		},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildTestMountPointBuffer(windowsReparseTagMountPoint, tc.substitute, tc.printName)
			got, ok := parseWindowsJunctionTarget(raw)
			if !ok || got != tc.want {
				t.Fatalf("parseWindowsJunctionTarget(%q) = %q, %v; want %q, true", tc.substitute, got, ok, tc.want)
			}
		})
	}

	rejected := []struct {
		name string
		raw  []byte
	}{
		{name: "symlink tag", raw: buildTestMountPointBuffer(0xA000000C, `\??\D:\AppData-Moved\Kimi`, `D:\AppData-Moved\Kimi`)},
		{name: "appexec tag", raw: buildTestMountPointBuffer(0x8000001B, `\??\D:\AppData-Moved\Kimi`, "")},
		{name: "af_unix tag", raw: buildTestMountPointBuffer(0x80000023, `\??\D:\AppData-Moved\Kimi`, "")},
		{name: "lx symlink tag", raw: buildTestMountPointBuffer(0xA000001D, `\??\D:\AppData-Moved\Kimi`, "")},
		{name: "unc target", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, `\??\UNC\server\share\Kimi`, "")},
		{name: "volume guid mount point", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, `\??\Volume{12345678-1234-1234-1234-1234567890ab}\`, "")},
		{name: "missing nt prefix", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, `D:\AppData-Moved\Kimi`, "")},
		{name: "drive relative", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, `\??\D:AppData-Moved`, "")},
		{name: "dotdot segment", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, `\??\D:\AppData\..\Kimi`, "")},
		{name: "dot segment", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, `\??\D:\AppData\.\Kimi`, "")},
		{name: "empty segment", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, `\??\D:\AppData\\Kimi`, "")},
		{name: "forward slash", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, `\??\D:\AppData/Kimi`, "")},
		{name: "embedded nul", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, "\\??\\D:\\AppData\x00Kimi", "")},
		{name: "control char", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, "\\??\\D:\\AppData\nKimi", "")},
		{name: "empty substitute", raw: buildTestMountPointBuffer(windowsReparseTagMountPoint, "", "")},
		{name: "short buffer", raw: make([]byte, 12)},
	}
	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			if got, ok := parseWindowsJunctionTarget(tc.raw); ok {
				t.Fatalf("unexpectedly accepted %s as %q", tc.name, got)
			}
		})
	}

	t.Run("reject/truncated data length", func(t *testing.T) {
		raw := buildTestMountPointBuffer(windowsReparseTagMountPoint, `\??\D:\AppData-Moved\Kimi`, "")
		if got, ok := parseWindowsJunctionTarget(raw[:len(raw)-2]); ok {
			t.Fatalf("accepted truncated buffer as %q", got)
		}
	})
	t.Run("reject/odd substitute length", func(t *testing.T) {
		raw := buildTestMountPointBuffer(windowsReparseTagMountPoint, `\??\D:\AppData-Moved\Kimi`, "")
		raw[10] |= 1
		if got, ok := parseWindowsJunctionTarget(raw); ok {
			t.Fatalf("accepted odd substitute length as %q", got)
		}
	})
}
