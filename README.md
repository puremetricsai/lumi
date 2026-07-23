# Lumi

Lumi is an open-source, local-first work memory for Apple Silicon Macs. It continuously captures every display plus system and microphone audio, extracts screen text with full-display Apple Vision OCR (using macOS Accessibility for focused-app attribution), transcribes speech on-device with Apple SpeechAnalyzer, stores the media on disk, indexes the text in SQLite FTS5, and lets you search and manage it from a Go CLI. An optional `lumi ask` command sends only the retrieved text context to an inference backend — hosted Cerebras or a local `llama-server` — for an answer.

Lumi deliberately targets a small surface: capture → process → store → query, with no GUI, server, or plugins.

## Requirements

- Apple Silicon Mac running macOS 26 or newer (`darwin/arm64`)
- Go 1.24 or newer (to build)
- Xcode Command Line Tools and a Swift toolchain (`swiftc` compiles the SpeechAnalyzer bridge into a static archive that cgo links)
- Screen Recording, Accessibility, Microphone, and Speech Recognition permissions for your terminal or the Lumi binary
- For `lumi ask` only: a Cerebras API key, or `llama-server` on `PATH` (`brew install llama.cpp`)

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
├── config.json
├── screenshots/
└── audio/
```

Override it with `--data-dir` or `LUMI_HOME`. The background recorder also writes its state file and log into the root, as does a Lumi-launched `llama-server`.

## Search

```sh
./lumi search "quarterly roadmap"
./lumi search "launch budget" --type audio --since 8h
./lumi search --app "Safari" --window "Quarterly Plan" --since 24h
./lumi search --type screen --since 2026-07-18T09:00:00-07:00 --json
```

Search terms are safely combined with FTS5 `AND`. With no text argument, Lumi returns the most recent events. `--app` is an exact case-insensitive filter; `--window` is a case-insensitive substring filter. Results include timestamps and paths to the original screenshot or WAV chunk. JSON output also preserves screen-text source, display ID, audio source, and processor diagnostics.

## Configure

`lumi ask` needs an inference backend. Configuration is persisted to `config.json` in the data directory (mode `0600`); there is no environment-variable fallback.

```sh
./lumi configure          # interactive: provider first, then that provider's fields
./lumi configure --show   # print the current config (Cerebras key masked)
```

Hosted Cerebras (the default provider):

```sh
./lumi configure --provider cerebras --api-key "..." --model gpt-oss-120b
```

Local llama.cpp, which requires `llama-server` on `PATH` (`brew install llama.cpp`; `configure` refuses to select the provider until it is installed):

```sh
./lumi configure --provider llama.cpp --llama-model ~/models/qwen3-8b-q4.gguf
./lumi configure --provider llama.cpp --llama-model unsloth/Qwen3-8B-GGUF
```

`--llama-model` takes a GGUF file path or a HuggingFace repo id. `--llama-base-url` overrides where `llama-server` listens (default `http://127.0.0.1:8080`).

`./lumi doctor` reports the active provider, whether the key or model is set, and — for llama.cpp — whether the server is reachable.

## Ask

```sh
./lumi ask "What did I work on this afternoon?"
./lumi ask "What was on screen around 9:15 pm?"
./lumi ask "What changed in the quarterly plan?" --app "Safari" --since 24h
```

Lumi derives the time window from the question itself: expressions like "around 9:15 pm", "yesterday", "this morning", "last 2 hours", and "last night" set the window in your local timezone, and the words are stripped so they never leak into the search terms. Directional connectors change the shape — "after 10:15 pm" opens forward to the end of that day, "before 9 am" opens backward from that day's start, while "around"/"at" keep a centered ±15-minute band. The interpreted window is printed to stderr. An explicit `--since` skips derivation entirely.

Lumi then turns the question into search terms (dropping question words, broad activity words, recording-modality words, and time words) and retrieves in stages: events matching every term, else events ranked by best partial match, else the most recent events. When it falls back to a recency-based stage it says so on stderr and distinguishes a broad overview from terms that matched nothing, so a recency-based answer is never mistaken for a retrieved one.

It sends only the retrieved text and metadata to the configured backend's OpenAI-compatible `POST /v1/chat/completions` and prints the answer. Media files are never sent by this command.

The activity context is capped so a large Vision or Accessibility result cannot blow the model's context window — 60000 characters by default, adjustable with `--max-context-chars`. Lumi selects records in retrieval order, renders the selected set chronologically, consolidates adjacent identical screen evidence, and labels title-only screens and untranscribed audio explicitly. Dropped events are reported inline in the context. `--limit` caps how many records are considered (default 50).

`--model` overrides the configured model for a single invocation; otherwise the resolved value comes from `config.json`, falling back to `gpt-oss-120b` for Cerebras.

### Local llama-server lifecycle

When the llama.cpp provider is active, `ask` checks `/health` and launches `llama-server` itself if it isn't already running, then leaves it running so the model stays warm. The process is detached, logs to `llama-server.log` in the data directory, and records its pid in `llama-server.pid`.

```sh
./lumi llama status   # reachability and the pid Lumi launched
./lumi llama stop     # terminate the Lumi-launched server
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
                         └─→ Accessibility (attribution) ├─→ events + FTS5 ─→ search ─→ ask
ScreenCaptureKit system + microphone ─→ WAV ─→ SpeechAnalyzer (in-process) ─┘
```

- `internal/macosnative`: cgo bridge to ScreenCaptureKit, Accessibility, Vision, and permission APIs
- `internal/capture`: testable capture orchestration, perceptual deduplication, and transcription
- `internal/store`: versioned SQLite migrations, FTS5 triggers, inserts, and filtered search
- `internal/retention`: age-, size-, and wipe-based event/media pruning
- `internal/llm`: shared OpenAI-compatible chat-completions client and system prompt
- `internal/cerebras`, `internal/llamacpp`: thin provider wrappers over that client, plus `llama-server` lifecycle
- `internal/config`: data-directory paths and the persisted `config.json`
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
