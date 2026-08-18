<p align="center">
  <img src="assets/img/lumi-logo.png" alt="Lumi" width="160" height="160">
</p>

# Lumi

Lumi is a minimal, open source AI memory of everything you do. It sees your screen and hears your meetings, then turns all of it into a searchable record of your work — so you never have to take notes again. Connect it to Claude or any other AI agent and ask what happened.

Everything runs on your Mac. Nothing is uploaded, and it's completely free.

Lumi deliberately targets a small surface: capture → process → store → query. The `lumi` CLI remains the core interface; a native menu-bar `Lumi.app` is also available when building from source.

## Requirements

- Apple Silicon Mac running macOS 26 or newer (`darwin/arm64`)
- Screen Recording, Accessibility, Microphone, and Speech Recognition permissions for your terminal or the Lumi binary

To build from source (not needed for `brew install`, which ships a prebuilt binary):

- Go 1.25 or newer
- Xcode Command Line Tools and a Swift toolchain (`swiftc` compiles the SpeechAnalyzer bridge into a static archive that cgo links)

Transcription runs entirely on-device through Apple SpeechAnalyzer — no external processor or model file to install. The recognition assets for your locale (default `en-US`) download automatically on first use; override the locale with `record --speech-locale`.

ScreenCaptureKit captures system output and the default microphone directly; no loopback audio device is required. Lumi excludes its own process audio from system capture.

## Installation

```sh
brew tap puremetricsai/lumi https://github.com/puremetricsai/lumi
brew trust --formula puremetricsai/lumi/lumi
brew install lumi
lumi permissions --request
lumi doctor
lumi record start
```

Homebrew downloads the prebuilt `darwin/arm64` binary from the [latest release](https://github.com/puremetricsai/lumi/releases), so no Go or Swift toolchain is involved. Upgrade with `brew upgrade lumi`.

To build and install from source instead:

```sh
task install # compiles the Swift SpeechAnalyzer bridge (task speech), then go install
```

`task install` places the `lumi` binary in your Go bin directory — `go env GOBIN` if set, otherwise `$(go env GOPATH)/bin` — and prints the path it used. Add that directory to your `PATH` if it isn't there already. Remove the binary with `task uninstall`.

macOS grants capture permissions to a specific binary, so re-run `lumi permissions --request` after installing if you had previously granted them to a `./lumi` built in the repository.

### Native menu-bar app

```sh
task app          # build build/Lumi.app
task app:install  # install ~/Applications/Lumi.app
task app:run      # install and launch it
```

The app embeds the same `lumi` binary and offers to symlink it into a writable directory on your shell's `PATH`; it never replaces an existing `lumi` command. It supervises that CLI rather than capturing anything itself: the menu bar item shows capture state, and Settings holds recording, storage, permissions, MCP, and data-deletion controls. A recorder the app owns is visible to `lumi record status` in a terminal, and vice versa — either interface refuses to start a second one.

The `lumi` CLI reaches the app in the other direction:

```sh
lumi app             # open it, or bring it to the front (alias: lumi open)
lumi app --settings  # open its Settings window
lumi app --quit      # quit it, stopping the recorder it owns
```

The local bundle is ad-hoc signed, so every rebuild changes its TCC identity and requires Screen Recording, Accessibility, Microphone, and Speech Recognition to be granted again. `./scripts/restart-lumi-app.sh` is the development loop that does the rebuild, the TCC reset, and the relaunch in one step. Developer ID signing, notarization, DMG packaging, and automatic updates remain out of scope.

## Build and run (Development)

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
./lumi search "standup" --type audio
```

Search terms are safely combined with FTS5 `AND`. With no text argument, Lumi returns the most recent events. `--app` is an exact case-insensitive filter; `--window` is a case-insensitive substring filter. Results include timestamps and paths to the original screenshot or WAV chunk. JSON output also preserves screen-text source, display ID, audio source, and processor diagnostics.

Audio chunks are recorded from system output and from the microphone — and the two are stored as separate rows sharing one timestamp, distinguished by `audio_source`. A search returns whichever of them matched your filters, never a merge of the two. A meeting played through your speakers is transcribed on both; a call where you also speak is not. Search does not try to tell those apart, because a shared timestamp is a shared 30-second interval and not a shared sound. `lumi transcript` is the view that resolves it, returning the conversation once with the machine's own speech deduplicated.

## Transcript

```sh
./lumi transcript --since 2h
./lumi transcript --since 8h --origin external
./lumi transcript backfill --since 7d
```

`lumi search` returns audio as 30-second windows of one track, which reads poorly as conversation. `lumi transcript` reads the same audio as an ordered conversation instead, labelling every turn by where the sound came from — `internal` for what this machine played, `external` for what the microphone heard in the room, `unknown` when machine audio produced no transcript — and showing the machine's words once rather than twice. A leading `~` marks a turn whose position was inferred; a trailing score marks uncertain attribution.

Turns are derived from the two transcripts, so `lumi transcript backfill` is what fills them in for audio captured before this existed. The default pass works from the index alone and needs no WAV files; `--retranscribe` re-runs recognition to recover word timings, which is far slower, needs the audio still on disk, and refuses to run beside a live recorder. The work queue is whatever is unattributed, so an interrupted run simply resumes.

## Connect an AI agent

`lumi mcp` serves the index to any MCP-capable agent — Claude Desktop, Claude Code, Cursor, Codex — over stdin/stdout. The agent brings its own model; Lumi does no inference.

You never run the server yourself. The agent spawns `lumi mcp` as a child process when it needs it and shuts it down afterwards, so there is no daemon to keep alive, no port, and no terminal to leave open.

Because the agent keeps that process for the whole session, upgrading Lumi mid-session would otherwise leave it serving the old build — the replaced file on disk does not affect a process already running. So `lumi mcp` watches its own binary and, once the session is briefly idle, replaces itself in place with the new one. The agent's connection is preserved across the swap and it never sees an interruption. Until that happens, and if the index turns out to have been written by a newer Lumi than the server is running, every tool result says so in its `notice` rather than quietly returning results from the older build.

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

Four tools are exposed:

| Tool | Parameters | Returns |
|---|---|---|
| `search_events` | `query`, `kind` (`screen`/`audio`), `app`, `window`, `since`, `until`, `limit`, `match` (`all`/`any`), `require_text`, `max_text_chars` — all optional | matching events, ranked by relevance when `query` is set and newest first otherwise, with text capped at 600 characters by default and 20 events per page (500 maximum) |
| `get_event` | `id` | one event with its full untruncated text and processor metadata |
| `list_apps` | `app`, `since`, `until`, `limit` — all optional | the applications captured, most active first, or the window titles for one application |
| `get_transcript` | `since`, `until`, `origin`, `min_confidence`, `max_turns`, `max_text_chars` — all optional | captured audio as one ordered conversation, each turn labelled by origin with the machine's own speech deduplicated, 100 turns by default (1000 maximum) |

`since` and `until` take an RFC3339 timestamp or a duration such as `2h`. When `search_events` truncates an event's text it says so and reports the true length, so an agent can fetch the rest with `get_event`.

`get_transcript` answers the question `search_events` cannot: what was actually said, in order. Each turn is labelled `internal` (sound this machine produced — the far side of a call, a video, music, a notification, and not necessarily a person), `external` (sound the microphone picked up from the room), or `unknown` (machine audio played but produced no transcript). Because the machine's audio also bleeds into the microphone, its words appear once here rather than twice. Every turn carries a `confidence` and an `order_confidence` — `exact` when position was measured, `sequence` when order is reliable but absolute times are not, `approximate` when position was inferred — and a range holding more audio than one call returns says so and names where to resume.

`search_events` never merges an audio chunk's two rows. It does return only what matched, though — every filter applies per row, so a query hitting one track's transcript, `require_text`, or a limit falling between the two gives you one row of a pair, and that is not evidence the chunk had one track. Nor does a pair necessarily hold one sound: the microphone re-records your speakers, but it also records the room. `get_transcript` is what answers both, reading each chunk whole and deciding per segment against word timings and an energy envelope.

Results also come back with a `notice` when the outcome would otherwise be ambiguous: whether an empty page means the index itself is empty (the notice names the database file, so a mistyped `--data-dir` is obvious) or that the filters simply matched nothing, and whether a full page was capped and more results exist.

**No screenshot or audio ever leaves your machine through this interface.** The tools return text and metadata only. `media_path` is a local path you can open yourself; no tool reads those bytes.

Smoke-test the server without an agent:

```sh
task mcp
```

## Retention

Captured media stays on disk until `lumi prune` removes it — nothing is scheduled automatically. Age and
size policies, the irreversible `--all` wipe, and dry-run previews:
[docs/retention.md](docs/retention.md).

## Compression

`lumi compress` re-encodes media already on disk into smaller files without deleting any event —
screenshots to HEIC, audio to lossless FLAC, then a database rebuild. Roughly 3x on a real index. The
screenshot pass is a second lossy generation and is the one decision worth reading before you run it:
[docs/compress.md](docs/compress.md).

## Architecture

How capture, processing, storage, and query fit together, what each package owns, and how frame
deduplication and permissions work: [docs/architecture.md](docs/architecture.md).

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
