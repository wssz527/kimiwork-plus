package kimi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// Kimi's desktop owns `config.toml` and rewrites it wholesale, dropping any field
// it does not know. Every writer in this package therefore edits only bytes it
// can prove it wrote: a marker-delimited block with an owner line and a body
// hash, or a table whose values match an ownership receipt. Nothing here
// re-serialises the document, so a foreign section, an unknown key and the
// file's own line endings survive untouched.
//
// This file is the shared TOML surface: line splitting, string scanning, key
// parsing and value rendering. It is deliberately not a general TOML parser —
// a construct it cannot read is refused rather than guessed at.

// tomlLine is one physical line with its own terminator, so the file's mixed
// endings can be reproduced byte for byte.
type tomlLine struct {
	text string
	eol  string
}

func splitTOML(raw string) []tomlLine {
	var lines []tomlLine
	at := 0
	for at < len(raw) {
		next := strings.IndexAny(raw[at:], "\r\n")
		if next < 0 {
			lines = append(lines, tomlLine{text: raw[at:]})
			break
		}
		cut := at + next
		eol := "\n"
		if raw[cut] == '\r' {
			if cut+1 < len(raw) && raw[cut+1] == '\n' {
				eol = "\r\n"
			} else {
				eol = "\r"
			}
		}
		lines = append(lines, tomlLine{text: raw[at:cut], eol: eol})
		at = cut + len(eol)
	}
	return lines
}

func joinTOML(lines []tomlLine) string {
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(line.text)
		out.WriteString(line.eol)
	}
	return out.String()
}

func tomlEOL(raw string) string {
	if strings.Contains(raw, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// tomlStringEnd returns the index just past the closing quote, or -1. A basic
// string honours backslash escapes; a literal string does not.
func tomlStringEnd(text string, at int) int {
	quote := text[at]
	for i := at + 1; i < len(text); i++ {
		if quote == '"' && text[i] == '\\' {
			i++
			continue
		}
		if text[i] == quote {
			return i + 1
		}
	}
	return -1
}

// tomlCode masks strings and comments with spaces so structure can be read.
// A multi-line string or an unterminated one is refused: the generator cannot be
// trusted to reproduce it, so guessing would corrupt the file.
func tomlCode(text string) (string, string) {
	var out strings.Builder
	at := 0
	for at < len(text) {
		character := text[at]
		if character == '#' {
			break
		}
		if character == '"' || character == '\'' {
			if strings.HasPrefix(text[at:], strings.Repeat(string(character), 3)) {
				return "", "multiline"
			}
			end := tomlStringEnd(text, at)
			if end < 0 {
				return "", "string"
			}
			out.WriteString(strings.Repeat(" ", end-at))
			at = end
			continue
		}
		out.WriteByte(character)
		at++
	}
	return out.String(), ""
}

// tomlKeySegments parses a possibly dotted, possibly quoted key.
func tomlKeySegments(text string, start, end int) []string {
	var segments []string
	at := start
	for at < end {
		for at < end && isTOMLSpace(text[at]) {
			at++
		}
		if at >= end {
			break
		}
		var segment string
		if quote := text[at]; quote == '"' || quote == '\'' {
			close := tomlStringEnd(text, at)
			if close < 0 || close > end {
				return nil
			}
			raw := text[at+1 : close-1]
			if quote == '"' {
				unescaped, ok := tomlUnescapeBasic(raw)
				if !ok {
					return nil
				}
				segment = unescaped
			} else {
				segment = raw
			}
			at = close
		} else {
			begin := at
			for at < end && !isTOMLSpace(text[at]) && text[at] != '.' {
				at++
			}
			segment = text[begin:at]
			if segment == "" {
				return nil
			}
		}
		segments = append(segments, segment)
		for at < end && isTOMLSpace(text[at]) {
			at++
		}
		if at < end && text[at] == '.' {
			at++
		}
	}
	return segments
}

func isTOMLSpace(character byte) bool {
	return character == ' ' || character == '\t'
}

// tomlUnescapeBasic decodes the escapes a basic string may carry. An escape it
// does not recognise answers false, so a value is never half-decoded.
func tomlUnescapeBasic(raw string) (string, bool) {
	var out strings.Builder
	for at := 0; at < len(raw); at++ {
		character := raw[at]
		if character != '\\' {
			out.WriteByte(character)
			continue
		}
		at++
		if at >= len(raw) {
			return "", false
		}
		switch raw[at] {
		case 'b':
			out.WriteByte('\b')
		case 't':
			out.WriteByte('\t')
		case 'n':
			out.WriteByte('\n')
		case 'f':
			out.WriteByte('\f')
		case 'r':
			out.WriteByte('\r')
		case '"':
			out.WriteByte('"')
		case '\\':
			out.WriteByte('\\')
		case 'u', 'U':
			length := 4
			if raw[at] == 'U' {
				length = 8
			}
			if at+length >= len(raw) {
				return "", false
			}
			digits := raw[at+1 : at+1+length]
			code, err := strconv.ParseUint(digits, 16, 32)
			if err != nil || code > 0x10ffff {
				return "", false
			}
			out.WriteRune(rune(code))
			at += length
		default:
			return "", false
		}
	}
	return out.String(), true
}

// tomlValueString reads a single-line string value starting at `at`. Anything
// that is not exactly one quoted string answers empty with ok=false.
func tomlValueString(text string, at int) (string, bool) {
	for at < len(text) && isTOMLSpace(text[at]) {
		at++
	}
	if at >= len(text) || (text[at] != '"' && text[at] != '\'') {
		return "", false
	}
	end := tomlStringEnd(text, at)
	if end < 0 {
		return "", false
	}
	rest := strings.TrimSpace(text[end:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", false
	}
	raw := text[at+1 : end-1]
	if text[at] == '"' {
		return tomlUnescapeBasic(raw)
	}
	return raw, true
}

// tomlString renders a value as a basic string. Only control characters, quotes
// and backslashes are escaped, so ordinary text stays readable.
func tomlString(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if character < 0x20 || character == 0x7f {
				out.WriteString(`\u`)
				hexDigits := strconv.FormatInt(int64(character), 16)
				out.WriteString(strings.Repeat("0", 4-len(hexDigits)))
				out.WriteString(hexDigits)
				continue
			}
			out.WriteRune(character)
		}
	}
	out.WriteByte('"')
	return out.String()
}

// tomlDigest is the body hash an owner line records.
func tomlDigest(body []string) string {
	digest := sha256.Sum256([]byte(strings.Join(body, "\n")))
	return hex.EncodeToString(digest[:])
}

// tomlTextValid accepts a single-line value that may reach a TOML key or value.
func tomlTextValid(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

var errTOMLUnreadable = errors.New("toml_unreadable")

// configPaths is the per-Kimi-home layout the native modules share. It is
// derived from an explicit kimiHome, never from a guessed default.
type configPaths struct {
	homeDir    string
	configPath string
}

// newConfigPaths builds the layout for one explicit Kimi home. Every native
// writer requires this input, so a caller cannot accidentally target the
// user's real installation by omitting an argument.
func newConfigPaths(kimiHome string) (configPaths, error) {
	if kimiHome == "" || !isAbsPath(kimiHome) {
		return configPaths{}, errors.New("kimi_home_required")
	}
	home := joinPath(kimiHome, "runtime", "kimi-code", "home")
	return configPaths{homeDir: home, configPath: joinPath(home, "config.toml")}, nil
}

// guardConfig checks the composed directories and the config file itself. Only
// the directories this package composes are inspected, so an unrelated symlink
// far above the user's home cannot veto the whole feature.
func (p configPaths) guardConfig() string {
	for _, directory := range []string{
		parentOf(p.homeDir, 3), parentOf(p.homeDir, 2), parentOf(p.homeDir, 1), p.homeDir,
	} {
		info, err := lstatPath(directory)
		if err != nil {
			if isNotExist(err) {
				return "config_missing"
			}
			return "path_unavailable"
		}
		if info.isSymlink {
			return "path_symlink"
		}
		if !info.isDir {
			return "path_not_directory"
		}
	}
	info, err := lstatPath(p.configPath)
	if err != nil {
		if isNotExist(err) {
			return "config_missing"
		}
		return "config_unavailable"
	}
	if info.isSymlink {
		return "config_symlink"
	}
	if !info.isFile {
		return "config_not_file"
	}
	if info.size > nativeConfigMaxBytes {
		return "config_too_large"
	}
	return ""
}

const nativeConfigMaxBytes = 8 << 20
