# internal/transcript

Decides where captured sound came from and assembles it into one ordered transcript, backing
`lumi transcript` and the `get_transcript` MCP tool. Pure: no database, no cgo, no filesystem, so every rule
is testable without permissions. `Attribute` never returns an error — missing data lowers labels and
confidence instead. It has a timed path (word-level overlap plus token alignment) and a text-only path for
chunks whose WAVs are gone, and `AssembleTurns` merges segments into turns. Named for what it produces:
nothing here clusters voices, and the labels name provenance rather than people. The two `Segment` types —
this one and `store.Segment` — shadow each other the way `internal/mcp`'s `AttributionRecord` shadows
`store.Attribution`; `internal/store` imports this package, never the reverse.

The two writers that call `Attribute` are the recorder (`internal/capture`) and the backfill
(`internal/cli`); the rules they share are single-sourced here and in `internal/store`, never restated —
see `internal/capture/CLAUDE.md`.

## Invariants

- **Origin is `internal`/`external`, naming provenance rather than identity.** `internal` is sound this
  machine produced — the far side of a call, a video, music, a notification — and is *not necessarily a
  person*; `external` is sound the microphone picked up from the room. Labelling these `remote`/`self`
  would assert an identity the data cannot support, since a video is not a conversation partner. Keep the
  physical track names (`system`/`microphone`) separate: they say which WAV a segment was read from, which
  differs from its origin exactly when bleed was found.
- **Bleed is one-directional, so internal-track content needs no verification.** The system track is a tap
  on the audio output graph; room sound has no acoustic path into it. Measured across ten chunks of
  continuous user speech, the system track held *exactly zero samples* in nine. So every system-track
  segment is `internal` unconditionally, and the entire problem reduces to which external-track spans are
  re-recordings. The only possible error is failing to detect bleed, whose fallback is `unknown`.
  Deliberate loopback routing (BlackHole, Loopback.app) breaks this; that is a stated limitation, not
  something to detect.
- **Bleed must never be *assumed* to exist.** With headphones there is none, so the overlap and similarity
  tests stay even though the internal direction is certain.
- **The energy gate is a presence test, never a loudness test, and it reads an envelope not a whole-file
  RMS.** The recognizer returns nothing both for a silent track and for one carrying untranscribable
  audio, and only energy separates them. Absolute dBFS is not portable across sessions — clear speech
  measured −26 dBFS in one recording and −68 dBFS in another — but a digital output tap reads *exactly
  zero* when nothing plays, so presence is unmistakable. Whole-file RMS would let one notification blip,
  measured occupying a single 100 ms window in thirty seconds, mark the whole chunk ambiguous.
  `EnvelopeWindowMS` and `NeedsInternalEnergy` are exported because both writers apply them.
- **A silent internal track and an untranscribed one are different findings**, needing opposite
  conclusions: silence means the microphone is confidently the room's, audio-without-words means a
  re-recording of unseen speech is possible and the honest answer is `unknown`.
- **Asking the question always records an answer, so `Attribute` never returns an empty slice for a chunk
  with a track.** The routing gate and the exit filter have to share one notion of a word, and they did not:
  `hasSpeech` accepted any non-blank text while `finalize` drops every fragment without a letter or digit.
  SpeechAnalyzer emits exactly that — 7 microphone rows in a live index held a bare `.` — so those chunks
  routed to `externalOnly`, emitted one punctuation segment, lost it in `finalize`, and returned nothing.
  Nothing is not `silent`: `IsSilent` needs a row to read, so no marker was written, and because the work
  queue is *derived* from the chunks with no segments, each one sat there permanently while `lumi transcript`
  counted it as a gap and advised a backfill that reached the same dead end. Measured on a live index, 5 of 6
  gaps in one hour were this. `hasSpeech` now asks `hasWord`, and `attributed` falls back to the marker
  whenever a path's output is emptied anyway — the gate is the fast path, the fallback is the guarantee,
  since any route can lose its last word in `finalize`. A chunk with no track still returns nothing: there
  is no event to store a row against, and orphaning a segment is worse than leaving it queued.
- **Overlap comes from word intervals, never `result.range`.** A recognizer result extends to its
  finalization boundary; one measured reporting 3000–9660 ms held a single word at 3000–3660 ms. The wider
  span drags neighbouring room speech into an overlap it never had.
- **The external transcript is the ordered spine; the internal transcript is the labelling oracle.** The
  microphone WAV holds everything audible in one recognizer pass, so its reading order *is* conversational
  order. Treating the two as peer tracks to be merged by time is what makes ordering look unrecoverable
  when it is not — only absolute time is lost without timestamps, never sequence.
- **Whatever a bleed region emits is exactly what must count as accounted for.** Deriving the emitted text
  from a region's whole span but coverage from the underlying blocks let the words between blocks be
  emitted twice — once inside the region and again as unheard machine audio — reintroducing, in the
  transcript, the duplication this feature exists to remove.
- **Interpolated speech never merges with anchored speech.** Machine audio the microphone never captured
  has no position of its own. Folding it into a well-anchored turn both drags a large correctly-positioned
  passage down to `approximate` and inserts guessed text mid-passage. Confidence still aggregates to the
  minimum; order confidence is kept separate instead.
- **Interpolated speech is anchored to an emitted position, never to a region index.** A bleed region emits
  a variable number of segments, so its index among the regions says nothing about where it sits in the
  finished transcript. Anchoring on the index put unheard machine audio at seq 1 of 7, four turns ahead of
  the phrase it followed; `approximate` excuses an imprecise position, not a wrong one.
- **Turn continuation across a chunk boundary is structural, not gap-based.** There is now no inter-chunk
  dead air at all, and in the index recorded before the stream was held open there was 0.4–2.9 s of it,
  unobservable and overlapping a natural pause. What *is* observable either way is adjacency: 3,028 of
  3,040 consecutive chunks were captured 30–33 s apart, with the next outliers at 39 s, 48 s, and 91 s,
  and a continuously open stream lands at exactly 30 s rather than scattered through that band. 35 s still
  separates "the next chunk" from "the recorder stopped", for old rows and new alike, and a turn must never
  bridge a stop.
