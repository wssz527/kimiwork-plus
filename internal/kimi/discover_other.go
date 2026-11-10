//go:build !windows

package kimi

// windowsCandidates is the Windows discovery list, available on every host so
// the rules can be exercised anywhere. Process discovery is Windows-only and
// contributes nothing here.
func windowsCandidates(env map[string]string) []string { return windowsCandidateList(env, nil) }

func discoverWindowsArchives(map[string]string) []string { return nil }
