# macos

`Lumi.app`: a SwiftUI menu-bar shell around the `lumi` CLI. `Lumi/Sources/` holds the app delegate and
menu bar item (`LumiApp.swift`), the CLI runner (`LumiCLI.swift`), the recorder supervisor
(`RecorderController.swift`), the decoded CLI JSON (`Models.swift`), the Lumi window (`LumiWindow.swift`),
and one file per Settings tab — the six run Recording (in `SettingsWindow.swift`), `StorageSettings`,
`MCPSettings`, `DangerSettings`, `PermissionsSettings`, `AboutSettings`, an order that is deliberate and
explained where it is written. `build-app.sh` builds the bundle; `task app` runs it, `task app:install`
places it at `~/Applications/Lumi.app`, and `../scripts/restart-lumi-app.sh` is the development loop.

The app is a supervisor and nothing else — root `CLAUDE.md` states that rule; this file is how it is kept.

- **Every capability comes from running `lumi` with `--json` and decoding it.** Swift never opens the
  database, reads media, calls a capture framework, or writes a file the CLI owns. A rule that has a Go
  home is read from there, never restated here: `record status --json` rather than parsing `record.json`
  (`internal/cli/CLAUDE.md` owns that format), `mcp setup --dry-run --json` rather than inspecting a
  client's config, `Result.manual_hint` rather than rendering a client's JSON or TOML
  (`internal/mcpsetup/CLAUDE.md`). Every invocation passes the app's `--data-dir`, because a child inherits
  no shell environment.
- **`LumiCLI.decoder` maps keys with `.convertFromSnakeCase`, so `Models.swift` carries no key tables.**
  A model that needs a `CodingKeys` enum anyway must spell its cases in the *converted* names — a case
  written `= "manual_hint"` matches nothing once the strategy has already produced `manualHint`, and the
  field decodes as absent with no error anywhere. Go's field name is the contract; spell it in camelCase
  and it lands.
- **The app-owned recorder is a `record start --foreground --register-state` child, held for its whole
  life.** Not a detached start: launchd would become the TCC responsible process instead of the bundle, and
  the grants belong to the bundle. `--register-state` is what makes it visible to `record status`, `record
  stop`, `compress`, `transcript backfill`, and the duplicate-start refusal in a terminal — and what makes
  the app refuse to start a second one over a terminal's. `--emit-levels` feeds the meters, several JSON
  lines a second per track on stderr, measured live inside the capture callback
  (`internal/capture/CLAUDE.md`).
- **A level's staleness is counted from when the app received it, and a stale one is pruned rather than
  tested at draw time.** Freshness turns on wall-clock, which `@Observable` cannot track: nothing else in
  the recording card changes while capture is healthy, so a draw-time check never re-ran and the meters held
  their last height for as long as the window stayed open — a claim about sound nobody measured.
  `RecorderController.dropStaleLevels` prunes on the existing poll, which makes presence in `levels` *be*
  freshness, so no reader asks twice. Levels arrive continuously now, silence included, so a track going
  stale means capture stopped rather than the room going quiet.
- **A missing level is drawn as missing — "No signal yet" and an amber dot, never floor bars and a green
  one.** A track that wrote no file is never an event (`internal/macosnative`), so a denied or absent
  microphone produces no level at all, and bars at their floor beside a hardcoded `healthy: true` rendered
  that identically to a quiet room. Levels are live, so this clears within a second of capture starting: a
  row still reading "No signal yet" after that is a real answer about that track.
- **Stopping is SIGTERM then wait (`stopTimeout`, 20s), never SIGKILL** — in-flight media is still being
  written and indexed. This is also the app-quit path: `AppDelegate` returns `.terminateLater` while a
  child is held, so ⌘Q asks first and then shuts down gracefully.
- **Never restart the recorder when displays change.** The Go recorder rediscovers displays every interval.
- **A settings change restarts on `isSupervisingRecorder`, never on `state == .recording`.** A permission
  revoked mid-capture moves the UI to `.needsPermissions` while the child keeps running and keeps writing;
  gating on the UI state saves the setting and leaves the live recorder on the old flags.
- **Children get `FileHandle.nullDevice` for stdin, so no CLI prompt can ever be answered.** Anything
  interactive must be pre-answered — `prune --all --yes`, gated by the app's own typed confirmation sheet.
- **Every *development* rebuild destroys the app's TCC grants.** A local bundle is ad-hoc signed
  (`codesign -s -`), so its code identity changes each build and Screen Recording and Accessibility land on
  `denied`, which does not re-prompt. Measured in `docs/research/2026-08-17-tcc-spike.md`; batch UI changes
  into single builds, and reach for `tccutil reset <service> com.puremetricsai.lumi` when a System Settings
  row reads enabled while the app reads denied. Not a bug to fix in code, and not what a released bundle
  does: `build-app.sh` signs with `CODESIGN_IDENTITY`, ad-hoc only by default, and a release passes a real
  identity whose designated requirement is the certificate rather than the hash — so those grants survive
  an upgrade. `docs/release.md` owns that half.
- **The executable is `Contents/MacOS/LumiApp`, beside the embedded `Contents/MacOS/lumi`.** macOS
  filesystems are case-insensitive, so naming it `Lumi` makes it the same file as `lumi` and the CLI
  silently overwrites the app. `build-app.sh` compares inodes and fails loudly if that returns.
- **`lumi app` hands off over the `lumi://` URL scheme, delivered to a resolved bundle path.**
  LaunchServices drops `--args` to an already-running app, which is a menu bar app's normal state, and
  AppleScript would cost an Automation prompt. The Go side of this is `internal/cli/CLAUDE.md`.
- **The first-launch CLI symlink never replaces an existing `lumi`.** It asks the login shell for its
  `PATH` (a Finder-launched app inherits none) and offers the first writable directory on it.
- **The build stays on the default Swift language mode.** `-swift-version 6` fails today:
  `Preferences.shared` is a `static let` on a non-Sendable `@Observable` class, and `LumiCLI` captures
  non-Sendable values across its pipe reads.

There is no Swift test target. Changes are verified by driving the built app, which needs the grants
re-established after the rebuild; the Go side of each seam is covered in `internal/cli`.
