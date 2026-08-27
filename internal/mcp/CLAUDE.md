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
  which clients can read it.
- **The media path is split, and the contract says how to rejoin it.** Every event of a kind comes from one
  directory, so repeating it per event was two thirds of each path and the largest constant cost on a page;
  `media_dir` states it once. Two rules keep the split honest. `media_file` is `filepath.Base` of the stored
  path and never a name composed from `captured_at` — the recorder's rename to the timestamped form is
  best-effort (`internal/capture/audio.go`), so a chunk whose rename fell through keeps a name that formula
  would not produce, and a caller composing one would ask for a file that does not exist. And a kind whose
  files sit in two directories — an index carried between data dirs — is left un-hoisted with whole paths,
  because a short name joined to the wrong directory is worse than a long one.
- **Every timestamp a tool returns goes through `localStamp`** — the machine's local zone with its offset,
  at nanosecond precision, matching what `lumi search` prints. The precision is what lets a value round-trip
  when it is handed back as a `since` or `until` bound. The *zone* matters for a reason that only appears
  between fields: an agent cannot know two timestamps in one response are the same kind of thing, so it
  compares them as strings. `resume_from` was rendered UTC while `captured_at`, `started_at`, `ended_at` and
  `last_seen` were local — both parse, both round-trip, and nothing failed, so the only symptom was an agent
  reading a resume point as hours away from the turns it continues. The notice offering `resume_from` names
  `covered_until` in the same sentence, which printed the two spellings side by side.
  `lumi transcript` had the identical pair and was fixed with it. Note that `resume_from` legitimately sorts
  *at or before* the last turn's `ended_at` when a cap falls inside a chunk — the next page re-reads that
  chunk by design (`internal/store/transcript.go`), so ordering is not a property to assert here.
  `TestEveryTimestampIsRenderedInTheLocalZone` walks whole payloads rather than named fields, so a timestamp
  added later is covered without anyone remembering. Storage and range comparison stay UTC.
- **Truncation lives at the MCP boundary, never in the store.** `search_events` caps `text` in the handler
  after `Search` returns, counting runes so multi-byte text is never split. Pushing `max_text_chars` into
  the SQL `SELECT` would corrupt `lumi search --json`, a faithful export.
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
