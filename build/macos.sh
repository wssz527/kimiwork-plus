#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${GO:=go}"
: "${OUTPUT_DIR:=$PWD/build/output}"
: "${VERSION:=1.0.0}"
: "${BUILD_ID:=development}"
: "${CODESIGN_IDENTITY:=-}"
APP="$OUTPUT_DIR/Kimi Work+.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
export MACOSX_DEPLOYMENT_TARGET=13.0
export CGO_CFLAGS="${CGO_CFLAGS:-} -mmacosx-version-min=13.0"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -mmacosx-version-min=13.0"
LDFLAGS="-s -w -X kimiworkplus.local/migration/internal/bridge.Version=$VERSION -X kimiworkplus.local/migration/internal/bridge.BuildID=$BUILD_ID"
"$GO" build -trimpath -ldflags="$LDFLAGS" -o "$APP/Contents/MacOS/KimiWorkPlus" ./cmd/desktop
"$GO" build -trimpath -ldflags="$LDFLAGS" -o "$APP/Contents/MacOS/gateway-daemon" ./cmd/gateway-daemon
cp build/Info.plist "$APP/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $VERSION" "$APP/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $VERSION" "$APP/Contents/Info.plist"
cp build/kimi-work-plus.icns "$APP/Contents/Resources/kimi-work-plus.icns"
codesign --force --deep --sign "$CODESIGN_IDENTITY" "$APP"
codesign --verify --deep --strict "$APP"
file "$APP/Contents/MacOS/KimiWorkPlus" "$APP/Contents/MacOS/gateway-daemon"
