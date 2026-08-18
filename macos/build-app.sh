#!/bin/bash
# Build Lumi.app: a SwiftUI menu bar shell wrapped around the existing lumi
# binary, which it embeds at Contents/MacOS/lumi and drives.
#
# The Go core is not rebuilt here — `task app` depends on `task build` for that,
# so there is exactly one place the binary is produced.
#
# Signing is ad-hoc (`codesign -s -`). That is deliberate for now and it has a
# measured cost: an ad-hoc signature's designated requirement is the code
# directory hash itself, so every rebuild is a different application as far as
# TCC is concerned and the app's permissions must be granted again. See
# docs/research/2026-08-17-tcc-spike.md before treating that as a bug.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
BUILD="$REPO/build"
APP="$BUILD/Lumi.app"
CONTENTS="$APP/Contents"

LUMI_BINARY="$REPO/lumi"
if [ ! -x "$LUMI_BINARY" ]; then
  echo "error: $LUMI_BINARY is missing; run \`task build\` first" >&2
  exit 1
fi

rm -rf "$APP"
mkdir -p "$CONTENTS/MacOS" "$CONTENTS/Resources"

# The version has exactly one source: the `version` var in internal/cli, set at
# release by -ldflags. It is read from the binary being shipped rather than
# duplicated as a literal anywhere in the app.
VERSION="$("$LUMI_BINARY" version | tr -d '\n')"

# CFBundleVersion must be one to three period-separated integers. A development
# build reports "0.1.0-dev", which is not, and LaunchServices treats an invalid
# value as unversioned. CFBundleShortVersionString keeps the full string because
# it is the one a person reads; only the machine-readable field is trimmed.
BUILD_VERSION="$(printf '%s' "$VERSION" | sed -E 's/^v?([0-9]+(\.[0-9]+){0,2}).*$/\1/')"
case "$BUILD_VERSION" in
  ''|*[!0-9.]*) BUILD_VERSION="0.0.0" ;;
esac
echo "--- Lumi.app version $VERSION (CFBundleVersion $BUILD_VERSION)"

sed -e "s|<key>CFBundleShortVersionString</key><string>0.0.0</string>|<key>CFBundleShortVersionString</key><string>${VERSION}</string>|" \
    -e "s|<key>CFBundleVersion</key><string>0</string>|<key>CFBundleVersion</key><string>${BUILD_VERSION}</string>|" \
    "$HERE/Lumi/Resources/Info.plist" > "$CONTENTS/Info.plist"

# The menu bar glyph ships as a template image, so it follows the menu bar's own
# light/dark appearance instead of carrying its own colour.
if [ -f "$REPO/assets/img/lumi-logo.png" ]; then
  sips -z 36 36 "$REPO/assets/img/lumi-logo.png" \
       --out "$CONTENTS/Resources/menubar-glyph.png" >/dev/null
  cp "$REPO/assets/img/lumi-logo.png" "$CONTENTS/Resources/AppIcon.png"
fi

swiftc \
  -O \
  -target arm64-apple-macosx26.0 \
  -framework AppKit -framework SwiftUI \
  -o "$CONTENTS/MacOS/LumiApp" \
  "$HERE"/Lumi/Sources/*.swift

cp "$LUMI_BINARY" "$CONTENTS/MacOS/lumi"

# macOS filesystems are case-insensitive by default, so an app executable named
# "Lumi" and a CLI named "lumi" are one file and the copy above silently
# replaces the app. That failure looks like the bundle launching straight into
# the CLI's help text, which is a long way from its cause.
if [ "$(stat -f %i "$CONTENTS/MacOS/LumiApp")" = "$(stat -f %i "$CONTENTS/MacOS/lumi")" ]; then
  echo "error: the app executable and the CLI resolved to the same file" >&2
  exit 1
fi

# The nested binary is signed before the bundle: signing the bundle seals its
# contents, so a nested binary signed afterwards invalidates the seal.
codesign --force --sign - --timestamp=none "$CONTENTS/MacOS/lumi"
codesign --force --sign - --timestamp=none "$CONTENTS/MacOS/LumiApp"
codesign --force --sign - --timestamp=none "$APP"

echo "--- built $APP"
codesign -dvvv "$APP" 2>&1 | grep -E 'Identifier=|CDHash=|Signature='
