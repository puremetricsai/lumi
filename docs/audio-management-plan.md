# Collapse duplicate microphone/system audio events, keeping track provenance

## Context

Lumi records every 30-second audio chunk **twice** — once from the system output
track and once from the default microphone — as two independent `events` rows
(`internal/capture/recorder.go:336-375`). Speaker audio bleeds into the mic, so
when both tracks capture speech they transcribe the *same* utterance with only
ASR drift between them. An agent searching audio events sees the sentence twice
and summarizes it as two separate moments.

Collapsing the pair is only half the job. **Which track the speech came from is
the signal worth keeping**: system-only means a remote speaker or media playing
on the machine, microphone-only means the user talking in the room, and both
means speaker bleed. A collapse that discards the twin throws that away.

I sampled the live index before designing this. What the data actually says:

- Both frames of a chunk come from **one** `Audio.Record` call and are stamped
  with the same `now` (`recorder.go:338` → `:359`), so their `captured_at`
  strings are **byte-identical** — 250/250 pairs in the 500 most recent audio
  rows. This is a guarantee of the capture code, not a property of the sample.
  Grouping is exact equality; no time-window tolerance is needed, and adding one
  would risk merging genuinely distinct chunks.
- A system row exists at *every* timestamp, empty or not. A rule phrased as
  "keep mic events with no matching system event" would drop everything; it has
  to be conditioned on the partner having **non-blank** text.
- When both carry text they are near-duplicates, and the system side is the
  better transcript (`"$52 million"` vs the mic's `"$52000000"`). Only 1 of 7
  observed collisions was byte-identical.

**On the sample's emptiness — do not design around it.** In the window I
sampled the system track was usually blank (7 of ~886 text-bearing rows). That
is an artifact of a quiet period, *not* a property of the index: normal use is
system-audio-heavy, so `audio_origin: "both"` should be expected as the common
case, not the exception. Nothing in the design below may lean on blank system
rows being frequent, and the verification step must not assert it. The load-
bearing consequence runs the other way: with heavy system audio, **system
precedence fires constantly and mic transcripts are merged away on most
chunks**. That is the intended behavior, and it is why `audio_tracks` carrying
the dropped mic row's `id` and `text_length` is not a nicety — under the
expected workload it is the only route back to the user's own voice on a chunk
where the speakers were also playing.

Outcome: one row per audio chunk by default, each carrying where its audio came
from and what was merged into it.

## Approach

A pure, exported function in `internal/store`, called by the MCP handler after
`Search` returns. Deliberately **not** a SQL-level filter: the collapse must
choose among rows that actually *matched*. With an FTS query like `52000000`
only the mic row matches; a SQL `NOT EXISTS` filter would consult the
non-matching system row, judge it "better", and drop the only hit — returning
zero results.

This follows existing precedent: `MatchAny`, `RequireText`, and
`HasSearchableTerms` are all store-level facts reached only through `lumi mcp`,
per CLAUDE.md's "a rule about the store lives in the store".

### 1. `internal/store` — the collapse rule and the provenance it keeps

New file `internal/store/audio.go`:

```go
// AudioTrack is one row of a collapsed audio chunk, including the survivor.
type AudioTrack struct {
    ID          int64
    AudioSource string // "system" or "microphone"
    // TextLength is the transcript's rune count after TrimSpace, so 0
    // unambiguously means this track recorded silence. This is a different
    // question from EventRecord.TextLength, which pairs with Truncated and
    // counts the untrimmed text.
    TextLength int
    MediaPath  string
}

// AudioChunk is what one survivor was collapsed from.
type AudioChunk struct {
    // Origin is "system", "microphone", "both", or "silent" — which tracks
    // carried speech, not which one won. "both" is speaker bleed. Nothing in
    // the data distinguishes a remote speaker from media playback; both are
    // "system", and the docs must not claim otherwise.
    Origin string
    Tracks []AudioTrack // every row in the group, survivor first
}

// CollapseAudioTracks returns the surviving events in input order, plus each
// survivor's provenance keyed by its id.
func CollapseAudioTracks(events []Event) (kept []Event, chunks map[int64]AudioChunk)
```

- Group only `KindAudio` events, keyed on
  `e.CapturedAt.UTC().Format(time.RFC3339Nano)` — the same format `Insert`
  writes, so the key matches storage exactly. `KindScreen` events pass through
  untouched even when they share a timestamp, and get no entry in `chunks`.
- **System audio takes precedence on every real duplicate.** Whenever both rows
  of a chunk carry speech — the only case where anything is actually duplicated
  — the **system** row survives and the microphone row is merged into it, no
  matter which transcript is longer. The mic is re-recording what the speakers
  are playing, so the system track is the original and measurably the cleaner
  transcript (`"$52 million"` vs the mic's `"$52000000"`).
- Winner within a group is the lexicographic maximum of
  `(hasText, isSystem, runeLen(text), -id)`:
  1. **`hasText`** — `strings.TrimSpace(Text) != ""`. Non-blank always beats
     blank. This key sits above `isSystem` on purpose, and the reason does not
     depend on how often the system track is empty: a *silent* row is not a
     duplicate of anything, so letting it win would not be deduplication, it
     would be deleting a transcript. System precedence applies to duplicates,
     not to silence — at any rate of occurrence.
  2. **`isSystem`** — `AudioSource == "system"`. This is the precedence rule
     above; because it outranks text length, system wins **unconditionally**
     among rows that both have text.
  3. **`runeLen(text)`**, then **lowest id** — deterministic tie-breaks. They
     are only reachable between two same-source rows, so they can never
     override key 2.
- When system wins over a longer mic transcript, nothing is lost: the mic
  track's `TextLength` and `ID` ride along in `Tracks`, so an agent can see the
  mic captured more and call `get_event` for it.
- `Origin` is derived from which tracks have `TextLength > 0`, independently of
  who won. A mic-only group is `"microphone"` even though the blank system row
  was dropped; an all-blank group is `"silent"`.
- The survivor takes the **earliest position** its group occupied in the input,
  so bm25 relevance ordering is preserved (the group keeps the best rank any
  member earned).
- `chunks` gets an entry for every audio group, single-row groups included, so
  `Origin` is always available. Whether a one-entry `Tracks` is worth putting on
  the wire is the MCP boundary's call (below).

Reuse `strings.TrimSpace`; do not restate the SQL trim set from `Search`'s
`RequireText` clause (`store.go:178`).

**Provenance must describe the chunk, not the result set.** Deriving `Origin`
from matched rows alone gets it wrong in exactly the case that motivated keeping
the SQL filter out: a `52000000` query matches only the mic row, so the group
has one member and `Origin` reads `"microphone"` — while that chunk's system
track demonstrably has speech. The fix is a second, narrow lookup:

```go
// AudioTracksAt returns every audio row at each of the given capture times,
// keyed by the RFC3339Nano timestamp, regardless of what a search matched.
func (s *Store) AudioTracksAt(ctx context.Context, times []string) (map[string][]AudioTrack, error)
```

One `SELECT id, audio_source, text, media_path FROM events WHERE kind = 'audio'
AND captured_at IN (…)`, batched with the existing `deleteBatchSize` (900)
bound so a 500-row page can never exceed SQLite's variable limit. This splits
the two jobs cleanly: **collapse decides among matched rows** (so the mic hit
survives), **provenance reports the whole chunk** (so `Origin` is truthful).

A consequence worth stating rather than hiding: on that query the result is
`audio_source: "microphone"` with `audio_origin: "both"`. Those differing is
correct and informative — "the mic transcript is what matched your terms, and
the speakers were also playing" — and the system row is reachable by id in
`audio_tracks`. Note this is the one path where the survivor is *not* the system
row despite `origin: "both"`; system precedence orders the collapse, and it
cannot promote a row the query never matched.

### 2. `internal/mcp` — the tool parameter, default on

`internal/mcp/tools.go`:

- `searchEventsInput` gains
  `CollapseAudioTracks *bool json:"collapse_audio_tracks,omitempty"` — a pointer
  so nil means **true**, matching the `MaxTextChars *int` precedent
  (`tools.go:115`). The schema description states the default and that `false`
  returns both tracks unmerged.
- `EventRecord` gains, for audio events only:
  - `AudioOrigin string json:"audio_origin,omitempty"` — `"system"` |
    `"microphone"` | `"both"` | `"silent"`.
  - `AudioTracks []AudioTrackRecord json:"audio_tracks,omitempty"` — every row
    of the chunk per `AudioTracksAt`, emitted **only when the chunk held more
    than one row**. For a genuinely lone audio row every field would restate the
    top-level `id`/`audio_source`/`media_path`, and `audio_origin` already
    carries the answer.
  - `AudioTrackRecord` mirrors `store.AudioTrack` with
    `id` / `audio_source` / `text_length` / `media_path`. It follows the
    package's media invariant: `media_path` is a string the user can open, never
    bytes, and it is the only way to reach the dropped chunk's WAV at all.
  - The dropped track's **text** is not inlined — `text_length` says whether it
    held speech and `id` lets the agent call `get_event` for it, which is
    exactly how truncation already works here.

Resulting shape:

```json
{
  "id": 28206, "kind": "audio",
  "audio_source": "system", "audio_origin": "both",
  "text": "Yo, what's up? It's Mustafa...",
  "audio_tracks": [
    {"id": 28206, "audio_source": "system",     "text_length": 96, "media_path": ".../…-system.wav"},
    {"id": 28207, "audio_source": "microphone", "text_length": 97, "media_path": ".../…-microphone.wav"}
  ]
}
```

- **Over-fetch so a page stays full.** A chunk yields at most two rows, so
  `fetchLimit = min(2*opts.Limit, store.MaxSearchLimit)` guarantees at least
  `limit` survivors. Query with `fetchLimit`, collapse, then trim to
  `opts.Limit`.
- **Keep the cap notice honest — this is the sharp edge of the over-fetch.**
  Today the condition is `len(out.Events) == opts.Limit` (`tools.go:177`).
  Over-fetching breaks any condition phrased on the *raw* fetch:
  `TestSearchEventsCapNoticeIsAnElseBranch` seeds 3 screen events and asks for
  `Limit: 3`, so `fetchLimit` becomes 6, `len(rawEvents)` is 3, and a
  `len(rawEvents) == fetchLimit` test evaluates `3 == 6` → false, silently
  dropping the notice the full-page assertion at `tools_test.go:546` requires.
  The correct condition is computed on the **post-collapse page**, with the raw
  fetch only as a second disjunct:

  ```go
  capped := len(out.Events) == opts.Limit || len(rawEvents) == fetchLimit
  ```

  The first disjunct preserves today's contract verbatim (a page that exactly
  fills the requested limit is ambiguous, so say so). The second catches the new
  case: 40 rows fetched, collapsed to 15, `limit` 20 — the page is short but
  more results exist. When collapsing is off, `fetchLimit == opts.Limit` and the
  two disjuncts coincide, so behavior is bit-for-bit unchanged.
- **Compose notices.** `Notice` becomes a `"; "`-join of non-empty parts, with a
  collapse part added only when something was actually collapsed — e.g.
  *"collapsed N duplicate audio events: the microphone and system tracks record
  the same 30-second chunk; each result lists its merged tracks in audio_tracks
  and which carried speech in audio_origin, and collapse_audio_tracks: false
  returns them unmerged."* It must not contain the word `capped`.

`TestSearchEventsCapNoticeIsAnElseBranch` (`tools_test.go:529`) has **three**
sub-assertions, and all three must be re-checked against the change — it seeds
only screen events, so `collapsed` is 0 throughout and no collapse part is ever
joined in:

| sub-assertion | line | why it still holds |
|---|---|---|
| full page (`Limit: 3`, 3 events) contains `capped` | 546 | first disjunct: `3 == 3` |
| partial page (`Limit: 10`, 3 events) notice is `""` | 555 | `3 != 10` and `3 != 20`; nothing collapsed, so no part is joined |
| empty result says `matched`, not `capped` | 564 | `0 != 20` and `0 != 40`; the collapse wording must not contain the substring `capped` |

`TestSearchEventsClampsLimitToMax` (`tools_test.go:572`) must also be re-checked:
`Limit: 100000` clamps to 500, `fetchLimit` is `min(1000, 500) == 500`,
`len(rawEvents)` is 3 — so `3 != 500` on both disjuncts and the page correctly
reads as partial.

### 3. `internal/cli` — opt-in only

`searchCommand` (`internal/cli/root.go:166`) gains `--collapse-audio`
(default **false**), applied after `s.Search`.

- Human output: `printEvents` shows `origin=both` and the merged track ids on
  collapsed audio rows.
- `--json`: the bare `[]store.Event` array is what a default export must stay,
  so it is unchanged when the flag is off. With `--collapse-audio` **and**
  `--json`, emit `{"events": [...], "audio_chunks": [...]}` instead — an
  explicitly requested different view may have a different shape, and silently
  dropping the provenance from the JSON path would be the worse trade.

`searchOptions` is untouched; collapsing is not a `SearchOptions` field.

### 4. Documentation

- CLAUDE.md `internal/store` section: `CollapseAudioTracks` sits beside
  `HasSearchableTerms` as a store-owned rule the MCP boundary reads.
- CLAUDE.md `internal/mcp` section: `collapse_audio_tracks` defaults on;
  `audio_origin`/`audio_tracks` keep the merge visible and reversible.
- New invariant bullet covering all four of: **system audio outranks the
  microphone on every duplicate, but never outranks a non-empty transcript with
  silence** — the ordering of those two keys is the whole rule, and inverting
  either one is a silent data-loss bug; the pair shares one `captured_at` **by
  construction** (not by coincidence); the collapse runs over *matched* rows and
  must never become a SQL pre-filter (spell out the `52000000` failure); and a
  collapse must never destroy which track the speech came from, because
  `audio_origin` is the only thing separating a remote speaker from the user's
  own voice.

## Files

| File | Change |
|---|---|
| `internal/store/audio.go` | **new** — `AudioTrack`, `AudioChunk`, `CollapseAudioTracks`, `AudioTracksAt` |
| `internal/store/audio_test.go` | **new** — rule + origin tests |
| `internal/mcp/tools.go` | input/output fields, over-fetch, notice composition |
| `internal/mcp/tools_test.go` | boundary tests |
| `internal/cli/root.go` | `--collapse-audio`, printing, JSON wrapper |
| `internal/cli/root_test.go` | flag tests |
| `internal/capture/recorder_test.go` | pin the shared-`captured_at` guarantee |
| `CLAUDE.md` | store/mcp sections + invariant |

## Tests

`internal/store/audio_test.go`
- **`TestSystemAudioWinsEveryDuplicate`** — table over both orderings, system
  text shorter / longer / equal to the mic's, and both id orderings: the
  survivor is the system row in every case. This is the test that fails if
  anyone ever reorders `isSystem` below `runeLen`.
- blank system + mic with text → mic survives, `Origin == "microphone"`, both
  tracks present in `Tracks` with the system track at `TextLength: 0` — the
  bound on system precedence
- both have text → **system** survives, `Origin == "both"`, mic track retained
- whitespace-only system text → `TextLength: 0` and `Origin == "microphone"`,
  pinning that the track length is trimmed
- both blank → one survivor, `Origin == "silent"`
- mic text `"."` vs blank system → mic survives (matches real rows in the index)
- a screen event at the same `captured_at` is never grouped and gets no chunk
- unpaired audio event passes through with a one-entry `Tracks` and a real
  `Origin`
- input order preserved; survivor sits at its group's earliest index
- `AudioTracksAt` returns both rows of a chunk when handed one timestamp, keys
  by the exact RFC3339Nano string, ignores screen rows, and batches past 900
  timestamps without tripping the variable limit

`internal/mcp/tools_test.go`
- default (parameter omitted) collapses, sets `audio_origin` and a two-entry
  `audio_tracks`, and the notice names the escape hatch
- a lone audio row carries `audio_origin` but **no** `audio_tracks`
- `collapse_audio_tracks: false` returns both rows, no collapse notice, and no
  `audio_origin`/`audio_tracks`
- the dropped track's id resolves through `get_event` to its full text
- **`TestAudioOriginDescribesTheChunkNotTheMatch`** — seed a pair where only the
  mic text matches the query and the system row has different speech; the single
  result must carry `audio_source: "microphone"` **and** `audio_origin: "both"`,
  with the unmatched system row present in `audio_tracks`. This is the test that
  fails if provenance is ever derived from the matched set alone.
- over-fetch: 20 pairs with `limit: 10` returns 10 survivors, not 5, and the cap
  notice fires
- **cap notice under over-fetch** — a new test alongside the existing one,
  seeding *audio pairs* rather than screen events, covering the two disjuncts
  separately: (a) exactly `limit` survivors → capped; (b) a short page whose raw
  fetch hit `fetchLimit` → still capped; (c) fewer rows than `fetchLimit` and
  fewer survivors than `limit` → not capped
- existing empty / no-match / full-page / partial-page notice assertions and
  `TestSearchEventsClampsLimitToMax` still pass unmodified

`internal/capture/recorder_test.go`
- the multi-source fake's mic and system events carry an identical `CapturedAt`
  — what `CollapseAudioTracks` groups on, currently only implicit in
  `recorder.go`

## Verification

1. `task check` — fmt → vet → full suite.
2. Focused: `task speech && go test ./internal/store ./internal/mcp ./internal/cli -run 'Collapse|Audio|Notice' -v`
3. End-to-end against the real index:
   - `task build`
   - `./lumi search --type audio --limit 20` vs the same with
     `--collapse-audio` — up to half as many rows, since a system row exists at
     every timestamp. Make no assumption about which `origin` dominates; the mix
     depends entirely on how much system audio was playing.
   - **Exercise the system-audio-heavy path deliberately**, since the sampled
     window under-represents it: `./lumi record start --foreground --duration
     90s` while playing speech through the speakers, then search that window.
     Expect `audio_origin: "both"` on those chunks, the **system** row as the
     survivor, and the mic row present in `audio_tracks` with a non-zero
     `text_length`. This is the case the design is actually tuned for.
   - Restart the MCP client, call `search_events` with `kind: "audio",
     limit: 20`: expect ~20 distinct chunks, each with `audio_origin`, and a
     notice naming the collapsed count. Repeat with
     `collapse_audio_tracks: false` and confirm the pairs reappear unannotated.
   - Query `search_events` around `2026-07-28T23:12:58` (the Fish Audio pair —
     system id 28127, mic id 28128, the one byte-identical collision) and
     confirm a single result whose `id` is **28127**, with
     `audio_source: "system"`, `audio_origin: "both"`, and both ids in
     `audio_tracks`. Repeat at `23:13:29` (system 28140 / mic 28141), where the
     two transcripts differ — the survivor must still be the system row.
   - Query `query: "52000000", kind: "audio"` (the mic-only ASR spelling near
     `2026-07-28T23:13:29`) and confirm the mic row (28141) is still returned —
     the case a SQL pre-filter would have lost — **and** that it reports
     `audio_origin: "both"` with system row 28140 listed in `audio_tracks`,
     proving provenance describes the chunk rather than the match.
4. `./lumi search --type audio --json` (no flag) must be byte-identical to its
   output before the change, confirming the default export stays faithful.
