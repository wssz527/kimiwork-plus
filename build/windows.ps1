param(
  [string]$OutputDir = "out/windows-amd64",
  [string]$Go = "go",
  [string]$Version = "1.0.0",
  [string]$BuildId = "development",
  [string]$UpdatePublicKey = "",
  [string]$UpdateGitHubRepo = ""
)
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null

$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
$Flags = "-s -w -X github.com/wssz527/kimiwork-plus/internal/bridge.Version=$Version -X github.com/wssz527/kimiwork-plus/internal/bridge.BuildID=$BuildId"
if ($UpdatePublicKey) { $Flags += " -X github.com/wssz527/kimiwork-plus/internal/appupdate.DefaultPublicKeyHex=$UpdatePublicKey" }
if ($UpdateGitHubRepo) { $Flags += " -X github.com/wssz527/kimiwork-plus/internal/appupdate.DefaultGitHubRepo=$UpdateGitHubRepo" }

$DesktopExe = Join-Path $OutputDir "KimiWorkPlus.exe"
$DaemonExe = Join-Path $OutputDir "gateway-daemon.exe"

& $Go build -trimpath -ldflags "$Flags -H windowsgui" -o $DesktopExe ./cmd/desktop
if ($LASTEXITCODE -ne 0) { throw "Desktop build failed" }
& $Go build -trimpath -ldflags "$Flags -H windowsgui" -o $DaemonExe ./cmd/gateway-daemon
if ($LASTEXITCODE -ne 0) { throw "Gateway build failed" }

$TmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("kwp-winpkg-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null
try {
  $StubExe = Join-Path $TmpDir "wininstaller-stub.exe"
  & $Go build -trimpath -ldflags "$Flags -H windowsgui" -o $StubExe ./cmd/wininstaller
  if ($LASTEXITCODE -ne 0) { throw "Installer build failed" }
  $StubSize = (Get-Item $StubExe).Length

  $ZipPath = Join-Path $OutputDir "KimiWorkPlus-$Version-windows-amd64.zip"
  $SetupPath = Join-Path $OutputDir "KimiWorkPlus-$Version-windows-amd64-setup.exe"
  $PayloadZip = Join-Path $TmpDir "payload.zip"

  Add-Type -AssemblyName System.IO.Compression.FileSystem
  foreach ($p in @($ZipPath, $PayloadZip, $SetupPath)) {
    if (Test-Path $p) { Remove-Item -Force $p }
  }

  $Items = @($DesktopExe, $DaemonExe)
  foreach ($extra in @("LICENSE", "THIRD_PARTY_NOTICES.txt")) {
    if (Test-Path $extra) { $Items += (Resolve-Path $extra).Path }
  }

  $zf = [System.IO.Compression.ZipFile]::Open($ZipPath, [System.IO.Compression.ZipArchiveMode]::Create)
  try {
    foreach ($item in $Items) {
      [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zf, $item, ("Kimi Work+/" + [System.IO.Path]::GetFileName($item))) | Out-Null
    }
  } finally { $zf.Dispose() }

  $MetaJson = (@{ version = $Version; buildId = $BuildId; stubSize = $StubSize } | ConvertTo-Json -Compress)
  $MetaPath = Join-Path $TmpDir ".installer-meta.json"
  [System.IO.File]::WriteAllText($MetaPath, $MetaJson, [System.Text.UTF8Encoding]::new($false))

  $pf = [System.IO.Compression.ZipFile]::Open($PayloadZip, [System.IO.Compression.ZipArchiveMode]::Create)
  try {
    [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($pf, $MetaPath, ".installer-meta.json") | Out-Null
    foreach ($item in $Items) {
      [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($pf, $item, [System.IO.Path]::GetFileName($item)) | Out-Null
    }
  } finally { $pf.Dispose() }

  $outStream = [System.IO.File]::Create($SetupPath)
  try {
    $s1 = [System.IO.File]::OpenRead($StubExe); try { $s1.CopyTo($outStream) } finally { $s1.Dispose() }
    $s2 = [System.IO.File]::OpenRead($PayloadZip); try { $s2.CopyTo($outStream) } finally { $s2.Dispose() }
  } finally { $outStream.Dispose() }
} finally {
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $TmpDir
}

Get-FileHash -Algorithm SHA256 (Join-Path $OutputDir "*.exe"),(Join-Path $OutputDir "*.zip") | Format-Table Hash,Path
Write-Host "Requires the system Microsoft Edge WebView2 Runtime. No Node.js or Chromium runtime is bundled."
