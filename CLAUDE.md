# CLAUDE.md

Guidance for Claude Code (claude.ai/code) when working in this repository.

## What Lumi is

A local-first memory CLI for Apple Silicon Macs: continuously capture all displays plus system and
microphone audio, extract screen text locally (full-display Apple Vision OCR, with Accessibility for
focused-app attribution), transcribe audio with in-process Apple SpeechAnalyzer, store media on disk, index
text in SQLite FTS5, and query it. Inspired by screenpipe but deliberately narrower — no GUI, server, or
plugins.

## Commands

```sh
task build                                  # Swift bridge (task speech), then go build
task test                                   # full suite
task check                                  # fmt → vet → test; the verification command
task speech && go test ./internal/store -run TestSearch -v   # single test
task test:native                            # permission-gated native smoke test
task mcp                                    # hand-fed MCP handshake smoke test
```

Run `task build`/`task test`, never raw `go build`/`go test` — nothing links without `liblumispeech.a`,
which `task speech` produces.

`internal/capture/recorder_test.go` runs the whole capture→store→search pipeline with fakes, needing no
permissions or external binaries. Prefer extending it over invoking real frameworks.

The CLI refuses to run on anything but `darwin/arm64` (`platform.Validate` in `PersistentPreRunE`); native
microphone capture needs macOS 26+. `./lumi doctor` checks platform, permissions, speech assets, and the
data directory. Bounded manual smoke test: `./lumi record start --foreground --no-audio --duration 10s`.

## Architecture

```
ScreenCaptureKit displays ─→ Vision OCR (full screen) ─┐
                         └─→ Accessibility (attribution) ├─→ events + events_fts ─→ search
ScreenCaptureKit system + microphone ─→ WAV ─→ SpeechAnalyzer (in-process) ─┘
```

Data flows one way: `internal/cli` wires concrete processors into a `capture.Recorder`, the recorder writes
`store.Event` rows, and `search` reads them back. Never the reverse.

**Every package below has its own `CLAUDE.md` carrying the invariants for that package. Read it before
changing anything there** — the rationale lives with the code it constrains, not here.

| Package | What it is |
| --- | --- |
| `internal/macosnative` | cgo/Objective-C bridge: ScreenCaptureKit, Accessibility, Vision, AVFoundation WAV writing, CoreAudio process enumeration, permission preflight. Non-macOS stub. |
| `internal/capture` | `Recorder`: independent screen and audio goroutines, native processors behind small interfaces, everything in-process. Owns what an event's `app` means, and what produced an audio row's sound. |
| `internal/store` | Single-file SQLite (`modernc.org/sqlite`, no cgo), FTS5, versioned migrations, search, `audio_segments`, transcript assembly and coverage. |
| `internal/transcript` | Pure: decides where captured sound came from (`internal`/`external`) and assembles turns. No database, cgo, or filesystem. |
| `internal/wav` | Reads Lumi's mono 16-bit PCM WAVs and measures their energy. |
| `internal/vocabulary` | The custom vocabulary file's format, cache, and cap. |
| `internal/retention` | Age- and size-based pruning behind `lumi prune`. No background scheduler. |
| `internal/cli` | Cobra commands and all the wiring. |
| `internal/mcp` | Stdio MCP server: four read-only tools over `internal/store` and nothing else of Lumi's. |
| `internal/mcpsetup` | Registers `lumi mcp` with Claude Code, Claude Desktop, and Codex. |
| `internal/config` | Resolves `Paths` from `--data-dir`, else `LUMI_HOME`, else `~/Library/Application Support/Lumi`; directories 0700. |
| `internal/platform` | The `darwin/arm64` gate. |

## Rules that span packages

- **Never lose captured media.** If Accessibility, Vision, comparison, or transcription fails after a file
  was written, preserve and index the event with diagnostic metadata. Never convert a downstream failure
  into an early return that drops the file. → `internal/capture/CLAUDE.md`
- **A rule about a package lives in that package.** When a caller needs to know what another package will
  do, export the answer and read it. Two copies of a rule are correct only until one moves, and the drift
  is invisible to both test suites — that is why `store.HasSearchableTerms`, `transcript.IsSilent`,
  `store.AnyFailedTranscription`, and `transcript.EnvelopeWindowMS` are exported at all.
- **The recorder and the backfill share every rule they both apply.** Three write paths converge on
  `store.ReplaceChunkSegments`; none of them may restate a gate the others hold.
  → `internal/capture/CLAUDE.md`
- **Nothing may write to stdout in the `lumi mcp` path except JSON-RPC frames.** A stray `fmt.Println`,
  default `slog` handler, or cobra usage dump silently corrupts the session. → `internal/mcp/CLAUDE.md`
- **Schema changes go through `internal/store/migrations.go`.** Append a new `migration` with the next
  version; never edit shipped SQL. → `internal/store/CLAUDE.md`
- **Real captured conversation never becomes a test fixture.** Harnesses that need real audio read a path
  from the environment and skip without it. The measured numbers belong in the repository; the words do not.

## External dependencies

Xcode Command Line Tools plus a Swift toolchain, to build the native cgo bridge (`swiftc` compiles the
SpeechAnalyzer bridge into `liblumispeech.a`). Capture and processing are fully native — no external
binaries, and no network calls beyond Apple's on-device speech-asset download. Lumi performs no inference.
