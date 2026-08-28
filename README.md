<p align="center">
  <img src="assets/img/lumi-logo.png" alt="Lumi" width="160" height="160">
</p>

<h1 align="center">Lumi</h1>

<p align="center">
  <b>A local-first AI memory of everything you do on your Mac.</b><br>
  It sees your screen and hears your meetings, then turns all of it into a searchable record of your
  work — so you never have to take notes again. Connect Claude or any other AI agent and ask what happened.
</p>

<p align="center">
  <a href="https://github.com/puremetricsai/lumi/releases"><img src="https://img.shields.io/github/v/release/puremetricsai/lumi?color=111111&label=release" alt="Latest release"></a>
  <img src="https://img.shields.io/badge/macOS-26%2B%20%C2%B7%20Apple%20Silicon-111111" alt="macOS 26 or newer, Apple Silicon">
  <img src="https://img.shields.io/badge/data-100%25%20on--device-111111" alt="All data stays on device">
  <a href="LICENSE"><img src="https://img.shields.io/github/license/puremetricsai/lumi?color=111111" alt="MIT licensed"></a>
</p>

<p align="center">
  <img src="assets/img/lumi-toolbar.png" alt="Lumi toolbar: recording state, screen, microphone, and system audio" width="370">
</p>

## Why Lumi

You already forgot most of last week. The decision made on a call, the error message you skimmed, the
name of the tool somebody shared — it happened on your screen, and now it is gone.

Tools that fix this usually want your day on their servers, behind a subscription, feeding a model you
do not control.

Lumi keeps the whole loop on your Mac. Capture, OCR, transcription, and the index all run locally with
Apple's own frameworks — nothing is uploaded, there is no account, and it is free. Lumi performs no
inference at all; it hands your own searchable history to whichever AI agent you already trust.

## Quickstart

**1. Install.**

```sh
curl -fsSL https://raw.githubusercontent.com/puremetricsai/lumi/main/install.sh | sh
```

**2. Grant permissions.** Open Lumi, then **Settings → Permissions**. Press the buttons for Screen
Recording, Accessibility, Microphone, and Speech Recognition.

**3. Record, then connect an agent.** Start recording from the menu bar item, and press **Set up** in
**Settings → MCP** to register Lumi with every MCP client on this Mac.

Now ask your agent about your own day.

## Ask it things

Once Lumi is connected, these are plain questions to your agent — it reads your index through Lumi's
four read-only tools and answers from what actually happened:

- *"What did I work on yesterday? Break it down by app and tell me where the time went."*
- *"Summarize the call I was on this morning and pull out anything I committed to."*
- *"A few days ago I hit an error about a failed migration. Find it and show me the exact message."*
- *"I read a pricing page last week and cannot remember the vendor. What was it?"*
- *"Write my weekly update from what I actually did, not from what I remember doing."*
- *"Which apps ate my afternoon, and what was I doing in them?"*
- *"Somebody said a book title on a call last month. What was it?"*
- *"Replay the last hour of that meeting as a transcript — I stepped away."*

Nothing leaves your Mac to answer these except the question you type into your own agent. The tools
return text and metadata only — never a screenshot or a recording.

## Requirements

- Apple Silicon Mac running macOS 26 or newer (`darwin/arm64`)
- Screen Recording, Accessibility, Microphone, and Speech Recognition permissions, granted to Lumi

To build from source (not needed to install Lumi, which ships as a prebuilt app):

- Go 1.25 or newer
- Xcode Command Line Tools and a Swift toolchain (`swiftc` compiles the SpeechAnalyzer bridge into a static archive that cgo links)

Transcription runs entirely on-device through Apple SpeechAnalyzer — no external processor or model file to install. The recognition assets for your locale (default `en-US`) download automatically on first use; the locale is a setting in the Recording tab.

ScreenCaptureKit captures system output and the default microphone directly; no loopback audio device is required. Lumi excludes its own process audio from system capture.

## Installation details

The command above downloads the latest release and puts `/Applications/Lumi.app` in place. Re-run the same command to upgrade — the script quits a running Lumi first so an in-flight recording shuts down cleanly. Nothing else is installed: no command on your `PATH` and no daemon.

macOS grants capture permissions to Lumi itself, so the prompts come from the app and nothing else needs approving.

The script downloads a prebuilt `darwin/arm64` bundle from the [latest release](https://github.com/puremetricsai/lumi/releases), so no Go or Swift toolchain is involved. Uninstall by dragging `Lumi.app` to the Trash; that leaves `~/Library/Application Support/Lumi` — your database and captured media — untouched.

Lumi tells you when a new release exists and can install it for you. Once a day it asks
github.com for the newest release tag; the request carries nothing but itself — no account, no
machine identifier, no usage. When there is an update, the menu bar offers it, and taking it runs the
same `install.sh` above: one install channel, not two. Lumi stops recording, installs, and reopens.

That daily check is the only time Lumi contacts the network on its own, and **Settings → About →
Check for updates automatically** turns it off. With it off Lumi never reaches the network unless you
ask it to — **Check Now** in the same tab still sends the one request, on the spot. Re-running the
install command still upgrades you either way, and the [releases
page](https://github.com/puremetricsai/lumi/releases) still lists what changed.

### The app is not notarized yet

The released app is signed with a real Apple Developer ID certificate, but it is not notarized. That is invisible if you install with the command above: `curl` does not mark the download with `com.apple.quarantine`, so Gatekeeper never blocks the first launch.

Download the ZIP in a browser instead and it does get quarantined — Gatekeeper then refuses the first launch with "Apple could not verify Lumi is free of malware". Allow it once from **System Settings → Privacy & Security → Open Anyway**.

Use the install command and none of that applies. All of it goes away with notarization.

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

Lumi is a Swift menu-bar app wrapped around a Go binary that does all the work. Building it, the `task` targets, the app rebuild loop, and driving the pipeline from the binary directly: [docs/development.md](docs/development.md).

Lumi is licensed under the MIT License.
