# internal/mcp

Stdio MCP server on `github.com/modelcontextprotocol/go-sdk` (pinned v1.6.1; import aliased as `sdk`, since
our package is also named `mcp`). `Serve(ctx, *store.Store, Options)` registers four read-only tools —
`search_events`, `get_event`, `list_apps`, `get_transcript` — and runs until stdin closes or the context is
cancelled. It depends on `internal/store` and nothing else of Lumi's.

`search_events` truncates text per event and reports `truncated` plus true `text_length`, and never merges
an audio chunk's two rows; `get_event` is the untruncated escape hatch and the only tool returning
metadata, which it filters. Every tool returns its data once, as `structuredContent`, with a digest in
`Content`. `get_transcript` reads audio as one ordered conversation
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

- **Nothing may write to stdout in the `lumi mcp` path except JSON-RPC frames.** A stray `fmt.Println`,
  default `slog` handler, or cobra usage dump silently corrupts the session and the user just sees the agent
  lose Lumi. `mcpCommand` (in `internal/cli`) sets `SilenceUsage`/`SilenceErrors` explicitly; every
  diagnostic goes to stderr; `TestServeWritesOnlyJSONRPCFramesToStdout` pins it.
- **MCP tools return text and metadata only — screenshots and WAVs never leave the machine.** `media_dir`
  joined to an event's `media_file` is a path the user can open themselves. There is no `read_media`, and no
  filesystem-reading call anywhere in this package — `hoistMediaDir` splits a string and never touches the
  disk. No test can prove this negative; keep it true by construction.
- **A payload crosses the wire once.** Every handler returns a non-nil `*sdk.CallToolResult` whose `Content`
  is a digest, because the go-sdk fills `Content` with the entire marshalled output when a handler leaves it
  nil (`mcp/server.go`, the `res.Content == nil` branch) — the same bytes in `structuredContent` and again as
  text, on every call. The digest must carry the `notice` verbatim: a client that renders only `Content`
  would otherwise be told nothing about a result that was capped, filtered, or drawn from a range holding
  unattributed audio, which is exactly when a partial answer reads as a complete one.
  `TestToolResultsDoNotRepeatTheStructuredPayload` and `TestDigestCarriesTheNotice` pin both halves.
- **The media path is split, and the contract says how to rejoin it.** Every event of a kind comes from one
  directory, so repeating it per event was two thirds of each path and the largest constant cost on a page;
  `media_dir` states it once. Two rules keep the split honest. `media_file` is `filepath.Base` of the stored
  path and never a name composed from `captured_at` — the recorder's rename to the timestamped form is
  best-effort (`internal/capture/audio.go`), so a chunk whose rename fell through keeps a name that formula
  would not produce, and a caller composing one would ask for a file that does not exist. And a kind whose
  files sit in two directories — an index carried between data dirs — is left un-hoisted with whole paths,
  because a short name joined to the wrong directory is worse than a long one.
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
- **No tool may attribute microphone content to a person or an application.** It is room audio with no
  recoverable owner and every microphone row is `unattributed` with no `source_app`. `get_transcript`'s
  `external` origin carries the same caveat: it may be the user, other people present, a TV, another
  machine, or ambient playback (root `CLAUDE.md`).
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
