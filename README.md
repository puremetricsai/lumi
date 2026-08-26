<p align="center">
  <img src="assets/img/lumi-logo.png" alt="Lumi" width="160" height="160">
</p>

# Lumi

Lumi is a minimal, open source AI memory of everything you do. It sees your screen and hears your meetings, then turns all of it into a searchable record of your work — so you never have to take notes again. Connect it to Claude or any other AI agent and ask what happened.

Everything runs on your Mac. Nothing is uploaded, and it's completely free.

Lumi deliberately targets a small surface: capture → process → store → query. `Lumi.app` is the whole of it — a menu-bar app that captures in the background and answers questions through any AI agent you connect.

## Requirements

- Apple Silicon Mac running macOS 26 or newer (`darwin/arm64`)
- Screen Recording, Accessibility, Microphone, and Speech Recognition permissions, granted to Lumi

To build from source (not needed for `brew install`, which ships a prebuilt app):

- Go 1.25 or newer
- Xcode Command Line Tools and a Swift toolchain (`swiftc` compiles the SpeechAnalyzer bridge into a static archive that cgo links)

Transcription runs entirely on-device through Apple SpeechAnalyzer — no external processor or model file to install. The recognition assets for your locale (default `en-US`) download automatically on first use; the locale is a setting in the Recording tab.

ScreenCaptureKit captures system output and the default microphone directly; no loopback audio device is required. Lumi excludes its own process audio from system capture.

## Installation

```sh
brew tap puremetricsai/lumi https://github.com/puremetricsai/lumi
brew trust --cask puremetricsai/lumi/lumi
brew install --cask puremetricsai/lumi/lumi
```

That puts `/Applications/Lumi.app` in place. Upgrade it with:

```sh
brew upgrade --cask puremetricsai/lumi/lumi
```

Open Lumi, then grant capture permissions from **Settings → Permissions**. macOS grants them to Lumi itself, so the prompts come from the app and nothing else needs approving. Recording starts from the menu bar item or the Lumi window.

Homebrew downloads a prebuilt `darwin/arm64` build from the [latest release](https://github.com/puremetricsai/lumi/releases), so no Go or Swift toolchain is involved. Uninstalling leaves `~/Library/Application Support/Lumi` — your database and captured media — untouched.

Homebrew is the update mechanism. Lumi has no self-updater and is not getting one.

### Upgrading from 0.2.0 or earlier

Those versions also put a `lumi` command on your `PATH` and let you drive Lumi from a terminal. Lumi is now the app alone, and Homebrew no longer installs that command. Your database and captured media are unaffected, but AI agents were registered against the old path and stop resolving once it goes, so re-register them from **Settings → MCP** — a client still naming it shows as a conflict with a **Replace** button.

If you took Lumi up on its old offer to link the command into a directory of your own, Homebrew does not manage that link and will not remove it. It keeps working, because it points inside the bundle. Delete it yourself if you would rather it were gone.

Releases up to 0.7.0 also shipped a `lumi` *formula*. If you are still on it, the tap points the old name at the cask, but the two cannot replace each other in place:

```sh
brew uninstall --formula lumi
brew install --cask puremetricsai/lumi/lumi
```

macOS grants capture permissions to a specific application, so re-approve them in **Settings → Permissions** afterwards.

### The app is not notarized yet

Lumi has no Apple Developer ID certificate, so the released app carries the project's own signing certificate instead. macOS quarantines the cask install and refuses the first launch. Open Lumi once from Finder, then open **System Settings → Privacy & Security**, find the message naming Lumi, and choose **Open Anyway**.

Quarantine is written to every file inside the bundle, so if Lumi still will not start after you have approved it, clear the flag once:

```sh
xattr -dr com.apple.quarantine /Applications/Lumi.app
```

This is once per machine, not once per release: the signing certificate is stable, so Homebrew keeps the approval across upgrades and your granted permissions survive them. All of it goes away with notarization.

## Using Lumi

The menu bar item shows capture state and starts and stops recording. The Lumi window shows what is being captured, with live level meters per audio track — a track reading "No signal yet" after the first second is a real answer about that track, not a quiet room.

Settings holds six tabs:

- **Recording** — screen interval, audio chunk length, which sources to capture, and the speech locale. Changing one restarts capture on the new settings.
- **Storage** — where the data lives and how much of it there is.
- **Permissions** — the four grants Lumi needs, and buttons that request them.
- **MCP** — connect an AI agent. See below.
- **Danger** — retention and compression, and the irreversible delete-everything.
- **About** — version and links.

Capture continues with every window closed; that is the point of a menu-bar app. ⌘Q asks the recorder to stop gracefully first, so in-flight media finishes being indexed rather than being lost.

The default data directory is `~/Library/Application Support/Lumi`:

```text
Lumi/
├── lumi.db
├── record.json
├── record.log
├── screenshots/
└── audio/
```

The Storage tab can move it. `record.json` and `record.log` are the recorder's state file and log.

Earlier versions also wrote a `config.json` here to hold provider settings for a removed feature. Lumi no longer reads it, and upgrading leaves it in place. If yours holds an API key, delete the file and rotate the key.

## Connect an AI agent

This is how you ask Lumi what happened. Lumi serves its index to any MCP-capable agent — Claude Desktop, Claude Code, Cursor, Codex — and the agent brings its own model. Lumi does no inference.

Open **Settings → MCP** and press **Set up**. It writes the Lumi entry into every MCP client installed on this Mac, and running it twice changes nothing. A client that already has a *different* Lumi entry is never overwritten silently: it shows as a conflict, displays the entry that is in the way, and offers **Replace** for that one client. Clients Lumi does not know about get a **Copy client config** button with the exact snippet to paste, in whichever format that client reads.

You never run the server yourself. The agent starts it as a child process when it needs it and shuts it down afterwards, so there is no daemon to keep alive, no port, and no terminal to leave open. Lumi registers it with an absolute path and an explicit data directory, always — agents start MCP servers with a bare environment, so nothing from a shell profile would reach it.

- **Claude Code** is configured at user scope through the `claude` CLI, which is the only supported way to modify `~/.claude.json` — that file is live application state, not just settings.
- **Claude Desktop** has no CLI, so its `claude_desktop_config.json` is edited in place. Every other key is preserved and a `.lumi-backup` copy is written first, though top-level keys come back in alphabetical order. **Quit Claude Desktop before setting up and reopen it afterwards** — it only reads the config at launch, and quitting also avoids racing its own writes. The MCP tab says when this is needed.
- **Codex CLI** is configured through the `codex` CLI in both directions, which is the only supported writer for `~/.codex/config.toml` and preserves the comments and top-level keys a hand-rolled TOML round-trip would drop. Note that `codex` itself rewrites the whole `mcp_servers` table when it adds an entry, so other servers there may come back reformatted — their values are unchanged, and everything outside that table is untouched.
- Clients that are not installed are skipped rather than reported as a failure.

Because the agent keeps that server process for the whole session, upgrading Lumi mid-session would otherwise leave it serving the old build — the replaced file on disk does not affect a process already running. So the server watches its own binary and, once the session is briefly idle, replaces itself in place with the new one. The agent's connection is preserved across the swap and it never sees an interruption. Until that happens, and if the index turns out to have been written by a newer Lumi than the server is running, every tool result says so in its `notice` rather than quietly returning results from the older build.

Four tools are exposed:

| Tool | Parameters | Returns |
|---|---|---|
| `search_events` | `query`, `kind` (`screen`/`audio`), `app`, `window`, `since`, `until`, `limit`, `match` (`all`/`any`), `require_text`, `max_text_chars` — all optional | matching events, ranked by relevance when `query` is set and newest first otherwise, with text capped at 600 characters by default and 20 events per page (500 maximum) |
| `get_event` | `id` | one event with its full untruncated text and processor metadata |
| `list_apps` | `app`, `since`, `until`, `limit` — all optional | the applications captured, most active first, or the window titles for one application |
| `get_transcript` | `since`, `until`, `origin`, `min_confidence`, `max_turns`, `max_text_chars` — all optional | captured audio as one ordered conversation, each turn labelled by origin with the machine's own speech deduplicated, 100 turns by default (1000 maximum) |

`since` and `until` take an RFC3339 timestamp or a duration such as `2h`. When `search_events` truncates an event's text it says so and reports the true length, so an agent can fetch the rest with `get_event`.

`get_transcript` answers the question `search_events` cannot: what was actually said, in order. Each turn is labelled `internal` (sound this machine produced — the far side of a call, a video, music, a notification, and not necessarily a person), `external` (sound the microphone picked up from the room), or `unknown` (machine audio played but produced no transcript). Because the machine's audio also bleeds into the microphone, its words appear once here rather than twice. Every turn carries a `confidence` and an `order_confidence` — `exact` when position was measured, `sequence` when order is reliable but absolute times are not, `approximate` when position was inferred — and a range holding more audio than one call returns says so and names where to resume.

Audio is captured from system output and from the microphone, and the two are stored as separate rows sharing one timestamp. `search_events` never merges them. It does return only what matched, though — every filter applies per row, so a query hitting one track's transcript, `require_text`, or a limit falling between the two gives you one row of a pair, and that is not evidence the chunk had one track. Nor does a pair necessarily hold one sound: the microphone re-records your speakers, but it also records the room. `get_transcript` is what answers both, reading each chunk whole and deciding per segment against word timings and an energy envelope.

Results also come back with a `notice` when the outcome would otherwise be ambiguous: whether an empty page means the index itself is empty (the notice names the database file) or that the filters simply matched nothing, and whether a full page was capped and more results exist.

**No screenshot or audio ever leaves your machine through this interface.** The tools return text and metadata only. `media_path` is a local path you can open yourself; no tool reads those bytes.

## Retention

Captured media stays on disk until it is pruned — nothing is scheduled automatically. **Settings → Danger** previews and applies an age policy. The age and size policies, the irreversible wipe, and how dry-run accounting works: [docs/retention.md](docs/retention.md).

## Compression

Compression re-encodes media already on disk into smaller files without deleting any event — screenshots to HEIC, audio to lossless FLAC, then a database rebuild. Roughly 3x on a real index, run from **Settings → Danger**. The screenshot pass is a second lossy generation and is the one decision worth reading before you run it: [docs/compress.md](docs/compress.md).

## Architecture

How capture, processing, storage, and query fit together, what each package owns, and how frame deduplication and permissions work: [docs/architecture.md](docs/architecture.md).

## Development

Lumi is a Swift menu-bar app wrapped around a Go binary that does all the work. The app supervises that binary as a child process; the binary is embedded in the bundle at `Contents/MacOS/lumi` and is not installed separately.

```sh
task build   # compiles the Swift SpeechAnalyzer bridge, then go build
task test    # full suite (raw go build/go test will not link without the Swift archive)
task check   # fmt → vet → test; the verification command
```

```sh
task app          # build build/Lumi.app
task app:install  # install ~/Applications/Lumi.app
task app:run      # install and launch it
```

`./scripts/restart-lumi-app.sh` is the app development loop: quit, rebuild, reset TCC, relaunch. A bundle built locally is ad-hoc signed, so every rebuild changes its TCC identity and the four permissions have to be granted again — batch UI changes into single builds. The released cask carries one stable certificate, so its grants survive an upgrade.

The binary is also how the pipeline is driven directly while working on it:

```sh
./lumi permissions --request   # or: task permissions
./lumi doctor                  # platform, permissions, speech assets, data directory
./lumi record start --foreground --no-audio --duration 10s   # bounded smoke test
./lumi search "quarterly roadmap"
./lumi search "launch budget" --type audio --since 8h
./lumi transcript --since 2h
./lumi transcript backfill --since 7d
```

`search` returns audio as 30-second windows of one track, which reads poorly as conversation. `transcript` reads the same audio as an ordered conversation instead, labelling every turn by where the sound came from and showing the machine's words once rather than twice. A leading `~` marks a turn whose position was inferred; a trailing score marks uncertain attribution. Turns are derived from the two transcripts, so `transcript backfill` is what fills them in for audio captured before that existed — the default pass works from the index alone, while `--retranscribe` re-runs recognition to recover word timings and refuses to run beside a live recorder.

Two more smoke tests, both permission-gated:

```sh
task test:native   # bounded native framework test
task mcp           # hand-fed MCP JSON-RPC handshake
```

Lumi is licensed under the MIT License.
