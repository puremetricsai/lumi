# CLAUDE.md

Guidance for Claude Code (claude.ai/code) when working in this repository.

## What Lumi is

A local-first work-memory CLI for Apple Silicon Macs: continuously capture all displays plus system and
microphone audio, extract screen text locally (full-display Apple Vision OCR, with Accessibility for
focused-app attribution), transcribe audio with in-process Apple SpeechAnalyzer, store media on disk, index
text in SQLite FTS5, and query it. Inspired by screenpipe but deliberately narrower — no GUI, server, or
plugins.

## Commands

```sh
task build                                  # Swift bridge (task speech), then go build
task test                                   # full suite
task check                                  # fmt → vet → test; the verification command
task speech && go test ./internal/store -run TestSearch -v   # single test
task test:native                            # permission-gated native smoke test
task mcp                                    # hand-fed MCP handshake smoke test
```

Run `task build`/`task test`, never raw `go build`/`go test` — nothing links without `liblumispeech.a`,
which `task speech` produces.

`internal/capture/recorder_test.go` runs the whole capture→store→search pipeline with fake
`ScreenSource`/`ContextExtractor`/`TextExtractor`/`AudioSource`/`SpeechTranscriber` implementations, needing
no permissions or external binaries. Prefer extending it over invoking real frameworks.

The CLI refuses to run on anything but `darwin/arm64` (`platform.Validate` in `PersistentPreRunE`); native
microphone capture needs macOS 26+. `./lumi doctor` checks platform, permissions, speech assets, and the
data directory. Bounded manual smoke test: `./lumi record start --foreground --no-audio --duration 10s`.

## Architecture

```
ScreenCaptureKit displays ─→ Vision OCR (full screen) ─┐
                         └─→ Accessibility (attribution) ├─→ events + events_fts ─→ search
ScreenCaptureKit system + microphone ─→ WAV ─→ SpeechAnalyzer (in-process) ─┘
```

Data flows one way: `internal/cli` wires concrete processors into a `capture.Recorder`, the recorder writes
`store.Event` rows, and `search` reads them back. Never the reverse.

**`internal/macosnative`** — cgo/Objective-C bridge to ScreenCaptureKit, Accessibility, Apple Vision,
AVFoundation WAV writing, and permission preflight. Has a non-macOS stub.

**`internal/capture`** — `Recorder` runs independent screen and audio goroutines until the context is
cancelled, with native processors behind small interfaces. Displays are re-enumerated each interval for
hotplug. Everything runs in-process — no subprocesses. One focused-window snapshot per tick is stamped onto
**every** display's frame, so on a multi-display setup an `app` filter returns frames from displays where
that app was not visible: attribution answers "what was the user working in", not "what is shown in this
image". Per-display attribution is deliberately not done.

`ScreenContext` degrades rather than failing: `Snapshot` errors only when nothing at all could be read, and
a failed Accessibility read arrives as a *populated* context carrying `AccessibilityError`. `Degraded()`
("something was lost") and `Unattributed()` ("no app name at all") are different questions — conflating
them fires warnings on routine operation. `Trusted` is a `*bool` so "revoked" and "never sampled" stay
distinct. Sustained degradation escalates on **elapsed time**, not tick count, since `--interval` is a flag.

**`internal/store`** — single-file SQLite via `modernc.org/sqlite` (pure Go, no cgo), `MaxOpenConns(1)` plus
WAL. Schema changes are versioned migrations (`migrations.go`) applied on `Open`, tracked by `user_version`.
The `events_fts` external-content table is trigger-synced, so writes go only to `events`. Timestamps are
RFC3339Nano UTC strings compared lexicographically — any new time column must match or range filters break.

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

`AttributionHealth` backs `lumi doctor`; its `LastAttributed` is a scalar
subquery over *all* history, because scoping it to the window would blank the field exactly when the outage
exceeds the window. The event column list lives in one `eventSelect` const shared by `Expired`,
`AllEvents`, and `EventByID`.

**`internal/retention`** — age- and size-based pruning behind `lumi prune`. Age runs before size; size walks
oldest-first. `Options.All` enumerates every row via `store.AllEvents` (unbounded, so a far-future
`captured_at` is never skipped), deletes rows-before-files, then sweeps `Options.MediaDirs` for orphans.
Only `All` sweeps directories. No background scheduler.

**`internal/cli`** — Cobra commands (`record start`/`status`/`stop`, `search`, `mcp`, `prune`, `doctor`,
`permissions`, `native-smoke`, `transcribe`, `transcript`, `version`). `record start` detaches to the
background by default
(`--foreground` keeps it inline) as a re-exec tracked by a JSON state file and log under the data dir
(`record_daemon.go`); `record stop` sends SIGTERM and waits for graceful shutdown. `search` offers exact
case-insensitive app filtering, case-insensitive window-substring filtering, `--type all|screen|audio`, and
`--collapse-audio`. `transcript` keeps its own `RunE` for the same reason `mcp` does: reading a transcript
is the useful thing, and `backfill` is maintenance hanging off it. `permissions --request` invokes native
TCC flows — never add `tccutil reset` as a side effect.

`mcp` opens the store through the same `openStore` as every other command (an agent launches it with a bare
environment, so `--data-dir`/`LUMI_HOME` must be the whole story) and treats a cancelled context as a clean
exit. It keeps its own `RunE` rather than becoming a bare parent that prints help — this command *is* the
server, and a help dump would land on the JSON-RPC stream. `mcp setup` is its only subcommand; there is
deliberately no `mcp start`, HTTP transport, or daemon. `resolveLumiBinary`, `verifyLumiBinary`, and
`newSetupTargets` are package vars purely as test seams — without them a test run would rewrite the
developer's own Claude config.

**`internal/mcp`** — stdio MCP server on `github.com/modelcontextprotocol/go-sdk` (pinned v1.6.1; import
aliased as `sdk`, since our package is also named `mcp`). `Serve(ctx, *store.Store, Options)` registers
four read-only tools — `search_events`, `get_event`, `list_apps`, `get_transcript` — and runs until stdin
closes or the context is cancelled. It depends on `internal/store` and nothing else of Lumi's.
`search_events` truncates text per event and reports `truncated` plus true `text_length`, collapses audio
duplicates by default (`collapse_audio_tracks` is a `*bool`, nil meaning on), and over-fetches
(`min(2*limit, MaxSearchLimit)`) then trims so a collapsed page stays full; `get_event` is the
untruncated escape hatch and the only tool returning metadata. `get_transcript` reads audio as one
ordered conversation with the machine's own speech deduplicated, and its `confidence` and
`order_confidence` carry no `omitempty` for the same reason `truncated` does not: a doubtful label must
never be something an agent infers from a missing key.
Validation and store failures come back as tool results with `isError`, never JSON-RPC
protocol errors.

An agent cannot see the shape of the index, so this package is explicit about ambiguity: a `query` with no
letters or digits is a tool error rather than a fall-through to `Search`'s no-MATCH behavior, an empty
result says whether the index itself is empty (naming `Options.DatabasePath`) or the filters matched
nothing, a full page says results were capped, and `search_events` clamps `limit` itself so the number it
reports is the one enforced.

**`internal/mcpsetup`** — registers `lumi mcp` with installed MCP clients, backing `lumi mcp setup`. `Spec`
carries a name, binary path, and argv; `internal/cli` supplies all three. It has no native or third-party
dependencies. The three targets are asymmetric because what each client will tell us differs: Claude Code is
*read* from `~/.claude.json` and *written* only via `claude mcp add`/`remove`; Claude Desktop has no CLI and
is read-modify-written in place; Codex is mediated by `codex mcp get --json`/`add`/`remove` in both
directions, so `~/.codex/config.toml` is never touched. Every external command goes through the `Runner`
seam and every path is an injectable field, so tests need no client present.

**`internal/vocabulary`** — owns the custom vocabulary file's format, cache, and cap, for the same reason
`HasSearchableTerms` lives in `internal/store`: a caller that needs the rule reads it rather than restating
it. `Loader.Load()` caches a successful read against device, inode, mtime, size, **and mode**, but never
caches a failure, so a `chmod`-broken file is retried every call while a routine re-read costs one `stat`.
`Snapshot.Terms` is always usable and `Err` is advisory, mirroring how `ScreenContext` reports a degraded
Accessibility read rather than failing outright. `MaxTerms` (100) caps the list in file order; terms past it
are dropped and counted in `Snapshot.Dropped`, never silently truncated. No native or third-party
dependency, so every rule is testable without permissions or `liblumispeech.a`.

**`internal/transcript`** — decides where captured sound came from and assembles it into one ordered
transcript, backing `lumi transcript` and the `get_transcript` MCP tool. Pure: no database, no cgo, no
filesystem, so every rule is testable without permissions. `Attribute` never returns an error — missing
data lowers labels and confidence instead. It has a timed path (word-level overlap plus token alignment)
and a text-only path for chunks whose WAVs are gone, and `AssembleTurns` merges segments into turns.
Named for what it produces: nothing here clusters voices, and the labels name provenance rather than
people. The two `Segment` types — this one and `store.Segment` — shadow each other the way
`internal/mcp`'s `AttributionRecord` shadows `store.Attribution`; `internal/store` imports this package,
never the reverse.

**`internal/wav`** — reads the mono 16-bit PCM WAVs Lumi captures and measures their energy. Exists
because the recognizer returns nothing both for a silent track and for one carrying audio it could not
transcribe, and those need opposite conclusions. `ReadMono16` walks RIFF chunks generically and accepts
fmt tag `0xFFFE` with a PCM SubFormat GUID, because Lumi's own writer emits `RIFF/WAVE` → `JUNK` → `fmt `
→ `FLLR` → `data` — a reader assuming `fmt ` at offset 12 fails on every file Lumi has recorded.

**`internal/config`** — resolves `Paths` from `--data-dir`, else `LUMI_HOME`, else `~/Library/Application
Support/Lumi`; directories created 0700.

## Invariants worth preserving

### Capture

- **Never lose captured media.** If Accessibility, Vision, comparison, or transcription fails after a file
  was written, preserve and index the event with diagnostic metadata. Don't convert downstream failures
  into early returns that drop the file.
- **Deduplicate per display, not globally.** `FrameComparer` uses SHA-256 as an exact fast path and a
  sampled RGB histogram for near-duplicates; active input raises sensitivity. Two retention deadlines:
  `MaxSilence` (10s) when bytes *changed* but scored similar (video, advancing slides), and `ExactSilence`
  (5min) when bytes are identical, so a frozen screen leaves a bounded presence marker instead of
  re-indexing the same JPEG. `ExactSilence` is clamped up to `MaxSilence`.
- **Capture retries without discarding completed work.** Screen failures retry on the next interval; audio
  failures retry after one second. Media returned during cancellation gets a short cancellation-free window
  for insertion.
- **Preserve provenance.** `text_source`, `display_id`, and `audio_source` are first-class event columns
  (migration 3) and appear in JSON exports. In metadata, `app_source` and `attribution_source` answer
  different questions — which source named the *app*, and which supplied the *window title* — and routinely
  differ. Merging them would change what `attribution_source` means for every indexed row.

### Speech vocabulary

- **A failed vocabulary read is never cached.** `chmod` changes neither size nor mtime, so a stat-keyed
  cache could never observe recovery, and the recorder would transcribe without vocabulary indefinitely
  while `doctor` — a fresh process with a cold cache — read the same file successfully and called it
  healthy. Found as a defect in the design's first draft, before any code existed.
- **`Snapshot.Changed` compares the resulting snapshot, not the stat key.** That is what lets the
  unconditional retry above cost one log line per failure instead of one per chunk.
- **Absence is `Exists`, never `Err`**, because whether a missing file is acceptable is the caller's policy:
  routine for the recorder, fatal for an explicit `lumi transcribe --vocabulary` path. Gating that guard on
  `Err` alone lets a typo'd path silently produce a baseline transcript — the second defect the design's
  adversarial review found, since a silently-successful baseline is precisely the failure this exception
  exists to prevent.
- **`MaxTerms` is a real cap, not hygiene**: contextual biasing is a budget, and an oversized list dilutes
  every term while inviting false substitutions.

### Attribution

- **The frontmost pid resolves Accessibility → window list → `NSWorkspace`, and `NSWorkspace` coming last is
  the point.** `frontmostApplication` is backed by activation state maintained through run-loop
  notifications. The recorder is a detached daemon (`Setsid`) that runs no loop, so the value freezes at
  whatever was frontmost at start — the launching terminal — while the window title, read against that
  stale pid, keeps advancing. `runningApplications`/`isActive` freeze identically; neither may lead.
- **`LumiActivationPID` leads because it answers *activation*, not visibility.** An app with every window
  minimized is still what the user is working in. Two stages: system-wide AX
  `kAXFocusedApplicationAttribute` first (unreliable per-app, and retrying is *not* the remedy — identical
  errors), then `LumiFrontmostValidatedPID`, which walks window-list owners front-to-back asking each over
  *per-application* AX whether it is frontmost (`kAXFrontmostAttribute`). The second stage fixed
  misattribution at app-switch boundaries, where the top layer-0 window is still the previous app while
  activation has moved. `app_source` distinguishes the stages (`accessibility` vs `accessibility_frontmost`).
- **Candidate eligibility must stay filtered to `NSApplicationActivationPolicyRegular`.**
  `LumiFrontmostCandidates` lists on-screen window owners front-to-back (bounded to 8), then dock-visible
  `NSWorkspace.runningApplications` owning no window — the windowless-app case that would otherwise be
  unaskable. Widening the filter to every running application is the obvious generalisation and is wrong:
  background agents answer `kAXFrontmost` affirmatively, and an unfiltered walk was measured attributing
  frames to Notification Center. Only *membership* is read from `runningApplications`, never `isActive`.
- **A name is borrowed from `NSWorkspace` only when both sources mean the same pid.** Borrowing across
  differing pids names one app while reading another's title — the original bug. Where the window list
  names a pid nothing can name an app for, `LumiResolveFrontmostLive` falls back to the `NSWorkspace` pair
  **wholesale, pid included**: a stale-but-consistent pair beats a mismatched one.
- **Never lose attribution to a permission failure.** `lumi_accessibility_snapshot_json` gathers everything
  needing no AX grant — frontmost app name, input activity, `AXIsProcessTrusted()` — *before* the first AX
  call, and falls back to `CGWindowListCopyWindowInfo` for the title. Returning `NULL` there once cost 7,705
  of 12,104 events their app; it is reserved for genuine total failure, *both* sources failing, never one.
  Trust is sampled per tick, not at startup, so `doctor` reports observed attribution from the index
  alongside permission status — and never opens the store through `openStore`, which would create a
  mistyped `--data-dir` and call the empty result healthy.
- **Never assert one native frontmost read against another.** `Accessibility` and `FrontmostDiagnostic` are
  separate calls; a focus change between them makes them differ legitimately, so `native-smoke` reports the
  diagnostic and never compares it. Relatedly, pure resolvers are exposed as `*_json` entry points (as is
  `lumi_hid_access_name`) because asserting the live resolution passes vacuously in any foreground process,
  so it would fail only in the daemon, where nothing is asserting.
- **Report `denied` separately from `not_determined` wherever macOS lets you**, since they need opposite
  remedies. Input Monitoring uses `IOHIDCheckAccess`; Microphone and Speech Recognition carry the
  distinction already. Screen Recording and Accessibility stay `denied_or_not_determined` on purpose —
  splitting them needs Full Disk Access or raises a prompt as a side effect. Over SSH no status call can
  prompt at all, so `--request` is a no-op.

### Audio origin

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
- **A silent internal track and an untranscribed one are different findings**, needing opposite
  conclusions: silence means the microphone is confidently the room's, audio-without-words means a
  re-recording of unseen speech is possible and the honest answer is `unknown`.
- **Overlap comes from word intervals, never `result.range`.** A recognizer result extends to its
  finalization boundary; one measured reporting 3000–9660 ms held a single word at 3000–3660 ms. The wider
  span drags neighbouring room speech into an overlap it never had.
- **The external transcript is the ordered spine; the internal transcript is the labelling oracle.** The
  microphone WAV holds everything audible in one recognizer pass, so its reading order *is* conversational
  order. Treating the two as peer tracks to be merged by time is what makes ordering look unrecoverable
  when it is not — only absolute time is lost without timestamps, never sequence.
- **`events.text` for an audio row is the recognizer's results concatenated with no separator.** Runs
  carry their own leading whitespace. Segments derive from it, never the reverse, and joining with a space
  would silently re-index the corpus.
- **A segment-write failure never costs an event.** Rows insert first; attribution is a second pass whose
  retry mechanism is the backfill's derived work queue, so no retry loop belongs in the recorder.
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
- **A chunk holding no words still writes a marker row** (`origin` = `silent`, no text, `MethodSilent`).
  The work queue is derived, so a verdict that writes nothing is indistinguishable from one never reached —
  and since silence is the common case, every such chunk would stay queued forever, be re-derived on every
  backfill, and be counted as a permanent coverage hole while the transcript advised a backfill that could
  not change it. `silent` is deliberately not `unknown`: unknown *warns* that hidden machine speech may be
  present, which is the opposite claim.
- **A transcript filters turns, never the segments it assembles them from.** Removing one origin before
  assembly hides the interjection that separated two replies, so `AssembleTurns` reads them as adjacent and
  merges them — inventing continuity and deleting the boundary. Turns are single-origin by construction, so
  filtering afterwards selects without reshaping.
- **Coverage counts describe the range the turns reach, not the range requested.** They exist to reveal a
  partial transcript; counting the whole window while the text stopped early makes them corroborate the
  omission instead. A transcript also never ends mid-chunk, which is what lets `CoveredUntil` be a resume
  point rather than an estimate.
- **Turn continuation across a chunk boundary is structural, not gap-based.** Inter-chunk dead air is
  unobservable and measures 0.4–2.9 s, overlapping a natural pause; what *is* observable is that 3,028 of
  3,040 consecutive chunks were captured 30–33 s apart, with the next outliers at 39 s, 48 s, and 91 s.
  35 s separates "the next chunk" from "the recorder stopped", and a turn must never bridge a stop.
- **`origin` is TEXT with no `CHECK`**, so distinguishing machine-side participants later is a value
  change rather than a migration. `silent` already uses that room, and is why the column could gain a
  fourth value without touching the schema.
- **`audio_segments` dies with its event through a trigger, not only a foreign key**, because
  `PRAGMA foreign_keys` is per-connection and a replaced pooled connection would silently stop enforcing
  it.
- **Real captured conversation never becomes a test fixture.** The calibration and real-pair harnesses
  read their input from a path in the environment and skip without it. The measured numbers belong in the
  repository; the words do not.

### Store and search

- **FTS input must go through `ftsExpression`** (`internal/store/query.go`). It quotes each term and joins
  with `AND`/`OR` per `SearchOptions.Match`; the quoting is what stops raw user text being read as FTS5
  syntax. `MatchAll` is the zero value, so `lumi search` stays conjunctive. Terms with no letters or digits
  are dropped; an empty expression means "run no FTS query at all" — an empty MATCH is a syntax error, not
  a zero-result search.
- **`store.MatchAny`, `SearchOptions.RequireText`, and the `bm25(events_fts, 1.0, 0.4, 0.4)` weights are
  reached only through `lumi mcp`** (its `match: "any"` and `require_text` parameters). No CLI command sets
  them. The weights matter: without them a one-word `app` or `window` hit outranks a page of screen text.
- **A rule about the store lives in the store.** When a caller needs to know what `Search` will do, export
  the answer and read it rather than restating it. `HasSearchableTerms` exists because `internal/mcp` had
  reimplemented the unexported drop rule — a copy is correct only until the original moves, and the drift
  is invisible to both test suites.
- **Schema changes go through `internal/store/migrations.go`.** Append a new `migration` with the next
  version; never edit shipped SQL.
- **Pruning deletes rows before files.** Orphaned files are recoverable; rows pointing at missing media are
  not. `lumi prune` is the only path permitted to delete media. Keep dry-run accounting equivalent to a real
  age-then-size run, and keep deletes batched below SQLite's variable limit. `--all` is irreversible and
  requires an interactive `yes` (`confirmPruneAll`); only `--yes` or `--dry-run` may skip it. `--all` also
  sweeps `Paths.Screenshots`/`Paths.Audio` for orphans — that is what makes the wipe a real privacy
  guarantee. Age/size policies must never remove orphans.

### MCP server

- **Nothing may write to stdout in the `lumi mcp` path except JSON-RPC frames.** A stray `fmt.Println`,
  default `slog` handler, or cobra usage dump silently corrupts the session and the user just sees the agent
  lose Lumi. `mcpCommand` sets `SilenceUsage`/`SilenceErrors` explicitly; every diagnostic goes to stderr;
  `TestServeWritesOnlyJSONRPCFramesToStdout` pins it.
- **MCP tools return text and metadata only — screenshots and WAVs never leave the machine.** `media_path`
  is a string the user can open themselves. There is no `read_media`, and no filesystem-reading call
  anywhere in `internal/mcp`. No test can prove this negative; keep it true by construction.
- **Truncation lives at the MCP boundary, never in the store.** `search_events` caps `text` in the handler
  after `Search` returns, counting runes so multi-byte text is never split. Pushing `max_text_chars` into
  the SQL `SELECT` would corrupt `lumi search --json`, a faithful export.
- **Audio collapse: system outranks the microphone on every duplicate, but never outranks a non-empty
  transcript with silence.** `CollapseAudioTracks` orders survivors by `(hasText, isSystem, runeLen, -id)`,
  and the first two keys *are* the rule — `hasText` above `isSystem` so a silent row never deletes a
  transcript; `isSystem` above `runeLen` because the mic is re-recording the speakers, so the system track
  is the cleaner original. Inverting either is silent data loss (`TestSystemAudioWinsEveryDuplicate`). The
  pair shares one `captured_at` by construction (one `Audio.Record` call, one `now`), so collapse groups on
  the timestamp, never id adjacency.
- **Collapse must never become a SQL pre-filter.** An FTS query landing only in the microphone transcript
  matches just that row, and a `NOT EXISTS` filter consulting the non-matching system row would drop the
  only hit. Provenance therefore comes from `AudioTracksAt` reading the whole chunk, so `audio_origin` stays
  truthful even where the survivor is not the system row. `audio_origin` is the only thing separating a
  remote speaker or media (`system`) from the user's own voice (`microphone`). On the CLI collapse is opt-in
  (`--collapse-audio`) so the default `--json` stays a bare `[]store.Event`.

### MCP client setup

- **Never hand-write `~/.claude.json`.** It is ~150KB of live Claude Code state rewritten by a running app,
  so a read-modify-write can drop whatever the app wrote in the interim. Lumi may *read* it to detect an
  entry (reading cannot corrupt, and `claude mcp get` can't answer "does this differ"). The only supported
  writers are `claude mcp add --scope user <name> -- <command> [args…]` and `claude mcp remove`. **The `--`
  separator is load-bearing** — without it `claude`'s parser eats `--data-dir` and the server silently
  points at the default index. Because `--force` removes before it adds, a failed `add` triggers a rollback
  re-adding the original on a fresh timeout detached from cancellation; if that also fails, the error
  carries the lost entry as paste-able JSON.
- **Preserve every key in `claude_desktop_config.json` that Lumi did not write.** Decode to
  `map[string]json.RawMessage` at both levels, replace only `mcpServers[<name>]`, write via
  temp-file-plus-rename so a running app can't read a torn file. Invalid JSON is an error, never something
  to repair — the repair would discard the user's preferences. Detection is on the *directory*, and setup
  never creates it.
- **Never hand-write `~/.codex/config.toml`, and never create `~/.codex`.** It *is* a settings file —
  comments, `[features]`, dozens of `[projects."…"]` tables — and no Go TOML encoder preserves comments.
  `codex mcp add`/`remove` are the only supported writers; they normalize the whole `mcp_servers` table
  (dropping a sibling's `args = []`, floating its timeout, sorting its `env`), which is the client's own
  behaviour and not something Lumi can narrow. Reads go through `codex mcp get <name> --json`, the
  structured comparison `claude mcp get` couldn't give. **The `--` separator is load-bearing here too**
  (`TestCodexAddPassesArgsAfterASeparator`). Detection is the `codex` binary on PATH and nothing else —
  `~/.codex/` is created by the ChatGPT desktop app too.
- **A disabled Codex entry is a difference, not a match.** An entry with Lumi's exact command and args but
  `enabled = false` is one codex will not launch; comparing only command and args reported `unchanged` while
  the agent silently never saw Lumi. It is a difference, so `--force` fixes it, and `Result.Current` appends
  `(disabled)`. The decoded field is a `*bool` — codex writes no key for an enabled server, so absent and
  `false` must not collapse. It also blocks rollback: `codex mcp add` cannot re-add an entry disabled, so
  `codexEntry.restorable()` refuses and the error carries the raw JSON.
- **Setup never overwrites an entry it did not write.** A differing entry is a conflict that prints current
  against desired and exits non-zero; only `--force` replaces it, and only after a `.lumi-backup`. Silently
  overwriting destroys a hand-tuned entry; warning but exiting zero leaves the agent pointed at the wrong
  index — the worst failure mode. An entry under Lumi's name that does not *decode* is a conflict too, in
  all three targets.
- **`--dry-run` writes nothing at all, including directories.** `runMCPSetup` skips `Paths.Ensure` under it,
  so previewing a mistyped `--data-dir` doesn't create the root. It may still *read*: `codex mcp get` is the
  only way to know what a dry run would do. That command exits 1 both for an unknown name and an unparseable
  config, so a failure is followed by `codex mcp list --json` as a health probe. A read it cannot trust is
  `StatusFailed`, deliberately not `StatusConflict` — nothing is in the way, and offering `--force` would be
  advice that cannot work. `Changed` stays false in every dry run.
- **`lumi mcp setup` bakes an absolute binary path and absolute `--data-dir` into the argv** — always, even
  at the default root. Same bare-environment reason as `lumi mcp`, plus it makes the desired entry a pure
  function of (binary, root), which is what lets the "already configured?" check be an exact comparison. It
  deliberately does not `EvalSymlinks`: a packaged install is reached through a stable symlink whose target
  moves every version bump.

## External dependencies

Xcode Command Line Tools plus a Swift toolchain, to build the native cgo bridge (`swiftc` compiles the
SpeechAnalyzer bridge into `liblumispeech.a`). Capture and processing are fully native — no external
binaries, and no network calls beyond Apple's on-device speech-asset download. Lumi performs no inference.
