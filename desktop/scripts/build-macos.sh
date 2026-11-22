#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin ]]; then
    printf 'Build the macOS package on a macOS host.\n' >&2
    exit 2
fi
if [[ "${1:-}" != "" && "$1" != "--launch" ]]; then
    printf 'Usage: %s [--launch]\n' "$0" >&2
    exit 2
fi

desktop_dir="$(cd "$(dirname "$0")/.." && pwd -P)"
output_dir="$(dirname "$desktop_dir")/dist"
mkdir -p "$output_dir"
stage="$(mktemp -d "$output_dir/.slopguard-macos.XXXXXXXX")"
trap 'rm -rf -- "$stage"' EXIT

app="$stage/SlopGuard.app"
iconset="$stage/SlopGuard.iconset"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" "$iconset"

(
    cd "$desktop_dir"
    CGO_ENABLED=1 go build -trimpath -o "$app/Contents/MacOS/slopguard-desktop" .
)
cp "$desktop_dir/packaging/Info.plist" "$app/Contents/Info.plist"

for size in 16 32 128 256 512; do
    sips -z "$size" "$size" "$desktop_dir/assets/slopguard.png" --out "$iconset/icon_${size}x${size}.png" >/dev/null
    double="$((size * 2))"
    sips -z "$double" "$double" "$desktop_dir/assets/slopguard.png" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app/Contents/Resources/SlopGuard.icns"

plutil -lint "$app/Contents/Info.plist"
test -x "$app/Contents/MacOS/slopguard-desktop"
file "$app/Contents/MacOS/slopguard-desktop"
codesign --force --sign - "$app"
codesign --verify --strict "$app"
ditto -c -k --keepParent "$app" "$stage/slopguard-macos.zip"

installed="$output_dir/SlopGuard.app"
if [[ -e "$installed" ]]; then
    mv "$installed" "$stage/previous.app"
fi
if ! mv "$app" "$installed"; then
    if [[ -e "$stage/previous.app" ]]; then
        mv "$stage/previous.app" "$installed"
    fi
    exit 1
fi
mv -f "$stage/slopguard-macos.zip" "$output_dir/slopguard-macos.zip"
printf 'macOS application: %s\nmacOS archive: %s\n' "$installed" "$output_dir/slopguard-macos.zip"
if [[ "${1:-}" == "--launch" ]]; then
    open "$installed"
fi
