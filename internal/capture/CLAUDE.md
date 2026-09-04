# internal/capture

`Recorder` runs independent screen and audio goroutines until the context is cancelled, with native
processors behind small interfaces. Displays are re-enumerated each interval for hotplug, and a display
selection is re-applied against that enumeration on every tick rather than resolved once at startup. Screen capture is
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
- **Captured filenames are unique per display per instant, and per chunk per track, and something else now
  depends on that.** `internal/compress` pairs a file with an event by swapping its extension, so two
  events whose media differed only by extension would let it overwrite or delete the wrong one. A coarser
  naming scheme re-arms that; it has a backstop (`findConflicts`) but the naming is the real guarantee.
- **A display selection that names nothing connected records every display, and says so.** `NativeScreens`
  takes `DisplayIDs`; empty means every display. The intersection is taken natively, against the very list
  the capture loop is about to iterate — never against a second enumeration, which could name a display
  ScreenCaptureKit is not offering and so match nothing, capturing nothing at all. Recording the wrong
  screen is recoverable; recording no screen is a hole in the index. Nothing in the resulting rows says
  which happened, so `SelectionFallback` rides on the frame and `captureScreen` logs the transition —
  **once**, not per tick: at a two-second interval a standing condition logged every tick buries the moment
  it began. It is kept separate from `CaptureError` because degraded and failed are different questions.
- **`Screens` is the screen tick's `Levels`.** One `ScreenCapture` per tick naming the displays that tick
  actually captured, so a supervisor can say how many displays are being *recorded* rather than how many
  are connected — different numbers as soon as a selection exists, and only the recorder knows the first.
  It carries its own `IntervalMS` so a reader ages it without assuming a flag's value.
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
- **A chunk's `captured_at` is the instant its audio began, *measured* at rotation** by reading the host
  clock and ageing it back to the boundary presentation timestamp. Both tracks of a chunk still share one
  timestamp — that is the key their segments are written under, and per-track stamps would break it.
  - It used to be arithmetic on the session anchor (`anchor + N×chunkDuration`). That is uniform but not
    observed: every audio timestamp in an index shared one sub-second fraction (`.943675136` across 24
    events in one measured sample), so clock drift was undetectable, a dropped chunk renumbered silently
    instead of leaving a visible hole, and audio↔screen correlation degraded over a long session with
    nothing to show for it.
  - **Ageing the clock is what keeps this from being the old bug.** Reading `time.Now()` between chunks
    made every timestamp absorb the previous chunk's processing time, which let indexed chunks sit 32 s
    apart while each held 30 s of sound. Subtracting the buffer's age puts the value back at the instant
    the audio began.
  - **The drift-free grid is not lost, only moved.** `stream_offset_ms` carries the exact distance from
    the session anchor and `grid_started_at` the grid point itself, so coverage arithmetic stays exact.
  - **Two guards in `rotate`, and the tolerance comes from the turn-merge headroom, not the chunk
    duration.** A measured stamp at or before its predecessor falls back to the grid, because turn
    continuation requires a strictly positive gap. So does a drift beyond `LumiMaxMeasuredDriftNS`
    (250 ms): chunks sit 30 s apart and `transcript.DefaultMaxChunkGap` is 35 s, so there are only **5 s**
    of slack — a half-chunk tolerance would admit a 5–15 s jump and silently split a continuously recorded
    turn mid-sentence, reintroducing exactly what the contiguous stream removed. The tolerance is capped at
    half the *configured* chunk as well, since `--audio-chunk` has no lower bound and 250 ms spans two
    whole 100 ms chunks. **Falling back is not sufficient on its own**: the previous chunk may have kept a
    measured stamp ahead of its grid point, so the fallback value can still be non-increasing and is
    clamped to `previous + 1`. Every fallback sets `clock_anomaly` in metadata; capture never stops for one.
- **Cancellation stops capture but never abandons it.** `Stop` finalizes the chunk in flight and `Next`
  keeps delivering queued chunks — including that partial one — before reporting `ErrAudioStreamClosed`.
  The native session has no reader to interrupt, so `Next` polls in short slices; that poll interval, not
  the chunk duration, bounds shutdown latency.
- **Preserve provenance.** `text_source`, `display_id`, and `audio_source` are first-class event columns
  (migration 3), joined by `audio_attribution`, `source_apps_json`, and `stream_offset_ms` (migration 5),
  and all of them appear in JSON exports. Audio rows carry `text_source` too — the transcriber names
  itself (`SpeechAnalyzerSource`) rather than the recorder hardcoding it, so the two cannot fork once a
  second recognizer exists, and a *failed* recognition names none: it must never claim a source for text
  that does not exist. In metadata, `app_source` and `attribution_source` answer
  different questions — which source named the *app*, and which supplied the *window title* — and routinely
  differ. Merging them would change what `attribution_source` means for every indexed row.

## What an audio row is attributed to

An audio row answers **two** questions with two separate sets of fields, and conflating them is the defect
this design exists to remove. `app`/`window` say what the user was working in; `audio_attribution` and
`source_apps_json` say what was producing the sound. Measured on a live capture, they disagreed in 2 of 12
chunks over eight minutes, and the ratio scales with how much the user switches applications.

- **An audio row's `app`/`window` name the *focused* application, not the one making the sound.** They
  answer the same question `app` answers for every screen row — "what was the user working in" — sampled
  once per chunk. Putting the sound's source in `events.app` would fork the column's meaning by row kind,
  and it cannot hold the answer anyway: it is a *set*. `internal/mcp` therefore renames the pair to
  `foreground_app`/`foreground_window` on audio rows at the boundary; the SQL columns keep their names
  because FTS5, `Search`'s app filter, and `ListAttribution` all depend on them.
- **`DecideAudioAttribution` is one pure function and the only place the rule lives.** Its precedence is:
  a microphone track is `unattributed` unconditionally; otherwise observed processes win
  (`emitting_process`), then window markers (`foreground_inferred`), then `unattributed`. It performs no
  I/O so the rule is exercisable without permissions, CoreAudio, or a recording.
- **Several emitters is not a downgrade.** The attribution names how the claim was earned, not how many
  earned it. The system track really is the mix of the whole output graph, so naming one of three would be
  *less* true; ordering by sample count carries the prominence instead.
- **`foreground_inferred` does not mean "we used the focused app".** No path may do that. It means the
  evidence came from the window layer — a window whose own title said it was playing audio — rather than
  from CoreAudio. It also does not mean CoreAudio *failed*: the branch fires whenever CoreAudio names
  nobody, and the common case is a clean read that found nothing. Describing it as a read failure
  overstates how unusual the value is and understates the evidence behind it, so the tool description and
  the `AttributionForegroundInferred` doc must both say "named no process", not "could not be read".
- **Emitters are sampled across the chunk, not once at its close.** A 30 s chunk can span several
  application switches, and a single close-of-chunk read attributed all of it to whatever was true at the
  end. `emitterTimeline` collects observations throughout and `foldEmitters` reduces them to a union
  ordered by how much of the chunk each application was present for. A synchronous fallback covers the
  chunk in flight at shutdown and the very short chunks tests use.
- **Exactly one goroutine samples Accessibility at a time.** `AccessibilityContext.Snapshot` serializes on
  a package-level mutex shared with the screen tick, so when screen capture is on the emitter loop reuses
  the snapshot that tick already takes, and only takes its own when there is no screen loop to contend
  with. `TestEmitterLoopTakesNoAccessibilitySnapshotWhileScreenCaptureIsOn` pins it.
- **An empty `source_apps_json` (`[]`) and an absent one (`""`) are different findings.** Sampled-and-quiet
  versus could-not-sample; collapsing them is unrecoverable afterwards. Same rule, one level up, as an
  absent `active_audio_output_processes` against its `..._error` sibling — and the same reason a silent
  chunk is `silent` rather than `unknown`.
- **The two evidence sources succeed and fail *independently*, and are counted separately.** A shared
  "did this observation work" flag lets one source's success mask the other's failure: with CoreAudio
  failing every sample and the window scan merely finding nothing, the chunk was recorded as
  sampled-and-quiet when no process list had ever been read — the exact conflation above, arrived at from
  the other side. So `foldEmitters` keeps a per-source observation count, reports a source's error whenever
  *that* source never read, and sets `Observations` to the **minimum** of the two: only a sample that read
  every source can support "nothing was emitting". The native scan must therefore fail loudly too — a
  `NULL` window list is an error, never an empty result.
- **The synchronous fallback uses its own sample, never a widened window query.** Re-querying the timeline
  through `time.Now()` sweeps in observations belonging to *later* chunks, so a chunk whose indexing ran
  long could be attributed to an application that only began playing after its audio ended.
- **A microphone row is never attributed, however loud the machine was.** The microphone records the room;
  nothing in that signal names an application, and the only app-shaped value within reach is what the user
  had focused — a fact about the user, not about the sound. The claim would also outlive its evidence,
  since the WAV is deleted on the retention schedule while any summary built from it survives. Lumi does
  not identify speakers and does not try: the requirement is that the ambiguity is *stated*, in the data
  and in the MCP tool descriptions, not resolved.
- **The window-title marker is the fallback, and it scans *every* on-screen window.** Chromium appends
  " - Audio playing" to a title while a tab plays sound; measured on a live index, 116 of 117 Comet events
  captured during playback carried it. Scanning only the frontmost window would be useless here — the case
  worth catching is a browser playing in the background while the user works elsewhere, which is precisely
  what a focused-window reading gets wrong. It is the weaker signal (a self-report, absent for any app that
  does not update its title) so it is consulted only when CoreAudio names nobody. It is a separate seam
  from `AudioOutputs` rather than another return value on it, because the decision has to tell "no process
  held a stream" from "the window scan failed" and one error cannot carry both.
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
- **Both tracks of a chunk carry the same stamp and the same attribution.** The stamp is the key
  `ReplaceChunkSegments` writes the chunk's segments under, so a per-track one would split a chunk across
  two keys and leave half of it on the backfill's derived queue forever. The attribution has to match for a
  narrower reason: the tracks are attributed independently, so if they could disagree, which app a search
  reported for a chunk would depend on which track happened to match. Stamping identically makes both
  stable by construction.
- **`events.text` for an audio row is the recognizer's results concatenated with no separator.** Runs
  carry their own leading whitespace. Segments derive from it, never the reverse, and joining with a space
  would silently re-index the corpus.

## Writing origin verdicts (`attributeChunk`)

- **`ReadAudioEnvelope` is the single place that decides how a captured audio file is opened**, and both
  the recorder and the backfill go through it. `internal/wav` reads mono 16-bit PCM RIFF and nothing else,
  deliberately — it is pure Go, so it builds and tests anywhere — while `lumi compress` stores a chunk as
  FLAC. So the split is by half rather than by package: `internal/macosnative` knows containers,
  `internal/wav` measures samples, and this chooses between them on the extension (matched on `.wav`, so
  any container stored later takes the native path instead of failing as a RIFF file). Two copies of that
  choice would give the recorder and a backfill different bleed verdicts for the same audio, and *silently*
  — both callers discard the error, so a chunk that could not be read simply reaches its verdict without an
  envelope.
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
- **`Recorder.Levels` is nil by default, and nil means the measurement is never taken.** Nothing in the
  pipeline needs it — only a supervising app drawing a meter (`levels.go`, `lumi record start
  --emit-levels`). It is measured *live*: sound is summed inside the ScreenCaptureKit callback as it
  arrives, and drained on a ticker every `LevelInterval`, because a meter whose job is to tell a user
  whether their microphone works cannot wait for a chunk to close. The formula and the silence floor stay
  in `internal/wav` — the native side accumulates mean squares of normalised samples and nothing else, and
  `wav.DBFSFromMeanSquare` is the only conversion. The sink runs on the level goroutine, never the capture
  path, and must not block.
- **The level goroutine may never slow capture, and the accumulator may never reject a buffer it does not
  understand *silently enough to matter*.** ScreenCaptureKit does not deliver one format: measured on macOS
  26.5 the system track is non-interleaved 32-bit float at 16kHz and the microphone is interleaved 24-bit
  packed signed integer, stereo, at 48kHz. An accumulator written for float alone measured the system track
  and dropped every microphone buffer — a meter that never moved for the one source a user is most likely
  to be testing, with nothing in any log to say so. Widths are handled explicitly in `LumiLevelMeterAdd`;
  add a case rather than assuming the next one matches.
- **An empty drain is not silence.** No completed window means the poll outran the window length; silence
  completes windows too, at `wav.SilenceFloorDBFS`. Collapsing the two draws a dead microphone as a quiet
  room, which is the exact question the meter exists to answer.
