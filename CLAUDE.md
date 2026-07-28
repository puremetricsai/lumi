# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What Lumi is

A local-first work-memory CLI for Apple Silicon Macs: continuously capture all displays plus system and microphone audio, extract screen text locally (full-display Apple Vision OCR, with Accessibility for focused-app attribution), transcribe audio with in-process Apple SpeechAnalyzer, store media on disk, index text in SQLite FTS5, and query it. Inspired by screenpipe but deliberately narrower — no GUI, server, or plugins.

## Commands

```sh
task build                                  # compiles the Swift bridge (task speech), then go build
task test                                   # full suite
task check                                  # fmt → vet → test; the verification command
task vet
task speech && go test ./internal/store -run TestSearch -v   # single test (needs the Swift archive)
task test:native                            # permission-gated native smoke test
```

`internal/capture/recorder_test.go` runs the whole capture→store→search pipeline with fake `ScreenSource`/`ContextExtractor`/`TextExtractor`/`AudioSource`/`SpeechTranscriber` implementations, so it needs no permissions or external binaries. Prefer extending it over invoking real frameworks. `task test:native` builds the stable `./lumi` binary and runs the explicit, permission-gated integration smoke test.

The CLI itself refuses to run on anything but `darwin/arm64` (`platform.Validate` in `PersistentPreRunE`), and native microphone capture requires macOS 26+. `./lumi doctor` checks platform, capture permissions, speech assets, and the data directory. For a bounded manual smoke test: `./lumi record start --foreground --no-audio --duration 10s`.

`task build` compiles the Swift SpeechAnalyzer bridge (`task speech`) before `go build`; run `task build`/`task test` rather than raw `go build`/`go test`, which will not link without `liblumispeech.a`.

## Architecture

```
ScreenCaptureKit displays ─→ Vision OCR (full screen) ─┐
                         └─→ Accessibility (attribution) ├─→ events + events_fts ─→ search
ScreenCaptureKit system + microphone ─→ WAV ─→ SpeechAnalyzer (in-process) ─┘
```

Data flows one way: `internal/cli` wires concrete processors into a `capture.Recorder`, the recorder writes `store.Event` rows, and `search` reads them back. There is no dependency in the reverse direction.

**`internal/macosnative`** — cgo/Objective-C bridge to ScreenCaptureKit, Accessibility, Apple Vision, AVFoundation WAV writing, and macOS permission preflight. It compiles into the Go binary and has a non-macOS stub for static/test portability.

**`internal/capture`** — `Recorder` runs independent screen and audio goroutines until the context is cancelled. Native processors remain behind small interfaces (`ScreenSource`, `ContextExtractor`, `TextExtractor`, `AudioSource`, `SpeechTranscriber`). ScreenCaptureKit enumerates displays on each interval for hotplug; full-display Apple Vision OCR is the primary screen-text source (so the indexed text reflects the entire screen, not just the focused window), the Accessibility snapshot supplies focused-app attribution (App/Window/InputActive) and its focused-window text is preserved in event metadata when substantive, and the comparer maintains independent per-display state. All capture and processing runs in-process through native Apple frameworks; no external subprocess remains.

**`internal/store`** — single-file SQLite via `modernc.org/sqlite` (pure Go, no cgo). `MaxOpenConns(1)` plus WAL; schema changes are versioned migrations in `internal/store/migrations.go`, applied on every `Open` and tracked by SQLite's `user_version` pragma (each migration runs in its own transaction). The `events_fts` external-content FTS5 table is kept in sync by insert/delete/update triggers, so writes go only to `events`. Timestamps are stored as RFC3339Nano UTC strings and compared lexicographically — any new time column must use the same format or range filters break.

**`internal/retention`** — explicit age- and size-based pruning used by `lumi prune`. Age pruning runs before size pruning; size enforcement walks indexed events oldest-first. `Options.All` is a "wipe everything" policy that enumerates every row via `store.AllEvents` (an unbounded `SELECT ... FROM events` with no timestamp cutoff, so a far-future `captured_at` is never skipped the way a bounded cutoff's strict `<` compare would), deletes rows-before-files, then sweeps `Options.MediaDirs` (wired to `Paths.Screenshots`/`Paths.Audio`) to unlink any orphaned media file no row referenced. Only the `All` policy sweeps directories; age/size are unchanged. Dry-run accounting stays equivalent to a real run (orphan bytes/files are reported, the just-removed referenced files are excluded so they aren't double-counted, and nothing is deleted). There is no background scheduler or default retention policy. Rows are deleted in bounded batches before media files are unlinked.

**`internal/cli`** — Cobra commands (`record start`/`status`/`stop`, `search`, `prune`, `doctor`, `permissions`, `native-smoke`, `version`). `record` is a parent command: `record start` runs the capture pipeline, detaching to the background by default (`--foreground` keeps it in the terminal); the background worker is a re-exec of `record start --foreground` tracked by a JSON state file and log under the data dir (`internal/cli/record_daemon.go`). `record stop` sends SIGTERM and waits for the graceful-shutdown path. Capture and audio-chunk intervals and transcription settings are flags; native framework implementations are production defaults. `search` exposes exact case-insensitive app filtering and case-insensitive window-substring filtering, plus `--type all|screen|audio`, which defaults to `all`. `permissions --request` invokes native TCC request flows; never add `tccutil reset` as an automatic side effect.

**`internal/config`** — resolves `Paths` from `--data-dir`, else `LUMI_HOME`, else `~/Library/Application Support/Lumi`; directories are created 0700.

## Invariants worth preserving

- **Never lose captured media.** If Accessibility, Vision, comparison, or transcription fails after a file was written, preserve and index the event with diagnostic metadata. Don't convert downstream processor failures into early returns that drop the file.
- **Deduplicate per display, not globally.** `FrameComparer` uses SHA-256 as an exact fast path and a sampled RGB histogram for near-duplicates. Active input raises sensitivity. A frame is still periodically retained, but on two deadlines, because a byte-identical frame carries no information a near-duplicate does: `MaxSilence` (ten seconds) forces a retained frame when the bytes *changed* but scored as similar — subtitles, video, an advancing slide — while the longer `ExactSilence` (five minutes) applies when the bytes are identical, so a frozen screen leaves a bounded presence marker instead of re-indexing the same JPEG every ten seconds. `ExactSilence` is clamped up to `MaxSilence`, so an unchanged frame is never retained more eagerly than a changed one.
- **Preserve provenance.** `text_source`, `display_id`, and `audio_source` are first-class event columns introduced by append-only migration 3 and are included in JSON exports.
- **FTS input must go through `ftsExpression`** (`internal/store/query.go`). It quotes each term and joins with `AND` or `OR` depending on `SearchOptions.Match`; the quoting is what keeps raw user text from being interpreted as FTS5 syntax, and it applies to both joiners. `MatchAll` is the zero value, so `lumi search` keeps its conjunctive semantics without opting in. Terms with no letters or digits are dropped, and an expression that comes back empty means "run no FTS query at all" — an empty MATCH is a syntax error, not a zero-result search.
- **`store.MatchAny`, `SearchOptions.RequireText`, and the `bm25(events_fts, 1.0, 0.4, 0.4)` column weights have no caller right now, and stay anyway.** They become `lumi mcp` tool parameters. The weights in particular encode a finding worth keeping: without them a one-word `app` or `window` hit outranks a page of relevant screen text. `internal/store` tests cover `MatchAny` and `RequireText` independently of any command; the column weights are documented here rather than pinned by a test.
- **Schema changes go through `internal/store/migrations.go`.** Append a new `migration` with the next version number; never edit shipped SQL. The applied version lives in SQLite's `user_version` pragma.
- **Pruning deletes rows before files.** Orphaned files are recoverable; rows pointing at missing media are not. `lumi prune` is the only code path permitted to delete media. Keep dry-run accounting equivalent to a real combined age-then-size run, and keep large deletes batched below SQLite's variable limit. `lumi prune --all` wipes everything and is irreversible, so it requires an interactive `yes` confirmation (`confirmPruneAll` in `internal/cli/root.go`); only `--yes` (scripts) or `--dry-run` (deletes nothing) may skip the prompt — keep that guard. `--all` enumerates every row with an unbounded query (never a far-future cutoff) and, after deleting rows and their files, additionally sweeps `Paths.Screenshots`/`Paths.Audio` to unlink orphaned media no row referenced — this makes the wipe a real privacy guarantee. Only `--all` sweeps directories; age/size policies must never remove orphans.
- **Capture retries without discarding completed work.** Screen failures naturally retry on the next interval; audio failures retry after a one-second delay. Media returned during cancellation gets a short cancellation-free preservation window for diagnostics and insertion.

## External dependencies

Xcode Command Line Tools plus a Swift toolchain are required to build the native cgo bridge (`swiftc` compiles the SpeechAnalyzer bridge into `liblumispeech.a`, linked by cgo). Capture and processing are fully native — no external binaries, and no network calls of its own beyond Apple's on-device speech-asset download. Lumi performs no inference.
