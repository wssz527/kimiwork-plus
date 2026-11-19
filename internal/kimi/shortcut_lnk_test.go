package kimi

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

func encodeUTF16Z(s string) []byte {
	units := append(utf16.Encode([]rune(s)), 0)
	out := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[i*2:], u)
	}
	return out
}

func buildUnicodeLinkInfoLnk(localBase, commonSuffix string) []byte {
	var buf bytes.Buffer
	header := make([]byte, lnkHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], lnkHeaderSize)
	copy(header[4:20], shellLinkCLSID[:])
	binary.LittleEndian.PutUint32(header[20:24], lnkHasTargetIDList|lnkHasLinkInfo|lnkIsUnicode)
	buf.Write(header)

	// Minimal 4-byte TargetIDList (2-byte item terminator + 2-byte size prefix)
	idList := []byte{0x02, 0x00, 0x00, 0x00}
	buf.Write(idList)

	ansiBase := []byte("C:\\fallback\x00")
	ansiSuffix := []byte("\x00")
	uBase := encodeUTF16Z(localBase)
	uSuffix := encodeUTF16Z(commonSuffix)

	const infoHeaderSize = 0x24
	volOffset := infoHeaderSize
	volData := []byte{0x10, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04, 0x10, 0x00, 0x00, 0x00}
	ansiBaseOffset := volOffset + len(volData)
	ansiSuffixOffset := ansiBaseOffset + len(ansiBase)
	uBaseOffset := ansiSuffixOffset + len(ansiSuffix)
	uSuffixOffset := uBaseOffset + len(uBase)
	totalInfo := uSuffixOffset + len(uSuffix)

	info := make([]byte, totalInfo)
	binary.LittleEndian.PutUint32(info[0:4], uint32(totalInfo))
	binary.LittleEndian.PutUint32(info[4:8], infoHeaderSize)
	binary.LittleEndian.PutUint32(info[8:12], lnkVolumeAndLocalPath)
	binary.LittleEndian.PutUint32(info[12:16], uint32(volOffset))
	binary.LittleEndian.PutUint32(info[16:20], uint32(ansiBaseOffset))
	binary.LittleEndian.PutUint32(info[24:28], uint32(ansiSuffixOffset))
	binary.LittleEndian.PutUint32(info[28:32], uint32(uBaseOffset))
	binary.LittleEndian.PutUint32(info[32:36], uint32(uSuffixOffset))
	copy(info[volOffset:], volData)
	copy(info[ansiBaseOffset:], ansiBase)
	copy(info[ansiSuffixOffset:], ansiSuffix)
	copy(info[uBaseOffset:], uBase)
	copy(info[uSuffixOffset:], uSuffix)
	buf.Write(info)
	return buf.Bytes()
}

func TestParseWindowsShortcutTargetReadsUnicodeLinkInfoAndEnvBlock(t *testing.T) {
	lnk := buildUnicodeLinkInfoLnk(`D:\Apps\Kimi`, `\Kimi.exe`)
	got := parseWindowsShortcutTarget(lnk, nil)
	if got != `D:\Apps\Kimi\Kimi.exe` {
		t.Fatalf("parseWindowsShortcutTarget = %q, want D:\\Apps\\Kimi\\Kimi.exe", got)
	}

	// Build a shortcut using EnvironmentVariableDataBlock (%LOCALAPPDATA%\Programs\Kimi\Kimi.exe).
	var envBuf bytes.Buffer
	header := make([]byte, lnkHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], lnkHeaderSize)
	copy(header[4:20], shellLinkCLSID[:])
	binary.LittleEndian.PutUint32(header[20:24], lnkIsUnicode)
	envBuf.Write(header)
	block := make([]byte, lnkEnvVarBlockSize)
	binary.LittleEndian.PutUint32(block[0:4], lnkEnvVarBlockSize)
	binary.LittleEndian.PutUint32(block[4:8], lnkEnvVarBlockSig)
	copy(block[268:], encodeUTF16Z(`%LOCALAPPDATA%\Programs\Kimi\Kimi.exe`))
	envBuf.Write(block)
	envBuf.Write([]byte{0, 0, 0, 0})

	expanded := parseWindowsShortcutTarget(envBuf.Bytes(), map[string]string{"LocalAppData": `C:\Users\Alice\AppData\Local`})
	if expanded != `C:\Users\Alice\AppData\Local\Programs\Kimi\Kimi.exe` {
		t.Fatalf("expanded shortcut = %q", expanded)
	}

	for _, bad := range [][]byte{nil, make([]byte, 10), make([]byte, lnkHeaderSize)} {
		if target := parseWindowsShortcutTarget(bad, nil); target != "" {
			t.Fatalf("accepted malformed shortcut: %q", target)
		}
	}
}

// buildLinkInfoLnk writes a shortcut whose LinkInfo has a local base path and a
// common path suffix. A 0x24 header makes the Unicode offsets part of the
// structure; withUnicode false leaves them zero, as a tool that only wrote the
// ANSI strings does.
func buildLinkInfoLnk(base, suffix string, headerSize uint32, withUnicode bool) []byte {
	var buf bytes.Buffer
	header := make([]byte, lnkHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], lnkHeaderSize)
	copy(header[4:20], shellLinkCLSID[:])
	binary.LittleEndian.PutUint32(header[20:24], lnkHasLinkInfo)
	buf.Write(header)

	ansiBase := append([]byte(base), 0)
	ansiSuffix := append([]byte(suffix), 0)
	volOffset := int(headerSize)
	volData := make([]byte, 17)
	binary.LittleEndian.PutUint32(volData[0:4], uint32(len(volData)))
	binary.LittleEndian.PutUint32(volData[4:8], 3)
	binary.LittleEndian.PutUint32(volData[12:16], 16)

	ansiBaseOffset := volOffset + len(volData)
	ansiSuffixOffset := ansiBaseOffset + len(ansiBase)
	unicodeBaseOffset := ansiSuffixOffset + len(ansiSuffix)
	uBase := encodeUTF16Z(base)
	uSuffix := encodeUTF16Z(suffix)
	total := unicodeBaseOffset
	if withUnicode {
		total += len(uBase) + len(uSuffix)
	}

	info := make([]byte, total)
	binary.LittleEndian.PutUint32(info[0:4], uint32(total))
	binary.LittleEndian.PutUint32(info[4:8], headerSize)
	binary.LittleEndian.PutUint32(info[8:12], lnkVolumeAndLocalPath)
	binary.LittleEndian.PutUint32(info[12:16], uint32(volOffset))
	binary.LittleEndian.PutUint32(info[16:20], uint32(ansiBaseOffset))
	binary.LittleEndian.PutUint32(info[24:28], uint32(ansiSuffixOffset))
	if withUnicode {
		binary.LittleEndian.PutUint32(info[28:32], uint32(unicodeBaseOffset))
		binary.LittleEndian.PutUint32(info[32:36], uint32(unicodeBaseOffset+len(uBase)))
	}
	copy(info[volOffset:], volData)
	copy(info[ansiBaseOffset:], ansiBase)
	copy(info[ansiSuffixOffset:], ansiSuffix)
	if withUnicode {
		copy(info[unicodeBaseOffset:], uBase)
		copy(info[unicodeBaseOffset+len(uBase):], uSuffix)
	}
	buf.Write(info)
	return buf.Bytes()
}

// buildEnvBlockLnkWithStringData places both optional string data and an
// EnvironmentVariableDataBlock after the header, the shape Windows writes for a
// shortcut that also records a description and working directory.
func buildEnvBlockLnkWithStringData(target string) []byte {
	var buf bytes.Buffer
	header := make([]byte, lnkHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], lnkHeaderSize)
	copy(header[4:20], shellLinkCLSID[:])
	binary.LittleEndian.PutUint32(header[20:24], lnkHasName|lnkHasRelativePath|lnkHasWorkingDir|
		lnkHasArguments|lnkHasIconLocation|lnkIsUnicode)
	buf.Write(header)

	writeString := func(value string) {
		units := utf16.Encode([]rune(value))
		var count [2]byte
		binary.LittleEndian.PutUint16(count[:], uint16(len(units)))
		buf.Write(count[:])
		for _, unit := range units {
			var pair [2]byte
			binary.LittleEndian.PutUint16(pair[:], unit)
			buf.Write(pair[:])
		}
	}
	for _, value := range []string{"Kimi", `..\Kimi\Kimi.exe`, `C:\Program Files\Kimi`, "--profile", "Kimi.exe"} {
		writeString(value)
	}

	block := make([]byte, lnkEnvVarBlockSize)
	binary.LittleEndian.PutUint32(block[0:4], lnkEnvVarBlockSize)
	binary.LittleEndian.PutUint32(block[4:8], lnkEnvVarBlockSig)
	copy(block[268:], encodeUTF16Z(target))
	buf.Write(block)
	buf.Write([]byte{0, 0, 0, 0})
	return buf.Bytes()
}

func TestParseWindowsShortcutTargetReadsANSIPathAndSkipsOptionalStrings(t *testing.T) {
	ansi := buildLinkInfoLnk(`C:\Apps\Kimi`, `\Kimi.exe`, 0x1c, false)
	if got := parseWindowsShortcutTarget(ansi, nil); got != `C:\Apps\Kimi\Kimi.exe` {
		t.Fatalf("ANSI LinkInfo = %q", got)
	}

	// A 0x24 header whose Unicode offsets are zero points back into the header.
	// Reading those bytes as a string would answer garbage instead of falling
	// back to the ANSI path that is present.
	legacy := buildLinkInfoLnk(`C:\Legacy\Kimi`, `\Kimi.exe`, 0x24, false)
	if got := parseWindowsShortcutTarget(legacy, nil); got != `C:\Legacy\Kimi\Kimi.exe` {
		t.Fatalf("0x24 header without Unicode strings = %q", got)
	}

	env := buildEnvBlockLnkWithStringData(`%LOCALAPPDATA%\Programs\Kimi\Kimi.exe`)
	expanded := parseWindowsShortcutTarget(env, map[string]string{"LocalAppData": `C:\Users\Alice\AppData\Local`})
	if expanded != `C:\Users\Alice\AppData\Local\Programs\Kimi\Kimi.exe` {
		t.Fatalf("environment block after string data = %q", expanded)
	}

	// A shortcut with neither a LinkInfo local path nor an environment block
	// names no local target; ignoring its name and arguments is the point.
	noTarget := buildEnvBlockLnkWithStringData("")
	if got := parseWindowsShortcutTarget(noTarget, nil); got != "" {
		t.Fatalf("empty target = %q", got)
	}
}

func TestWindowsShortcutFoldersUsePhysicalKnownFolders(t *testing.T) {
	folders := windowsShortcutFolders(map[string]string{
		"userprofile": `C:\Users\Alice\`,
		"Public":      `C:\Users\Public`,
		"OneDrive":    `C:\Users\Alice\OneDrive`,
		"APPDATA":     `C:\Users\Alice\AppData\Roaming`,
		"programdata": `C:\ProgramData`,
	})
	want := []string{
		`C:\Users\Alice\Desktop`,
		`C:\Users\Public\Desktop`,
		`C:\Users\Alice\OneDrive\Desktop`,
		`C:\Users\Alice\AppData\Roaming\Microsoft\Windows\Start Menu\Programs`,
		`C:\ProgramData\Microsoft\Windows\Start Menu\Programs`,
	}
	if len(folders) != len(want) {
		t.Fatalf("folders = %v", folders)
	}
	for i := range want {
		if folders[i] != want[i] {
			t.Fatalf("folder %d = %q, want %q", i, folders[i], want[i])
		}
	}
	if got := windowsShortcutFolders(map[string]string{"USERPROFILE": ""}); len(got) != 0 {
		t.Fatalf("empty profile produced %v", got)
	}
}

func TestExpandWindowsEnvKeepsUnknownVariables(t *testing.T) {
	env := map[string]string{"USERPROFILE": `C:\Users\Alice`}
	got := expandWindowsEnv(`%USERPROFILE%\Kimi\%MISSING%\Kimi.exe`, env)
	if got != `C:\Users\Alice\Kimi\%MISSING%\Kimi.exe` {
		t.Fatalf("expandWindowsEnv = %q", got)
	}
	if got := expandWindowsEnv(`%UNCLOSED`, env); got != `%UNCLOSED` {
		t.Fatalf("unclosed variable = %q", got)
	}
}
