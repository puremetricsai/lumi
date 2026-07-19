# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What Lumi is

A local-first work-memory CLI for Apple Silicon Macs: continuously capture screen + audio, extract text locally (Tesseract OCR, whisper.cpp), store media on disk, index text in SQLite FTS5, and query it. Inspired by screenpipe but deliberately narrower — no GUI, server, plugins, or provider abstraction. Cerebras is the *only* inference backend, used solely by `lumi ask`.

## Commands

```sh
go build -o lumi ./cmd/lumi
go test ./...
go vet ./...
go test ./internal/store -run TestSearch -v   # single test
```

`internal/capture/recorder_test.go` runs the whole capture→store→search pipeline with fake `ScreenSource`/`TextExtractor`/`AudioSource`/`SpeechTranscriber` implementations, so it needs no external binaries. Prefer extending it over shelling out to real tools.

The CLI itself refuses to run on anything but `darwin/arm64` (`platform.Validate` in `PersistentPreRunE`), so `lumi record` cannot be exercised on other hosts. `./lumi doctor` reports which external binaries, whisper model, and API key are present. For a bounded manual smoke test: `./lumi record --no-audio --duration 10s`.

## Architecture

```
macOS screencapture ─→ JPEG ─→ Tesseract ─┐
                                          ├─→ events + events_fts ─→ search ─→ Cerebras ask
FFmpeg AVFoundation ─→ WAV ─→ whisper.cpp ─┘
```

Data flows one way: `internal/cli` wires concrete processors into a `capture.Recorder`, the recorder writes `store.Event` rows, and `search`/`ask` read them back. There is no dependency in the reverse direction.

**`internal/capture`** — `Recorder` runs independent screen and audio goroutines until the context is cancelled. Every external tool sits behind a small interface (`ScreenSource`, `TextExtractor`, `AudioSource`, `SpeechTranscriber`); the concrete types (`ScreenCapturer`, `OCR`, `AudioRecorder`, `Transcriber`) are thin `exec.CommandContext` wrappers whose binary path is injectable via flags. Add new capture or processing sources by implementing an interface, not by editing the loops.

**`internal/store`** — single-file SQLite via `modernc.org/sqlite` (pure Go, no cgo). `MaxOpenConns(1)` plus WAL; schema is applied idempotently in `migrate` on every `Open` — there is no versioned migration system, so schema changes must stay `CREATE ... IF NOT EXISTS`-safe against existing databases. The `events_fts` external-content FTS5 table is kept in sync by insert/delete/update triggers, so writes go only to `events`. Timestamps are stored as RFC3339Nano UTC strings and compared lexicographically — any new time column must use the same format or range filters break.

**`internal/cli`** — Cobra commands (`record`, `search`, `ask`, `doctor`, `version`). All processor binaries, languages, and intervals are flags with sane defaults, so tests and users can substitute tools.

**`internal/config`** — resolves `Paths` from `--data-dir`, else `LUMI_HOME`, else `~/Library/Application Support/Lumi`; directories are created 0700.

## Invariants worth preserving

- **Never lose captured media.** If OCR or transcription fails after a file was written, the event is still inserted with `{"processor_error": ...}` in `metadata_json` (`processorMetadata`). Don't convert processor failures into early returns that drop the file.
- **Deduplicate screen frames by SHA-256 of the JPEG bytes.** Identical consecutive frames are deleted and not indexed.
- **FTS input must go through `ftsExpression`** (`internal/store/query.go`). It quotes each term and joins with `AND` or `OR` depending on `SearchOptions.Match`; the quoting is what keeps raw user text from being interpreted as FTS5 syntax, and it applies to both joiners. `MatchAll` is the zero value, so `lumi search` keeps its conjunctive semantics without opting in. Terms with no letters or digits are dropped, and an expression that comes back empty means "run no FTS query at all" — an empty MATCH is a syntax error, not a zero-result search.
- **`ask` stages its retrieval: all-terms → any-term → recency.** `retrieveContext` (`internal/cli/retrieve.go`) strips stopwords from the question first. Feeding a raw question to an `AND` query matches nothing, which is what previously made `ask` recency-based in practice. Only `MatchAny` uses `bm25(events_fts, 1.0, 0.4, 0.4)` — without the column weights a one-word `app`/`window` hit outranks a page of relevant OCR.
- **A degraded retrieval is never silent.** Any stage past `all-terms` prints a note to stderr, so a recency-shaped answer is never mistaken for a retrieved one.
- **`lumi ask` sends text and metadata only** — screenshots and WAVs are never uploaded. Keep it that way.
- **The activity context is byte-budgeted in `contextFor`** (`internal/cli/context.go`): `maxEventChars` per event, `--max-context-chars` overall, always at least one event. The store never truncates `Event.Text` — that would corrupt `lumi search --json`, which is a data-export path.

## External dependencies

`screencapture` (system), `tesseract`, `ffmpeg`, `whisper-cli` + a model via `LUMI_WHISPER_MODEL`, and `CEREBRAS_API_KEY` for `ask` only. The Cerebras model resolves as `--model` → `LUMI_CEREBRAS_MODEL` → `config.DefaultCerebrasModel`. Frontmost app/window comes from `osascript` and degrades to empty strings on failure.
