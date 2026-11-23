package kimi

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

const (
	windowsReparseTagMountPoint uint32 = 0xA0000003
	windowsReparseHeaderSize           = 8
	windowsMountPointHeaderSize        = 8
)

func validateProfileDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("existing_login_profile_invalid")
	}
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return errors.New("existing_login_profile_invalid")
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	return validateProfileDirectoryPlatform(clean, info)
}

func parseWindowsJunctionTarget(raw []byte) (string, bool) {
	if len(raw) < windowsReparseHeaderSize+windowsMountPointHeaderSize {
		return "", false
	}
	if binary.LittleEndian.Uint32(raw[0:4]) != windowsReparseTagMountPoint {
		return "", false
	}
	dataLen := int(binary.LittleEndian.Uint16(raw[4:6]))
	if dataLen < windowsMountPointHeaderSize || windowsReparseHeaderSize+dataLen > len(raw) {
		return "", false
	}
	subOff := int(binary.LittleEndian.Uint16(raw[8:10]))
	subLen := int(binary.LittleEndian.Uint16(raw[10:12]))
	pathBuf := raw[windowsReparseHeaderSize+windowsMountPointHeaderSize : windowsReparseHeaderSize+dataLen]
	if subLen == 0 || subOff%2 != 0 || subLen%2 != 0 || subOff+subLen > len(pathBuf) {
		return "", false
	}
	units := make([]uint16, subLen/2)
	for i := range units {
		u := binary.LittleEndian.Uint16(pathBuf[subOff+i*2 : subOff+i*2+2])
		if u == 0 {
			return "", false
		}
		units[i] = u
	}
	return normalizeWindowsJunctionTarget(string(utf16.Decode(units)))
}

func normalizeWindowsJunctionTarget(substitute string) (string, bool) {
	if !strings.HasPrefix(substitute, `\??\`) {
		return "", false
	}
	target := substitute[4:]
	if len(target) < 3 || !isASCIIAlpha(target[0]) || target[1] != ':' || target[2] != '\\' {
		return "", false
	}
	for _, r := range target {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	if strings.ContainsRune(target, '/') {
		return "", false
	}
	for len(target) > 3 && target[len(target)-1] == '\\' {
		target = target[:len(target)-1]
	}
	if len(target) > 3 {
		for _, part := range strings.Split(target[3:], `\`) {
			if part == "" || part == "." || part == ".." {
				return "", false
			}
		}
	}
	return target, true
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}
