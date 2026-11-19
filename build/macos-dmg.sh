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
if [ ! -d "$APP" ] || [ "${SKIP_BUILD:-0}" != "1" ]; then
  GO="$GO" OUTPUT_DIR="$OUTPUT_DIR" VERSION="$VERSION" BUILD_ID="$BUILD_ID" \
    CODESIGN_IDENTITY="$CODESIGN_IDENTITY" ARCH="$ARCH" bash build/macos.sh
fi

codesign --verify --deep --strict "$APP"

ZIP_NAME="KimiWorkPlus-${VERSION}-macos-${ARCH}.zip"
DMG_NAME="KimiWorkPlus-${VERSION}-macos-${ARCH}.dmg"
ZIP_PATH="$OUTPUT_DIR/$ZIP_NAME"
DMG_PATH="$OUTPUT_DIR/$DMG_NAME"

rm -f "$ZIP_PATH" "$DMG_PATH"
(cd "$OUTPUT_DIR" && COPYFILE_DISABLE=1 zip -qry "$ZIP_PATH" "Kimi Work+.app")

STAGE="$(mktemp -d "${TMPDIR:-/tmp}/kwp-dmg-stage.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT

ditto "$APP" "$STAGE/Kimi Work+.app"
ln -s /Applications "$STAGE/Applications"

cat > "$STAGE/首次打开说明.txt" <<'EOF'
Kimi Work+ 安装与首次打开说明（macOS）
========================================

1. 将“Kimi Work+.app”拖入右侧的“Applications（应用程序）”文件夹。

2. 本应用使用项目固定自签名证书签名，未购买 Apple 开发者公证。
   通过浏览器下载的应用首次打开时，macOS 会拦截提示无法验证开发者。
   （macOS 15 及以上版本已无法通过右键“打开”绕过）

3. 首次打开步骤（只需操作一次）：
   方法 A（系统设置）：
   - 在“应用程序”中先双击一次“Kimi Work+.app”，出现拦截弹窗后点“完成”或“取消”；
   - 打开“系统设置 -> 隐私与安全性”，滑到页面下方“安全性”区域；
   - 点击“Kimi Work+”右侧的“仍要打开”，输入本机开机密码确认即可。

   方法 B（终端命令）：
   xattr -dr com.apple.quarantine "/Applications/Kimi Work+.app"

4. 后续版本使用应用内自动更新下载安装，不会再次触发拦截，且固定签名证书
   可保持系统钥匙串连续读取已保存的账号和 API Key。
EOF

if [ -f build/dmg-first-open.png ]; then
  cp build/dmg-first-open.png "$STAGE/首次打开指引.png"
fi
for extra in LICENSE THIRD_PARTY_NOTICES.txt; do
  if [ -f "$extra" ]; then
    cp "$extra" "$STAGE/$extra"
  fi
done

hdiutil create \
  -volname "Kimi Work+" \
  -srcfolder "$STAGE" \
  -ov \
  -format UDZO \
  "$DMG_PATH"

hdiutil verify "$DMG_PATH"

(
  cd "$OUTPUT_DIR"
  shasum -a 256 "$ZIP_NAME" "$DMG_NAME" > SHA256SUMS-macos.txt
)
cat "$OUTPUT_DIR/SHA256SUMS-macos.txt"
