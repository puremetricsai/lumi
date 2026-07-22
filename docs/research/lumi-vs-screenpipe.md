# Lumi vs. screenpipe: architectural differences

**Updated:** 2026-07-22

**Lumi commit reviewed:** `8074e6a`

**Lumi changes reviewed:** `e688ad9..8074e6a`

**screenpipe reference:** [mediar-ai/screenpipe](https://github.com/mediar-ai/screenpipe) (`screenpipe/screenpipe` redirects there)

Findings from a side-by-side review of Lumi's current Go implementation against screenpipe's Rust workspace. This update incorporates Lumi's versioned migrations, filtered search, retention command, and native macOS capture subsystem. It remains scoped to *architectural* differences; the GUI gap is assumed and not discussed.

---

## Licensing and project status — read this first

Two facts that change how this comparison should be used:

1. **screenpipe is no longer open source.** As of 2026-06-10 it is *source-available* under the "Screenpipe Commercial License": free for personal/non-commercial use, paid for commercial use ($25/mo standard, $50/seat pro, $150/seat enterprise). Company is Mediar, Inc. (YC S26). Treat its source as a design reference, not as something to copy code from.
2. **The plugin system was rewritten.** Most public writing about screenpipe describes the old Next.js/Bun "pipes" with `pipe.json` manifests and a pipe store. That system is gone. A pipe is now a single `pipe.md` file (YAML frontmatter + prompt body) executed by an external agent process.

---

## Summary table

| Dimension | Lumi | screenpipe |
|---|---|---|
| Media storage | One JPEG per retained frame; separate WAV chunks | HEVC/libx265 fragmented MP4 chunks plus audio chunks |
| Primary text source | Focused-window macOS Accessibility tree; Apple Vision fallback | macOS Accessibility tree; OCR fallback |
| OCR engine (macOS) | Apple Vision | Apple Vision |
| Frame dedup | Per-display SHA-256 fast path + sampled RGB histogram; input-aware threshold and 10s maximum silence | Image-hash fast path + downscaled histogram; adaptive threshold and maximum skip interval |
| Capture rate | Every 2s by default, configurable | 1.0 fps default, 0.5 on macOS; adaptive mode |
| Monitors | All displays concurrently, re-enumerated for hotplug | All monitors concurrently, hotplug-aware |
| Audio input | Simultaneous system/output + default microphone via ScreenCaptureKit | Input + system/output audio via ScreenCaptureKit |
| Audio processing | whisper-cli on each source's 30s WAV chunk | Silero VAD → Pyannote diarization → GPU whisper |
| Retention | Explicit `prune` by age and/or byte cap; no automatic policy | 10 GB cache cap, 7-day frame retention, hourly sweep |
| Schema evolution | Append-only migrations tracked by SQLite `user_version` | `sqlx::migrate!`, versioned, checksum recovery |
| Query filters | Kind, time, exact app, window substring | App, window, browser URL, speaker, length, focus, content type, and time |
| Interface | CLI only | CLI + Axum HTTP API, SSE, WebSocket, MCP, JS SDK |
| Process model | Foreground process with loop-level retries | Tauri app + sidecar server, PID watchdog, recovery loops |
| Text retrieval | FTS5/BM25 with staged all-term → any-term → recent retrieval for `ask` | FTS5/BM25 (vector search is vestigial — see below) |

---

## 1. Storage format — still the largest structural difference

Lumi writes one JPEG per retained display frame (`internal/capture/recorder.go`). At the default two-second interval there are up to **43,200 capture opportunities per display per day**, although perceptual deduplication removes redundant frames before indexing. System and microphone audio are written as separate mono 16 kHz WAV chunks.

screenpipe pipes raw frames into ffmpeg and encodes **HEVC (libx265) MP4 chunks**. CRF and preset derive from a quality setting; **B-frames are disabled** so every frame seeks independently. The container uses fragmented MP4 (`frag_keyframe+empty_moov+default_base_moof`) so frames can be extracted from a recording that is still being written. The `frames` table stores `video_chunk_id` + `offset_index` rather than a file path; single-frame display goes through `ffmpeg -ss <offset> -vframes 1` → MJPEG → base64, behind a `FrameImageCache`.

Lumi now has an explicit pruning path, so growth need not be unbounded, but JPEG-per-frame storage still creates many files and gives up video compression across adjacent frames. This remains the main reason screenpipe can retain denser visual history for a given disk budget.

## 2. Text extraction — the high-level gap is closed; event depth is not

Lumi now uses the same high-level macOS strategy as screenpipe: **Accessibility first, Apple Vision second**. It reads the frontmost application's focused window, recursively collects title/description/value attributes while excluding secure text fields, and associates that snapshot with the display containing the focused window. Vision handles that display when Accessibility is unavailable or empty and handles the other displays. `text_source` and `display_id` are stored as first-class event fields.

screenpipe's Accessibility subsystem goes further. `screenpipe-accessibility` captures clicks, aggregated text input, keystrokes, application switches, window-focus changes, clipboard operations and focused-element context into a dedicated `ui_events` stream and FTS index, in addition to AX-derived screen content.

The remaining difference is therefore not OCR quality. Lumi takes a periodic focused-window text snapshot; screenpipe also models user interaction as a separate event stream. That lets screenpipe answer questions about actions and state transitions that cannot reliably be reconstructed from periodic snapshots.

## 3. Deduplication — now broadly comparable

Lumi's `FrameComparer` (`internal/capture/compare.go`) maintains independent state for each display. It uses SHA-256 of the JPEG bytes as an exact fast path, then compares sampled RGB histograms for visually similar frames. The default similarity threshold is 0.985 while idle and 0.995 during recent input, retaining subtler changes while the user is active. A ten-second maximum-silence rule forces a periodic retained frame.

screenpipe follows the same broad design: image-hash early exit, a downscaled histogram comparison, an adaptive threshold driven by Accessibility input activity, and a maximum skip interval. The implementations differ in sampling and tuning, but near-duplicate handling is no longer a meaningful architectural gap.

Lumi's measured 1920×1080 baseline was about 22 µs for the exact-hash path and 5.7 ms when histogram decoding was needed; see `docs/benchmarks/2026-07-19-native-capture.md`.

## 4. Audio — source coverage is closed; processing depth remains different

Lumi now records **system/output audio and the default microphone simultaneously** from one ScreenCaptureKit stream. It excludes its own process audio and emits each source as a separately attributed mono 16 kHz WAV file. This removes the old AVFoundation-device/loopback requirement and makes meeting audio capture practical.

Lumi still sends every source chunk directly to `whisper-cli`, with no speech gate or speaker model. screenpipe adds:

- **VAD** (Silero v5/v6 ONNX by default, WebRTC alternative), so silence does not reach whisper.
- **Speaker diarization** via Pyannote ONNX, producing a `speaker_id` on transcription rows.
- **In-process GPU whisper** through `whisper-rs` rather than a `whisper-cli` subprocess per chunk.
- **Smart mode**, where an `IdleDetector` defers transcription above roughly 70% CPU while audio continues to be written.

The main gap is now post-capture efficiency and speaker attribution, not the ability to capture both sides of a call.

## 5. Retention — available in Lumi, but operator-driven

Lumi's `prune` command can delete events older than a duration/RFC3339 cutoff, reduce total indexed media below `--max-bytes` by deleting oldest events first, combine both policies, and preview the result with `--dry-run`. It deletes database rows before files so an interruption leaves recoverable orphaned media rather than rows pointing at missing files. Large row deletions are batched below SQLite's parameter limit.

There is still **no default retention policy or scheduler**. An operator must invoke `lumi prune` periodically, for example from launchd.

screenpipe runs an hourly `run_cache_manager` against a `FrameDiskCache` with `max_cache_size_gb` (default 10) and `frame_retention_days` (default 7), pruning by both age cutoff and total size. Its architectural advantage is automatic enforcement, not the absence of equivalent pruning primitives in Lumi.

## 6. Schema evolution — the gap is closed

Lumi now applies ordered, append-only migrations from `internal/store/migrations.go`. Each pending migration runs in its own transaction and the applied version is recorded in SQLite's `user_version` pragma. The current schema is version 3, including indexes for app, display, and audio-source access plus first-class `text_source`, `display_id`, and `audio_source` columns. External-content FTS5 remains synchronized by insert/delete/update triggers.

screenpipe uses `sqlx::migrate!` against versioned migration files, with explicit checksum-mismatch recovery and foreign keys disabled during migration. The mechanisms differ, but both projects now have a forward upgrade path for databases already in the field.

## 7. Query surface — Lumi added the highest-value filters

Lumi's `SearchOptions` now supports content kind, time range, **exact case-insensitive application**, and **case-insensitive window-title substring** filters. The CLI exposes these on both `search` and `ask`. FTS terms are safely quoted and use conjunctive BM25 search by default; `ask` strips question stopwords and degrades explicitly through all-term, weighted any-term, then recent-event retrieval.

screenpipe's `GET /search` additionally filters on `browser_url`, text length, speaker IDs/name, and focus state, with offset pagination and a Moka LRU cache keyed on the full parameter hash. A second `GET /search/keyword` endpoint adds fuzzy matching, explicit ordering (`timestamp_asc|timestamp_desc|relevance`), and result grouping. `browser_url` is captured atomically with the screenshot to avoid timing mismatch.

Lumi has closed the app/window usability gap, but it does not capture the browser, speaker, and focus metadata needed for screenpipe's remaining filters, nor does it expose pagination, grouping, or fuzzy-order controls.

## 8. Process model, recovery, and permissions

Lumi's native subsystem now preflights Screen Recording, Accessibility, Input Monitoring, and Microphone permissions. `lumi permissions --request` invokes Apple's native request flows, `doctor` reports actionable status, and `native-smoke` provides a bounded integration check. Input Monitoring is optional because Lumi detects recent input from CoreGraphics session state rather than installing an event tap.

Recording is still a foreground CLI process. A failed screen capture is retried on the next interval, and the audio loop waits one second and retries after a capture failure. Already-written media is preserved and indexed with processor diagnostics when Accessibility, Vision, comparison, transcription, or shutdown finalization fails.

screenpipe runs two supervised processes: the Tauri app spawns `screenpipe-server` as a sidecar in a dedicated thread with its own Tokio runtime. The server watches its parent PID and self-terminates if the app dies. Capture tasks run recovery loops, `/health` detects stalls from recent frame/audio timestamps, and the tray polls it every 30 seconds. A permission monitor polls every ten seconds and reports permission loss after two consecutive failures.

Lumi now handles native permissions and transient loop failures, but still has no launchd service, PID/status commands, health endpoint, stall detector, continuous permission-loss monitor, or process supervisor.

## 9. Interfaces beyond the CLI

Lumi remains a CLI with no server, socket, or streaming interface.

screenpipe exposes an Axum server on `localhost:3030` with `/search`, `/frames/:id`, `/tags/*`, `/raw_sql`, `/ui-events`, SSE frame streaming at `/stream/frames`, and a health WebSocket. On top of that are the `@screenpipe/js` and `@screenpipe/browser` SDKs and an MCP server (`npx -y screenpipe-mcp`) exposing `search-content`, `search-ui-events`, `get-ui-event-stats`, and `export-video`.

This is an intentional scope distinction in Lumi rather than unfinished capture or indexing work.

---

## What is *not* a gap

**Semantic/vector search.** `sqlite_vec` is imported in `screenpipe-db`, but there is no text-embedding table or vector query path. The only real embeddings are Pyannote *audio* embeddings for speaker identity. screenpipe's advertised "semantic search using embeddings" is, in the code, an LLM writing FTS5 keyword queries. Lumi's FTS5/BM25-only retrieval matches screenpipe's actual behavior; building a text-embedding pipeline would be leapfrogging, not catching up.

**Provider abstraction.** screenpipe supports OpenAI, Anthropic, Ollama, custom OpenAI-compatible endpoints, and its own gateway, with requests routed through an external agent process (`@mariozechner/pi-coding-agent`) over JSON-RPC. Lumi's single-backend Cerebras decision is a deliberate scope choice, not an oversight.

**Basic macOS capture coverage.** Both projects capture all displays, handle display hotplug, prefer Accessibility text with Apple Vision fallback, record system and microphone audio, and suppress near-duplicate frames. Differences remain in storage density, Accessibility event richness, and audio processing, but the basic source matrix is now aligned.

---

## Resource figures (sources disagree)

Worth recording because the numbers are quoted inconsistently across screenpipe's own docs: README says 5–10% CPU, 0.5–3 GB RAM, ~20 GB storage/month; internal docs say 10% CPU and ~15 GB/month; another says ~5–10 GB/month; DeepWiki's storage page says ~30 GB/month at 1 fps. Treat storage as **roughly 5–30 GB/month** depending on fps and quality settings.

Lumi's bounded native sample on an Apple M5 Max used about 104 MiB peak RSS and 6.5 MiB for 12 seconds with one display and five-second audio chunks. That sample validates the pipeline but is too short and workload-specific to extrapolate into a credible monthly figure.

---

## Related

- `docs/plans/2026-07-19-tier1-retention-filters-migrations.md` documents the now-landed migration, filter, and retention work.
- `docs/benchmarks/2026-07-19-native-capture.md` records automated benchmarks, permission-gated native smoke results, and a bounded resource sample for the native capture subsystem.
