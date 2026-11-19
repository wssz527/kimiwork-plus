//go:build windows

package kimi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// windowsCandidates is the Windows discovery list: fixed folders plus anything
// the user's own shortcuts or a running Kimi point at.
func windowsCandidates(env map[string]string) []string {
	return windowsCandidateList(env, discoverWindowsArchives(env))
}

func runningKimiExecutables() []string {
	images, err := snapshotNamedProcesses("kimi.exe", "kimi-desktop.exe")
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(images))
	for _, image := range images {
		paths = append(paths, image.Path)
	}
	return paths
}

// discoverWindowsArchives maps discovered executables to their app.asar
// without spawning PowerShell or WScript.Shell COM objects. Any failure answers
// no candidates: discovery is an extra source, never the only one, and a broken
// probe must not become a trusted path.
func discoverWindowsArchives(env map[string]string) []string {
	if runtime.GOOS != "windows" || env["KIMI_DISABLE_DISCOVERY"] == "1" {
		return nil
	}
	var candidates []string
	shortcutNames := []string{"Kimi.lnk", filepath.Join("Kimi", "Kimi.lnk")}
	for _, folder := range windowsShortcutFolders(env) {
		for _, rel := range shortcutNames {
			data, err := os.ReadFile(filepath.Join(folder, rel))
			if err != nil || len(data) > 256*1024 {
				continue
			}
			if target := parseWindowsShortcutTarget(data, env); target != "" {
				candidates = append(candidates, target)
			}
		}
	}
	candidates = append(candidates, runningKimiExecutables()...)

	var archives []string
	for _, executable := range candidates {
		executable = strings.TrimSpace(executable)
		if executable == "" {
			continue
		}
		base := strings.ToLower(filepath.Base(executable))
		if base != "kimi.exe" && base != "kimi-desktop.exe" {
			continue
		}
		archive := filepath.Join(filepath.Dir(executable), "resources", "app.asar")
		if _, err := os.Stat(archive); err == nil {
			archives = append(archives, archive)
		}
	}
	return archives
}
