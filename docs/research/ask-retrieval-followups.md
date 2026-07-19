# `lumi ask` retrieval — follow-ups

Deferred work left after the staged-retrieval + context-budget change (`internal/store/query.go`, `internal/cli/retrieve.go`, `internal/cli/context.go`). Neither item was needed to fix the reported defects; both would make retrieval measurably better.

## 1. Map time expressions onto `--since`

**Problem.** `questionTerms` (`internal/cli/retrieve.go`) drops temporal words — `yesterday`, `last week`, `this morning` — as stopwords. It has to: leaving them in makes the all-terms FTS pass fail on nearly every naturally phrased question, because "yesterday" rarely appears in an event's OCR text. But dropping them throws the information away entirely. "What did I read **yesterday**" and "what did I read **last week**" retrieve over the identical `--since 24h` default window, so the time constraint the user stated is silently ignored.

**Approach.** Parse a small set of relative time expressions out of the question *before* stopword removal, and translate them into `SearchOptions.Since`/`Until` instead of discarding them:

- `yesterday` → `[start-of-yesterday, start-of-today]`
- `today` / `this morning` / `this afternoon` → sub-ranges of the current day
- `last week` / `past week` → `Since = now-7d`
- `last N days/hours` → `Since = now-N`

An explicit `--since`/`--until` flag must win over anything parsed from the question — the flag is the user being deliberate. Keep the vocabulary deliberately small; full natural-language date parsing is a rabbit hole and a dependency this project does not want. The parsed words still get removed from the term list afterward, so the FTS pass is unaffected.

**Testing.** Pure-function table test over `(question, now) → (terms, since, until)`; `now` must be injected, not read from the clock, so the test is deterministic. Add an `ask`-level case asserting an explicit `--since` overrides a "yesterday" in the question.

**Watch out for.** Timestamps are RFC3339Nano UTC strings compared lexicographically (`internal/store` invariant) — day boundaries must be computed in the user's local zone and then `.UTC()`-formatted, or "yesterday" straddles the wrong events near midnight.

## 2. Porter stemming for the FTS index

**Problem.** The tokenizer is `unicode61 remove_diacritics 2` (`internal/store/store.go`), which matches only exact word forms. A question about "postgres **indexes**" does not match an event that says "postgres **indexing**", and the any-term stage cannot recover it — the term never tokenizes to a shared stem. This caps recall on exactly the paraphrase-heavy questions staged retrieval is meant to handle. Query-side prefix matching was rejected during design because it is asymmetric (it only helps when the user's term is the shorter stem); index-side stemming is the symmetric fix.

**Approach.** Change the FTS5 tokenizer to `porter unicode61 remove_diacritics 2`, so both indexed text and query terms reduce to the same stem.

**The blocker — and why it may already be gone.** `migrate` currently applies schema with `CREATE VIRTUAL TABLE IF NOT EXISTS`, so a tokenizer change is a silent no-op on any existing database: the table already exists, the new DDL is skipped, and the index keeps its old tokenizer with no error. A correct change therefore needs a detect-and-rebuild path (drop `events_fts`, recreate with the new tokenizer, `INSERT INTO events_fts(events_fts) VALUES('rebuild')`), gated so it runs exactly once.

The **Tier 1 plan** (`docs/plans/2026-07-19-tier1-retention-filters-migrations.md`) replaces the idempotent-DDL blob with a versioned `PRAGMA user_version` migration runner. Once that lands, this becomes a single new migration that rebuilds `events_fts` — do **not** hand-roll a separate detect-and-rebuild here. Sequence this work *after* Tier 1 Task 1.

**Testing.** Insert "postgres indexing"; assert a search for "indexes" (or "index") returns it. Add a legacy-DB test: open a DB created with the old tokenizer, run migrations, confirm the rebuild ran and stemmed search now works — the rebuild-on-existing-data case is where this breaks.

**Watch out for.** Porter is English-only and lossy (`universal`/`university` → `univers`), a small precision cost for a real recall gain — acceptable here, but note it. Rebuilding the FTS index over a large capture history is O(rows) and runs inside the one migration; it is a one-time cost but not free.
