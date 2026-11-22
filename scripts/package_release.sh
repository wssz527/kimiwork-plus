#!/bin/bash
set -euo pipefail

# Build, sign, and verify macOS Universal + Windows amd64 release assets for KimiWork +.
# Produces a single host-agnostic update-manifest.json using relative artifact URLs so
# the same manifest works on both CNB domestic mirror and GitHub Releases (and remains
# compatible with v1.1.3+ clients).

cd "$(dirname "$0")/.."

: "${GO:=go}"
: "${VERSION:?VERSION is required, e.g. 1.1.4}"
: "${BUILD_ID:?BUILD_ID is required, e.g. 1.1.4-abcdef0}"
: "${OUTPUT_DIR:=$PWD/build/release-$VERSION}"
: "${NOTES_FILE:?NOTES_FILE is required (path to UTF-8 release notes)}"
: "${CHANNEL:=stable}"
: "${SIGNING_KEY:=$HOME/Library/Application Support/kimi-work-plus/release-signing/ed25519/ed25519.key}"
: "${CODESIGN_IDENTITY:=Kimi Work+ Code Signing}"
: "${UPDATE_PUBLIC_KEY:=}"
: "${UPDATE_GITHUB_REPO:=}"
: "${UPDATE_MIRROR_BASE:=}"
: "${MANIFEST_BASE_URL:=}"

if [ ! -f "$SIGNING_KEY" ]; then
  echo "Release signing key not found: $SIGNING_KEY" >&2
  exit 1
fi
if [ ! -f "$NOTES_FILE" ]; then
  echo "Release notes file not found: $NOTES_FILE" >&2
  exit 1
fi

MAC_OUT="$OUTPUT_DIR/macos"
WIN_OUT="$OUTPUT_DIR/windows"
ASSETS_OUT="$OUTPUT_DIR/release-assets"
mkdir -p "$MAC_OUT" "$WIN_OUT" "$ASSETS_OUT"

echo "==> Building macOS Universal app, zip, and dmg ($VERSION / $BUILD_ID)"
GO="$GO" VERSION="$VERSION" BUILD_ID="$BUILD_ID" \
  CODESIGN_IDENTITY="$CODESIGN_IDENTITY" \
  UPDATE_PUBLIC_KEY="$UPDATE_PUBLIC_KEY" \
  UPDATE_GITHUB_REPO="$UPDATE_GITHUB_REPO" \
  UPDATE_MIRROR_BASE="$UPDATE_MIRROR_BASE" \
  OUTPUT_DIR="$MAC_OUT" \
  bash build/macos-dmg.sh

echo "==> Building Windows amd64 binaries, zip, and setup.exe ($VERSION / $BUILD_ID)"
GO="$GO" VERSION="$VERSION" BUILD_ID="$BUILD_ID" \
  UPDATE_PUBLIC_KEY="$UPDATE_PUBLIC_KEY" \
  UPDATE_GITHUB_REPO="$UPDATE_GITHUB_REPO" \
  UPDATE_MIRROR_BASE="$UPDATE_MIRROR_BASE" \
  OUTPUT_DIR="$WIN_OUT" \
  bash build/windows.sh

MAC_ZIP="KimiWorkPlus-${VERSION}-macos-universal.zip"
MAC_DMG="KimiWorkPlus-${VERSION}-macos-universal.dmg"
WIN_ZIP="KimiWorkPlus-${VERSION}-windows-amd64.zip"
WIN_SETUP="KimiWorkPlus-${VERSION}-windows-amd64-setup.exe"

cp "$MAC_OUT/$MAC_ZIP" "$ASSETS_OUT/$MAC_ZIP"
cp "$MAC_OUT/$MAC_DMG" "$ASSETS_OUT/$MAC_DMG"
cp "$WIN_OUT/$WIN_ZIP" "$ASSETS_OUT/$WIN_ZIP"
cp "$WIN_OUT/$WIN_SETUP" "$ASSETS_OUT/$WIN_SETUP"

MANIFEST_NAME="update-manifest.json"
if [ "$CHANNEL" != "stable" ]; then
  MANIFEST_NAME="update-manifest-${CHANNEL}.json"
fi

echo "==> Signing update manifest ($MANIFEST_NAME)"
SIGN_ARGS=(
  -key "$SIGNING_KEY"
  -version "$VERSION"
  -channel "$CHANNEL"
  -notes-file "$NOTES_FILE"
  -artifact "darwin::$ASSETS_OUT/$MAC_ZIP"
  -artifact "windows:amd64:$ASSETS_OUT/$WIN_ZIP"
  -out "$ASSETS_OUT/$MANIFEST_NAME"
)
if [ -n "$MANIFEST_BASE_URL" ]; then
  SIGN_ARGS+=(-base-url "$MANIFEST_BASE_URL")
fi
env -u GOOS -u GOARCH CGO_ENABLED=0 "$GO" run ./cmd/signrelease "${SIGN_ARGS[@]}"

(
  cd "$ASSETS_OUT"
  shasum -a 256 \
    "$MAC_DMG" \
    "$MAC_ZIP" \
    "${MAC_ZIP}.sig" \
    "$WIN_SETUP" \
    "$WIN_ZIP" \
    "${WIN_ZIP}.sig" \
    "$MANIFEST_NAME" > SHA256SUMS.txt
)

echo "==> Packaged release assets in $ASSETS_OUT"
ls -la "$ASSETS_OUT"
cat "$ASSETS_OUT/SHA256SUMS.txt"
