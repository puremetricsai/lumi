# Attribute captured audio by origin, and assemble it into one ordered transcript

> **Partly superseded.** Passages here reasoning about `CollapseAudioTracks` and `audio_origin` describe a
> feature since removed; see `internal/store/CLAUDE.md`'s "Audio tracks". The attribution design itself
> still stands.

## Context

Lumi records every audio chunk twice, from the machine's output and from the
microphone. The speakers sit in the same room as the microphone, so the
microphone track holds **both** streams: whatever the machine played, bleeding
back acoustically, plus whoever was actually talking. The system track holds only
what the machine played.

The consequences compounded. Nothing said which track a phrase belonged to, so a
captured call could not be read as a conversation. Every sentence the machine
played was indexed twice, so `search_events` ranked against two copies of the same
text. And a turn spanning two 30-second windows arrived as two unrelated results.

Labels are **`internal`** and **`external`**, naming provenance rather than
identity:

- **`internal`** — sound this machine produced: the far side of a call, a video,
  music, a notification. Emphatically not always a person.
- **`external`** — sound the microphone picked up from the room, usually the user.
- **`unknown`** — machine audio was playing but produced no transcript, so
  provenance could not be determined.

The physical tracks keep their existing names (`system` / `microphone`, matching
`audio_source`); those say which WAV a segment was *read from*. `origin` is the
derived claim. They differ exactly when bleed is found, and that difference is the
feature.

### What the data said

Five measurements shaped the design more than any argument did.

**Bleed is one-directional, by architecture.** The system track is a tap on the
audio output graph; room sound has no path into it. Across ten captured chunks
where the user spoke continuously for thirty seconds, the system track held
*exactly zero samples* in nine, and the tenth measured −46.0 dBFS — louder than
the microphone's −57.6 dBFS speech, so real machine audio rather than room bleed.

That collapses the whole problem. Anything in the system track is machine-origin
with no inference required, and the only remaining question is **which spans of
the microphone transcript are re-recordings**. Everything else in the microphone
track is room audio by elimination. There is no speaker identification anywhere
here, and the sole error mode is *failing* to detect bleed, whose fallback is
`unknown`.

**Timings existed and were being discarded.** `SpeechTranscriber.Result.range` is
a non-optional `CMTimeRange` available with no attribute options at all, and
`String(result.text.characters)` was throwing it away along with per-run
`.audioTimeRange`. A 30-second microphone chunk yields **10 results and 79 timed
words**, with per-word confidence — not one opaque blob.

**`result.range` is unusable for overlap.** A result reporting 3000–9660 ms held a
single word occupying 3000–3660 ms; the range extends to the finalization
boundary. Attribution unions *word* intervals instead. With the result range, a
user's clean speech at 2280–7560 ms showed `f ≈ 0.86` against machine audio and
took a crosstalk penalty; with word intervals it shows `f = 0.125` and is
correctly `external` at full confidence.

**Similarity is sharply bimodal.** Over 61 real pairs: 9 scored 0.0–0.1, then
**four consecutive empty buckets**, then 51 above 0.5 (34 of them above 0.9). Both
thresholds sit in a 0.4-wide valley, so neither is delicate.

**A third of bleed chunks need splitting.** 17 of those 51 matched under 80% of the
microphone transcript, meaning real room speech sat alongside the re-recording in
one window. Labelling wholesale would bury the user's words on a third of chunks.

## Approach

### 1. Timed transcription and a real audio start

`speech.swift` requests `[.audioTimeRange, .transcriptionConfidence]` and gains
`lumi_transcribe_audio_segments_json` **alongside** the existing string entry
point. Both go through one `runTranscription`, so the flat transcript and the
timed segments cannot disagree and neither can silently lose the
`AnalysisContext` vocabulary biasing. That sharing is safe because the spike
confirmed attributed text is byte-identical to baseline.

`events.text` stays the recognizer's results concatenated with **no separator**.
Runs carry their own leading whitespace, so that is what produces readable prose —
and it is what every already-indexed row holds. Joining with a space would
re-index the corpus for reasons unrelated to this feature.

`native.m` records each writer's session-start PTS and the wall clock of its first
sample buffer. Both tracks come from one `SCStream`, so their PTS values share a
host timebase; measured skew between the two writers is **60.9 ms**, with the PTS
delta and wall-clock delta agreeing to 0.35 µs. `captured_at` is not a substitute:
it is read before ScreenCaptureKit is even asked for shareable content.

### 2. `audio_segments` (migration 4) and attribution

`origin` is TEXT with no `CHECK`, so distinguishing machine-side participants
later is a value change rather than a migration. `started_at`/`ended_at` are
nullable RFC3339Nano UTC; `seq` is the chunk-global reading position and the only
ordering key present on every row, because the text-only path produces no times.
Rows die with their event through **both** a foreign key and an `AFTER DELETE`
trigger, since `PRAGMA foreign_keys` is per-connection.

`internal/transcript` is pure — no database, no cgo, no filesystem. It has two
paths and one entry point that never returns an error:

- **Timed**: every system segment is `internal` unconditionally. Each microphone
  segment is measured for overlap against unioned machine word intervals, then
  compared by token alignment: high similarity is bleed, low is simultaneous
  speech at reduced confidence, and the band between splits the segment at the
  matched span.
- **Text-only**: the microphone transcript is the ordered spine and the system
  transcript the labelling oracle. This works because the microphone WAV is a
  single stream holding everything audible, so one recognizer pass over it is
  chronological by construction. Only absolute time is lost, never sequence.

`internal/wav` exists for one reason: the recognizer returns **nothing** both for a
silent track and for one that played audio it could not transcribe, and those need
opposite conclusions. Only energy separates them. The gate reads a per-window
envelope rather than whole-file RMS, because one notification blip — measured
occupying a single 100 ms window in thirty seconds — would otherwise mark the
whole chunk ambiguous.

### 3. Turn assembly and read surfaces

Continuation across a chunk boundary is **structural**, not gap-based. Inter-chunk
dead air is unobservable and measures 0.4–2.9 s, straddling a natural pause; what
is observable is adjacency, and 3,028 of 3,040 consecutive chunks were captured
30–33 s apart with the next outliers at 39 s, 48 s, and 91 s. 35 s sits in that
empty band and separates "the next chunk" from "the recorder restarted".

Confidence aggregates to the minimum. Order confidence does not aggregate at all:
interpolated speech never merges with anchored speech, so a large well-positioned
passage is not dragged to `approximate` by a fragment, and guessed text is never
inserted mid-passage. Overlapping turns are legal output — simultaneous speech
produces two turns covering the same seconds, reported rather than serialized into
a false order.

`store.Transcript` is the only way callers reach assembly, and it clamps the turn
limit itself so the number the MCP schema documents is the number enforced. It
returns coverage counts so a caller can say a transcript has holes instead of
serving a partial one that looks complete.

Both ways a transcript can be short are reported, because they are different
facts: `Capped` drops turns after assembly, `Truncated` drops segments before it.
Truncation stops on a chunk boundary and returns `CoveredUntil`, so a long window
is read in passes rather than silently cut off mid-turn — and coverage is measured
over what the turns reach, since counting the requested window would have the
completeness signal vouch for text that was dropped. Filtering by origin happens
on the assembled turns, never on the segments: hiding one side of a conversation
before assembly makes two separate replies look adjacent and merges them.

### 4. Backfill

The work queue is derived — audio chunks with no segments — so there is no state
file to go stale, an interrupted run simply resumes, and it doubles as the
recorder's retry path. That only works if every verdict writes something, so a
chunk holding no words writes a wordless `silent` marker: with nothing written,
"attributed and empty" and "never attributed" are the same absence, and silence
is the common case rather than an edge one. Text-only is the default and needs no audio files at all;
`--retranscribe` recovers word timings and refuses to run beside the live recorder
unless overridden, because a second speech workload can make the recorder's inline
transcription time out and cost a live chunk its transcript.

## Deliberately not done

**Rewriting the microphone row's `events.text`.** `CollapseAudioTracks` already
returns one survivor per chunk, so `search_events` returns one hit today; the
remaining gain is bm25 corpus hygiene. The cost is real: `OriginOf` derives
`audio_origin` from `TextLength > 0`, so a bleed-heavy row rewritten to near-empty
would flip `"both"` to `"system"` — and `audio_origin` is the only thing separating
machine audio from the user's own voice. Doing it properly needs a `raw_text`
column and chunk origin moved onto segments. Deferred, gated on measuring whether
double-indexing actually hurts ranking. `search_events` gains only a notice
pointing at `get_transcript`.

**App attribution for audio events.** *Superseded — audio events now carry an
app.* The original objection was that a 30-second chunk spans many focus changes,
so one app stamp is a worse lie than an empty field, and that if ever done it
belonged in `metadata_json`, never in `events.app`.

What changed is that the question split in two. CoreAudio's process objects
(`kAudioHardwarePropertyProcessObjectList` plus `kAudioProcessPropertyIsRunningOutput`)
report *which processes held an active audio output stream* as observed fact
rather than inference, which the original note had no way to obtain. That is
weaker than "was emitting sound" — a paused player still answers yes — so the
field is named for what it proves. The set — genuinely a set, and genuinely
unflattenable to a scalar — went to `metadata_json` as
`active_audio_output_processes`, exactly where this note said attribution belonged, while
`events.app`/`events.window` took the focused application, keeping the meaning
`app` carries everywhere else in the index.

The imprecision the note warned about is real and accepted: one stamp per chunk,
sampled at close. It is bounded by the fact that both tracks of a chunk get the
*same* stamp, so `CollapseAudioTracks` cannot make the reported app depend on
which track happened to transcribe. `AttributionHealth` stays screen-only, but
now because mixing kinds would measure the screen path badly, not because audio
has nothing to contribute.

**Cross-correlating the two WAVs.** Measured writer-start lag is 30–70 ms with a
flat correlation surface. Recording the session-start PTS makes it exact for new
captures, and for older ones the residual sits far inside the alignment slack.

## Files

| File | Change |
|---|---|
| `internal/macosnative/speech.swift` | attribute options; one shared `runTranscription`; `lumi_transcribe_audio_segments_json` |
| `internal/macosnative/native.m` | writer records session-start PTS and first-buffer wall clock; frame JSON gains three fields |
| `internal/macosnative/native.{go,_stub.go}` | `SpeechRun`/`SpeechSegment`/`Transcription`, `TranscribeAudioSegments`, widened `AudioFrame` |
| `internal/capture/audio.go` | mirrored timed types; `NativeSpeech` returns `Transcription`; `AudioFrame.StartedAt` |
| `internal/capture/recorder.go` | widened `SpeechTranscriber`; `attributeChunk` runs **after** both inserts |
| `internal/transcript/` | **new** — `Attribute`, token aligner, `AssembleTurns` |
| `internal/wav/` | **new** — chunk-walking reader (`0xFFFE`, `JUNK`, `FLLR`), `Envelope`, `RMSDBFS` |
| `internal/store/migrations.go` | migration 4 |
| `internal/store/{segments,transcript}.go` | **new** — segment CRUD, coverage, `Transcript` |
| `internal/mcp/transcript.go`, `server.go`, `tools.go` | `get_transcript`; a notice in `search_events` |
| `internal/cli/transcript{,_backfill}.go` | **new** — `lumi transcript` and `lumi transcript backfill` |

## Tests

Weight sits in `internal/transcript`, pure and table-driven with no DB and no cgo.
Fixtures are **synthetic**: the shapes come from real captured chunks, but real
chunks are private conversation and do not belong in a source repository. The
calibration and real-pair harnesses read their input from a path in the
environment and skip without it, so the measured numbers live in the repo and the
words do not.

Notable cases: every system segment is internal without verification; bleed is
excluded from a transcript but never deleted; simultaneous speech scores strictly
below the clean baseline; an ambiguous segment splits without losing words; a
silent machine and an untranscribed one reach opposite verdicts; the silence gate
is local so one blip cannot taint a chunk; the text path never emits the same
machine words twice; a failed segment write still leaves the audio indexed *and*
queued for backfill; segments die with their event with `foreign_keys` explicitly
**off**, proving the trigger rather than the FK.

## Verification

1. `task check`, plus `task test:native` and `LUMI_NATIVE_SMOKE=1` for the native
   changes.
2. `./lumi native-smoke` — both tracks must report a start anchor; the printed PTS
   delta and wall-clock delta should agree.
3. `./lumi transcript backfill --since … --until … --explain --dry-run` — expect a
   bimodal similarity distribution. **If it is not bimodal the thresholds are
   wrong and the timed path will be too.**
4. Run it, then `./lumi transcript --since … --until …`: machine sentences appear
   once, room speech survives as `external`, turns continue across 31-second
   boundaries and never across a 91-second one.
5. `./lumi search --type audio --json` must be unchanged for pre-existing rows.
6. On a **copy** of the database, `./lumi prune --older-than …` then assert no
   `audio_segments` rows remain before the cutoff.
