package kimi

import "strings"

// windowsCandidateList is the pure part, so the folder rules can be exercised on
// any host.
func windowsCandidateList(env map[string]string, discovered []string) []string {
	local := env["LOCALAPPDATA"]
	if local == "" {
		if profile := env["USERPROFILE"]; profile != "" {
			local = winJoin(profile, "AppData", "Local")
		}
	}
	roots := []string{}
	if local != "" {
		roots = append(roots, winJoin(local, "Programs"), local)
	}
	for _, name := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if value := env[name]; value != "" {
			roots = append(roots, value)
		}
	}
	roots = append(roots, `C:\Program Files`)

	var out []string
	seen := map[string]bool{}
	for _, root := range roots {
		for _, name := range []string{"Kimi", "kimi-desktop"} {
			candidate := winJoin(root, name, "resources", "app.asar")
			key := strings.ToLower(candidate)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, candidate)
		}
	}
	for _, found := range discovered {
		key := strings.ToLower(found)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, found)
	}
	return out
}

// winJoin joins Windows path segments with backslashes regardless of the host,
// so the candidate list can be exercised on macOS. path/filepath would use the
// host separator and produce a POSIX path here.
func winJoin(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimRight(part, `\/`)
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	return strings.Join(cleaned, `\`)
}
