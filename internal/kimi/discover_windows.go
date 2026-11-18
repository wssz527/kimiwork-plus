//go:build windows

package kimi

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// windowsCandidates is the Windows discovery list: fixed folders plus anything
// the user's own shortcuts or a running Kimi point at.
func windowsCandidates(env map[string]string) []string {
	return windowsCandidateList(env, discoverWindowsArchives(env))
}

// installShortcutScript is fixed text with no interpolated values. It reads two
// read-only sources: the signed-in user's own shortcuts, and the executable
// path of an already running Kimi. Nothing is written, and a shortcut that does
// not point at a Kimi executable is ignored so a hostile .lnk cannot contribute
// a path this package would trust.
const installShortcutScript = `$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$shell = New-Object -ComObject WScript.Shell
$names = @('Kimi.lnk', 'Kimi\Kimi.lnk')
$folders = @([Environment]::GetFolderPath('DesktopDirectory'), [Environment]::GetFolderPath('CommonDesktopDirectory'), [Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('CommonPrograms'))
foreach ($folder in $folders) {
  if (-not $folder) { continue }
  foreach ($name in $names) {
    $file = Join-Path -Path $folder -ChildPath $name
    if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { continue }
    try { $target = [string]$shell.CreateShortcut($file).TargetPath } catch { $target = '' }
    if ($target) { [Console]::Out.Write($target + [Environment]::NewLine) }
  }
}
foreach ($process in @(Get-Process -Name 'Kimi', 'kimi-desktop' -ErrorAction SilentlyContinue)) {
  try { [Console]::Out.Write($process.Path + [Environment]::NewLine) } catch {}
}`

// discoverWindowsArchives maps discovered executables to their app.asar. Any
// failure answers no candidates: discovery is an extra source, never the only
// one, and a broken probe must not become a trusted path.
func discoverWindowsArchives(env map[string]string) []string {
	if runtime.GOOS != "windows" || env["KIMI_DISABLE_DISCOVERY"] == "1" {
		return nil
	}
	command := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command",
		installShortcutScript)
	hideBackgroundCommand(command)
	out, err := command.Output()
	if err != nil {
		return nil
	}
	var archives []string
	for _, line := range strings.Split(string(out), "\n") {
		executable := strings.TrimSpace(line)
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
