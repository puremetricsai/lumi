# internal/mcp

Stdio MCP server on `github.com/modelcontextprotocol/go-sdk` (pinned v1.6.1; import aliased as `sdk`, since
our package is also named `mcp`). `Serve(ctx, *store.Store, Options)` registers four read-only tools —
`search_events`, `get_event`, `list_apps`, `get_transcript` — and runs until stdin closes or the context is
cancelled. It depends on `internal/store` and nothing else of Lumi's.

`search_events` truncates text per event and reports `truncated` plus true `text_length`, collapses audio
duplicates by default (`collapse_audio_tracks` is a `*bool`, nil meaning on), and over-fetches
(`min(2*limit, MaxSearchLimit)`) then trims so a collapsed page stays full; `get_event` is the untruncated
escape hatch and the only tool returning metadata. `get_transcript` reads audio as one ordered conversation
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
- **MCP tools return text and metadata only — screenshots and WAVs never leave the machine.** `media_path`
  is a string the user can open themselves. There is no `read_media`, and no filesystem-reading call
  anywhere in this package. No test can prove this negative; keep it true by construction.
- **Truncation lives at the MCP boundary, never in the store.** `search_events` caps `text` in the handler
  after `Search` returns, counting runes so multi-byte text is never split. Pushing `max_text_chars` into
  the SQL `SELECT` would corrupt `lumi search --json`, a faithful export.
- **A rule about the store is read from the store, not reimplemented here.** `HasSearchableTerms` exists
  because this package had copied the unexported term-drop rule, and the drift was invisible to both test
  suites (`internal/store/CLAUDE.md`).

## What an `app` filter means to an agent

- **Because audio rows carry an app, every app-shaped query spans both kinds, and the tools must let a
  caller say which one they mean.** `Search`'s `app`/`window` filters are unqualified SQL predicates, so
  `search_events(app: "Zed")` returns whatever the speakers were playing while Zed was focused — with the
  `window` title of an unrelated document stamped on it. That is the design working as specified, but it
  is not what "filter by app" reads as, and the results look legitimate. `ListAttribution` therefore takes
  a `Kind` and `list_apps` exposes it; `search_events` already had `kind`. Summing the two into one count
  is what makes the conflation invisible: the split is the whole signal. Measured on a live index,
  `app = "Zed"` over 30 minutes was 30 screen rows and 8 audio rows, and the audio was a podcast.
- **Neither kind's `app` says where the *content* came from, so no tool description may imply it does.**
  The tempting shorthand — "`kind: "screen"` shows where the text was read from" — is false twice over:
  screen text is full-display OCR carrying every visible window, and one focused-window snapshot is
  stamped onto every display's frame. A single indexed event was measured holding a Gmail inbox under
  `app = "Calendar"`. Both kinds answer "what was the user working in"; `kind` separates how that app
  earned its count, never what produced the content.
- **`get_transcript` reports `ResumeFrom`, never `CoveredUntil`**, and names chunks whose recognition
  failed apart from real coverage gaps so it never recommends a backfill that cannot help
  (`internal/store/CLAUDE.md`).
