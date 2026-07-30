# internal/capture

`Recorder` runs independent screen and audio goroutines until the context is cancelled, with native
processors behind small interfaces. Displays are re-enumerated each interval for hotplug. Screen capture is
a per-tick call, but audio is a *stream*: `AudioSource.Open` returns an `AudioStream` whose `Next` yields
chunks while capture keeps running, so transcription, indexing, and attribution all happen alongside the
next chunk rather than between two recordings. Everything runs in-process — no subprocesses. One
focused-window snapshot per tick is stamped onto **every** display's frame, so on a multi-display setup an
`app` filter returns frames from displays where that app was not visible: attribution answers "what was the
user working in", not "what is shown in this image". Per-display attribution is deliberately not done.

`ScreenContext` degrades rather than failing: `Snapshot` errors only when nothing at all could be read, and
a failed Accessibility read arrives as a *populated* context carrying `AccessibilityError`. `Degraded()`
("something was lost") and `Unattributed()` ("no app name at all") are different questions — conflating
them fires warnings on routine operation. `Trusted` is a `*bool` so "revoked" and "never sampled" stay
distinct. Sustained degradation escalates on **elapsed time**, not tick count, since `--interval` is a flag.

`recorder_test.go` runs the whole capture→store→search pipeline with fake
`ScreenSource`/`ContextExtractor`/`TextExtractor`/`AudioSource`/`SpeechTranscriber` implementations, needing
no permissions or external binaries. Prefer extending it over invoking real frameworks.

How the frontmost app and title are actually resolved is `internal/macosnative`'s; how the resulting verdict
rows are shaped is `internal/store`'s; the labelling rules the recorder applies are `internal/transcript`'s.

## Invariants

- **Never lose captured media.** If Accessibility, Vision, comparison, or transcription fails after a file
  was written, preserve and index the event with diagnostic metadata. Don't convert downstream failures
  into early returns that drop the file.
- **Deduplicate per display, not globally.** `FrameComparer` uses SHA-256 as an exact fast path and a
  sampled RGB histogram for near-duplicates; active input raises sensitivity. Two retention deadlines:
  `MaxSilence` (10s) when bytes *changed* but scored similar (video, advancing slides), and `ExactSilence`
  (5min) when bytes are identical, so a frozen screen leaves a bounded presence marker instead of
  re-indexing the same JPEG. `ExactSilence` is clamped up to `MaxSilence`.
- **Capture retries without discarding completed work.** Screen failures retry on the next interval; an
  audio stream that fails is reopened after one second. Media returned during cancellation gets a short
  cancellation-free window for insertion.
- **The audio tap never closes between chunks.** One `SCStream` stays open for the whole recording and
  `LumiAudioSession` rotates the WAV writers on presentation-timestamp boundaries, on the same serial queue
  the sample buffers arrive on, so the buffer crossing a boundary opens the next chunk whole. Cycling the
  stream per chunk — start, sleep, stop, finalize, start again — cost 2.0–2.3 s of every 32 s: chunks were
  exactly 30.000 s but arrived 32.0–32.3 s apart, a 6.5% loss landing mid-sentence, visible as one chunk
  ending "Instead of running." and the next opening "splits the main task into smaller parts". Roughly
  1.7 s of that was the native lifecycle and 0.45 s was transcription blocking the loop, so fixing either
  alone leaves most of the hole. Measured after: consecutive chunk starts exactly one chunk duration apart,
  each file holding that whole interval to within one sample buffer.
- **A chunk's `captured_at` is the instant its audio began**, derived by offsetting the session anchor
  rather than by reading the clock at rotation, so the grid cannot drift and both tracks of a chunk share
  one timestamp — which is what audio collapse groups on. Reading `time.Now()` between chunks made every
  timestamp absorb the previous chunk's processing time, which is what let indexed chunks sit 32 s apart
  while each held 30 s of sound.
- **Cancellation stops capture but never abandons it.** `Stop` finalizes the chunk in flight and `Next`
  keeps delivering queued chunks — including that partial one — before reporting `ErrAudioStreamClosed`.
  The native session has no reader to interrupt, so `Next` polls in short slices; that poll interval, not
  the chunk duration, bounds shutdown latency.
- **Preserve provenance.** `text_source`, `display_id`, and `audio_source` are first-class event columns
  (migration 3) and appear in JSON exports. In metadata, `app_source` and `attribution_source` answer
  different questions — which source named the *app*, and which supplied the *window title* — and routinely
  differ. Merging them would change what `attribution_source` means for every indexed row.

## What an audio row is attributed to

- **An audio row's `app`/`window` name the *focused* application, not the one making the sound.** They
  answer the same question `app` answers for every screen row — "what was the user working in" — sampled
  once when the chunk closes. Which processes held the audio output is a different question with a
  different answer, and lives in `metadata_json` as `active_audio_output_processes`. Putting one of those
  in `events.app` would fork the column's meaning by row kind, and it cannot hold the answer anyway: it
  is a *set*.
- **`active_audio_output_processes` names stream occupancy, not audible sound, and is named for what it
  can prove.** `kAudioProcessPropertyIsRunningOutput` reports "running IO with at least one active output
  stream": a *paused* player still answers yes, while the same app with its document closed does not —
  both measured with QuickTime. Calling the field `emitting_processes` claimed audibility the data cannot
  support, and a field name is the first thing an agent reads. Nothing cheaper is available: the
  per-process property set is closed (PID, BundleID, Devices, IsRunning, IsRunningInput,
  IsRunningOutput) and carries no level, so proving real emission needs a process tap. Measured
  false-positive rate under ordinary use is low — 44 samples over 100s of editor/terminal/browser
  activity found *no* process holding a stream — but a low rate makes the signal useful, not the name
  true.
- **An absent `active_audio_output_processes` means no process held a stream; the `..._error` key means
  Lumi could not tell.** Writing an empty list would collapse the two, and nothing downstream could
  separate them afterwards. Same reason `silent` is not `unknown`. Absence carries that meaning only
  because `internal/cli` always wires `Recorder.AudioOutputs`; leaving it nil makes absence mean "never
  sampled" instead. That precondition is documented on the field rather than enforced with a marker,
  since Go's `internal/` rule puts the unwired state out of reach of anything but this module's tests.
- **Both tracks of a chunk carry the same stamp.** `CollapseAudioTracks` picks a survivor by
  `(hasText, isSystem, runeLen, -id)`, so per-track stamps would make the app a search reports depend on
  which track happened to transcribe. Stamping identically makes the survivor's attribution stable by
  construction, as the shared `captured_at` already is.
- **`events.text` for an audio row is the recognizer's results concatenated with no separator.** Runs
  carry their own leading whitespace. Segments derive from it, never the reverse, and joining with a space
  would silently re-index the corpus.

## Writing origin verdicts (`attributeChunk`)

- **A segment-write failure never costs an event.** Rows insert first; attribution is a second pass whose
  retry mechanism is the backfill's derived work queue, so no retry loop belongs in the recorder.
- **The recorder and the backfill share every rule they both apply, rather than each stating it.**
  `ReplaceChunkSegments` promises all three write paths converge on the same rows, and a rule copied across
  a package boundary is correct only until one copy moves — invisibly to both test suites, since neither
  can see the other writer. So the shared pieces are exported and single-sourced: `store.SegmentFrom` /
  `SegmentRows` (verdict → row), `transcript.IsSilent` and `store.AnyFailedTranscription` (the silent-and-
  failed gate), `transcript.TrackSystem` / `TrackMicrophone` (the track vocabulary),
  `transcript.EnvelopeWindowMS` and `NeedsInternalEnergy` (which chunks are worth measuring, and at what
  resolution), and `capture.TimedSegmentsFrom` (the bridge's timings). The energy gate is the cautionary
  case: it was duplicated, and the two copies had already drifted on whitespace-only transcripts — the
  backfill reading a 960KB WAV for every silent chunk, and the pair reaching different verdicts for the
  same audio.
- **A chunk holding no words still writes a marker row** (`origin` = `silent`, no text, `MethodSilent`).
  The work queue is derived, so a verdict that writes nothing is indistinguishable from one never reached —
  and since silence is the common case, every such chunk would stay queued forever, be re-derived on every
  backfill, and be counted as a permanent coverage hole while the transcript advised a backfill that could
  not change it. `silent` is deliberately not `unknown`: unknown *warns* that hidden machine speech may be
  present, which is the opposite claim.
- **A chunk whose recognition failed is never labelled `silent`, and never drains from the queue.** The
  recognizer returns an empty transcript both for a quiet room and for a track it never got to, so silence
  is the one verdict a *failure* can turn into a false claim about the world — and the marker that records
  it is exactly what makes the claim permanent. Both the recorder (`attributeChunk`) and the backfill
  (`attributeStoredChunk` in `internal/cli`) therefore gate on the pair "verdict is silent **and** some
  track carries `processor_error`" — `transcript.IsSilent` and `store.AnyFailedTranscription`, each
  exported so the two writers share the rule rather than restating it. The gate is the *verdict*, not the
  failure: a track that did transcribe is labelled on its own evidence, so a mic failure beside a speaking
  system track still attributes. These chunks are the one thing the derived queue cannot drain, which is
  what `store.ChunksFailedTranscription` exists to count — `lumi transcript` and `get_transcript` name them
  apart from real gaps so neither recommends a backfill that would reach the same dead end.
