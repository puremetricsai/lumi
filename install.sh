#!/bin/sh
# Installs Lumi.app into /Applications from the latest GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/puremetricsai/lumi/main/install.sh | sh
#
# Re-run it to upgrade. Uninstall by dragging Lumi.app to the Trash; that leaves
# ~/Library/Application Support/Lumi -- the database and captured media -- alone.
#
# curl does not set com.apple.quarantine, so the un-notarized app launches without
# the Gatekeeper "Open Anyway" detour a browser download needs.
set -eu

REPO="puremetricsai/lumi"
ASSET="lumi-macos-arm64.zip"
APP="/Applications/Lumi.app"

die() { echo "install.sh: $*" >&2; exit 1; }

[ "$(uname -s)" = "Darwin" ] || die "Lumi runs on macOS only."
[ "$(uname -m)" = "arm64" ] || die "Lumi needs Apple Silicon."
[ "$(sw_vers -productVersion | cut -d. -f1)" -ge 26 ] || die "Lumi needs macOS 26 or newer."
[ -w /Applications ] || die "/Applications is not writable; re-run with sudo."

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Downloading the latest Lumi..."
# /latest/download/ redirects to the newest release, so no tag or GitHub API here.
curl -fsSL -o "$TMP/$ASSET" "https://github.com/$REPO/releases/latest/download/$ASSET" \
  || die "download failed."

# ditto, not unzip: the signature is sealed over extended attributes that unzip drops,
# which would break the bundle's identity and with it every TCC grant.
/usr/bin/ditto -x -k "$TMP/$ASSET" "$TMP" || die "the download is not a readable archive."
[ -d "$TMP/Lumi.app" ] || die "the archive does not contain Lumi.app."
codesign --verify --strict "$TMP/Lumi.app" || die "the downloaded app failed signature verification."

# Overwriting the bundle out from under a running app leaves it executing deleted
# files. Quit through the Apple event so an in-flight recording shuts down and
# flushes its media; never SIGKILL it.
if pgrep -x LumiApp >/dev/null 2>&1; then
  echo "Quitting Lumi..."
  osascript -e 'quit app id "com.puremetricsai.lumi"' >/dev/null 2>&1 || true
  i=0
  while pgrep -x LumiApp >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 30 ] || die "Lumi is still running. Quit it and re-run."
    sleep 1
  done
fi

# Move the old bundle aside rather than deleting it, so a failed swap can put it
# back. The backup is a sibling in /Applications and not in $TMP, which the EXIT
# trap clears -- a backup there would be deleted by the very failure it exists
# for. Both moves are same-volume renames, so the window with no Lumi.app is
# milliseconds, and a live `lumi mcp` sits it out: selfexec treats a binary it
# cannot stat as no change.
#
# `if`, not `[ -d ] && mv`: a false test makes that AND-OR list the statement's
# failing last command, which `set -e` exits on -- silently aborting the fresh
# install it was meant to skip past.
rm -rf "$APP.old"
if [ -d "$APP" ]; then mv "$APP" "$APP.old"; fi
if ! mv "$TMP/Lumi.app" "$APP"; then
  if [ -d "$APP.old" ]; then mv "$APP.old" "$APP"; fi
  die "could not install into /Applications; the previous Lumi.app is unchanged."
fi
rm -rf "$APP.old"

echo "Installed $APP"
echo "Open it, then grant capture permissions in Settings > Permissions."
