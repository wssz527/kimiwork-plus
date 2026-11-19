#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${GO:=go}"
: "${OUTPUT_DIR:=$PWD/build/output}"
: "${VERSION:=1.0.0}"
: "${BUILD_ID:=development}"
: "${CODESIGN_IDENTITY:=-}"
: "${ARCH:=universal}"

APP="$OUTPUT_DIR/Kimi Work+.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

export MACOSX_DEPLOYMENT_TARGET=13.0
export CGO_ENABLED=1
LDFLAGS="-s -w -X kimiworkplus.local/migration/internal/bridge.Version=$VERSION -X kimiworkplus.local/migration/internal/bridge.BuildID=$BUILD_ID"
if [ -n "${UPDATE_PUBLIC_KEY:-}" ]; then
  LDFLAGS="$LDFLAGS -X kimiworkplus.local/migration/internal/appupdate.DefaultPublicKeyHex=$UPDATE_PUBLIC_KEY"
fi
if [ -n "${UPDATE_GITHUB_REPO:-}" ]; then
  LDFLAGS="$LDFLAGS -X kimiworkplus.local/migration/internal/appupdate.DefaultGitHubRepo=$UPDATE_GITHUB_REPO"
fi

build_arch() {
  local goarch="$1"
  local clang_arch="$2"
  local dest_dir="$3"
  mkdir -p "$dest_dir"
  GOOS=darwin GOARCH="$goarch" \
    CGO_CFLAGS="-arch $clang_arch -mmacosx-version-min=13.0" \
    CGO_LDFLAGS="-arch $clang_arch -mmacosx-version-min=13.0" \
    "$GO" build -trimpath -ldflags="$LDFLAGS" -o "$dest_dir/KimiWorkPlus" ./cmd/desktop
  GOOS=darwin GOARCH="$goarch" \
    CGO_CFLAGS="-arch $clang_arch -mmacosx-version-min=13.0" \
    CGO_LDFLAGS="-arch $clang_arch -mmacosx-version-min=13.0" \
    "$GO" build -trimpath -ldflags="$LDFLAGS" -o "$dest_dir/gateway-daemon" ./cmd/gateway-daemon
}

case "$ARCH" in
  universal)
    TMP_ARCH="$(mktemp -d "${TMPDIR:-/tmp}/kwp-universal.XXXXXX")"
    trap 'rm -rf "$TMP_ARCH"' EXIT
    build_arch arm64 arm64 "$TMP_ARCH/arm64"
    build_arch amd64 x86_64 "$TMP_ARCH/amd64"
    lipo -create -output "$APP/Contents/MacOS/KimiWorkPlus" \
      "$TMP_ARCH/arm64/KimiWorkPlus" "$TMP_ARCH/amd64/KimiWorkPlus"
    lipo -create -output "$APP/Contents/MacOS/gateway-daemon" \
      "$TMP_ARCH/arm64/gateway-daemon" "$TMP_ARCH/amd64/gateway-daemon"
    ;;
  arm64)
    build_arch arm64 arm64 "$APP/Contents/MacOS"
    ;;
  amd64)
    build_arch amd64 x86_64 "$APP/Contents/MacOS"
    ;;
  *)
    echo "Unsupported ARCH=$ARCH (expected universal, arm64, or amd64)" >&2
    exit 1
    ;;
esac

cp build/Info.plist "$APP/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $VERSION" "$APP/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $VERSION" "$APP/Contents/Info.plist"
cp build/kimi-work-plus.icns "$APP/Contents/Resources/kimi-work-plus.icns"

for extra in LICENSE THIRD_PARTY_NOTICES.txt; do
  if [ -f "$extra" ]; then
    cp "$extra" "$APP/Contents/Resources/$extra"
  fi
done

codesign --force --identifier "dev.kimi-work-plus.daemon" --sign "$CODESIGN_IDENTITY" "$APP/Contents/MacOS/gateway-daemon"
codesign --force --sign "$CODESIGN_IDENTITY" "$APP"
codesign --verify --deep --strict "$APP"

file "$APP/Contents/MacOS/KimiWorkPlus" "$APP/Contents/MacOS/gateway-daemon"
lipo -info "$APP/Contents/MacOS/KimiWorkPlus" "$APP/Contents/MacOS/gateway-daemon"
