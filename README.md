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
./lumi search "standup" --type audio --collapse-audio
```

Search terms are safely combined with FTS5 `AND`. With no text argument, Lumi returns the most recent events. `--app` is an exact case-insensitive filter; `--window` is a case-insensitive substring filter. Results include timestamps and paths to the original screenshot or WAV chunk. JSON output also preserves screen-text source, display ID, audio source, and processor diagnostics.

Each audio chunk is recorded twice — once from system output and once from the microphone — so a meeting played through your speakers is transcribed on both tracks. `--collapse-audio` merges that duplicate into one result and reports where the speech came from: `system` (a remote speaker or media), `microphone` (your own voice in the room), `both`, or `silent`. It is opt-in because it changes the shape of `--json` from a bare event array to `{events, audio_chunks}`, where each chunk names the surviving event's origin and the tracks merged into it.

## Custom vocabulary

Apple's on-device transcriber sometimes mishears names and jargon outside its general vocabulary, and a
misheard term is permanently unsearchable. Drop an optional `vocabulary.txt` in your data directory — one
term or phrase per line, UTF-8 — to bias recognition toward it:

```text
# people
Mostafa
Lumi

# jargon
SpeechAnalyzer
```

Blank lines and lines starting with `#` are ignored, surrounding whitespace is trimmed, and exact duplicate
terms collapse to their first occurrence. File order is priority order: only the first 100 terms are used,
and anything past that cap is dropped rather than silently ignored — `lumi doctor` reports how many. An edit
takes effect on the next audio chunk; no restart needed.

Compare the effect on fixed audio with `lumi transcribe`, which replays one WAV through the same
transcription path the recorder uses:

```sh
./lumi transcribe recording.wav --no-vocabulary   # baseline
./lumi transcribe recording.wav                    # with vocabulary.txt applied
./lumi transcribe recording.wav --vocabulary other.txt  # a specific list instead
```

Comparing two live recordings would confound the vocabulary with how the words happened to be spoken;
replaying the same file isolates the term list as the only variable. `lumi transcribe` also takes
`--speech-locale` (same default as `record`, `en-US`), for replaying audio in a non-default locale.

An explicit `--vocabulary <path>` that is missing or unreadable is a hard, non-zero error — the behavior
you're most likely to hit by accident, for example a typo'd path or an unset `--vocabulary="$VOCAB"`. This
is deliberate: silently falling back would print an ordinary baseline transcript that looks
vocabulary-assisted, defeating the comparison this command exists to make. The default file (no
`--vocabulary` given) is different — its absence stays silent, since running with no vocabulary at all is a
legitimate baseline.

## Connect an AI agent

`lumi mcp` serves the index to any MCP-capable agent — Claude Desktop, Claude Code, Cursor, Codex — over stdin/stdout. The agent brings its own model; Lumi does no inference.

You never run the server yourself. The agent spawns `lumi mcp` as a child process when it needs it and shuts it down afterwards, so there is no daemon to keep alive, no port, and no terminal to leave open.

### Automatic setup

```sh
./lumi mcp setup                          # Claude Code, Claude Desktop, and Codex CLI
./lumi mcp setup --dry-run                # show what would change, write nothing
./lumi mcp setup --client codex           # one client only: code, desktop, or codex
./lumi mcp setup --client desktop --force # replace an entry that already exists
```

Setup writes the entry with an absolute binary path and an explicit `--data-dir`, always. That is deliberate: agents launch MCP servers with a bare environment, so `LUMI_HOME` from your shell profile never reaches the server, and a relative `command` may not resolve either.

- **Claude Code** is configured at user scope through the `claude` CLI, which is the only supported way to modify `~/.claude.json` — that file is live application state, not just settings.
- **Claude Desktop** has no CLI, so its `claude_desktop_config.json` is edited in place. Every other key is preserved and a `.lumi-backup` copy is written first, though top-level keys come back in alphabetical order. **Quit Claude Desktop before running setup and reopen it afterwards** — it only reads the config at launch, and quitting also avoids racing its own writes.
- **Codex CLI** is configured through the `codex` CLI in both directions: `codex mcp get --json` to see what is already registered, `codex mcp add`/`remove` to change it. That is the only supported writer for `~/.codex/config.toml`, and it preserves the comments and top-level keys a hand-rolled TOML round-trip would drop. Note that `codex` itself rewrites the whole `mcp_servers` table when it adds an entry, so other servers there may come back reformatted — their values are unchanged, and everything outside that table is untouched. There is no scope to choose: `codex mcp add` always writes the user-level file.
- Clients that are not installed are skipped, and setup prints the snippet to paste — JSON or TOML, whichever that client reads — for anything it does not cover.

It is idempotent: a second run reports `unchanged` and writes nothing. An entry that already exists with *different* settings is never overwritten — setup reports the conflict and exits non-zero, so `--dry-run` doubles as a health check. Use `--force` to replace it, or `--name` to register a second entry pointing at a second data directory.

### Manual setup

For a client Lumi does not know about, or if you would rather edit the file yourself:

```json
{ "mcpServers": { "lumi": { "command": "/usr/local/bin/lumi", "args": ["mcp", "--data-dir", "/Users/you/Lumi"] } } }
```

Three tools are exposed:

| Tool | Parameters | Returns |
|---|---|---|
| `search_events` | `query`, `kind` (`screen`/`audio`), `app`, `window`, `since`, `until`, `limit`, `match` (`all`/`any`), `require_text`, `max_text_chars`, `collapse_audio_tracks` — all optional | matching events, ranked by relevance when `query` is set and newest first otherwise, with text capped at 600 characters by default and 20 events per page (500 maximum) |
| `get_event` | `id` | one event with its full untruncated text and processor metadata |
| `list_apps` | `app`, `since`, `until`, `limit` — all optional | the applications captured, most active first, or the window titles for one application |

`since` and `until` take an RFC3339 timestamp or a duration such as `2h`. When `search_events` truncates an event's text it says so and reports the true length, so an agent can fetch the rest with `get_event`.

Unlike the CLI, `search_events` collapses each audio chunk's microphone/system duplicate by default, so an agent does not read the same meeting twice. The surviving result carries `audio_origin` and, when more than one track was merged, `audio_tracks` listing each track's id, source, text length, and media path — never its text, which `get_event` fetches by id. Pass `collapse_audio_tracks: false` to see both rows unmerged.

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
- `internal/mcpsetup`: registering `lumi mcp` with installed MCP clients
- `internal/vocabulary`: the custom vocabulary file's format, cache, and cap
- `internal/config`: data-directory path resolution
- `internal/cli`: Cobra commands and lifecycle

Frames use a hash fast path plus a sampled color-histogram comparison with independent state per display; recent user input makes the threshold more sensitive. Two safety intervals keep capture from going silent: a frame whose bytes changed but scored as a near-duplicate — a video, an advancing slide — is retained at least every ten seconds, while a byte-identical frame is retained every five minutes, so a frozen screen leaves a bounded presence marker instead of re-indexing the same JPEG. Full-display Vision OCR is the primary screen-text source, so the index reflects the whole screen rather than just the focused window; the Accessibility snapshot supplies focused-app attribution and its window text is kept in event metadata when substantive. If Accessibility, Vision, comparison, or transcription fails after media was captured, Lumi preserves and indexes the event with processor diagnostics instead of silently losing the original data.

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
