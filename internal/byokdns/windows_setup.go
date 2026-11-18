package byokdns

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"unicode/utf16"
)

func DefaultHostsPath(env map[string]string) string {
	root := ""
	if env == nil {
		root = os.Getenv("SystemRoot")
		if root == "" {
			root = os.Getenv("SYSTEMROOT")
		}
	} else if env["SystemRoot"] != "" {
		root = env["SystemRoot"]
	} else {
		root = env["SYSTEMROOT"]
	}
	if root == "" {
		root = `C:\Windows`
	}
	return strings.TrimRight(root, `\/`) + `\System32\drivers\etc\hosts`
}

func HostsEntry() string { return Address + " " + Domain }

var hostsLines = regexp.MustCompile(`[^\r\n]*(?:\r\n|\r|\n|$)`)

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

func powershellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func HostsScript(action, target string, flush bool, reportPath string) string {
	if action != "prepare" && action != "remove-dns" {
		return ""
	}
	keep := action == "prepare"
	lines := []string{
		`$ErrorActionPreference = "Stop"`,
		"$path = " + powershellLiteral(target),
		"$report = " + powershellLiteral(reportPath),
		"$marker = " + powershellLiteral(HostsMarker),
		"$entry = " + powershellLiteral(HostsEntry()),
		"function Write-Verdict([string]$value) {",
		"  if ($report) { [System.IO.File]::WriteAllText($report, $value, (New-Object System.Text.UTF8Encoding($false))) }",
		"}",
		"try {",
		`  if (([System.IO.File]::GetAttributes($path) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { throw "resolver_conflict" }`,
		"  $bytes = [System.IO.File]::ReadAllBytes($path)",
		`  if ($bytes -contains [byte]0) { throw "unsupported_hosts_encoding" }`,
		"  $encoding = [System.Text.Encoding]::GetEncoding(28591)",
		"  $raw = $encoding.GetString($bytes)",
		"  $bom = $encoding.GetString([byte[]]@(239,187,191))",
		`  $prefix = if ($raw.StartsWith($bom)) { $bom } else { "" }`,
		"  $body = $raw.Substring($prefix.Length)",
		`  $eol = [regex]::Match($body, '\r\n|\r|\n').Value`,
		"  if (-not $eol) { $eol = [string][char]13 + [char]10 }",
		`  $lines = @([regex]::Matches($body, '[^\r\n]*(?:\r\n|\r|\n|$)') | Where-Object { $_.Length -gt 0 } | ForEach-Object { $_.Value })`,
		"  $kept = New-Object System.Collections.ArrayList",
		"  $sawEntry = $false",
		"  for ($i = 0; $i -lt $lines.Count; $i++) {",
		"    $line = $lines[$i]",
		"    if ($line.Trim() -ceq $marker) {",
		`      if ($i + 1 -ge $lines.Count -or $lines[$i+1].Trim() -cne $entry) { throw "resolver_conflict" }`,
		"      $sawEntry = $true",
	}
	if keep {
		lines = append(lines, "      [void]$kept.Add($line); [void]$kept.Add($lines[$i+1])")
	}
	lines = append(lines,
		"      $i++; continue",
		"    }",
		`    $tokens = @($line.Split('#')[0].Trim() -split '\s+')`,
		"    if ($tokens.Count -gt 1) {",
		"      $names = @($tokens[1..($tokens.Count-1)])",
		"      if ($names -contains "+powershellLiteral(Domain)+") {",
		"        if ($tokens[0] -ne "+powershellLiteral(Address)+" -or $names.Count -ne 1) { throw \"resolver_conflict\" }",
		"        $sawEntry = $true",
		"      }",
		"    }",
		"    [void]$kept.Add($line)",
		"  }",
		`  $text = $prefix + ($kept -join "")`,
	)
	if keep {
		lines = append(lines, "  if (-not $sawEntry) { $text = $prefix + $marker + $eol + $entry + $eol + $body }")
	}
	lines = append(lines,
		"  $next = $encoding.GetBytes($text)",
		"  $before = [Convert]::ToBase64String($bytes)",
		"  $expected = [Convert]::ToBase64String($next)",
		"  if ($expected -cne $before) {",
		`    if ([Convert]::ToBase64String([System.IO.File]::ReadAllBytes($path)) -cne $before) { throw "resolver_conflict" }`,
		"    [System.IO.File]::WriteAllBytes($path, $next)",
		"  }",
		`  if ([Convert]::ToBase64String([System.IO.File]::ReadAllBytes($path)) -cne $expected) { throw "hosts_verify_failed" }`,
	)
	if flush {
		lines = append(lines, "  ipconfig /flushdns | Out-Null", `  if ($LASTEXITCODE -ne 0) { throw "dns_flush_failed" }`)
	}
	lines = append(lines,
		`  Write-Verdict "done"`,
		`  Write-Output "done"`,
		"  exit 0",
		"} catch {",
		`  $verdict = if ($_.Exception.Message -in @("resolver_conflict", "unsupported_hosts_encoding", "hosts_verify_failed", "dns_flush_failed")) { $_.Exception.Message } else { "hosts_edit_failed" }`,
		"  [Console]::Error.Write($verdict)",
		"  try { Write-Verdict $verdict } catch {}",
		`  if ($verdict -eq "resolver_conflict") { exit 73 }`,
		"  exit 1",
		"}",
	)
	return strings.Join(lines, "\n")
}

func elevationCommand(script string) string {
	encoded := base64.StdEncoding.EncodeToString(utf16LE(script))
	return strings.Join([]string{
		`$ErrorActionPreference = "Stop"`,
		"try {",
		"  $child = Start-Process -FilePath powershell.exe -Verb RunAs -Wait -PassThru -WindowStyle Hidden -ArgumentList @(",
		"    '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-EncodedCommand', '" + encoded + "')",
		"  exit $child.ExitCode",
		"} catch {",
		"  $failure = $_.Exception",
		"  while ($failure -and $failure.NativeErrorCode -ne 1223) { $failure = $failure.InnerException }",
		`  if ($failure) { [Console]::Error.Write("setup_cancelled"); exit 1 }`,
		`  [Console]::Error.Write("elevation_failed"); exit 1`,
		"}",
	}, "\n")
}

func utf16LE(value string) []byte {
	encoded := utf16.Encode([]rune(value))
	out := make([]byte, len(encoded)*2)
	for index, code := range encoded {
		binary.LittleEndian.PutUint16(out[index*2:], code)
	}
	return out
}

func windowsAdministrator(ctx context.Context, script, reportPath string) error {
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(utf16LE(elevationCommand(script))))
	hideElevationConsole(command)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	text := string(output)
	if reportPath != "" {
		if report, readErr := os.ReadFile(reportPath); readErr == nil {
			text += " " + string(report)
		}
	}
	return errors.New(strings.TrimSpace(text) + " " + err.Error())
}
