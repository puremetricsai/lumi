# Lumi

Lumi is an open-source, local-first work memory for Apple Silicon Macs. It continuously captures every display plus system and microphone audio, extracts screen text with full-display Apple Vision OCR (using macOS Accessibility for focused-app attribution), transcribes speech on-device with Apple SpeechAnalyzer, stores the media on disk, indexes the text in SQLite FTS5, and lets you search and manage it from a Go CLI. `lumi mcp` exposes the same index to the AI agent of your choice over MCP.

Lumi deliberately targets a small surface: capture → process → store → query, with no GUI, server, or plugins.

## Requirements

- Apple Silicon Mac running macOS 26 or newer (`darwin/arm64`)
- Go 1.25 or newer (to build)
- Xcode Command Line Tools and a Swift toolchain (`swiftc` compiles the SpeechAnalyzer bridge into a static archive that cgo links)
- Screen Recording, Accessibility, Microphone, and Speech Recognition permissions for your terminal or the Lumi binary

Transcription runs entirely on-device through Apple SpeechAnalyzer — no external processor or model file to install. The recognition assets for your locale (default `en-US`) download automatically on first use; override the locale with `record --speech-locale`.

ScreenCaptureKit captures system output and the default microphone directly; no loopback audio device is required. Lumi excludes its own process audio from system capture.

## Build and run

```sh
task build # compiles the Swift SpeechAnalyzer bridge (task speech), then go build
./lumi permissions --request # or: task permissions
./lumi doctor
./lumi record start
```

`record start` launches recording in the background and returns; check on it with
`./lumi record status` and stop it with `./lumi record stop` (which waits for a graceful
shutdown so in-flight media finishes indexing). Add `--foreground` to run in the current
terminal until `Ctrl-C` instead. Useful options include:

```sh
# Screen only, every ten seconds
./lumi record start --no-audio --interval 10s

# All displays plus 60-second system and microphone audio chunks
./lumi record start --audio-chunk 60s

# Bounded smoke test, in the foreground
./lumi record start --foreground --no-audio --duration 10s
```

The defaults are a two-second screen interval and 30-second audio chunks. `--no-screen` and `--speech-locale` are also available. Displays are discovered again on every interval, so connecting or disconnecting a display does not require restarting Lumi.

The default data directory is `~/Library/Application Support/Lumi`:

```text
Lumi/
├── lumi.db
├── record.json
├── record.log
├── screenshots/
└── audio/
```

Override it with `--data-dir` or `LUMI_HOME`. `record.json` and `record.log` are the background recorder's state file and log.

Earlier versions also wrote a `config.json` here to hold the provider settings for the removed `lumi ask` command. Lumi no longer reads it, and upgrading leaves it in place. If yours holds an API key, delete the file and rotate the key.

## Search

```sh
./lumi search "quarterly roadmap"
./lumi search "launch budget" --type audio --since 8h
./lumi search --app "Safari" --window "Quarterly Plan" --since 24h
./lumi search --type screen --since 2026-07-18T09:00:00-07:00 --json
```

Search terms are safely combined with FTS5 `AND`. With no text argument, Lumi returns the most recent events. `--app` is an exact case-insensitive filter; `--window` is a case-insensitive substring filter. Results include timestamps and paths to the original screenshot or WAV chunk. JSON output also preserves screen-text source, display ID, audio source, and processor diagnostics.

## Connect an AI agent

`lumi mcp` serves the index to any MCP-capable agent — Claude Desktop, Claude Code, Cursor, Codex — over stdin/stdout. The agent brings its own model; Lumi does no inference.

```json
{ "mcpServers": { "lumi": { "command": "lumi", "args": ["mcp"] } } }
```

If your capture data lives outside the default directory, pass it explicitly — agents launch MCP servers with a bare environment, so `LUMI_HOME` from your shell profile will not reach it:

```json
{ "mcpServers": { "lumi": { "command": "lumi", "args": ["mcp", "--data-dir", "/Users/you/Lumi"] } } }
```

Three tools are exposed:

| Tool | Parameters | Returns |
|---|---|---|
| `search_events` | `query`, `kind` (`screen`/`audio`), `app`, `window`, `since`, `until`, `limit`, `match` (`all`/`any`), `require_text`, `max_text_chars` — all optional | matching events, ranked by relevance when `query` is set and newest first otherwise, with text capped at 600 characters by default and 20 events per page (500 maximum) |
| `get_event` | `id` | one event with its full untruncated text and processor metadata |
| `list_apps` | `app`, `since`, `until`, `limit` — all optional | the applications captured, most active first, or the window titles for one application |

`since` and `until` take an RFC3339 timestamp or a duration such as `2h`. When `search_events` truncates an event's text it says so and reports the true length, so an agent can fetch the rest with `get_event`.

Results also come back with a `notice` when the outcome would otherwise be ambiguous: whether an empty page means the index itself is empty (the notice names the database file, so a mistyped `--data-dir` is obvious) or that the filters simply matched nothing, and whether a full page was capped and more results exist.

**No screenshot or audio ever leaves your machine through this interface.** The tools return text and metadata only. `media_path` is a local path you can open yourself; no tool reads those bytes.

Smoke-test the server without an agent:

```sh
task mcp
```

## Retention

Captured JPEG and WAV files remain on disk until explicitly pruned. Preview an age policy before applying it:

```sh
./lumi prune --older-than 720h --dry-run
./lumi prune --older-than 720h
```

You can instead cap indexed media by bytes, or combine both policies. Age pruning runs first; the size pass then removes the oldest remaining events until the cap is met.

```sh
# Keep indexed media under 50 GiB
./lumi prune --max-bytes 53687091200

# Combine age and size policies and emit a machine-readable preview
./lumi prune --older-than 2160h --max-bytes 53687091200 --dry-run --json
```

To wipe everything, `--all` deletes every indexed event and all media, then sweeps the `screenshots/` and `audio/` directories for orphaned files no row referenced. It is irreversible, so it prompts you to type `yes`; only `--yes` (for scripts) or `--dry-run` (which deletes nothing) skips the prompt.

```sh
./lumi prune --all --dry-run
./lumi prune --all
```

Lumi does not schedule retention automatically. Run `prune` periodically yourself if you want a fixed policy. Database rows are deleted before their media files, so an interrupted prune can leave recoverable orphaned files rather than indexed events whose media has disappeared.

## Architecture

```text
ScreenCaptureKit displays ─→ Vision OCR (full screen) ─┐
                         └─→ Accessibility (attribution) ├─→ events + FTS5 ─→ search / mcp
ScreenCaptureKit system + microphone ─→ WAV ─→ SpeechAnalyzer (in-process) ─┘
```

- `internal/macosnative`: cgo bridge to ScreenCaptureKit, Accessibility, Vision, and permission APIs
- `internal/capture`: testable capture orchestration, perceptual deduplication, and transcription
- `internal/store`: versioned SQLite migrations, FTS5 triggers, inserts, and filtered search
- `internal/retention`: age-, size-, and wipe-based event/media pruning
- `internal/mcp`: the read-only MCP tool surface served over stdio
- `internal/config`: data-directory path resolution
- `internal/cli`: Cobra commands and lifecycle

Frames use a hash fast path plus a sampled color-histogram comparison with independent state per display; recent user input makes the threshold more sensitive, and a ten-second safety interval prevents capture from going silent. Full-display Vision OCR is the primary screen-text source, so the index reflects the whole screen rather than just the focused window; the Accessibility snapshot supplies focused-app attribution and its window text is kept in event metadata when substantive. If Accessibility, Vision, comparison, or transcription fails after media was captured, Lumi preserves and indexes the event with processor diagnostics instead of silently losing the original data.

`lumi permissions --request` invokes Apple's native Screen Recording, Accessibility, Microphone, and Speech Recognition request flows. `lumi doctor` reports their current state with the matching System Settings location. Input Monitoring is informational and is only requested when `--input-monitoring` is explicitly passed; capture does not require an event tap.

## Development

```sh
task build   # compiles the Swift bridge, then go build
task test    # full suite (raw go build/go test will not link without the Swift archive)
go vet ./...
```

After granting permissions, run the bounded native framework smoke test with:

```sh
task test:native
```

Lumi is licensed under the MIT License.
