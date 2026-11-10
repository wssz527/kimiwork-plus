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
if [ -e "$target" ]; then
  echo 'A preview is already installed. Quit that preview and move it aside before installing this build.' >&2
  exit 1
fi
codesign --verify --deep --strict "$app"
mkdir -p "$HOME/Applications"
staging="$(mktemp -d "$HOME/Applications/.kimi-work-plus-preview.XXXXXX")"
trap 'rm -rf "$staging"' EXIT
ditto "$app" "$staging/Kimi Work+ Preview.app"
codesign --verify --deep --strict "$staging/Kimi Work+ Preview.app"
mv "$staging/Kimi Work+ Preview.app" "$target"
echo 'Installed independently as Kimi Work+ Preview.app. It has not been opened.'
echo 'The stable Kimi Work+.app, official Kimi, credentials and usage records were not changed.'
