# internal/store

Single-file SQLite via `modernc.org/sqlite` (pure Go, no cgo), `MaxOpenConns(1)` plus WAL. Schema changes
are versioned migrations (`migrations.go`) applied on `Open`, tracked by `user_version`. The `events_fts`
external-content table is trigger-synced, so writes go only to `events`. Timestamps are RFC3339Nano UTC
strings compared lexicographically — any new time column must match or range filters break.

Facts about querying live here, not in callers: `DefaultSearchLimit` (20) / `MaxSearchLimit` (500),
`HasSearchableTerms`, `HasEvents`, `EventByID` (with `ErrEventNotFound`), `ListAttribution`.
`CollapseAudioTracks` merges each chunk's mic/system duplicate into one survivor, returning `AudioChunk`
provenance (`Origin` of `system`/`microphone`/`both`/`silent` plus every merged `AudioTrack`);
`AudioTracksAt` re-reads every audio row at a set of timestamps so `Origin` describes the whole chunk even
when a search matched one track.

`audio_segments` (migration 4) holds each chunk's origin-attributed pieces, derived from events and never
the reverse, so re-deriving them is always safe — that is what makes the backfill idempotent.
`ReplaceChunkSegments` rewrites one chunk in a transaction; `ChunksMissingSegments` is a *derived* work
queue, so there is no state file to go stale and a failed write needs no retry loop. `Transcript` is the
only way callers reach turn assembly, and it clamps its own limits so the number `get_transcript`
documents is the number enforced. It also owns the two ways a transcript can be short — `Capped` (turns
dropped after assembly) and `Truncated` (segments dropped before it, with `CoveredUntil` naming where to
resume) — and measures coverage over what the turns reach, never over what was asked for.
`HasSpeechSegments` answers "is there anything to read", which `SegmentCoverage` deliberately does not:
a silent chunk is attributed but holds nothing.

`AttributionHealth` backs `lumi doctor`; its `LastAttributed` is a scalar subquery over *all* history,
because scoping it to the window would blank the field exactly when the outage exceeds the window. The
event column list lives in one `eventSelect` const shared by `Expired`, `AllEvents`, and `EventByID`.

This package imports `internal/transcript`, never the reverse. The two `Segment` types — `store.Segment`
and `transcript.Segment` — shadow each other the way `internal/mcp`'s `AttributionRecord` shadows
`store.Attribution`.

## Search

- **FTS input must go through `ftsExpression`** (`query.go`). It quotes each term and joins with `AND`/`OR`
  per `SearchOptions.Match`; the quoting is what stops raw user text being read as FTS5 syntax. `MatchAll`
  is the zero value, so `lumi search` stays conjunctive. Terms with no letters or digits are dropped; an
  empty expression means "run no FTS query at all" — an empty MATCH is a syntax error, not a zero-result
  search.
- **`store.MatchAny`, `SearchOptions.RequireText`, and the `bm25(events_fts, 1.0, 0.4, 0.4)` weights are
  reached only through `lumi mcp`** (its `match: "any"` and `require_text` parameters). No CLI command sets
  them. The weights matter: without them a one-word `app` or `window` hit outranks a page of screen text.
- **A rule about the store lives in the store.** When a caller needs to know what `Search` will do, export
  the answer and read it rather than restating it. `HasSearchableTerms` exists because `internal/mcp` had
  reimplemented the unexported drop rule — a copy is correct only until the original moves, and the drift
  is invisible to both test suites.
- **`Search`'s `app`/`window` filters are unqualified SQL predicates, so an app-shaped query spans both
  row kinds.** Callers that need to mean one of them say so: `ListAttribution` takes a `Kind`, and never
  sums the two into a single count — see `internal/mcp/CLAUDE.md` for what that conflation looked like.
- **`AttributionHealth` stays screen-only**, now because each chunk contributes two rows and an audio
  failure would be reported as a screen problem — not because audio carries no app.

## Schema

- **Schema changes go through `migrations.go`.** Append a new `migration` with the next version; never edit
  shipped SQL.
- **`origin` is TEXT with no `CHECK`**, so distinguishing machine-side participants later is a value
  change rather than a migration. `silent` already uses that room, and is why the column could gain a
  fourth value without touching the schema.
- **`audio_segments` dies with its event through a trigger, not only a foreign key**, because
  `PRAGMA foreign_keys` is per-connection and a replaced pooled connection would silently stop enforcing
  it.

## Audio collapse

- **System outranks the microphone on every duplicate, but never outranks a non-empty transcript with
  silence.** `CollapseAudioTracks` orders survivors by `(hasText, isSystem, runeLen, -id)`, and the first
  two keys *are* the rule — `hasText` above `isSystem` so a silent row never deletes a transcript;
  `isSystem` above `runeLen` because the mic is re-recording the speakers, so the system track is the
  cleaner original. Inverting either is silent data loss (`TestSystemAudioWinsEveryDuplicate`). The pair
  shares one `captured_at` by construction (one `Audio.Record` call, one `now`), so collapse groups on the
  timestamp, never id adjacency.
- **Collapse must never become a SQL pre-filter.** An FTS query landing only in the microphone transcript
  matches just that row, and a `NOT EXISTS` filter consulting the non-matching system row would drop the
  only hit. Provenance therefore comes from `AudioTracksAt` reading the whole chunk, so `audio_origin` stays
  truthful even where the survivor is not the system row. `audio_origin` is the only thing separating a
  remote speaker or media (`system`) from the user's own voice (`microphone`). On the CLI collapse is opt-in
  (`--collapse-audio`) so the default `--json` stays a bare `[]store.Event`.

## Transcript assembly and coverage

- **A transcript filters turns, never the segments it assembles them from.** Removing one origin before
  assembly hides the interjection that separated two replies, so `AssembleTurns` reads them as adjacent and
  merges them — inventing continuity and deleting the boundary. Turns are single-origin by construction, so
  filtering afterwards selects without reshaping.
- **Coverage counts describe the range the turns reach, not the range requested.** They exist to reveal a
  partial transcript; counting the whole window while the text stopped early makes them corroborate the
  omission instead. A transcript also never ends mid-chunk, which is what lets the boundary be exact rather
  than an estimate. Both ways a transcript can stop short move that boundary: truncation *and* the turn cap,
  which is measured from the last retained turn's `LastCapturedAt` — `Turn.CapturedAt` is where a turn
  *began*, so a turn spanning chunks would bound the page short of text it already printed.
- **`CoveredUntil` and `ResumeFrom` are separate fields because they need opposite inclusivity.**
  `SegmentsBetween` is inclusive at both ends, so a caller told to resume at the last chunk covered re-reads
  that whole chunk and sees its turns twice on every page. `ResumeFrom` is therefore the first chunk *not*
  covered, and is zero when the transcript is complete. It equals `CoveredUntil` only in the two cases where
  an overlap is unavoidable rather than accidental: a single chunk too large to return whole, and a cap
  falling inside a chunk — where skipping the chunk's later turns would be the worse error. `lumi transcript`
  and `get_transcript` must offer `ResumeFrom`, never `CoveredUntil`.
