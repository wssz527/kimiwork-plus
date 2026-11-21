package byokdns

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"runtime"
	"strings"
)

// DarwinHostsPath is the static hosts file on macOS. Unlike a scoped resolver in
// /etc/resolver, an entry here keeps resolving after the gateway daemon stops.
const DarwinHostsPath = "/etc/hosts"

func HostsEntry() string { return Address + " " + Domain }

var hostsLines = regexp.MustCompile(`[^\r\n]*(?:\r\n|\r|\n|$)`)

// UpdateHosts adds or removes the managed entry while preserving every other
// byte the file had: BOM, line endings, comments and unrelated entries.
//
// The managed entry is the marker line followed by the address line, or an
// exact two-field "address domain" line someone wrote by hand. Any other line
// that names the domain is a conflict: this function never shadows a host the
// user or another product configured.
func UpdateHosts(content []byte, action string) ([]byte, bool, error) {
	if action != "prepare" && action != "remove-dns" {
		return nil, false, errors.New("invalid_setup_action")
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, false, errors.New("unsupported_hosts_encoding")
	}
	original := string(content)
	prefix := ""
	if strings.HasPrefix(original, "\xef\xbb\xbf") {
		prefix = "\xef\xbb\xbf"
	}
	body := original[len(prefix):]
	eol := "\r\n"
	if match := regexp.MustCompile(`\r\n|\r|\n`).FindString(body); match != "" {
		eol = match
	}
	lines := hostsLines.FindAllString(body, -1)
	kept := []string{}
	sawEntry := false
	for at := 0; at < len(lines); at++ {
		line := lines[at]
		if line == "" {
			continue
		}
		if strings.TrimSpace(line) == HostsMarker {
			if at+1 >= len(lines) || strings.TrimSpace(lines[at+1]) != HostsEntry() {
				return nil, false, errors.New("resolver_conflict")
			}
			sawEntry = true
			if action == "prepare" {
				kept = append(kept, line, lines[at+1])
			}
			at++
			continue
		}
		uncommented := strings.Split(line, "#")[0]
		fields := strings.Fields(uncommented)
		if len(fields) > 1 {
			for _, name := range fields[1:] {
				if strings.EqualFold(name, Domain) {
					if fields[0] != Address || len(fields) != 2 {
						return nil, false, errors.New("resolver_conflict")
					}
					sawEntry = true
				}
			}
		}
		kept = append(kept, line)
	}
	result := prefix + strings.Join(kept, "")
	if action == "prepare" && !sawEntry {
		result = prefix + HostsMarker + eol + HostsEntry() + eol + body
	}
	return []byte(result), result != original, nil
}

// HasManagedHostsEntry reports whether the hosts file already carries a valid
// static entry for Domain. It is a pure read: it never writes and needs no
// elevation.
//
// A symlink, an unreadable file, a file with NUL bytes or a conflicting line all
// report false, because none of those is something the setup may silently reuse.
func HasManagedHostsEntry(hostsPath string) bool {
	if hostsPath == "" {
		if runtime.GOOS == "windows" {
			hostsPath = DefaultHostsPath(nil)
		} else {
			hostsPath = DarwinHostsPath
		}
	}
	info, err := os.Lstat(hostsPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false
	}
	content, err := os.ReadFile(hostsPath)
	if err != nil || bytes.IndexByte(content, 0) >= 0 {
		return false
	}
	// UpdateHosts reports changed=false only when the file already holds a valid
	// managed entry; an empty or unrelated file always grows by the entry.
	_, changed, err := UpdateHosts(content, "prepare")
	return err == nil && !changed
}
