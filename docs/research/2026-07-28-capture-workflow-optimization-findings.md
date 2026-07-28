# Capture Workflow Optimization Findings

**Date:** 2026-07-28  
**Scope:** Read-only review of Lumi's screenshot capture, OCR, audio capture,
transcription, storage, and full-text indexing workflow.

## Executive summary

Lumi has a sound baseline architecture for a local-first recorder. Its
per-display screenshot deduplication and SQLite FTS5 indexing are already
appropriate for the current event rate. The workflow is not yet fully optimized
for reliable, always-on operation, however.

The highest-impact issue is audio continuity: Lumi records a chunk, stops
capture, transcribes the system and microphone files sequentially, and only then
starts the next chunk. Transcription and stream restart time therefore create
gaps in the audio record.

The next-largest concern is media growth. Both audio sources are stored as
uncompressed PCM WAV, while screenshots are captured at the display's full
backing resolution and stored as quality-0.82 JPEGs. Native processing and media
storage will become bottlenecks before SQLite does.

Recommended order of work:

1. Add current end-to-end instrumentation.
2. Decouple audio recording from transcription.
3. Benchmark compressed media formats and introduce configurable retention.
4. Profile full-display Vision OCR on representative multi-display workloads.
5. Optimize OCR only if those measurements show a material need.

## Current workflow

### Screenshots

The screen loop runs immediately and then on a two-second ticker by default.
Each pass:

1. Enumerates the currently shareable displays.
2. Captures every display at its backing pixel scale with
   `SCCaptureResolutionBest`.
3. Writes a JPEG at quality 0.82.
4. Compares the file against the last retained frame for the same display.
5. Deletes exact or perceptual duplicates.
6. Runs full-display Apple Vision OCR on each retained frame.
7. Adds focused application and window attribution from Accessibility.
8. Inserts the event and media path into SQLite.

Vision OCR is the primary indexed text. Substantive Accessibility text is used
as the searchable fallback when Vision returns no usable text; otherwise it is
kept as event metadata.

Screen processing is synchronous within the screen loop. With multiple displays,
capture, comparison, and OCR are processed serially, so sufficiently slow OCR
can reduce the effective capture cadence.

### Audio

The audio loop records system output and the default microphone concurrently
from one ScreenCaptureKit stream. The sources are written as separate mono,
16 kHz, 16-bit PCM WAV files.

After the configured chunk completes—30 seconds by default—the loop stops and
finalizes the stream. It then transcribes each returned file sequentially with
Apple SpeechAnalyzer and inserts the corresponding events. The next recording
does not begin until both transcription attempts have completed.

SpeechAnalyzer assets are checked at recorder startup, but the Swift
transcription bridge also creates a new transcriber and analyzer and checks
assets for every file.

### Indexing

Events are stored in a single SQLite database using WAL mode and one open
connection. An external-content FTS5 table indexes `text`, `app`, and `window`;
insert, update, and delete triggers keep it synchronized with the event table.
Search uses FTS5/BM25 plus kind, time, application, and window filters.

This is a good fit for Lumi's write rate. The database and FTS work are unlikely
to dominate CPU, latency, or storage compared with media capture and native
processing.

## Evidence

The current perceptual comparison benchmark was run on the repository's baseline
Apple M5 Max host:

```text
BenchmarkSampledHistogram-18                201   6197623 ns/op   5.36 MB/s   3201697 B/op   14408 allocs/op
BenchmarkFrameComparerExactDuplicate-18   53223     22591 ns/op               41384 B/op       5 allocs/op
```

A changed-frame histogram takes approximately 6.2 ms and an exact duplicate
takes approximately 22.6 microseconds. Both are far below the default two-second
screen interval. Although the changed-frame path allocates about 3.2 MB, it is
not currently the first optimization target.

The checked-in native capture sample should not be treated as a current
end-to-end performance result. It used an older Accessibility-primary screen
path and a fake failing transcriber. The current implementation performs
full-screen accurate Vision OCR for every retained frame and uses in-process
SpeechAnalyzer, neither of which is represented by that CPU measurement.

## Findings by priority

### 1. Audio capture has avoidable gaps

This is both a performance and data-completeness issue. The audio loop combines
capture and processing in one serial control flow:

```text
record chunk
  -> stop and finalize stream
  -> transcribe system audio
  -> transcribe microphone audio
  -> insert events
  -> start the next stream
```

The preferred design is:

```text
continuous capture
  -> rotate durable chunk
  -> insert pending event
  -> enqueue transcription
  -> continue capture immediately

transcription worker
  -> transcribe pending event
  -> update text and processing status
```

A SQLite-backed pending state or job table would allow unfinished work to resume
after a crash. A bounded worker count is important because concurrent
SpeechAnalyzers may increase memory pressure or reduce throughput. Start with
one worker and benchmark before enabling more.

A producer/worker split would remove transcription gaps while retaining the
existing stop/start chunk implementation. A longer-lived ScreenCaptureKit
stream with rotating writers could subsequently remove stream restart gaps if
measurements show they are material.

### 2. Media growth is likely the operational bottleneck

Two streams of mono, 16 kHz, 16-bit PCM require approximately 64,000 bytes per
second in total, or roughly 5.5 GB per 24 hours of uninterrupted recording,
before filesystem overhead.

The existing short native sample retained five screenshots totaling 5,312 KiB,
or about 1.06 MiB per image. That sample is not a long-term projection, but it
shows the potential scale:

- At one retained image every two seconds, that average would approach 45 GiB
  per display per day.
- At one retained near-duplicate checkpoint every ten seconds, it would approach
  9 GiB per display per day.
- Byte-identical screens use the longer five-minute checkpoint and grow much
  more slowly.

These figures vary significantly with resolution and image content, so format
changes should be benchmarked on representative displays.

Candidates include:

- Store spoken audio using AAC, ALAC, or another AVFoundation-supported format.
- Compare HEIC with JPEG for screenshot size, encoding cost, OCR compatibility,
  and export ergonomics.
- Test a maximum screenshot dimension or logical-resolution capture against an
  OCR recall corpus containing small text.
- Add an opt-in or configurable automatic age/size policy, or at minimum disk
  usage warnings. The current explicit `prune` behavior should remain the only
  deletion path unless retention semantics are deliberately changed.

### 3. Current instrumentation is insufficient

Before changing concurrency, OCR modes, or codecs, Lumi should measure:

- ScreenCaptureKit enumeration latency.
- Per-display capture and JPEG encoding latency.
- Screenshot bytes written.
- Exact duplicate, near-duplicate, and retained-frame counts.
- Comparison and Vision OCR latency.
- OCR character count and failure rate.
- Intended versus actual audio chunk start and end times.
- Gap duration between audio chunks.
- Transcription duration divided by audio duration, per source.
- Pending transcription queue depth and oldest-job age.
- SQLite insert and update latency.
- Total media growth per hour.

A permission-gated 10–30 minute benchmark should include static, active text,
video/subtitle, and multi-display scenarios. It should use real SpeechAnalyzer
and the current Vision-first path.

### 4. Full-display Vision OCR is the likely screen hot path

Each retained JPEG is reopened and decoded for
`VNRecognizeTextRequest` using accurate recognition and language correction.
The Go comparer also reads the JPEG and decodes it on the changed-frame path.

Possible optimizations, in increasing order of complexity, are:

1. Process retained displays with a small bounded OCR worker pool.
2. Use fast OCR for intermediate frames and accurate OCR for periodic
   checkpoints.
3. OCR changed regions, with occasional full-screen accurate checkpoints.
4. Derive the comparison signature from the original captured `CGImage` and
   avoid one JPEG read/decode cycle.
5. Reuse the captured image for OCR after it has safely been written to disk.

The last two options require a more integrated native boundary. The current
comparison cost is small, so that complexity is only justified if profiling
shows JPEG decoding or disk traffic is significant.

OCR optimizations must be evaluated against retrieval quality. Small text,
terminal output, subtitles, and changes in localized regions are especially
important cases.

### 5. SQLite does not need immediate optimization

The current FTS5 external-content design, synchronization triggers, timestamp
indexes, kind indexes, and WAL configuration are suitable for the expected
event volume. Separate autocommit inserts are acceptable at the current cadence.

Potential maintenance such as WAL checkpoint policy, FTS optimization, batching,
or pagination should be driven by measurements from a realistically large
database. They should not displace work on audio completeness or media growth.

## Additional quality considerations

- A global RGB histogram is inexpensive but relatively insensitive to small,
  localized text changes. The ten-second changed-frame checkpoint bounds the
  loss, but a tiled luminance signature or change map may improve recall if
  tests reveal missed short-lived content.
- Silent audio chunks are still passed through SpeechAnalyzer. A conservative
  speech-activity or energy gate could save processing, provided low-volume
  speech recall is tested and the original media remains preserved.
- System and microphone recordings can contain overlapping speech. Transcript
  deduplication may improve search results, but source provenance should remain
  intact.
- Processing status should be first-class if asynchronous processing is added.
  Empty text alone cannot distinguish silence, pending work, cancellation, and
  a transcription failure.

## Overall assessment

Lumi is well optimized in the narrow areas of screenshot deduplication and
text indexing. It is sufficiently efficient for bounded tests and likely for
light personal use. For dependable all-day capture, the workflow still needs an
asynchronous, durable audio-processing path, better control of media growth, and
current end-to-end measurements.

No repository implementation changes were made as part of this assessment.
