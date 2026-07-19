# Lumi vs. screenpipe: architectural differences

**Date:** 2026-07-19
**Lumi commit at time of writing:** `e688ad9`
**screenpipe reference:** [mediar-ai/screenpipe](https://github.com/mediar-ai/screenpipe) (`screenpipe/screenpipe` redirects there)

Findings from a side-by-side review of Lumi's Go implementation against screenpipe's Rust workspace. Scoped to *architectural* differences — the GUI gap is assumed and not discussed.

---

## Licensing and project status — read this first

Two facts that change how this comparison should be used:

1. **screenpipe is no longer open source.** As of 2026-06-10 it is *source-available* under the "Screenpipe Commercial License": free for personal/non-commercial use, paid for commercial use ($25/mo standard, $50/seat pro, $150/seat enterprise). Company is Mediar, Inc. (YC S26). Treat its source as a design reference, not as something to copy code from.
2. **The plugin system was rewritten.** Most public writing about screenpipe describes the old Next.js/Bun "pipes" with `pipe.json` manifests and a pipe store. That system is gone. A pipe is now a single `pipe.md` file (YAML frontmatter + prompt body) executed by an external agent process.

---

## Summary table

| Dimension | Lumi | screenpipe |
|---|---|---|
| Media storage | One JPEG per frame | HEVC/libx265 mp4 chunks, fragmented MP4 |
| Primary text source | Screenshot → Tesseract OCR | macOS Accessibility tree; OCR as fallback |
| OCR engine (macOS) | Tesseract | Apple Vision |
| Frame dedup | SHA-256 of JPEG bytes, single-slot | Downscaled histogram compare, adaptive threshold |
| Capture rate | 5s interval (configurable) | 1.0 fps default, 0.5 on macOS; adaptive mode |
| Monitors | One, via `--display` | All monitors concurrently, hotplug-aware |
| Audio input | One device via ffmpeg | Input + system/output audio via ScreenCaptureKit |
| Audio processing | whisper-cli on every 30s chunk | Silero VAD → Pyannote diarization → GPU whisper |
| Retention | None — unbounded growth | 10 GB cache cap, 7-day frame retention, hourly sweep |
| Schema evolution | Idempotent `CREATE IF NOT EXISTS`, no version | `sqlx::migrate!`, versioned, checksum recovery |
| Query filters | kind, time range | app, window, browser_url, speaker, length, focus, + time |
| Interface | CLI only | CLI + Axum HTTP API, SSE, WebSocket, MCP, JS SDK |
| Process model | Foreground process | Tauri app + sidecar server, PID watchdog, recovery loops |
| Text retrieval | FTS5 / BM25 | FTS5 / BM25 (vector search is vestigial — see below) |

---

## 1. Storage format — the largest single difference

Lumi writes one JPEG per captured frame (`internal/capture/recorder.go:99-158`). At the default 5s interval that is roughly **17,000 files per day**, forever.

screenpipe pipes raw frames into ffmpeg and encodes **HEVC (libx265) mp4 chunks**. CRF and preset derive from a quality setting; **B-frames are disabled** so every frame seeks independently. The container uses fragmented MP4 (`frag_keyframe+empty_moov+default_base_moof`) so frames can be extracted from a recording that is still being written. The `frames` table stores `video_chunk_id` + `offset_index` rather than a file path; single-frame display goes through `ffmpeg -ss <offset> -vframes 1` → mjpeg → base64, behind a `FrameImageCache`.

This is an order-of-magnitude disk difference and the reason screenpipe can quote ~15–30 GB/month while retaining video at all.

## 2. Text extraction: accessibility tree vs. OCR

Lumi's only text source is Tesseract over a screenshot.

screenpipe's README now describes capture as **"accessibility tree extraction with OCR fallback"** — the AX path is primary. `screenpipe-accessibility` uses macOS AX APIs directly (`cidre::ax`, `cidre::cg::event::access`) to capture clicks, aggregated text input, keystrokes, app switches, window focus changes, clipboard operations and content, and focused-element context (role/name/value; text fields read via `ax::attr::value()`). This lands in a `ui_events` table with its own FTS index, plus an `accessibility` table for AX-derived screen content.

Where screenpipe *does* OCR, it uses the platform native engine — **Apple Vision** on macOS, Windows.Media.Ocr on Windows, Tesseract only as the Linux/cross-platform fallback.

This is the difference that most changes what the tool can *know*. AX text is cleaner than OCR, cheaper to produce, and captures interaction events that no screenshot contains.

## 3. Dedup quality

Lumi hashes the full JPEG bytes with SHA-256 and compares against the immediately previous frame only (`recorder.go:127-134`). This catches **pixel-identical frames and nothing else** — a blinking cursor, a clock tick, or antialiasing jitter defeats it entirely, so near-duplicates are OCR'd and indexed indefinitely.

screenpipe's `FrameComparer`: image-hash early exit → downscale ~4x to 640x360 → **histogram comparison** (they explicitly dropped SSIM for speed) → adaptive threshold driven by AX input activity when `adaptive-fps` is on → a safety valve capping skips at ~10s so the pipeline never goes silent.

## 4. Audio depth

Lumi records one AVFoundation input device and runs whisper-cli on each 30s chunk.

screenpipe adds, in order of impact:
- **System/output audio** on macOS via ScreenCaptureKit (falls back to the default output device via cpal) — this is what makes meeting capture actually work.
- **VAD** (Silero v5/v6 ONNX default, WebRTC alternative) behind a `VadEngine` trait, so silence never reaches whisper.
- **Speaker diarization** via Pyannote ONNX (segmentation + embedding models), producing a `speaker_id` on transcription rows.
- **GPU whisper** — Metal on macOS, Vulkan on Windows, via `whisper-rs`.
- **Smart mode**: an `IdleDetector` defers transcription above ~70% CPU so video calls aren't degraded — but audio is still written to disk in realtime, so nothing is lost.

## 5. Retention

Lumi has **zero `DELETE` statements**. The only file ever removed is a duplicate screenshot and a whisper temp transcript. Screenshots, WAVs, and rows accumulate without bound.

screenpipe runs an hourly `run_cache_manager` against a `FrameDiskCache` with `max_cache_size_gb` (default 10) and `frame_retention_days` (default 7), pruning by both age cutoff and total size.

## 6. Schema evolution

Lumi applies one idempotent `CREATE ... IF NOT EXISTS` blob on every `Open` (`internal/store/store.go:73-112`). There is no `schema_version`, so any future schema change has no upgrade path from databases already in the field.

screenpipe uses `sqlx::migrate!` against versioned migration files, with explicit handling for checksum mismatches (update stored checksum, retry) and foreign keys disabled during migration.

## 7. Query surface

Lumi's `SearchOptions` (`internal/store/store.go:35-41`) supports **kind and time range only**. `app` and `window` are captured and indexed into FTS but are unreachable as filters — they can only be hit accidentally, as unweighted free-text tokens.

screenpipe's `GET /search` filters on `content_type`, `app_name`, `window_name`, `browser_url`, `min_length`/`max_length`, `speaker_ids`, `speaker_name`, `focused`, plus time range, with `offset` pagination and a Moka LRU cache keyed on the full parameter hash. A second `GET /search/keyword` endpoint adds fuzzy matching, explicit ordering (`timestamp_asc|timestamp_desc|relevance`), and result grouping. `browser_url` is captured atomically with the screenshot specifically to avoid timing mismatch.

## 8. Process model and operations

Lumi's `record` is a foreground process the user babysits. No launchd plist, no PID file, no health check, no `start/stop/status`.

screenpipe runs two processes: the Tauri app spawns `screenpipe-server` as a sidecar in a dedicated thread with its own Tokio runtime (to avoid CPU contention with the UI). The server watches its parent PID and self-terminates if the app dies. Capture tasks sit in recovery loops that restart after a 5s sleep. `/health` detects stalls by querying the most recent frame and audio timestamps; the tray polls it every 30s.

macOS permissions are handled explicitly: preflight checks for screen recording, microphone, accessibility, and input monitoring; a background monitor polls every 10s and emits `permission-lost` after two consecutive failures; `reset_and_request_permission` shells out to `tccutil`.

## 9. Interfaces beyond the CLI

Lumi is a CLI and nothing else — no server, socket, or streaming.

screenpipe exposes an Axum server on `localhost:3030` with `/search`, `/frames/:id`, `/tags/*`, `/raw_sql`, `/ui-events`, SSE frame streaming at `/stream/frames`, and a health WebSocket. On top of that: `@screenpipe/js` and `@screenpipe/browser` SDKs, and an MCP server (`npx -y screenpipe-mcp`) exposing `search-content`, `search-ui-events`, `get-ui-event-stats`, and `export-video`.

---

## What is *not* a gap

**Semantic / vector search.** `sqlite_vec` is imported in `screenpipe-db` but there is no text-embedding table and no vector query path. The only real embeddings are Pyannote *audio* embeddings for speaker identity. screenpipe's advertised "semantic search using embeddings" is, in the code, an LLM writing FTS5 keyword queries. Lumi's FTS5/BM25-only retrieval matches screenpipe's actual behavior — building an embedding pipeline would be leapfrogging, not catching up.

**Provider abstraction.** screenpipe supports OpenAI, Anthropic, Ollama, custom OpenAI-compatible endpoints, and its own gateway, with everything routed through an external agent process (`@mariozechner/pi-coding-agent`) over JSON-RPC. Lumi's single-backend Cerebras decision is a deliberate scope choice, not an oversight.

---

## Resource figures (sources disagree)

Worth recording because the numbers are quoted inconsistently across screenpipe's own docs: README says 5–10% CPU, 0.5–3 GB RAM, ~20 GB storage/month; internal docs say 10% CPU and ~15 GB/month; another says ~5–10 GB/month; DeepWiki's storage page says ~30 GB/month at 1 fps. Treat storage as **roughly 5–30 GB/month** depending on fps and quality settings.

---

## Related

Prioritized remediation is tracked in `docs/superpowers/plans/2026-07-19-tier1-retention-filters-migrations.md`, which covers the three cheapest high-impact items: versioned migrations (§6), app/window search filters (§7), and retention (§5).
