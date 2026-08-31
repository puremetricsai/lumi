# internal/mcp

Stdio MCP server on `github.com/modelcontextprotocol/go-sdk` (pinned v1.6.1; import aliased as `sdk`, since
our package is also named `mcp`). `Serve(ctx, *store.Store, Options)` registers four read-only tools —
`search_events`, `get_event`, `list_apps`, `get_transcript` — and runs until stdin closes or the context is
cancelled. It depends on `internal/store` and nothing else of Lumi's; the binary-watching and
process-replacing hooks in `Options` are injected by `internal/cli` from `internal/selfexec`, because this
package reads the store and never the filesystem.

`search_events` truncates text per event and reports `truncated` plus true `text_length`, and never merges
an audio chunk's two rows; `get_event` is the untruncated escape hatch and the only tool returning
metadata, which it filters. Every tool returns its payload both as `structuredContent` and, serialized, as
the `Content` text block. `get_transcript` reads audio as one ordered conversation
with the machine's own speech deduplicated, and its `confidence` and `order_confidence` carry no `omitempty`
for the same reason `truncated` does not: a doubtful label must never be something an agent infers from a
missing key. Validation and store failures come back as tool results with `isError`, never JSON-RPC protocol
errors.

An agent cannot see the shape of the index, so this package is explicit about ambiguity: a `query` with no
letters or digits is a tool error rather than a fall-through to `Search`'s no-MATCH behavior, an empty
result says whether the index itself is empty (naming `Options.DatabasePath`) or the filters matched
nothing, a full page says results were capped, and `search_events` clamps `limit` itself so the number it
reports is the one enforced.

## Invariants

- **A server process that no longer matches what is installed says so, in every tool's `notice`.** Both
  ways this drifts are silent by construction, which is why the reporting exists at all. An agent launches
  `lumi mcp` once and keeps it for the session, so an upgrade leaves the old image mapped and
  answering — the client owns the process lifecycle and will not relaunch it mid-session. And every
  migration is additive (`internal/store/migrations.go`), so an older build reading a newer file finds every
  column its fixed `eventSelect` names, succeeds, and returns rows missing only what the new build added.
  Nothing errors on either path; the user upgrades to get a fix, watches the old behavior persist, and has
  no way to see why. `stalenessNotice` compares `store.CodeSchemaVersion` against `Store.SchemaVersion` and
  asks `Options.BinaryChanged`, and `withStaleness` puts the result *first*, because it qualifies whatever
  follows: an agent that reads "no events matched" ahead of it will act on the filters. It is a notice and
  never a tool error — the results are real and worth having, and the skew makes them possibly-incomplete
  rather than wrong. `get_event` grew a `notice` field for this and this alone, since it is where an agent
  goes for the *complete* version of a truncated answer. A self-updating build words it differently
  (`Options.BinaryExec` present): the fix is imminent and automatic, so telling the user to restart the
  session would be wrong advice.
- **`lumi mcp` replaces its own process image when its binary changes, and the handshake is what has to
  survive.** `syscall.Exec` preserves fds 0/1/2, so the client keeps talking to the same pid on the same
  pipes and never learns anything happened — that is the whole reason this works where a client-side restart
  would not. But the 2025-11-25 protocol this SDK pins gates every method on having seen `initialize`
  (go-sdk's `ServerSession.handle`), so a replacement that came up cold would reject the client's next
  request with "method is invalid during session initialization" — and fail on request N+1, not at startup.
  `reexec.go` therefore stashes `sdk.ServerSessionState` in `LUMI_MCP_SESSION_STATE` across the exec and
  `Serve` restores it through `ServerSessionOptions.State`, which is why `Serve` cannot use the SDK's
  `Server.Run` (it always connects a fresh session). The variable is unset the moment it is read, so a stale
  handshake cannot be inherited by an unrelated child. Unparseable stashed state is discarded rather than
  fatal, and a failed `execve` leaves this image intact and serving, which is why it clears the variable
  again on the way out.
- **A request is in flight from the moment it leaves the pipe until its reply has been written, and the
  guard has to cover that whole span.** Neither end is where a handler runs, and each was a real defect found
  in review. *After:* jsonrpc2 serializes and writes the reply **after** `Handle` returns (`processResult`
  calls `c.write`) and the SDK's `ioConn.Write` ends in a blocking write to stdout, so a client that stopped
  draining its pipe leaves a reply pending with every handler already finished — counting handlers alone
  reported that session *idle*, and exec'ing there carries the reply away and hangs the client.
  *Before:* `acceptRequest` appends to an unbounded `handlerQueue` and only then starts `handleAsync`
  (`internal/jsonrpc2/conn.go`), so a call the client is already blocked on increments nothing until the
  handler reaches it. That end is the worse of the two, because it is unrecoverable: the replacement image
  **cannot re-read a frame this one already consumed**. The SDK's `IOTransport` makes this boundary earlier
  still: its decoder goroutine consumes stdin before `Connection.Read` returns, so recording the ID there
  alone cannot close the race. `guardedReader` polls without the updater lock, then holds it across the raw
  read and records activity before releasing it; if replacement won the lock, bytes remain in the inherited
  pipe for the new image. So `selfUpdater` tracks three things —
  running handlers (`middleware`), frames being written (`trackWrites`'s `Write`), and calls read but not yet
  answered (`trackWrites`'s `Read`, keyed by ID in `outstanding`) — and `idleLocked` states the rule once
  because `claimIdle` acts on it without releasing the lock. Notifications are deliberately *not* tracked:
  they carry no ID and get no response, so nothing could ever retire one and a permanent entry would block
  every future upgrade — a worse failure than the one being fixed — while losing one hangs nobody.
  A failed write retires its call for the same reason.
- **The idleness check and the `execve` are one atomic decision.** Checking and then exec'ing without the
  lock re-opens the race from the other side: the check passes, a request lands in the interval, and the
  replacement discards it. `claimIdle` holds `mu` across both, which is safe precisely because a successful
  `execve` never returns — there is no caller left to unblock. On failure the lock is released and the server
  carries on, so the guard is scoped to one attempt and can never wedge future upgrades; both failure paths
  (`stashSessionState` and `exec`) also clear `LUMI_MCP_SESSION_STATE`.
- **Tests for all of this must fail with the guard removed, which is not automatic here.** A never-used
  `selfUpdater` is never idle (`lastActivity` is zero), so an assertion that a request holds the guard passes
  vacuously unless a completed exchange precedes it —
  `TestAReadCallIsOutstandingBeforeAnyHandlerRuns` does that first, and was confirmed to fail with the `Read`
  interception taken out. `TestRequestBytesAreNotConsumedDuringReplacement` pins the earlier raw-transport
  boundary. `TestReplacementIsAtomicWithTheIdlenessCheck` probes inside the check-to-exec window
  via the `state()` callback and fails when `claimIdle` is reduced to check-then-act;
  `TestUpdaterMustNotBeIdleWhileAReplyIsStillBeingWritten` blocks a reply mid-write.
  `TestUpdaterIsIdleOnceTheReplyHasBeenWritten` pins the other direction, because a guard that never lets go
  means the upgrade never happens. `reexecQuietPeriod` is a margin on top of these guarantees — it covers the
  gap *between* two requests of one exchange — and is not what makes the replacement safe.
- **Nothing may write to stdout in the `lumi mcp` path except JSON-RPC frames.** A stray `fmt.Println`,
  default `slog` handler, or cobra usage dump silently corrupts the session and the user just sees the agent
  lose Lumi. `mcpCommand` (in `internal/cli`) sets `SilenceUsage`/`SilenceErrors` explicitly; every
  diagnostic goes to stderr; `TestServeWritesOnlyJSONRPCFramesToStdout` pins it.
- **MCP tools return text and metadata only — screenshots and WAVs never leave the machine.** `media_dir`
  joined to an event's `media_file` is a path the user can open themselves. There is no `read_media`, and no
  filesystem-reading call anywhere in this package — `hoistMediaDir` splits a string and never touches the
  disk. No test can prove this negative; keep it true by construction.
- **A payload crosses the wire twice, on purpose, and `Content` is never a summary of it.** Every handler
  returns a nil `*sdk.CallToolResult`, so the go-sdk fills `Content` with the serialized output
  (`mcp/server.go`, the `res.Content == nil` branch) — the same bytes in `structuredContent` and again as
  text. That duplication is the spec's own backwards-compatibility rule and it is load-bearing:
  `structuredContent` is optional for a client to read, and Codex CLI and Claude Desktop — two of the three
  clients `mcpsetup` registers — render the content blocks and nothing else. Setting `Content` ourselves is
  what silences the fallback, so a "cheaper" `Content` is not a smaller response, it is the *only* response
  those clients get. This was learned the expensive way: `Content` briefly held a digest — counts, a time
  span, and the `notice` — and an agent asked to write up a meeting it had watched Lumi record reported that
  Lumi returned metadata for the window but no transcript text. Every call had carried the full transcript
  in `structuredContent`. Nothing failed, no tool errored, and the agent's account of the failure was
  accurate about what it could see. `TestEveryToolResultCarriesItsWholePayloadAsText` pins it by asserting
  the text block parses and equals `structuredContent`, so a summary reintroduced on either side alone
  fails. Trim what a payload *contains* — see the media-path split and the metadata denylist below — never
  which clients can read it. And there is no model-token saving to be had there in any case: measured by
  calling the tool from a Claude Code session and reading what landed, only the content block enters the
  agent's context, once. The duplication costs wire bytes and nothing else. It was re-examined under that
  measurement and left exactly as it is.
- **The server's `Instructions` route between tools and restate nothing a tool description already says.**
  `newServer` passed `nil` for `*sdk.ServerOptions` and shipped no instructions at all — a channel wired and
  empty, since the SDK returns `Instructions` in the initialize result and clients render it into the
  agent's context once per session. The gap it closes is **cross-tool routing**, which no tool description
  can close by construction: a description is per-tool, so none of the four can say which one to reach for
  first. An agent opened audio questions with `search_events` and arrived at `get_transcript` a call later,
  oriented with `list_apps` when it did not need to, and answered `truncated` one `get_event` at a time. So
  the text says only which shape of question goes to which tool, that a truncated page is answered by
  raising `max_text_chars` while `get_event` is for one specific event, and that each tool states its own
  paging in its own `notice`. It stays routing-only, and that is a rule and not a budget: a second copy of a
  provenance or paging rule is precisely the drift the rest of this file exists to prevent, and it would be
  a copy with no test holding it to the Go. The arithmetic is favourable — roughly 200 tokens once per
  session against calls measured at 4,844 (`search_events`) and 8,635 (`get_transcript`) tokens each, so two
  avoided calls repay it many times over. **Not a Claude Code skill**: `mcpsetup` registers Claude Code,
  Claude Desktop and Codex, and a skill reaches one of the three; it would also put Lumi's rules in a fourth
  place, across a tool boundary, with nothing pinning them to the code. Instructions ship inside the binary
  with what they describe.
- **The media path is split, and the contract says how to rejoin it.** Every event of a kind comes from one
  directory, so repeating it per event was two thirds of each path and the largest constant cost on a page;
  `media_dir` states it once. Two rules keep the split honest. `media_file` is `filepath.Base` of the stored
  path and never a name composed from `captured_at` — the recorder's rename to the timestamped form is
  best-effort (`internal/capture/audio.go`), so a chunk whose rename fell through keeps a name that formula
  would not produce, and a caller composing one would ask for a file that does not exist. And a kind whose
  files sit in two directories — an index carried between data dirs — is left un-hoisted with whole paths,
  because a short name joined to the wrong directory is worse than a long one.
- **Every timestamp a tool returns goes through `localStamp`** — the machine's local zone with its offset,
  matching what `lumi search` prints. Precision is per value and follows what the value is *for*: anything a
  caller hands back as a `since` or `until` bound keeps nanoseconds so it round-trips exactly, which today
  means `resume_from` and nothing else, while a transcript turn's `started_at`/`ended_at` are rendered to
  the millisecond because a turn is assembled from a ~30-second chunk and nanoseconds on it are precision
  the measurement does not have. The *zone* is not scoped that way and never may be, for a reason that only
  appears between fields: an agent cannot know two timestamps in one response are the same kind of thing, so
  it compares them as strings. `resume_from` was rendered UTC while `captured_at`, `started_at`, `ended_at`
  and `last_seen` were local — both parse, both round-trip, and nothing failed, so the only symptom was an
  agent reading a resume point as hours away from the turns it continues. The notice offering `resume_from`
  names `covered_until` in the same sentence, which printed the two spellings side by side.
  `lumi transcript` had the identical pair and was fixed with it. Note that `resume_from` legitimately sorts
  *at or before* the last turn's `ended_at` when a cap falls inside a chunk — the next page re-reads that
  chunk by design (`internal/store/transcript.go`), so ordering is not a property to assert here.
  `TestEveryTimestampIsRenderedInTheLocalZone` walks whole payloads rather than named fields, so a timestamp
  added later is covered without anyone remembering; it asserts the offset and that the value parses, never
  how many digits it carries, which is why scoping precision per value left it untouched. Storage and range
  comparison stay UTC.
- **`confidence` is rounded to three decimals, not two, because `min_confidence` filters on the full
  precision.** The filter is applied against the stored value (`internal/store/transcript.go`) while the
  payload shows the rounded one, so any rounding opens a window in which a turn's displayed confidence
  contradicts the filter that removed it: at two decimals a turn stored as `0.595` displays as `0.6` and
  then vanishes under `min_confidence: 0.6`, which nothing in the response can explain. Three decimals
  narrow that window to ±0.0005 and cost almost nothing — measured on a real 100-turn response, two decimals
  saved 2.2% of the payload and three saved 1.9%. `"confidence": 0.8340000000000001` was the shape being
  paid for.
- **The rest of a transcript turn's envelope is not waste; what looks like waste is the doubt label.**
  Measured on a real 100-turn response, `text` is 29.7% of the payload and the median turn is 35 characters,
  so cutting fields is the obvious win: omitting `truncated: false` and `text_length` reaches −24%, dropping
  `ended_at` −36%. Both were declined, because they cut the doubt label itself — a field that appears only
  when it is interesting is a field an agent has to infer from an absence, which is the rule these fields
  carry no `omitempty` for. Hoisting `order_confidence` to the envelope was declined for three separate
  reasons, any one of them sufficient: `sdk.AddTool` infers the output schema from the Go struct, so
  `omitempty` drops the field from the per-turn required list with no way to express "either every turn
  carries it or the envelope does"; it falsifies the advertised description *"Every turn carries confidence
  and order_confidence"*; and it breaks `TestTranscriptTurnAlwaysSerializesConfidence`, which exists to
  guard this exact field against exactly this. Rounding and precision were where the payload could be made
  smaller without changing anything it promises.
- **Truncation lives at the MCP boundary, never in the store — and so does the choice of *which* window it
  shows.** `search_events` caps `text` in the handler after `Search` returns, counting runes so multi-byte
  text is never split. Pushing `max_text_chars` into the SQL `SELECT` would corrupt `lumi search --json`, a
  faithful export, and centring the excerpt would corrupt it the same way, which is why `excerptAround` sits
  beside `truncateText` here and not as a `snippet()` in the query. The cut used to be a blind prefix, and on
  full-display OCR the first 600 runes are the menu bar and the tab strip: sampled on the live index, the
  first occurrence of the query term sits past char 600 in 22% of rows for `claude`, 38% for `meeting`, and
  **100%** for `invoice`. A real call for `claude` came back with 842 chars beginning
  `"Comet\nFile\nEdit\nView\nAssistant…"` and the word nowhere in it, on 20 of 20 rows marked `truncated` —
  and the description's own advice then turned that page into twenty `get_event` calls for one question.
  The excerpt now centres on the earliest occurrence of any term `store.SearchTerms` returns, and the
  description points at raising `max_text_chars` rather than at `get_event`. `text_length` stays the rune
  count of the whole text and `truncated` stays true: the pair's contract is untouched, only the window is.
- **Go-side centring cannot reproduce FTS5's matching, and every row where it fails falls back to the head
  cut.** `events_fts` folds diacritics (`unicode61 remove_diacritics 2`, `internal/store/migrations.go`), a
  whitespace-separated term becomes a quoted phrase that can span tokens (`internal/store/query.go`), and the
  index covers `app` and `window` as well as `text` — so a row can legitimately match on its window title
  with the term absent from `text` entirely. `excerptAround` finds nothing on those and returns the prefix
  cut, which is exactly what shipped before it: **the floor is the old behaviour**, and the change is a
  strict improvement on every row where a match is found, which the sampling says is most of them. Closing
  the rest means FTS5-reported offsets, a new `Event` field and an opt-in plumbed through `Search`, spent on
  a better window for the minority of rows that already sit at the floor. That is why it was declined and
  not merely deferred.
- **The collapse is screen-only, and no flag may ever let an audio row into it.** `collapse_similar` folds a
  run of adjacent screen results sharing `app` + `window` + `display_id` whose text is near-identical into
  one representative, because 76% of adjacent same-app screen pairs are more than 0.9 identical at a 2–10s
  capture cadence. Audio is the same shape and the opposite case. All 967 audio pairs on the live index
  share `app`, `window` and `display_id` — an audio row's `display_id` is always 0 — and their two
  transcripts are **median 0.819 similar**, 87 of 140 above 0.7, precisely because the microphone re-records
  what the speakers played. Keyed on those fields the collapse would merge a chunk's two tracks on the
  majority of pairs, which is the defect the never-merge rule records as having deleted real transcripts,
  arrived at from a timestamp a second time. `collapsed_ids` does not rescue it: it preserves *reachability*,
  not content, so the microphone's account of the room leaves the visible result and the only way back is a
  `get_event` the agent has no reason to make. `kind == "screen"` is therefore a precondition of the
  collapse and not a filter layered over it, and a test asserts an audio pair survives
  `collapse_similar: true` as two rows.
- **The collapse runs after `LIMIT`, so the notice reports both counts.** The store returns `limit` rows and
  the fold happens on the way out, so a page of 20 that collapses to 6 has still exhausted the limit: a
  notice saying "capped at 20" beside six events contradicts its own payload, and one saying "capped at 6"
  invents a cap nothing enforced. It states what was fetched and what survived. Refilling the page by
  over-fetching was declined deliberately — a short page costs one clause of explanation, a refill loop
  costs an unbounded number of store round-trips to hide it, and nothing has shown short pages cost more
  calls than the tokens the fold saves.
- **A rule about the store is read from the store, not reimplemented here.** `HasSearchableTerms` exists
  because this package had copied the unexported term-drop rule, and the drift was invisible to both test
  suites (`internal/store/CLAUDE.md`).
- **`get_event`'s metadata is filtered by a denylist, never an allowlist.** `redundantMetadataKeys` drops
  keys whose meaning is already on the wire — `display_id`, `text_source` and `audio_source` are first-class
  columns rendered at the top of the same record; `active_audio_output_processes` and `audio_marker_windows`
  are the other rendering of the fold `source_app` already carries decoded and ordered — plus ones that
  answer a question about Lumi's capture rather than about what was captured. `accessibility_text` goes with
  them: it was a second, differently-sourced rendering of the same frame, as long again as the OCR text
  beside it, while `text_length` described only that text and nothing said the two overlapped. **The
  direction matters.** Metadata is where a failed processor leaves the only account of what went wrong, and
  a processor added later writes a key nothing here knows about; an allowlist would drop that account
  silently, which is the failure this channel exists to prevent. Every `*_error` key, `clock_anomaly` and
  `audio_attribution_reason` pass through, and `TestMetadataKeepsEveryDiagnostic` pins it. None of this
  reaches storage: the blob is kept whole and `lumi search --json` exports it whole.
- **What a boundary drops is not what Lumi forgets.** `source_app`'s `pid` stops here for the same reason
  the store's `FirstOffsetMS`/`LastOffsetMS` already did — it is valid only for the length of that recording
  and names nothing a caller off this machine can resolve, while looking exactly like a durable identity to
  key on. `samples` and `observations` became one `presence` ratio because that is what the pair always
  meant: `observations` is a chunk-level denominator repeated identically on every entry, and two numbers
  read as two independent measurements. `stream_offset_ms` went because `captured_at` already places the
  chunk and no tool accepts an offset. All of them remain in the columns and in `lumi search --json`.

## What an `app` filter means to an agent

- **Because audio rows carry an app, every app-shaped query spans both kinds, and the tools must let a
  caller say which one they mean.** `Search`'s `app`/`window` filters are unqualified SQL predicates, so
  `search_events(app: "Zed")` returns whatever the speakers were playing while Zed was focused — with the
  `window` title of an unrelated document stamped on it. That is the design working as specified, but it
  is not what "filter by app" reads as, and the results look legitimate. `ListAttribution` therefore takes
  a `Kind` and `list_apps` exposes it; `search_events` already had `kind`. Summing the two into one count
  is what makes the conflation invisible: the split is the whole signal. Measured on a live index,
  `app = "Zed"` over 30 minutes was 30 screen rows and 8 audio rows, and the audio was a podcast.
- **An audio row's provenance is three fields, and the tool description is where they are kept apart.**
  `audio_source` is the capture *device*; `source_app` is what was observed producing the sound;
  `foreground_app` is what the user had focused. `attribution` says how `source_app` was earned, so a
  consumer branching on it cannot mistake a guess for a fact. The description text is the real contract —
  it loads into an agent's context before any row is fetched — so `audioProvenanceContract` lives in
  `server.go` beside the tools rather than in a doc, and
  `TestToolDescriptionsStateTheMicrophoneCaveat` pins it.
- **A tool description states the microphone's ambiguity as a fact and stops there — it does not instruct
  the caller.** Microphone audio is room audio with no recoverable owner: every microphone row is
  `unattributed` with no `source_app`, and what it caught may be the user, other people present, a TV,
  another machine, or ambient playback. `get_transcript`'s `external` origin carries the same caveat (root
  `CLAUDE.md`). The descriptions used to follow that with prohibitions — never attribute this to a person
  or an application, never present it as something the user said — which read as a rule about people
  rather than a fact about the data, leaving an agent holding a microphone row that plainly did hold
  speech to reconcile the two. The fact is what the row supports and is what the tools say.
- **The `app`/`window` → `foreground_app`/`foreground_window` rename lives here, at the boundary.** The SQL
  columns cannot be renamed — FTS5, `Search`'s app filter, and `ListAttribution` depend on them — and
  `lumi search --json` stays a bare `[]store.Event`, so this is the only place an agent is told that an
  audio row's app is a focus field rather than a source field.
- **Neither kind's `app` says where the *content* came from, so no tool description may imply it does.**
  The tempting shorthand — "`kind: "screen"` shows where the text was read from" — is false twice over:
  screen text is full-display OCR carrying every visible window, and one focused-window snapshot is
  stamped onto every display's frame. A single indexed event was measured holding a Gmail inbox under
  `app = "Calendar"`. Both kinds answer "what was the user working in"; `kind` separates how that app
  earned its count, never what produced the content.
- **`get_transcript` reports `ResumeFrom`, never `CoveredUntil`**, and names chunks whose recognition
  failed apart from real coverage gaps so it never recommends a backfill that cannot help
  (`internal/store/CLAUDE.md`).
- **A notice about what a filter removed must fire on a transcript that is *not* empty.** Every
  explanation in `transcriptNotice` used to sit inside the `len(result.Turns) == 0` branch, so the one
  case that needed it most was the one case it could not reach: the largest attribution penalties fall on
  microphone turns, so `min_confidence` can delete the room, leave the machine, and return a
  complete-looking conversation with nobody in the room in it — no empty result, no notice, nothing an
  agent could detect. The counts come from `store.ConfidenceRemovals` rather than being phrased here, and
  the `min_confidence` schema text states the measured 0.6 cliff, because an agent reads the description
  before it ever sees a row. `confidence_filtered` is on the wire as well as in the notice, for the reason
  `resume_from` is: an agent should not have to parse prose to act on a fact.
