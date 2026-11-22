#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${GO:=go}"
: "${OUTPUT_DIR:=$PWD/build/output-windows}"
: "${VERSION:=1.0.0}"
: "${BUILD_ID:=development}"

mkdir -p "$OUTPUT_DIR"
export GOOS=windows
export GOARCH=amd64
export CGO_ENABLED=0
LDFLAGS="-s -w -X github.com/wssz527/kimiwork-plus/internal/bridge.Version=$VERSION -X github.com/wssz527/kimiwork-plus/internal/bridge.BuildID=$BUILD_ID"
if [ -n "${UPDATE_PUBLIC_KEY:-}" ]; then
  LDFLAGS="$LDFLAGS -X github.com/wssz527/kimiwork-plus/internal/appupdate.DefaultPublicKeyHex=$UPDATE_PUBLIC_KEY"
fi
if [ -n "${UPDATE_GITHUB_REPO:-}" ]; then
  LDFLAGS="$LDFLAGS -X github.com/wssz527/kimiwork-plus/internal/appupdate.DefaultGitHubRepo=$UPDATE_GITHUB_REPO"
fi
if [ -n "${UPDATE_MIRROR_BASE:-}" ]; then
  LDFLAGS="$LDFLAGS -X github.com/wssz527/kimiwork-plus/internal/appupdate.DefaultMirrorBase=$UPDATE_MIRROR_BASE"
fi

"$GO" build -trimpath -ldflags="$LDFLAGS -H windowsgui" -o "$OUTPUT_DIR/KimiWorkPlus.exe" ./cmd/desktop
"$GO" build -trimpath -ldflags="$LDFLAGS -H windowsgui" -o "$OUTPUT_DIR/gateway-daemon.exe" ./cmd/gateway-daemon

TMP_WIN="$(mktemp -d "${TMPDIR:-/tmp}/kwp-winpkg.XXXXXX")"
trap 'rm -rf "$TMP_WIN"' EXIT

"$GO" build -trimpath -ldflags="$LDFLAGS -H windowsgui" -o "$TMP_WIN/wininstaller-stub.exe" ./cmd/wininstaller
STUB_SIZE="$(wc -c < "$TMP_WIN/wininstaller-stub.exe" | tr -d ' ')"

ZIP_NAME="KimiWorkPlus-${VERSION}-windows-amd64.zip"
SETUP_NAME="KimiWorkPlus-${VERSION}-windows-amd64-setup.exe"
ZIP_PATH="$OUTPUT_DIR/$ZIP_NAME"
SETUP_PATH="$OUTPUT_DIR/$SETUP_NAME"

python3 - "$OUTPUT_DIR" "$TMP_WIN" "$ZIP_PATH" "$VERSION" "$BUILD_ID" "$STUB_SIZE" <<'PY'
import json, os, sys, zipfile

out_dir, tmp_dir, zip_path, version, build_id, stub_size = sys.argv[1:7]
files = ["KimiWorkPlus.exe", "gateway-daemon.exe"]
for extra in ("LICENSE", "THIRD_PARTY_NOTICES.txt"):
    if os.path.isfile(extra):
        files.append(extra)

with zipfile.ZipFile(zip_path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=6) as zf:
    for name in files:
        src = os.path.join(out_dir, name) if name.endswith(".exe") else name
        zf.write(src, arcname="Kimi Work+/" + name)

payload_zip = os.path.join(tmp_dir, "payload.zip")
meta = json.dumps({"version": version, "buildId": build_id, "stubSize": int(stub_size)})
with zipfile.ZipFile(payload_zip, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=6) as zf:
    zf.writestr(".installer-meta.json", meta)
    for name in files:
        src = os.path.join(out_dir, name) if name.endswith(".exe") else name
        zf.write(src, arcname=name)
PY

cat "$TMP_WIN/wininstaller-stub.exe" "$TMP_WIN/payload.zip" > "$SETUP_PATH"

env -u GOOS -u GOARCH CGO_ENABLED=0 "$GO" run ./cmd/wininstaller --check-icon "$OUTPUT_DIR/KimiWorkPlus.exe"
env -u GOOS -u GOARCH CGO_ENABLED=0 "$GO" run ./cmd/wininstaller --check-icon "$OUTPUT_DIR/gateway-daemon.exe"
env -u GOOS -u GOARCH CGO_ENABLED=0 "$GO" run ./cmd/wininstaller --check-payload "$SETUP_PATH"

(
  cd "$OUTPUT_DIR"
  shasum -a 256 KimiWorkPlus.exe gateway-daemon.exe "$ZIP_NAME" "$SETUP_NAME" > SHA256SUMS-windows.txt
)
cat "$OUTPUT_DIR/SHA256SUMS-windows.txt"
