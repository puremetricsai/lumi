# internal/store

Single-file SQLite via `github.com/ncruces/go-sqlite3` (pure Go via wazero, no cgo), `MaxOpenConns(1)`
plus WAL, and optional page-level encryption through the `adiantum` VFS. Schema changes are versioned
migrations (`migrations.go`) applied on `Open`, tracked by `user_version`. The `events_fts`
external-content table is trigger-synced, so writes go only to `events`. Timestamps are UTC strings compared
lexicographically — any new time column must go through `FormatCapturedAt` or range filters break.

## Opening

- **Every PRAGMA is applied in the connection-init callback, never with `ExecContext` after `Open`.**
  `database/sql` may discard and re-dial the single connection after a driver error, and a pragma set
  by `Exec` dies with the connection that ran it. That was survivable when the pragmas were journal
  mode and foreign keys. It is not survivable now: the encryption key is one of them, so a silently
  unkeyed replacement connection fails every later query with "file is not a database" — a bug that
  reads as file corruption.
- **The key goes first in that callback, before `fts5.Register`.** FTS5 is a registered extension in
  this driver rather than a compile flag, and registering it reads the schema. On a *new* database
  there is no schema to read, so getting the order wrong passes every first-open test and fails only
  on the second open, as `SQLITE_CANTOPEN` — which reads as a missing file rather than a missing key.
- **The key never appears in the DSN.** adiantum accepts a `hexkey=` URI parameter, and using it would
  put the key into `fmt.Errorf("open sqlite: %w", …)` and from there into `record.log`, `lumi doctor`
  output, and MCP notices. `PRAGMA hexkey` on the open connection is where nothing formats it. The one
  exception is `ConvertTo`'s `VACUUM INTO` target, which has no connection to run a PRAGMA on.
- **`Open` takes a key that is empty or exactly `KeyLen`, and never guesses.** Padding or truncating
  writes a database that nothing can open again.
- **`ConvertTo`'s target URI must name its VFS explicitly.** `VACUUM INTO` is `ATTACH` underneath, and
  ATTACH inherits the *connection's* VFS unless the URI overrides it — so a bare `file:out.db` target
  from an encrypted source opens the output through adiantum with no key, and the reverse mistake
  writes a file that looks fine and cannot be read.
- **`FileIsEncrypted` detects an incomplete conversion; it is not the answer to "is encryption on".**
  That answer is the Keychain's, and `internal/cli` owns it: a fresh install with encryption enabled
  has no database file at all, so reading intent off a missing header would create a plaintext one and
  strand the next keyed writer. A missing file is reported as not encrypted, deliberately.
- **`Stale` exists because a path outlives a file.** `lumi encrypt` replaces the database by rename,
  and a long-lived reader keeps serving the unlinked original: correct rows, no error, and an answer
  the user believes came from an encrypted index. Identity is taken *after* `migrate`, since SQLite
  creates the file lazily and a pre-migrate stat would record "no such file" as the identity forever.
  A stat failure is not staleness — the file is briefly absent mid-rename, the same reason
  `internal/selfexec.Watcher.Changed` refuses to call a stat error an upgrade.
- **`temp_store=memory`** because adiantum encrypts temp files under a random key: keeping them in
  memory skips that work, and no temp file means no plaintext spill either.

## Timestamps

- **`FormatCapturedAt` (`time.go`) is the only way to produce a `captured_at` string.** It is what stops
  `store.Insert` and `capture`'s `ReplaceChunkSegments` key from ever rendering one instant two ways:
  `ChunksMissingSegments` joins those two columns on equality, so a divergence parks every newly recorded
  chunk on the backfill queue forever and reports 0 % `SegmentCoverage` — with no error anywhere.
- **`CapturedAtLayout` is fixed-width because comparison is lexicographic.** `time.RFC3339Nano` trims
  trailing zeros, so `.12Z` (`.120000000`) sorts *after* `.123456789Z` — byte order disagreeing with time
  order is a wrong answer at any range boundary. `TestLexicographicOrderMatchesChronologicalOrder` pins it.
- **A `captured_at` key can never be rebuilt from an instant by picking one layout.** The index holds two
  renderings: rows written before `CapturedAtLayout` existed carry `RFC3339Nano`'s trimmed form, rows
  since carry the fixed-width one. So no equality lookup may mint its key from a `time.Time` — every one
  here (`SegmentsForChunk`, `ReplaceChunkSegments`, `AudioEventsAt`) is handed the stored string, from
  `ChunksMissingSegments` or `AudioChunkTimes` or from the recorder's own stamp. Two successive designs of
  a since-removed whole-chunk reader got this wrong in opposite directions — one truncating, one padding —
  and both failed the same way: the lookup matched nothing and a real two-track chunk read as empty, with
  no error anywhere. Passing the stored bytes through is what removes the question.
- **A range bound covers both renderings; an equality key must be exact.**
  `LowerCapturedAtBound`/`UpperCapturedAtBound` pick the smallest rendering for `>=`/`<` and the largest
  for `<=`, because a fixed-width upper bound silently excludes a legacy row sitting exactly on it —
  `.12Z` sorts *above* `.120000000Z` for the same instant. Every `since`/`until` comparison in this package
  goes through them (`lowerBound`/`upperBound` in `segments.go` delegate); `formatTime` stays exact because
  it also writes `audio_segments`' own columns. This fixes the boundary case only, not the general
  prefix-ordering flaw legacy screen rows carry.
- **`Insert` validates source provenance by *decoding* it, not by checking JSON syntax.** `["Comet"]` is
  valid JSON and invalid provenance; storing it puts a list in the index that nothing can read, and the MCP
  boundary drops an undecodable list silently, so the row reads as though it simply had no source. Decoding
  is also what makes the microphone guard below reachable — a malformed list used to slip past it.
- **`Insert` enforces the microphone invariant, not just the recorder.** A microphone row may not carry an
  attribution other than `unattributed` or name any source application. `internal/capture` decides the
  rule, but a guarantee this size cannot rest on one caller: a second writer would break it silently, and
  an invented speaker outlives its evidence once the WAV is pruned.
- **Existing rows are deliberately not rewritten *by migrations*.** A migration rewriting `captured_at`
  would be an irreversible edit of captured data and would fire the `events_au` FTS trigger once per row,
  for a boundary flaw that predates this layout and has never been observed to bite. Legacy screen rows
  keep their trimmed rendering; the ordering caveat above applies to them and only to them.
- **`UpdateMediaPath` is the one statement here that rewrites an existing row, and its predicate is a
  compare-and-swap.** Every reader of `media_path` resolves the path and *then* opens the file, so an
  unconditional UPDATE would silently clobber a row another writer had already moved. Zero rows affected
  means someone got there first and is **not** an error. It is a CAS on the row and not on the filesystem —
  what keeps two writers off the same *file* is `internal/cli`'s single-instance lock.
  It fires `events_au`, which deletes and reinserts the row's whole FTS entry naming `text`/`app`/`window`
  even though a media-path rewrite changes none of them. That churn is an **accepted, measured** cost
  rather than an oversight — it re-syncs to the same values, `TestUpdateMediaPathLeavesTheRowSearchable`
  pins that, and it is one of the two reasons `lumi compress` runs `VACUUM` last.
- **`Vacuum` reports a contended database as `ErrVacuumBusy`, which callers treat as a skipped step.**
  Busy detection masks to the primary result code because SQLite reports extended codes in the high bits,
  and it matches `SQLITE_BUSY` only: `SQLITE_LOCKED` is contention inside this connection, so reporting it
  as "another process holds the file" would name the wrong cause.

Facts about querying live here, not in callers: `DefaultSearchLimit` (20) / `MaxSearchLimit` (500),
`HasSearchableTerms`, `HasEvents`, `EventByID` (with `ErrEventNotFound`), `ListAttribution`.
`Search` treats an audio chunk's two tracks as the separate rows they are and never merges them; every
filter it applies is a per-row predicate, so one track of a chunk is returned alone whenever only it
matched (see the audio section below).

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
  reached only through `lumi mcp`** (its `match: "any"` and `require_text` parameters). No command sets
  them. The weights matter: without them a one-word `app` or `window` hit outranks a page of screen text.
- **A rule about the store lives in the store.** When a caller needs to know what `Search` will do, export
  the answer and read it rather than restating it. `HasSearchableTerms` exists because `internal/mcp` had
  reimplemented the unexported drop rule — a copy is correct only until the original moves, and the drift
  is invisible to both test suites. `SearchTerms` is exported for exactly that reason and no other:
  `lumi mcp` centres the excerpt it returns on the term that earned a row its place, so it has to know
  which terms those were, and deriving them here rather than re-splitting with `strings.Fields` at the
  boundary is what stops the drop rule being copied out of this file a second time. The terms come back
  **as the user wrote them**, not as FTS5 tokenizes them — `unicode61` folds diacritics the caller's literal
  comparison keeps, and a whitespace-separated term is a quoted phrase that may span several tokens — so a
  caller that fails to find one in raw text must read that as "nothing to centre on", never as "this row
  did not match".
- **`Search` closes both its orderings with `e.id DESC`.** `captured_at` is not unique — a chunk's two
  tracks share one by construction, and 21% of live rows share theirs — so without a tiebreaker the order
  inside a tie group is unspecified, and two identical calls can return different subsets of a group the
  `LIMIT` cuts through. `lumi mcp` pages browse results by handing the oldest `captured_at` on a page back
  as `until`, which needs the same boundary every time. `DESC` so it agrees with `captured_at DESC` and
  keeps the newest row of a tie group first.
- **`Search`'s `app`/`window` filters are unqualified SQL predicates, so an app-shaped query spans both
  row kinds.** Callers that need to mean one of them say so: `ListAttribution` takes a `Kind`, and never
  sums the two into a single count — see `internal/mcp/CLAUDE.md` for what that conflation looked like.
- **`AttributionHealth` stays screen-only**, now because each chunk contributes two rows and an audio
  failure would be reported as a screen problem — not because audio carries no app.

## Schema

- **Schema changes go through `migrations.go`.** Append a new `migration` with the next version; never edit
  shipped SQL. **Bump `CodeSchemaVersion` in the same change** — it is the constant callers compare against
  `SchemaVersion` to notice a database written by a different build, so leaving it behind reports every
  current index as "written by a newer Lumi", the exact opposite of the truth.
  `TestCodeSchemaVersionMatchesTheMigrations` pins it.
- **The skew between a build and a file it opens is invisible in the rows, which is why the version is
  exported.** Every migration here is additive — `ADD COLUMN`, `CREATE INDEX` — so an older build reading a
  newer file finds every column its fixed `eventSelect` names: the query succeeds and the rows come back
  missing only what the newer build added. Nothing errors, so a caller can only report it by comparing the
  two numbers itself. `internal/mcp` does, because a server process an agent holds for a whole session is
  exactly where the two drift apart.
- **A row identity referenced from outside its own table must not depend on a mutable rowid.** `VACUUM`
  renumbers the rowids of any table that does not declare an `INTEGER PRIMARY KEY`, and `lumi compress`
  runs `VACUUM` in the same invocation that writes `media_path` references — so a table added later and
  keyed on an implicit rowid would be renumbered by the very command that had just pointed at it, with no
  error anywhere. `events.id` and `audio_segments.id` are already safe, being `INTEGER PRIMARY KEY` and
  therefore rowid aliases; `TestVacuumPreservesEventIDs` pins it.
- **`origin` is TEXT with no `CHECK`**, so distinguishing machine-side participants later is a value
  change rather than a migration. `silent` already uses that room, and is why the column could gain a
  fourth value without touching the schema.
- **`audio_segments` dies with its event through a trigger, not only a foreign key**, because
  `PRAGMA foreign_keys` is per-connection and a replaced pooled connection would silently stop enforcing
  it.

## Audio tracks

- **The two rows of a chunk are never merged, and nothing here may decide they hold the same sound.**
  They are stored and returned as separate rows, distinguished by `audio_source`; `Search` returns
  whichever of them matched, since every filter it applies is a per-row predicate. A shared `captured_at`
  is a shared 30-second *interval*; the microphone re-records whatever the speakers play, but it also
  records the room, and the two rows routinely carry different speech. `CollapseAudioTracks` used to merge them on the timestamp
  alone, keep the system track, and report an `audio_origin` of `both` that its own comments described as
  speaker bleed — an unverified claim. Reported from a live index: the microphone was carrying an entirely
  separate talk, every word was dropped, and the merged result read as finished. Returning two rows is
  obviously incomplete; returning one that looks whole is worse.
- **Whether one track re-recorded the other is `internal/transcript`'s question, decided once.** It answers
  per segment against word timings, token alignment and an energy envelope, stores the verdict as
  `audio_segments.is_bleed`, and reaches callers through `Transcript` — which excludes a bleed span while
  keeping the room speech that brackets it. A second, cruder copy of that rule in the search path is what
  the deletion removed; the root `CLAUDE.md` rule about two copies of a rule is the general case.
- **The pair still shares one `captured_at` by construction** (one `Audio.Record` call, one `now`), because
  that string is the key `ReplaceChunkSegments` writes segments under and `ChunksMissingSegments` joins on.
  That is now the only thing depending on it.

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
- **`MinConfidence` sorts turns by origin at least as much as by quality, so what it removed is
  reported.** A turn's confidence is the recognizer's own score multiplied by penalties for how uncertain
  its attribution is. On the timed path `internal/transcript` reaches microphone-derived segments alone —
  a timed system segment is machine audio unconditionally and is never marked down (`attribute.go`'s
  crosstalk and ambiguity multipliers). Measured on a live index the two bands do not overlap: internal
  turns scored 0.682–0.983, external turns 0.331–0.592, so `min_confidence: 0.6` removed 100 % of external
  turns and 0 % of internal ones and returned a transcript that looked whole. `ConfidenceFiltered`
  therefore counts removals *by origin* — one total would read as ordinary quality filtering and hide the
  asymmetry that is the entire signal — and `ConfidenceRemovals` renders it so `lumi transcript` and
  `get_transcript` cannot phrase it two ways. Turns excluded by `Origin` are deliberately not counted:
  that omission is one the caller asked for by name. `Turn.Confidence` is the *minimum* over its
  segments, which compounds all of this, since longer turns score lower and external turns are longer.
- **A removal count is bounded by `ResumeFrom`, never by `CoveredUntil`, with the chunk on the boundary
  the single exception.** What it promises is what a page can keep — the turns this page's threshold
  removed from the ground this page covers — and *not* that summing pages totals each removal once,
  because pages do not partition that way: where `ResumeFrom` equals `CoveredUntil` the same chunk is
  deliberately served twice and its turns are returned twice with it.
  Three ways to get this wrong, all of which were live defects one round apart:
  bounding on `CoveredUntil` loses a chunk outright, since a chunk between the last turn *returned* and
  the first turn *deferred* holding nothing but rejected turns is out of range here and never read by the
  next page; bounding on `ResumeFrom` *unconditionally* drops the boundary chunk in the two cases where
  the two bounds are equal (a chunk too large to return whole — where it is the entire page — and a cap
  falling inside a chunk); and accepting the whole page when they are equal counts *later* chunks this
  page never covered, which the next page then reports as well. The rule that survives all three: count
  everything strictly before `ResumeFrom`, plus the chunk exactly on it when `ResumeFrom` has not moved
  past `CoveredUntil`.
- **The asymmetry is not the whole hazard, so nothing here may claim only microphone turns are
  penalised.** `TextPathPenalty` applies to every segment the text path emits, system ones included
  (`attribute.go`'s shared `segment` closure, and the untimed `internalOnly` fallback), so a chunk with no
  usable timings scores 0.48 on both sides and one threshold cuts the machine away too. This is why the
  design reports what a filter removed rather than predicting what it will remove.
- **`CoveredUntil` and `ResumeFrom` are separate fields because they need opposite inclusivity.**
  `SegmentsBetween` is inclusive at both ends, so a caller told to resume at the last chunk covered re-reads
  that whole chunk and sees its turns twice on every page. `ResumeFrom` is therefore the first chunk *not*
  covered, and is zero when the transcript is complete. It equals `CoveredUntil` only in the two cases where
  an overlap is unavoidable rather than accidental: a single chunk too large to return whole, and a cap
  falling inside a chunk — where skipping the chunk's later turns would be the worse error. `lumi transcript`
  and `get_transcript` must offer `ResumeFrom`, never `CoveredUntil`.
