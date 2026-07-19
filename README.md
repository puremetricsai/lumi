# Lumi

Lumi is an open-source, local-first work memory for Apple Silicon Macs. It continuously captures screen and audio activity, extracts text with local OCR and speech-to-text, stores the media on disk, indexes the text in SQLite FTS5, and lets you search it from a Go CLI. An optional `lumi ask` command sends only the retrieved text context to Cerebras for an answer.

Lumi is an early v1 implementation inspired by [screenpipe](https://github.com/mediar-ai/screenpipe). It intentionally targets a smaller surface: capture → process → store → query, with no GUI, server, plugins, provider abstraction, or non-Cerebras inference backend.

## Requirements

- Apple Silicon Mac (`darwin/arm64`)
- Go 1.24 or newer (to build)
- Screen Recording, Accessibility, and Microphone permissions for your terminal or the Lumi binary
- [Tesseract](https://tesseract-ocr.github.io/) for local OCR
- [FFmpeg](https://ffmpeg.org/) for AVFoundation audio capture
- [whisper.cpp](https://github.com/ggml-org/whisper.cpp) (`whisper-cli`) and a local model for transcription
- A Cerebras API key only for `lumi ask`

Homebrew can install the command-line processors:

```sh
brew install tesseract ffmpeg whisper-cpp
```

Download a whisper.cpp model, then set its path:

```sh
export LUMI_WHISPER_MODEL="$PWD/models/ggml-base.en.bin"
```

For microphone capture, the default AVFoundation audio device is index `0`. To record system output, select an installed loopback/aggregate AVFoundation device (for example, one configured with BlackHole) with `--audio-device`. List devices with:

```sh
ffmpeg -f avfoundation -list_devices true -i ""
```

## Build and run

```sh
go build -o lumi ./cmd/lumi
./lumi doctor
./lumi record
```

Recording runs until `Ctrl-C`. Useful options include:

```sh
# Screen/OCR only, every ten seconds
./lumi record --no-audio --interval 10s

# Screen plus 60-second audio chunks from AVFoundation audio device 2
./lumi record --audio-device 2 --audio-chunk 60s

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

Lumi retrieves matching FTS records (or recent records if the exact terms do not match), sends their text and metadata to Cerebras `POST /v1/chat/completions`, and prints the answer. The default model is `gpt-oss-120b`; change it with `--model` when your Cerebras account uses another Cerebras endpoint model. Media files are never sent by this command.

## Architecture

```text
macOS screencapture ─→ JPEG ─→ Tesseract ─┐
                                           ├─→ events + FTS5 ─→ search ─→ Cerebras ask
FFmpeg AVFoundation ─→ WAV ─→ whisper.cpp ─┘
```

- `internal/capture`: concrete macOS capture and local processing pipeline
- `internal/store`: SQLite schema, FTS5 triggers, inserts, and filtered search
- `internal/cerebras`: direct Cerebras chat-completions client
- `internal/cli`: Cobra commands and lifecycle

Screen frames with identical bytes are discarded. If OCR or transcription fails after a media file was captured, Lumi preserves and indexes the event with the processor error in `metadata_json`, so the original data is not silently lost.

## Development

```sh
go test ./...
go vet ./...
```

Lumi is licensed under the MIT License.
