# Lumi.app — build status and decisions

**Date:** 2026-08-17
**Purpose:** Hand-off note for the macOS menu-bar app work. It records what is done, what is
deliberately not done, and the decisions that are not recoverable from the code.

The plan being followed has six deliverables. **1–4 are complete. 5 and 6 are not started.**

## Where things stand

| # | Deliverable | State |
| --- | --- | --- |
| 1 | CLI surface inventory | Done (reported, not a file) |
| 2 | TCC spike | Done → `docs/research/2026-08-17-tcc-spike.md` |
| 3 | `lumi app` + `--register-state` + app-aware refusal | Done, tested, reviewed |
| 4 | Menu bar item + Lumi window, three states | Done, tested, reviewed |
| 5 | Settings window, six tabs | **Not started** — only the Recording tab exists |
| 6 | Packaging (`task app` polish, install paths) | Partly done — `task app`/`app:install`/`app:run` work |

## What exists

**Go (additive only; no existing behavior moved).**

- `lumi app` (alias `open`), with `--settings` and `--quit` — `internal/cli/app.go`.
- `record start --register-state` (requires `--foreground`), which publishes an app-owned
  recorder in `record.json`.
- `record start --emit-levels`, one JSON line per captured track on stderr.
- `permissions --json`.
- `recordState.Executable`, and an app-aware duplicate-start refusal.
- `internal/capture`: optional `Recorder.Levels` sink (`internal/capture/levels.go`).

**Swift** — `macos/Lumi/Sources/`, built by `macos/build-app.sh` via `task app`.

## Decisions that are not visible in the code

- **Levels are measured per finished chunk, not continuously.** The developer chose the
  low-resistance option over adding a native sampling path through `internal/macosnative`.
  Audio only reaches Go when a chunk closes, so the meters refresh once per `--audio-chunk`
  (30s by default). The figures come from the same windowed envelope that decides silence, so
  there is exactly one definition of "level" and it lives in `internal/wav`.
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
- **`macOS-design-files/` is gitignored** by the developer's decision, as is `build/`.
- **The bundle identifier is `com.puremetricsai.lumi`.**

## Things a future session must not get wrong

- **Every rebuild destroys the app's TCC grants.** This is the ad-hoc signature, measured in the
  spike, not a bug. Batch UI changes into single builds; the developer has to re-grant by hand
  each time. When permissions look wrong after a build, that is the cause.
- **`tccutil reset <service> com.puremetricsai.lumi`** clears a stale row that reads as enabled
  while the app is denied. It does not restore a grant.
- **Never restart the recorder on display changes.** The Go recorder owns hot-plug.
- **Never SIGKILL the recorder.** SIGTERM then wait up to 20s, or in-flight media is lost.

## Outstanding

- **Deliverable 5**: Storage, MCP, Danger, Permissions, and About tabs. The MCP tab needs
  `--json` on `lumi mcp setup`, which the developer approved but which is **not yet written** —
  `internal/mcpsetup` has no read-only status path, so the tab must read
  `mcp setup --dry-run --json`.
- **Deliverable 6**: cleanup of the throwaway `~/Applications/LumiTCCSpike.app` and its TCC rows
  is owed; see the `lumi-tcc-spike-cleanup` memory.
- **Nothing is committed to git yet.** The whole of deliverables 2–4 is uncommitted working tree.
