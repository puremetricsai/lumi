# internal/macosnative

cgo/Objective-C bridge to ScreenCaptureKit, Accessibility, Apple Vision, AVFoundation WAV writing, CoreAudio
process enumeration, and permission preflight. Has a non-macOS stub. `AudioSession` is the one exception to
the call-and-return shape of everything else here: it holds a live `SCStream` behind a handle and is read
with `Next`, because the stream has to stay open across chunk boundaries. `RecordAudio` takes one chunk from
a session and stops, so `native-smoke` and the recorder share a single capture path.

Nothing links without `liblumispeech.a` — build with `task build`/`task test`, never raw `go build`/`go
test`.

## Media transcoding

`TranscodeImageHEIC`, `EncodeAudioFLAC`, `DecodeMonoPCM16` and `InspectImage` back `lumi compress`. All are
pure file-to-file work needing no TCC grant, so their tests are ordinary build-tagged tests rather than
`LUMI_NATIVE_SMOKE` ones.

- **The encoders verify by reopening what they wrote.** `CGImageDestinationFinalize` reporting success does
  not establish that the bytes on disk decode to the right picture — a truncated write still finalises —
  and compress deletes the source once these report success.
- **`lumi_audio_decode_pcm16` returns a malloc'd buffer, not a C string**, the only entry point here that
  does: a 30-second chunk is 480,000 samples. Go owns the pointer, validates the reported frame count and
  sample rate *before* `C.GoBytes` (whose length is an `int`), and frees it. Anything else returning bulk
  data should follow that shape rather than the `nativeString`/`nativeJSON` one.
- **FLAC carries its source bit depth in the ASBD's `mFormatFlags`, and no `AVAudioFile` settings key
  reaches it.** `AVFormatIDKey` alone yields a format reported as "UNKNOWN source bit depth" whose first
  write fails, and `AVEncoderBitDepthHintKey` does not fill it in. There is no FLAC `AVFileType`, so
  `AVAssetWriter` is not an alternative. The writer must also be released before the file is read or
  stat'd — `AVAudioFile` finalises its header on deallocation, so anything earlier sees a 42-byte stub.
- **Audio reads are bounded by the file's own `length`**, not run until a zero-length read, which
  `AVAudioFile` does not reliably give for a compressed source.
- **The FLAC encoder emits nothing below `MinFLACFrames` (4608), and it does not flush the shorter tail on
  close.** It buffers one block plus its lookahead (4096 + 512) before writing a frame, so a shorter input
  finalises as a bare 42-byte STREAMINFO header whose reopen fails with `kAudioFileUnsupportedFileTypeError`
  (`'typ?'`) — a *silent* loss, since the encode itself reports success. The threshold is exact and
  rate-independent (measured at 8/16/48 kHz). `EncodeAudioFLAC` does not guard it because it does not decode
  its input; the frame count is known to `internal/compress`, which reads `MinFLACFrames` and declines the
  round trip rather than write a stub and mislabel it a verification failure. The sub-second tail chunks a
  stopped recording leaves are the real inputs this catches.

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
- **`LumiAudioMarkerWindowsIn` scans every on-screen window's title but reports only self-declared
  emitters.** It looks for the marker Chromium appends while a tab plays sound (`" - Audio playing"`,
  matched by containment — it sits *before* the browser name, so a suffix test never matches), returns only
  the windows carrying it with the marker stripped, and discards every other title in-place. It reads the
  same `CGWindowListCopyWindowInfo` the attribution snapshot already copies, so it needs no permission the
  recorder does not hold — `kCGWindowName` requires Screen Recording, which is definitionally held wherever
  there is captured audio to attribute. What it reports is the same class of data `events.window` already
  indexes, and only for rows whose audio Lumi is recording anyway. Lumi's own pid is excluded for the same
  reason `AudioProcesses` excludes it.
- **Chunk timestamps are measured at rotation, not derived from the session anchor.** `wallClockForPTS`
  survives as the drift-free *grid* reference and as the fallback when a guard rejects a measured read;
  `measuredWallClockForPTS` ages the host clock back to the boundary. See `internal/capture/CLAUDE.md` for
  why the guard tolerance is 250 ms and why it comes from the turn-merge headroom rather than the chunk
  duration.
- **The output-process list is read, never tapped, and excludes Lumi's own pid.** `AudioProcesses` reads
  CoreAudio process objects; creating a tap is what needs a TCC grant, enumerating does not — verified to
  work identically from the detached `Setsid` daemon, unlike `NSWorkspace.frontmostApplication`. Lumi is
  filtered because the capture session sets `excludesCurrentProcessAudio`, so listing it would claim
  provenance for sound the recording cannot contain. Nothing else is filtered: a process macOS cannot name
  keeps its pid, since dropping it would understate what was audible.

What the recorder does with these reads — and what an `app` on an audio row means — is
`internal/capture/CLAUDE.md`.

## The Keychain

Lumi's encryption key lives here for the same reason everything else in this package does: it is a
Security.framework call that needs cgo, and the rest of Lumi is written against a Go function.

- **Nothing in `keychain.m` may write to stdout.** `lumi mcp` reads the key on startup, and one stray
  byte corrupts the JSON-RPC stream for the whole session. `TestServeWritesOnlyJSONRPCFramesToStdout`
  does not cover this path, so the rule is kept by construction.
- **It is the legacy file-based keychain, and that was measured rather than assumed.** The
  data-protection keychain is better — it refuses other processes outright instead of prompting — but
  its access control is by application-identifier entitlement, and an ad-hoc build gets
  `errSecMissingEntitlement` (-34018) from `SecItemAdd`, so no development build could store a key at
  all. The legacy keychain takes a `SecAccess` ACL naming this binary, which is the per-binary control
  the feature actually wants. Its cost is that `kSecAttrAccessible*` is ignored there, so the
  accessibility attribute is deliberately absent rather than set and silently ineffective.
- **`HasEncryptionKey` asks for attributes, never for data, and that is the whole reason it exists.**
  Reading the *data* is what the ACL gates, so it prompts anyone who is not this binary — and `lumi
  search` only needs to know whether to refuse. Implementing it as "try to load the key and see"
  would pop a Keychain dialog on the way to printing an error. Measured with an ad-hoc signature:
  this binary reads its own key with no prompt, `/usr/bin/security` asking for the data blocks on a
  prompt, and an attributes query is ungated for anybody.
- **`StoreEncryptionKey` deletes before it adds.** An existing item carries an ACL trusting whatever
  binary wrote it, which after a rebuild is a code identity this process no longer has; re-adding is
  how the ACL is re-established.
- **The account name is fixed, not the data directory's path.** Keying it on the path would mean
  Storage settings' **Choose…** button silently produced a store nothing could decrypt. One key per
  user covering every data directory is the trade.
- **Development cost, flagged rather than solved.** `build-app.sh` signs ad-hoc, so the designated
  requirement is a bare `cdhash` that changes every build and the ACL trusts a binary that no longer
  exists — expect a Keychain prompt after each `restart-lumi-app.sh`. Same class as the TCC grants a
  rebuild already destroys (`macos/CLAUDE.md`), and the same non-fix.
