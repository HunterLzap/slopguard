#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_NAME="SlopGuard"
BUILD_DIR="$ROOT_DIR/build"
APP_DIR="$BUILD_DIR/$APP_NAME.app"
CONTENTS_DIR="$APP_DIR/Contents"
MACOS_DIR="$CONTENTS_DIR/MacOS"
RESOURCES_DIR="$CONTENTS_DIR/Resources"
SRC_FILE="$ROOT_DIR/macos/SlopGuardApp.swift"
OUT_BIN="$MACOS_DIR/$APP_NAME"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "error: build-macos-app.sh must be run on macOS" >&2
  exit 1
fi

if [[ ! -x /usr/bin/swiftc ]]; then
  echo "error: /usr/bin/swiftc not found (install Xcode Command Line Tools)" >&2
  exit 1
fi

rm -rf "$APP_DIR"
mkdir -p "$MACOS_DIR" "$RESOURCES_DIR"

/usr/bin/swiftc -O \
  -framework Cocoa \
  -framework WebKit \
  "$SRC_FILE" \
  -o "$OUT_BIN"

if [[ ! -x "$ROOT_DIR/bin/slopguard" ]]; then
  echo "error: missing executable $ROOT_DIR/bin/slopguard (run 'make build' first)" >&2
  exit 1
fi
cp "$ROOT_DIR/bin/slopguard" "$RESOURCES_DIR/slopguard"
chmod +x "$RESOURCES_DIR/slopguard"

cat > "$CONTENTS_DIR/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>en</string>
  <key>CFBundleExecutable</key>
  <string>SlopGuard</string>
  <key>CFBundleIdentifier</key>
  <string>com.hunterlzap.slopguard</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>SlopGuard</string>
  <key>CFBundleDisplayName</key>
  <string>SlopGuard</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>0.1.0</string>
  <key>CFBundleVersion</key>
  <string>1</string>
  <key>LSMinimumSystemVersion</key>
  <string>12.0</string>
  <key>LSUIElement</key>
  <true/>
  <key>NSHighResolutionCapable</key>
  <true/>
  <key>NSAppTransportSecurity</key>
  <dict>
    <key>NSAllowsLocalNetworking</key>
    <true/>
  </dict>
</dict>
</plist>
PLIST

if command -v codesign >/dev/null 2>&1; then
  codesign --force --deep --sign - "$APP_DIR" >/dev/null 2>&1 || true
fi

echo "Built $APP_DIR"
