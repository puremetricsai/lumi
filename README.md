# Lumi

Lumi is an open-source, local-first work memory for Apple Silicon Macs. It continuously captures every display plus system and microphone audio, extracts screen text from macOS Accessibility with Apple Vision fallback, transcribes speech on-device with Apple SpeechAnalyzer, stores the media on disk, indexes the text in SQLite FTS5, and lets you search and manage it from a Go CLI. An optional `lumi ask` command sends only the retrieved text context to Cerebras for an answer.

Lumi is an early v1 implementation inspired by [screenpipe](https://github.com/mediar-ai/screenpipe). It intentionally targets a smaller surface: capture → process → store → query, with no GUI, server, plugins, provider abstraction, or non-Cerebras inference backend.

## Requirements

- Apple Silicon Mac running macOS 26 or newer (`darwin/arm64`)
- Go 1.24 or newer (to build)
- Xcode Command Line Tools and a Swift toolchain (`swiftc` compiles the SpeechAnalyzer bridge into a static archive that cgo links)
- Screen Recording, Accessibility, Microphone, and Speech Recognition permissions for your terminal or the Lumi binary
- A Cerebras API key only for `lumi ask`

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

The defaults are a two-second screen interval and 30-second audio chunks. Displays are discovered again on every interval, so connecting or disconnecting a display does not require restarting Lumi.

The default data directory is `~/Library/Application Support/Lumi`:

```text
Lumi/
├── lumi.db
├── screenshots/
└── audio/
```

Override it with `--data-dir` or `LUMI_HOME`.

## Search

```sh
./lumi search "quarterly roadmap"
./lumi search "launch budget" --type audio --since 8h
./lumi search --app "Safari" --window "Quarterly Plan" --since 24h
./lumi search --type screen --since 2026-07-18T09:00:00-07:00 --json
```

Search terms are safely combined with FTS5 `AND`. With no text argument, Lumi returns the most recent events. `--app` is an exact case-insensitive filter; `--window` is a case-insensitive substring filter. Results include timestamps and paths to the original screenshot or WAV chunk. JSON output also preserves screen-text source, display ID, audio source, and processor diagnostics.

## Ask with Cerebras

Set the single supported inference provider's key and ask a question:

```sh
export CEREBRAS_API_KEY="..."
./lumi ask "What did I work on this afternoon?" --since 8h
./lumi ask "What changed in the quarterly plan?" --app "Safari" --since 24h
```

Lumi turns the question into search terms (dropping question words, broad activity words, and time words, which `--since` already covers) and retrieves in stages: events matching every term, else events ranked by best partial match, else the most recent events. When it falls back to a recency-based stage it says so on stderr and distinguishes a broad overview from terms that matched nothing, so a recency-based answer is never mistaken for a retrieved one.

It sends only the retrieved text and metadata to Cerebras `POST /v1/chat/completions` and prints the answer. Media files are never sent by this command.

The activity context is capped so a large Accessibility tree or Vision result cannot blow the model's context window — 60000 characters by default, adjustable with `--max-context-chars`. Lumi selects records in retrieval order, renders the selected set chronologically, consolidates adjacent identical screen evidence, and labels title-only screens and untranscribed audio explicitly. Dropped events are reported inline in the context.

The default model is `gpt-oss-120b`. Override it with `--model`, or set a default for every invocation:

```sh
export LUMI_CEREBRAS_MODEL="qwen-3-32b"
```

The flag wins over the environment variable, which wins over the built-in default. `./lumi doctor` prints the model it resolves to.

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

Lumi does not schedule retention automatically. Run `prune` periodically yourself if you want a fixed policy. Database rows are deleted before their media files, so an interrupted prune can leave recoverable orphaned files rather than indexed events whose media has disappeared.

## Architecture

```text
ScreenCaptureKit displays ─→ Accessibility ─┐
                         └─→ Vision fallback ├─→ events + FTS5 ─→ search ─→ Cerebras ask
ScreenCaptureKit system + microphone ─→ WAV ─→ SpeechAnalyzer (in-process) ─┘
```

- `internal/macosnative`: cgo bridge to ScreenCaptureKit, Accessibility, Vision, and permission APIs
- `internal/capture`: testable capture orchestration, perceptual deduplication, and transcription
- `internal/store`: versioned SQLite migrations, FTS5 triggers, inserts, and filtered search
- `internal/retention`: age- and size-based event/media pruning
- `internal/cerebras`: direct Cerebras chat-completions client
- `internal/cli`: Cobra commands and lifecycle

Frames use a hash fast path plus a sampled color-histogram comparison with independent state per display; recent user input makes the threshold more sensitive, and a ten-second safety interval prevents capture from going silent. Accessibility text is used for the focused display, while Vision handles fallback and other displays. If Accessibility, Vision, comparison, or transcription fails after media was captured, Lumi preserves and indexes the event with processor diagnostics instead of silently losing the original data.

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
