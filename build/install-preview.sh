#!/bin/bash
set -euo pipefail
source_dir="$(cd "$(dirname "$0")" && pwd)"
app="$source_dir/Kimi Work+ Preview.app"
target="$HOME/Applications/Kimi Work+ Preview.app"
if [ "$(uname -m)" != "arm64" ]; then
  echo 'This preview requires an Apple Silicon Mac.' >&2
  exit 1
fi
if [ ! -d "$app" ]; then
  echo 'Kimi Work+ Preview.app must be next to this installer.' >&2
  exit 1
fi
codesign --verify --deep --strict "$app"
mkdir -p "$HOME/Applications"
staging="$(mktemp -d "$HOME/Applications/.kimi-work-plus-preview.XXXXXX")"
backup=""
cleanup() {
  rm -rf "$staging"
  if [ -n "$backup" ] && [ ! -e "$target" ] && [ -e "$backup" ]; then
    mv "$backup" "$target"
    echo 'The previous preview was restored.' >&2
  fi
}
trap cleanup EXIT
if [ -x "$target/Contents/MacOS/gateway-daemon" ]; then
  endpoint="$HOME/Library/Application Support/kimi-work-plus-preview/desktop-endpoint.json"
  if [ -f "$endpoint" ]; then
    state="$HOME/Library/Application Support/kimi-work-plus-preview"
    endpoint_pid="$(/usr/bin/plutil -extract pid raw -o - "$endpoint" 2>/dev/null || true)"
    if [[ ! "$endpoint_pid" =~ ^[1-9][0-9]*$ ]] || kill -0 "$endpoint_pid" 2>/dev/null; then
      if ! "$target/Contents/MacOS/gateway-daemon" --state-dir "$state" --stop-daemon; then
        echo 'The preview daemon did not stop normally. The installed preview was not replaced.' >&2
        exit 1
      fi
    fi
  fi
fi
ditto "$app" "$staging/Kimi Work+ Preview.app"
codesign --verify --deep --strict "$staging/Kimi Work+ Preview.app"
if [ -e "$target" ]; then
  if [ ! -d "$target/Contents/MacOS" ]; then
    echo 'The existing preview target is not an application; it was not replaced.' >&2
    exit 1
  fi
  backup="$HOME/Applications/Kimi Work+ Preview.previous.app"
  if [ -e "$backup" ]; then
    echo 'A previous preview backup already exists; move it aside before upgrading.' >&2
    exit 1
  fi
  mv "$target" "$backup"
fi
if ! mv "$staging/Kimi Work+ Preview.app" "$target"; then
  echo 'Preview replacement failed; the previous application will be restored.' >&2
  exit 1
fi
codesign --verify --deep --strict "$target"
if [ -n "$backup" ]; then
  echo "The previous preview was retained at $backup."
fi
echo 'Installed independently as Kimi Work+ Preview.app. It has not been opened.'
echo 'The stable Kimi Work+.app, official Kimi, credentials and usage records were not changed.'
