# Lumi

Lumi is an open-source, local-first work memory for Apple Silicon Macs. It continuously captures every display plus system and microphone audio, extracts screen text from macOS Accessibility with Apple Vision fallback, transcribes speech locally, stores the media on disk, indexes the text in SQLite FTS5, and lets you search it from a Go CLI. An optional `lumi ask` command sends only the retrieved text context to Cerebras for an answer.

Lumi is an early v1 implementation inspired by [screenpipe](https://github.com/mediar-ai/screenpipe). It intentionally targets a smaller surface: capture → process → store → query, with no GUI, server, plugins, provider abstraction, or non-Cerebras inference backend.

## Requirements

- Apple Silicon Mac running macOS 15 or newer (`darwin/arm64`)
- Go 1.24 or newer (to build)
- Xcode Command Line Tools (the binary links native macOS frameworks through cgo)
- Screen Recording, Accessibility, and Microphone permissions for your terminal or the Lumi binary
- [whisper.cpp](https://github.com/ggml-org/whisper.cpp) (`whisper-cli`) and a local model for transcription
- A Cerebras API key only for `lumi ask`

Homebrew can install the only required command-line processor:

```sh
brew install whisper-cpp
```

Download a whisper.cpp model, then set its path:

```sh
export LUMI_WHISPER_MODEL="$PWD/models/ggml-base.en.bin"
```

ScreenCaptureKit captures system output and the default microphone directly; no loopback audio device is required. Lumi excludes its own process audio from system capture.

## Build and run

```sh
go build -o lumi ./cmd/lumi
./lumi permissions --request # or: task permissions
./lumi doctor
./lumi record
```

Recording runs until `Ctrl-C`. Useful options include:

```sh
# Screen only, every ten seconds
./lumi record --no-audio --interval 10s

# All displays plus 60-second system and microphone audio chunks
./lumi record --audio-chunk 60s

# Bounded smoke test
./lumi record --no-audio --duration 10s
```

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
./lumi search --type screen --since 2026-07-18T09:00:00-07:00 --json
```

Search terms are safely combined with FTS5 `AND`. With no text argument, Lumi returns the most recent events. Results include timestamps and paths to the original screenshot or WAV chunk.

## Ask with Cerebras

Set the single supported inference provider's key and ask a question:

```sh
export CEREBRAS_API_KEY="..."
./lumi ask "What did I work on this afternoon?" --since 8h
```

Lumi turns the question into search terms (dropping question words and time words, which `--since` already covers) and retrieves in stages: events matching every term, else events ranked by best partial match, else the most recent events. When it falls back past the first stage it says so on stderr, so a recency-based answer is never mistaken for a retrieved one.

It sends only the retrieved text and metadata to Cerebras `POST /v1/chat/completions` and prints the answer. Media files are never sent by this command.

The activity context is capped so a large Accessibility tree or Vision result cannot blow the model's context window — 60000 characters by default, adjustable with `--max-context-chars`. Dropped events are reported inline in the context.

The default model is `gpt-oss-120b`. Override it with `--model`, or set a default for every invocation:

```sh
export LUMI_CEREBRAS_MODEL="qwen-3-32b"
```

The flag wins over the environment variable, which wins over the built-in default. `./lumi doctor` prints the model it resolves to.

## Architecture

```text
ScreenCaptureKit displays ─→ Accessibility ─┐
                         └─→ Vision fallback ├─→ events + FTS5 ─→ search ─→ Cerebras ask
ScreenCaptureKit system + microphone ─→ WAV ─→ whisper.cpp ─┘
```

- `internal/macosnative`: cgo bridge to ScreenCaptureKit, Accessibility, Vision, and permission APIs
- `internal/capture`: testable capture orchestration, perceptual deduplication, and transcription
- `internal/store`: SQLite schema, FTS5 triggers, inserts, and filtered search
- `internal/cerebras`: direct Cerebras chat-completions client
- `internal/cli`: Cobra commands and lifecycle

Displays are enumerated on every capture so hotplug changes are picked up without restarting. Frames use a hash fast path plus a downsampled color-histogram comparison; recent user input makes the threshold more sensitive, and a ten-second safety interval prevents capture from going silent. If Accessibility or Vision extraction or transcription fails after media was captured, Lumi preserves and indexes the event with processor diagnostics instead of silently losing the original data.

`lumi permissions --request` invokes Apple's native Screen Recording, Accessibility, and Microphone request flows. `lumi doctor` reports their current state with the matching System Settings location. Input Monitoring is informational and is only requested when `--input-monitoring` is explicitly passed; capture does not require an event tap.

## Development

```sh
go test ./...
go vet ./...
```

After granting permissions, run the bounded native framework smoke test with:

```sh
task test:native
```

Lumi is licensed under the MIT License.
