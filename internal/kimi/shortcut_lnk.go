package kimi

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

var shellLinkCLSID = [16]byte{
	0x01, 0x14, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00,
	0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46,
}

const (
	lnkHeaderSize         = 0x4c
	lnkHasTargetIDList    = 1 << 0
	lnkHasLinkInfo        = 1 << 1
	lnkHasName            = 1 << 2
	lnkHasRelativePath    = 1 << 3
	lnkHasWorkingDir      = 1 << 4
	lnkHasArguments       = 1 << 5
	lnkHasIconLocation    = 1 << 6
	lnkIsUnicode          = 1 << 7
	lnkVolumeAndLocalPath = 1 << 0
	lnkEnvVarBlockSig     = 0xa0000001
	lnkEnvVarBlockSize    = 0x00000314
)

// windowsShortcutFolders is where the installer puts its shortcut and where a
// user's copy would be. These are the physical folder names, which stay English
// on every display language; only the names Explorer shows are localized.
func windowsShortcutFolders(env map[string]string) []string {
	var folders []string
	if profile, ok := lookupCaseInsensitive(env, "USERPROFILE"); ok && profile != "" {
		folders = append(folders, winJoin(profile, "Desktop"))
	}
	if publicDir, ok := lookupCaseInsensitive(env, "PUBLIC"); ok && publicDir != "" {
		folders = append(folders, winJoin(publicDir, "Desktop"))
	}
	// Desktop backup moves the per-user Desktop under OneDrive; the profile
	// folder itself then has no Desktop at all.
	if oneDrive, ok := lookupCaseInsensitive(env, "ONEDRIVE"); ok && oneDrive != "" {
		folders = append(folders, winJoin(oneDrive, "Desktop"))
	}
	if appData, ok := lookupCaseInsensitive(env, "APPDATA"); ok && appData != "" {
		folders = append(folders, winJoin(appData, "Microsoft", "Windows", "Start Menu", "Programs"))
	}
	if progData, ok := lookupCaseInsensitive(env, "PROGRAMDATA"); ok && progData != "" {
		folders = append(folders, winJoin(progData, "Microsoft", "Windows", "Start Menu", "Programs"))
	}
	return folders
}

// parseWindowsShortcutTarget extracts the local target executable path from a
// Windows Shell Link (.lnk) binary without invoking COM or PowerShell.
func parseWindowsShortcutTarget(data []byte, env map[string]string) string {
	if len(data) < lnkHeaderSize {
		return ""
	}
	if binary.LittleEndian.Uint32(data[0:4]) != lnkHeaderSize {
		return ""
	}
	if !bytes.Equal(data[4:20], shellLinkCLSID[:]) {
		return ""
	}
	flags := binary.LittleEndian.Uint32(data[20:24])
	pos := lnkHeaderSize

	if flags&lnkHasTargetIDList != 0 {
		if pos+2 > len(data) {
			return ""
		}
		idListSize := int(binary.LittleEndian.Uint16(data[pos : pos+2]))
		pos += 2 + idListSize
		if pos > len(data) {
			return ""
		}
	}

	var target string
	if flags&lnkHasLinkInfo != 0 {
		if pos+4 > len(data) {
			return ""
		}
		infoSize := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
		if infoSize < 28 || pos+infoSize > len(data) {
			return ""
		}
		info := data[pos : pos+infoSize]
		headerSize := binary.LittleEndian.Uint32(info[4:8])
		infoFlags := binary.LittleEndian.Uint32(info[8:12])
		if infoFlags&lnkVolumeAndLocalPath != 0 {
			if headerSize >= 0x24 && len(info) >= 36 {
				target = linkInfoTarget(info, headerSize,
					int(binary.LittleEndian.Uint32(info[28:32])),
					int(binary.LittleEndian.Uint32(info[32:36])), true)
			}
			if target == "" {
				target = linkInfoTarget(info, headerSize,
					int(binary.LittleEndian.Uint32(info[16:20])),
					int(binary.LittleEndian.Uint32(info[24:28])), false)
			}
		}
		pos += infoSize
	}
	if target != "" {
		return expandWindowsEnv(target, env)
	}

	isUnicode := flags&lnkIsUnicode != 0
	for _, bit := range []uint32{lnkHasName, lnkHasRelativePath, lnkHasWorkingDir, lnkHasArguments, lnkHasIconLocation} {
		if flags&bit == 0 {
			continue
		}
		if pos+2 > len(data) {
			return ""
		}
		count := int(binary.LittleEndian.Uint16(data[pos : pos+2]))
		pos += 2
		step := count
		if isUnicode {
			step = count * 2
		}
		if pos+step > len(data) {
			return ""
		}
		pos += step
	}

	for pos+4 <= len(data) {
		blockSize := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
		if blockSize < 8 || pos+blockSize > len(data) {
			break
		}
		sig := binary.LittleEndian.Uint32(data[pos+4 : pos+8])
		if sig == lnkEnvVarBlockSig && blockSize >= lnkEnvVarBlockSize {
			if u, ok := readUTF16Z(data[pos:pos+blockSize], 268); ok && u != "" {
				return expandWindowsEnv(u, env)
			}
			if a, ok := readASCIIZ(data[pos:pos+blockSize], 8); ok && a != "" {
				return expandWindowsEnv(a, env)
			}
		}
		pos += blockSize
	}
	return ""
}

// linkInfoTarget joins LocalBasePath and CommonPathSuffix, which the format
// defines as the two halves of one path. An offset that points into the LinkInfo
// header or past the structure is not a string: a shortcut whose Unicode fields
// are missing must fall back to its ANSI path instead of reading the header as
// text.
func linkInfoTarget(info []byte, headerSize uint32, baseOffset, suffixOffset int, unicode bool) string {
	read := readASCIIZ
	if unicode {
		read = readUTF16Z
	}
	if headerSize > uint32(len(info)) {
		return ""
	}
	header := int(headerSize)
	if baseOffset < header || baseOffset >= len(info) {
		return ""
	}
	base, ok := read(info, baseOffset)
	if !ok || base == "" {
		return ""
	}
	if suffixOffset < header || suffixOffset >= len(info) {
		return base
	}
	suffix, ok := read(info, suffixOffset)
	if !ok {
		return base
	}
	return base + suffix
}

func readUTF16Z(buf []byte, offset int) (string, bool) {
	if offset < 0 || offset >= len(buf) {
		return "", false
	}
	var units []uint16
	for at := offset; at+1 < len(buf); at += 2 {
		u := binary.LittleEndian.Uint16(buf[at : at+2])
		if u == 0 {
			return string(utf16.Decode(units)), true
		}
		units = append(units, u)
	}
	return "", false
}

func readASCIIZ(buf []byte, offset int) (string, bool) {
	if offset < 0 || offset >= len(buf) {
		return "", false
	}
	end := bytes.IndexByte(buf[offset:], 0)
	if end < 0 {
		return "", false
	}
	slice := buf[offset : offset+end]
	for _, b := range slice {
		if b < 0x20 || b > 0x7e {
			return "", false
		}
	}
	return string(slice), true
}

func expandWindowsEnv(path string, env map[string]string) string {
	if !strings.Contains(path, "%") || len(env) == 0 {
		return path
	}
	var out strings.Builder
	for i := 0; i < len(path); {
		if path[i] != '%' {
			out.WriteByte(path[i])
			i++
			continue
		}
		end := strings.IndexByte(path[i+1:], '%')
		if end <= 0 {
			out.WriteByte(path[i])
			i++
			continue
		}
		key := path[i+1 : i+1+end]
		if val, ok := lookupCaseInsensitive(env, key); ok && val != "" {
			out.WriteString(val)
			i += end + 2
			continue
		}
		out.WriteString(path[i : i+end+2])
		i += end + 2
	}
	return out.String()
}

func lookupCaseInsensitive(env map[string]string, wanted string) (string, bool) {
	if val, ok := env[wanted]; ok {
		return val, true
	}
	for k, v := range env {
		if strings.EqualFold(k, wanted) {
			return v, true
		}
	}
	return "", false
}
