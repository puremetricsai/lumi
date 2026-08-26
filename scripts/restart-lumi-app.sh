#!/bin/sh
# The Lumi.app development loop: quit the installed dev copy, rebuild and
# reinstall it, clear its TCC grants, relaunch.
#
# Every step names ~/Applications/Lumi.app explicitly rather than going through
# `lumi app --quit`. A `lumi` run from outside a bundle has no enclosing bundle
# to prefer, so it falls to `defaultAppInstallRoots` (internal/cli/app.go),
# which searches /Applications first — and on a machine that also has the
# released copy installed it then answers "Lumi is not running" about the wrong
# bundle, sends no quit, and the dev copy survives the whole script.
set -eu

APP="$HOME/Applications/Lumi.app"
EXE="$APP/Contents/MacOS/LumiApp"

# ponytail: `pgrep -f` takes a regex and this path is not escaped, unlike
# `pgrepPattern` in internal/cli/app.go. The only metacharacter a stock macOS
# home path contributes is the dot in "Lumi.app", which matches itself; if a
# build path ever carries more, quote it the way the Go side does.
is_running() { pgrep -f "^$EXE" >/dev/null; }

# Waited for, not merely requested. `open` returns as soon as LaunchServices
# accepts the URL, while the app is still showing its quit confirmation and
# then stopping the recorder (SIGTERM, up to 20s — RecorderController.stop).
# Racing ahead from there reinstalls over a running bundle and leaves the final
# `open -a` to just activate the surviving process, which still holds the
# answers CGPreflightScreenCaptureAccess and AXIsProcessTrusted cached at
# launch — so the reset below looks like it did nothing.
#
# 90s, because the worst legitimate case is the confirmation sitting unanswered,
# then a stop that times out at 20s, then ⌘Q running a second one.
if is_running; then
  open -a "$APP" lumi://quit
  echo "waiting for Lumi to quit (confirm the prompt if it is recording)..."
  for _ in $(seq 1 180); do
    is_running || break
    sleep 0.5
  done
  # Never escalated to a kill: the recorder holds media that is written but not
  # yet indexed, and that wait is the whole reason it is graceful.
  if is_running; then
    echo "Lumi is still running after 90s — declined the quit, or the recorder" >&2
    echo "did not stop. Not reinstalling over a live copy; re-run when it exits." >&2
    exit 1
  fi
fi

task app:install
# `|| true` because tccutil exits non-zero on some macOS versions when the
# bundle id has no entry to reset, and nothing having been granted yet is the
# normal state after a fresh build — not a reason to skip the relaunch.
tccutil reset ScreenCapture com.puremetricsai.lumi || true
tccutil reset Accessibility com.puremetricsai.lumi || true
open -a "$APP"
