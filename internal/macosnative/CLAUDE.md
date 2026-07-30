# internal/macosnative

cgo/Objective-C bridge to ScreenCaptureKit, Accessibility, Apple Vision, AVFoundation WAV writing, CoreAudio
process enumeration, and permission preflight. Has a non-macOS stub. `AudioSession` is the one exception to
the call-and-return shape of everything else here: it holds a live `SCStream` behind a handle and is read
with `Next`, because the stream has to stay open across chunk boundaries. `RecordAudio` takes one chunk from
a session and stops, so `native-smoke` and the recorder share a single capture path.

Nothing links without `liblumispeech.a` — build with `task build`/`task test`, never raw `go build`/`go
test`.

## Attribution

- **The frontmost pid resolves Accessibility → window list → `NSWorkspace`, and `NSWorkspace` coming last is
  the point.** `frontmostApplication` is backed by activation state maintained through run-loop
  notifications. The recorder is a detached daemon (`Setsid`) that runs no loop, so the value freezes at
  whatever was frontmost at start — the launching terminal — while the window title, read against that
  stale pid, keeps advancing. `runningApplications`/`isActive` freeze identically; neither may lead.
- **`LumiActivationPID` leads because it answers *activation*, not visibility.** An app with every window
  minimized is still what the user is working in. Two stages: system-wide AX
  `kAXFocusedApplicationAttribute` first (unreliable per-app, and retrying is *not* the remedy — identical
  errors), then `LumiFrontmostValidatedPID`, which walks window-list owners front-to-back asking each over
  *per-application* AX whether it is frontmost (`kAXFrontmostAttribute`). The second stage fixed
  misattribution at app-switch boundaries, where the top layer-0 window is still the previous app while
  activation has moved. `app_source` distinguishes the stages (`accessibility` vs `accessibility_frontmost`).
- **Candidate eligibility must stay filtered to `NSApplicationActivationPolicyRegular`.**
  `LumiFrontmostCandidates` lists on-screen window owners front-to-back (bounded to 8), then dock-visible
  `NSWorkspace.runningApplications` owning no window — the windowless-app case that would otherwise be
  unaskable. Widening the filter to every running application is the obvious generalisation and is wrong:
  background agents answer `kAXFrontmost` affirmatively, and an unfiltered walk was measured attributing
  frames to Notification Center. Only *membership* is read from `runningApplications`, never `isActive`.
- **A name is borrowed from `NSWorkspace` only when both sources mean the same pid.** Borrowing across
  differing pids names one app while reading another's title — the original bug. Where the window list
  names a pid nothing can name an app for, `LumiResolveFrontmostLive` falls back to the `NSWorkspace` pair
  **wholesale, pid included**: a stale-but-consistent pair beats a mismatched one.
- **Never lose attribution to a permission failure.** `lumi_accessibility_snapshot_json` gathers everything
  needing no AX grant — frontmost app name, input activity, `AXIsProcessTrusted()` — *before* the first AX
  call, and falls back to `CGWindowListCopyWindowInfo` for the title. Returning `NULL` there once cost 7,705
  of 12,104 events their app; it is reserved for genuine total failure, *both* sources failing, never one.
  Trust is sampled per tick, not at startup, so `doctor` reports observed attribution from the index
  alongside permission status.
- **Never assert one native frontmost read against another.** `Accessibility` and `FrontmostDiagnostic` are
  separate calls; a focus change between them makes them differ legitimately, so `native-smoke` reports the
  diagnostic and never compares it. Relatedly, pure resolvers are exposed as `*_json` entry points (as is
  `lumi_hid_access_name`) because asserting the live resolution passes vacuously in any foreground process,
  so it would fail only in the daemon, where nothing is asserting.

## Permissions and audio processes

- **Report `denied` separately from `not_determined` wherever macOS lets you**, since they need opposite
  remedies. Input Monitoring uses `IOHIDCheckAccess`; Microphone and Speech Recognition carry the
  distinction already. Screen Recording and Accessibility stay `denied_or_not_determined` on purpose —
  splitting them needs Full Disk Access or raises a prompt as a side effect. Over SSH no status call can
  prompt at all, so `--request` is a no-op.
- **The output-process list is read, never tapped, and excludes Lumi's own pid.** `AudioProcesses` reads
  CoreAudio process objects; creating a tap is what needs a TCC grant, enumerating does not — verified to
  work identically from the detached `Setsid` daemon, unlike `NSWorkspace.frontmostApplication`. Lumi is
  filtered because the capture session sets `excludesCurrentProcessAudio`, so listing it would claim
  provenance for sound the recording cannot contain. Nothing else is filtered: a process macOS cannot name
  keeps its pid, since dropping it would understate what was audible.

What the recorder does with these reads — and what an `app` on an audio row means — is
`internal/capture/CLAUDE.md`.
