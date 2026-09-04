# Architecture

```text
ScreenCaptureKit displays ─→ Vision OCR (full screen) ─┐
                         └─→ Accessibility (attribution) ├─→ events + FTS5 ─→ search / mcp
ScreenCaptureKit system + microphone ─→ WAV ─→ SpeechAnalyzer (in-process) ─┘

(the compress pass later re-encodes the stored JPEGs as HEIC and the WAVs as lossless FLAC in place.)
```

- `internal/macosnative`: cgo bridge to ScreenCaptureKit, Accessibility, Vision, and permission APIs
- `internal/capture`: testable capture orchestration, perceptual deduplication, and transcription
- `internal/store`: versioned SQLite migrations, FTS5 triggers, inserts, and filtered search
- `internal/retention`: age-, size-, and wipe-based event/media pruning
- `internal/compress`: re-encoding indexed media in place (HEIC, FLAC) and reclaiming database free pages
- `internal/mcp`: the read-only MCP tool surface served over stdio
- `internal/mcpsetup`: registering `lumi mcp` with installed MCP clients
- `internal/selfexec`: replacing the running process when its binary is upgraded
- `internal/config`: data-directory path resolution
- `internal/cli`: Cobra commands and lifecycle

Every connected display is captured by default; `record start --displays` narrows that to chosen CoreGraphics display IDs, which `lumi displays` lists and Lumi.app presents as a picker with a live preview of each screen. The selection is re-applied against the displays ScreenCaptureKit offers on every tick, so plugging a monitor in needs no restart, and a selection naming nothing connected records every display rather than none — reported in the log and in the app rather than left silent. Frames use a hash fast path plus a sampled color-histogram comparison with independent state per display; recent user input makes the threshold more sensitive. Two safety intervals keep capture from going silent: a frame whose bytes changed but scored as a near-duplicate — a video, an advancing slide — is retained at least every ten seconds, while a byte-identical frame is retained every five minutes, so a frozen screen leaves a bounded presence marker instead of re-indexing the same JPEG. Full-display Vision OCR is the primary screen-text source, so the index reflects the whole screen rather than just the focused window; the Accessibility snapshot supplies focused-app attribution and its window text is kept in event metadata when substantive. If Accessibility, Vision, comparison, or transcription fails after media was captured, Lumi preserves and indexes the event with processor diagnostics instead of silently losing the original data.

The app's Permissions tab invokes Apple's native Screen Recording, Accessibility, Microphone, and Speech Recognition request flows through `permissions --request`, and reports their current state with the matching System Settings location. Input Monitoring is informational and is only requested when `--input-monitoring` is explicitly passed; capture does not require an event tap.
