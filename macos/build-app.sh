#!/bin/bash
# Build Lumi.app: a SwiftUI menu bar shell wrapped around the existing lumi
# binary, which it embeds at Contents/MacOS/lumi and drives.
#
# The Go core is not rebuilt here — `task app` depends on `task build` for that,
# so there is exactly one place the binary is produced.
#
# Signing follows CODESIGN_IDENTITY, which defaults to ad-hoc (`-`). That is the
# development default and it has a measured cost: an ad-hoc signature's
# designated requirement is the code directory hash itself, so every rebuild is
# a different application as far as TCC is concerned and the app's permissions
# must be granted again. See docs/research/2026-08-17-tcc-spike.md before
# treating that as a bug. A release passes a real identity here instead, which
# gives the bundle a stable requirement and adds the Hardened Runtime and a
# secure timestamp that notarization needs — see docs/release.md.
#
# `build-app.sh --self-test` asserts the two decisions this script makes from
# that one variable, without building anything.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
BUILD="$REPO/build"
APP="$BUILD/Lumi.app"
CONTENTS="$APP/Contents"

CODESIGN_IDENTITY="${CODESIGN_IDENTITY:--}"

# The two version fields both come out of the one string the binary reports.
#
# CFBundleShortVersionString is the one a person reads, and it is the tag with
# its leading `v` stripped: `v0.6.0` is a git tag, `0.6.0` is a version. The
# cask's `version` field, the formula's, and this must agree, and only one of
# the three can carry a `v`.
#
# CFBundleVersion must be one to three period-separated integers. A development
# build reports "0.1.0-dev", which is not, and LaunchServices treats an invalid
# value as unversioned — so only the machine-readable field is trimmed.
normalize_version() {
  local short build
  short="${1#v}"
  build="$(printf '%s' "$short" | sed -E 's/^([0-9]+(\.[0-9]+){0,2}).*$/\1/')"
  case "$build" in
    ''|*[!0-9.]*) build="0.0.0" ;;
  esac
  printf '%s %s\n' "$short" "$build"
}

# Sets SIGN to the codesign arguments for an identity. Ad-hoc cannot carry a
# secure timestamp and gains nothing from the Hardened Runtime; a real identity
# needs both, because notarization rejects a bundle without them.
sign_args() {
  if [ "$1" = "-" ]; then
    SIGN=(--force --sign - --timestamp=none)
  else
    SIGN=(--force --sign "$1" --options runtime --timestamp)
  fi
}

if [ "${1:-}" = "--self-test" ]; then
  fail=0
  expect() {
    [ "$2" = "$3" ] || { echo "$1: got '$2', want '$3'" >&2; fail=1; }
  }
  expect "normalize_version v0.6.0"     "$(normalize_version v0.6.0)"     "0.6.0 0.6.0"
  expect "normalize_version 0.6.0"      "$(normalize_version 0.6.0)"      "0.6.0 0.6.0"
  expect "normalize_version v0.1.0-dev" "$(normalize_version v0.1.0-dev)" "0.1.0-dev 0.1.0"
  expect "normalize_version 0.1.0-dev"  "$(normalize_version 0.1.0-dev)"  "0.1.0-dev 0.1.0"
  expect "normalize_version v1.2"       "$(normalize_version v1.2)"       "1.2 1.2"
  expect "normalize_version dev"        "$(normalize_version dev)"        "dev 0.0.0"
  sign_args -
  expect "sign_args -" "${SIGN[*]}" "--force --sign - --timestamp=none"
  sign_args "Developer ID Application: Example (ABCDE12345)"
  expect "sign_args <identity>" "${SIGN[*]}" \
    "--force --sign Developer ID Application: Example (ABCDE12345) --options runtime --timestamp"
  [ "$fail" -eq 0 ] && echo "--- build-app.sh self-test ok"
  exit "$fail"
fi

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
# The binary is invoked in a plain assignment, deliberately. `set -e` fires on a
# failed command substitution there; inside a `read <<<` herestring it does not,
# and a `lumi` that cannot run would stamp the bundle "0.0.0" and build on.
VERSION="$("$LUMI_BINARY" version | tr -d '\n')"
read -r SHORT_VERSION BUILD_VERSION <<<"$(normalize_version "$VERSION")"
echo "--- Lumi.app version $SHORT_VERSION (CFBundleVersion $BUILD_VERSION, signed by ${CODESIGN_IDENTITY})"

sed -e "s|<key>CFBundleShortVersionString</key><string>0.0.0</string>|<key>CFBundleShortVersionString</key><string>${SHORT_VERSION}</string>|" \
    -e "s|<key>CFBundleVersion</key><string>0</string>|<key>CFBundleVersion</key><string>${BUILD_VERSION}</string>|" \
    "$HERE/Lumi/Resources/Info.plist" > "$CONTENTS/Info.plist"
plutil -lint "$CONTENTS/Info.plist" >/dev/null

# The seds above match the template's exact one-line spelling and cannot insert, so a
# reformatted Info.plist makes them no-op silently: `plutil -lint` still passes and the
# bundle ships as version 0.0.0, build 0. Assert what was written, not that it parses.
expect_plist() {
  local got
  got="$(/usr/libexec/PlistBuddy -c "Print :$1" "$CONTENTS/Info.plist")"
  [ "$got" = "$2" ] || { echo "error: $1 is '$got', expected '$2'" >&2; exit 1; }
}
expect_plist CFBundleShortVersionString "$SHORT_VERSION"
expect_plist CFBundleVersion "$BUILD_VERSION"

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
sign_args "$CODESIGN_IDENTITY"
codesign "${SIGN[@]}" "$CONTENTS/MacOS/lumi"
codesign "${SIGN[@]}" "$CONTENTS/MacOS/LumiApp"
codesign "${SIGN[@]}" "$APP"

codesign --verify --deep --strict "$APP"

# Both executables must remain native arm64 binaries. This also catches the
# case-insensitive-name collision above on a case-sensitive build volume.
lipo "$CONTENTS/MacOS/LumiApp" -verify_arch arm64
lipo "$CONTENTS/MacOS/lumi" -verify_arch arm64

echo "--- built $APP"
codesign -dvvv "$APP" 2>&1 | grep -E 'Identifier=|CDHash=|Signature='
