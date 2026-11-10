param([string]$OutputDir = "out/windows-amd64", [string]$Go = "go")
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
& $Go build -trimpath -ldflags "-s -w -H windowsgui" -o (Join-Path $OutputDir "KimiWorkPlus.exe") ./cmd/desktop
if ($LASTEXITCODE -ne 0) { throw "Desktop build failed" }
& $Go build -trimpath -ldflags "-s -w" -o (Join-Path $OutputDir "gateway-daemon.exe") ./cmd/gateway-daemon
if ($LASTEXITCODE -ne 0) { throw "Gateway build failed" }
Get-FileHash -Algorithm SHA256 (Join-Path $OutputDir "*.exe") | Format-Table Hash,Path
Write-Host "Requires the system Microsoft Edge WebView2 Runtime. No Node.js or Chromium runtime is bundled."
Write-Host "Preview data uses a separate identity. This script does not register a service or replace the stable installation."
