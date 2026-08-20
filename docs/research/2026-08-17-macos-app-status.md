# Lumi.app — build status and decisions

**Date:** 2026-08-17 (updated after deliverable 6)
**Purpose:** Hand-off note for the macOS menu-bar app work. It records what is done, what is
deliberately not done, and the decisions that are not recoverable from the code.

The plan's six deliverables are complete.

## Where things stand

| # | Deliverable | State |
| --- | --- | --- |
| 1 | CLI surface inventory | Done (reported, not a file) |
| 2 | TCC spike | Done → `docs/research/2026-08-17-tcc-spike.md` |
| 3 | `lumi app` + `--register-state` + app-aware refusal | Done, tested, reviewed |
| 4 | Menu bar item + Lumi window, three states | Done, tested, reviewed |
| 5 | Settings window, six tabs | Done — all six tabs exist and are wired |
| 6 | Packaging (`task app` polish, install paths) | Done — bundle validation, app/CLI install paths, and spike cleanup verified |

## What exists

**Go (additive only; no existing behavior moved).**

- `lumi app` (alias `open`), with `--settings` and `--quit` — `internal/cli/app.go`.
- `mcp setup --json`, orthogonal to `--dry-run` — `internal/cli/mcp_setup.go`.
- `record start --register-state` (requires `--foreground`), which publishes an app-owned
  recorder in `record.json`.
- `record start --emit-levels`, one JSON line per captured track on stderr.
- `permissions --json`.
- `recordState.Executable`, and an app-aware duplicate-start refusal.
- `internal/capture`: optional `Recorder.Levels` sink (`internal/capture/levels.go`).

**Swift** — `macos/Lumi/Sources/`, built by `macos/build-app.sh` via `task app`. Fourteen files: the
menu bar shell, the Lumi window, and one file per Settings tab. On first launch the app offers to
symlink its bundled CLI into the first writable directory on the login-shell `PATH`, unless a `lumi`
command already exists; it never replaces one.

## Decisions that are not visible in the code

- **Levels were measured per finished chunk, not continuously — superseded on 2026-08-18.** The
  original choice was the low-resistance one: audio reaches Go only when a chunk closes, so the
  meters refreshed once per `--audio-chunk` (30s by default). The developer required live meters
  on 2026-08-18 — a meter that answers "is my microphone working" cannot lag half a minute — and
  the native sampling path deferred here was built. Sound is now summed inside the
  ScreenCaptureKit callback and drained on a ticker; `--audio-chunk` no longer affects the meters
  at all. The figures still come from the same windowed envelope that decides silence, and there
  is still exactly one definition of "level", in `internal/wav` — the native side accumulates
  energy and never decibels. See `internal/capture/CLAUDE.md`.
- **The handoff is a `lumi://` URL, not `open -a … --args`.** LaunchServices drops arguments to
  an already-running app, which is the normal state for a menu bar app. It also gives `--quit`
  a path that costs no Automation TCC prompt, which AppleScript would.
- **`--register-state` also enforces the duplicate refusal**, not only the registration.
  Without it the app could start a second recorder over a terminal-owned one.
- **`record status` omits the `log` line when empty.** A foreground recorder opens no log of
  its own. No state that exists today has an empty `Log`, so nothing that printed before stops.
- **The app executable is `LumiApp`, not `Lumi`.** macOS filesystems are case-insensitive, so
  `Contents/MacOS/Lumi` and `Contents/MacOS/lumi` are one file and the CLI silently replaced the
  app. `build-app.sh` compares inodes and fails loudly if that recurs.
- **`macOS-design-files/` is gone from the working tree**, and its ignore rule with it; the mockups
  had served their purpose once the six tabs existed. `build/` stays ignored. The canvas is still at
  <https://claude.ai/design/p/ceb31d83-b57f-434b-bb9f-5de1da013218?via=share> if a later change needs
  to check a layout against it.
- **The bundle identifier is `com.puremetricsai.lumi`.**
- **`mcp setup --dry-run --json` is the MCP tab's status query, because there is no other one.**
  `internal/mcpsetup` exposes `Target.Apply` and nothing else, and `Apply` writes unless `DryRun` is set.
  So the tab asks what a run *would* do rather than asking what is registered. `--json` is deliberately
  orthogonal to `--dry-run`: the Set up button wants the same document back from a real run.
- **`Result.Manual`/`ManualHint` are now filled in on every result, not only where Lumi declines to
  write.** The "Copy client config" button is unconditional, and Swift must never render a client's JSON
  or TOML itself. The human output is unchanged — it still prints the snippet only on a skip or a conflict.
- **The Permissions tab follows `lumi permissions`' order, not the mockup's.** The mockup lists Microphone
  second. `Permissions.rows` is the contract and it wins; the file says so, so nobody "fixes" it back.
- **A denied *optional* service reads "Off", not red "Required".** Input Monitoring is never in
  `Permissions.missingFor`, and the spike's Result 6 shows it lands on `denied` after every rebuild.
- **Storage never migrates data.** Changing the directory re-spawns the recorder with the new
  `--data-dir` and names where the previous store stays. Moving it is the user's action.
- **"Delete all data" is refused while anything is capturing, app-owned or terminal-owned.** `prune
  --all` is the only policy that also sweeps the media directories, so it races a live recorder in the
  one direction the repository forbids: a file written just before the sweep is indexed just after it,
  leaving a row that names media which is gone. The app refuses rather than stopping capture for the
  user. Age-based prune has no such race — it never sweeps directories, and new events are never older
  than the cutoff.
- **A settings change restarts on `isSupervisingRecorder`, not on `state == .recording`.** A permission
  revoked mid-capture moves the UI to `.needsPermissions` while the child keeps running and keeps
  writing; gating on the UI state saved the setting and left the live recorder on the old flags.
- **`mcp setup --json` carries the per-client error, because the status cannot.** A target sets
  `added` *before* it attempts the write and returns that same result when the write fails. A human
  sees the error on stderr and a non-zero exit; a reader of stdout alone saw `"status": "added"` and
  rendered "registered" for a run that registered nothing.
- **The MCP tab's "run this in a terminal" advice names `--data-dir` when the app holds a custom
  one.** The app passes it on every invocation; a shell knows nothing about the app's UserDefaults, so
  the plain command would register the CLI's default root instead.
- **Quit lives in the menu bar menu, last, behind a separator — not in the window footer.** The brief
  (§4.1, §4.2) says the opposite; the developer asked for this on 2026-08-17 and it supersedes both. It
  is the only route out of the app, it carries ⌘Q, and it goes through `AppDelegate.quit()` so a live
  capture still asks first and still gets the graceful SIGTERM-then-wait. `LumiApp.confirmQuit()` was
  removed with the footer button that was its only caller.
- **Each ungranted permission row is its own button, and the primary button no longer opens a pane.**
  It used to open `missingPermissions.first`, which left the *second* missing service unreachable: with
  Screen Recording and Accessibility both missing, granting the first and returning sent the user to the
  same pane again. The two live in different panes, so one destination cannot serve both.
- **"No prompt appeared" is Result 6 of the spike, not a bug.** Screen Recording and Accessibility land
  on `denied` after a rebuild, and a denied service does not re-prompt. The window now says so. When the
  row reads enabled and the app still reads denied, that is Result 5 — `tccutil reset ScreenCapture
  com.puremetricsai.lumi` and the same for `Accessibility`, then re-enable by hand.
- **Nothing tests whether a service could still prompt.** `denied_or_not_determined` conflates "never
  asked" with "already refused" on purpose — `lumi_permissions_json` says splitting them needs Full Disk
  Access or a call that prompts as a side effect — so gating the request button on that guess would
  suppress a prompt that would have worked.
- **The Danger tab always passes `--yes` to `prune --all`.** The app gives its children
  `FileHandle.nullDevice` for stdin, so the CLI's own prompt could not be answered; the app's typed
  confirmation sheet is the gate.

## Things a future session must not get wrong

- **Every rebuild destroys the app's TCC grants.** This is the ad-hoc signature, measured in the
  spike, not a bug. Batch UI changes into single builds; the developer has to re-grant by hand
  each time. When permissions look wrong after a build, that is the cause.
- **`tccutil reset <service> com.puremetricsai.lumi`** clears a stale row that reads as enabled
  while the app is denied. It does not restore a grant.
- **Never restart the recorder on display changes.** The Go recorder owns hot-plug.
- **Never SIGKILL the recorder.** SIGTERM then wait up to 20s, or in-flight media is lost.

## Outstanding

- **The app runs and captures end to end, verified on 2026-08-17.** Permissions were granted by hand
  after a `tccutil reset`, and `lumi record status` from a terminal saw the app-owned recorder — pid,
  `screen=true audio=true` — which is `--register-state` working across the two interfaces.
- **The six Settings tabs have still not been driven by hand.** The Storage walk over a large store, the
  MCP tab's several-second load, and both Danger confirmations are the three worth exercising first.
- **The app cannot move to `-swift-version 6` yet.** `Preferences.shared` is a `static let` on a
  non-Sendable `@Observable` class, which is an error in that mode, and `LumiCLI` captures non-Sendable
  values across its pipe reads. The shipped build uses the default mode, so neither is a defect today.
