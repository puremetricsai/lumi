# Lumi Tier 1: Migrations, App/Window Filters, Retention Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Lumi a versioned schema-migration system, make the already-captured `app`/`window` columns filterable from the CLI, and add age- and size-based retention so the data directory stops growing without bound.

**Architecture:** Three independent, sequential additions to the existing one-way data flow (`cli` → `store`). Task 1 replaces the single idempotent `CREATE ... IF NOT EXISTS` blob in `store.migrate` with an ordered `[]migration` slice gated on SQLite's `PRAGMA user_version`, so existing databases upgrade in place. Task 2 extends `store.SearchOptions` with `App` and `Window` fields and threads `--app`/`--window` flags through `search` and `ask`. Task 3 adds a new `internal/retention` package that selects expired events, deletes their rows (FTS triggers clean the index automatically), and unlinks the media files, exposed as a `lumi prune` command.

**Tech Stack:** Go 1.x, `modernc.org/sqlite` (pure Go, no cgo), SQLite FTS5, `github.com/spf13/cobra`. Standard library `testing` only — no assertion libraries.

**Baseline:** Line references are accurate as of the working tree containing `internal/store/query.go` and `internal/cli/retrieve.go` (the staged-retrieval work). If those files are absent, you are on an older tree and the line numbers will not match — stop and re-derive them.

## Global Constraints

- Module path is `github.com/puremetricsai/lumi`. All internal imports use this prefix.
- SQLite connection is `SetMaxOpenConns(1)` (`internal/store/store.go:53`). Never open a second connection or a nested transaction while holding a `*sql.Rows` — the single connection will deadlock. Always fully drain and `Close()` a `rows` before issuing another statement.
- Timestamps are stored as `time.RFC3339Nano` UTC strings and compared **lexicographically**. Any new time value written or compared MUST use `.UTC().Format(time.RFC3339Nano)`.
- **Never lose captured media.** Deleting media files is only permitted by explicit `lumi prune` invocation, never as a side effect of capture or search.
- FTS input must go through `ftsExpression` (`internal/store/query.go`). Do not pass user text into a `MATCH` clause by any other path.
- **Do not modify `internal/store/query.go` or `internal/cli/retrieve.go`/`context.go`.** They own query construction and `ask` retrieval; this plan only adds filter predicates alongside them.
- New columns/indexes go in a new migration, never by editing an existing migration's SQL.
- Tests must not require `tesseract`, `ffmpeg`, `whisper-cli`, `screencapture`, or a network. Use `t.TempDir()`.
- Run `go vet ./...` and `go test ./...` before every commit.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/store/migrations.go` (create) | Ordered migration list + `user_version` runner. Sole owner of schema DDL. |
| `internal/store/migrations_test.go` (create) | Fresh-DB and legacy-DB upgrade coverage. |
| `internal/store/store.go` (modify) | `migrate` delegates to the runner; `SearchOptions` gains `App`/`Window`; `Search` gains the two filter clauses; `Expired`/`DeleteByIDs` added. |
| `internal/store/store_test.go` (modify) | Filter and deletion coverage. |
| `internal/retention/retention.go` (create) | Age/size selection, row deletion, media unlinking. Owns the prune policy. |
| `internal/retention/retention_test.go` (create) | Prune coverage against a real temp DB + temp files. |
| `internal/cli/root.go` (modify) | `--app`/`--window` flags on `search` and `ask`; new `prune` command. |

---

### Task 1: Versioned migrations

**Files:**
- Create: `internal/store/migrations.go`
- Create: `internal/store/migrations_test.go`
- Modify: `internal/store/store.go:74-113` (replace the body of `migrate`)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func (s *Store) migrate(ctx context.Context) error` (unchanged signature, new implementation); package-level `var migrations []migration` where `type migration struct { Version int; SQL string }`. Task 2 appends migration version 2 to this slice.

- [ ] **Step 1: Write the failing test**

Create `internal/store/migrations_test.go`:

```go
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateSetsUserVersion(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if want := len(migrations); version != want {
		t.Fatalf("user_version = %d, want %d", version, want)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lumi.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e := Event{Kind: KindScreen, Text: "roadmap", MediaPath: "a.jpg"}
	if err := first.Insert(ctx, &e); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening a migrated database must succeed: %v", err)
	}
	defer second.Close()

	got, err := second.Search(ctx, SearchOptions{Query: "roadmap"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the pre-existing row to survive migration, got %d rows", len(got))
	}
}

// A database created by the pre-migration build has the full schema but
// user_version = 0. Opening it must not fail and must not duplicate FTS rows.
func TestMigrateUpgradesLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lumi.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, migrations[0].SQL); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO events(kind, captured_at, text, media_path)
		 VALUES ('screen', '2026-07-19T10:00:00Z', 'legacy note', 'old.jpg')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("opening a legacy database must succeed: %v", err)
	}
	defer s.Close()

	got, err := s.Search(ctx, SearchOptions{Query: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("legacy row should be findable exactly once, got %d rows", len(got))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestMigrate -v`
Expected: FAIL — `undefined: migrations` (compile error).

- [ ] **Step 3: Write the migration runner**

Create `internal/store/migrations.go`:

```go
package store

import (
	"context"
	"fmt"
)

// migration is one forward-only schema change. Version numbers start at 1 and
// increase by one. The applied version is tracked in SQLite's user_version
// pragma, so the whole system costs no extra table.
//
// Never edit the SQL of a migration that has shipped — databases in the field
// have already applied it and will not re-run it. Append a new migration
// instead.
type migration struct {
	Version int
	SQL     string
}

var migrations = []migration{
	{
		Version: 1,
		SQL: `
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('screen', 'audio')),
  captured_at TEXT NOT NULL,
  text TEXT NOT NULL DEFAULT '',
  app TEXT NOT NULL DEFAULT '',
  window TEXT NOT NULL DEFAULT '',
  media_path TEXT NOT NULL,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  metadata_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS events_captured_at_idx ON events(captured_at DESC);
CREATE INDEX IF NOT EXISTS events_kind_captured_at_idx ON events(kind, captured_at DESC);
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(
  text, app, window,
  content='events', content_rowid='id',
  tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS events_ai AFTER INSERT ON events BEGIN
  INSERT INTO events_fts(rowid, text, app, window)
  VALUES (new.id, new.text, new.app, new.window);
END;
CREATE TRIGGER IF NOT EXISTS events_ad AFTER DELETE ON events BEGIN
  INSERT INTO events_fts(events_fts, rowid, text, app, window)
  VALUES ('delete', old.id, old.text, old.app, old.window);
END;
CREATE TRIGGER IF NOT EXISTS events_au AFTER UPDATE ON events BEGIN
  INSERT INTO events_fts(events_fts, rowid, text, app, window)
  VALUES ('delete', old.id, old.text, old.app, old.window);
  INSERT INTO events_fts(rowid, text, app, window)
  VALUES (new.id, new.text, new.app, new.window);
END;`,
	},
}

// runMigrations applies every migration whose version exceeds the database's
// current user_version, each in its own transaction.
func (s *Store) runMigrations(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d (SQLite must include FTS5): %w", m.Version, err)
		}
		// user_version does not accept a bound parameter.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record schema version %d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}
		current = m.Version
	}
	return nil
}
```

- [ ] **Step 4: Delegate `migrate` to the runner**

In `internal/store/store.go`, replace the entire `migrate` method (starting at line 74, through the closing brace of the function) with:

```go
func (s *Store) migrate(ctx context.Context) error {
	return s.runMigrations(ctx)
}
```

Leave the call site in `Open` (line 65) untouched.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS — the three new `TestMigrate*` tests plus every pre-existing store test.

- [ ] **Step 6: Verify the whole suite and vet**

Run: `go vet ./... && go test ./...`
Expected: no vet output; all packages `ok` or `no test files`.

- [ ] **Step 7: Commit**

```bash
git add internal/store/migrations.go internal/store/migrations_test.go internal/store/store.go
git commit -m "feat(store): add versioned migrations tracked by user_version"
```

---

### Task 2: App and window search filters

**Files:**
- Modify: `internal/store/migrations.go` (append migration version 2)
- Modify: `internal/store/store.go:35-42` (`SearchOptions`), and `Search` (from line 142)
- Modify: `internal/store/store_test.go` (append tests)
- Modify: `internal/cli/root.go:154,166,189` (`searchCommand`), `:198,210,236` (`askCommand`), `:297-298` (`searchOptions`)

**Interfaces:**
- Consumes: `migrations` slice and `migration` struct from Task 1.
- Produces:
  - `store.SearchOptions` gains `App string` and `Window string`. **The existing `Match MatchMode` field must be preserved** — it is load-bearing for `ask`'s staged retrieval.
  - `func likeEscape(s string) string` in package `store`.
  - `func searchOptions(query, kind, since, until, app, window string, limit int) (store.SearchOptions, error)` in package `cli` — **note the two new parameters, inserted before `limit`**. Both existing call sites must be updated.

**Semantics (decided, do not re-litigate):** `App` is an exact, case-insensitive match — you know the app name you want. `Window` is a case-insensitive substring match — window titles are long and volatile. Both apply to the recency path (empty query) as well as the FTS path.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`:

```go
func TestSearchFiltersByApp(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	events := []Event{
		{Kind: KindScreen, CapturedAt: now, Text: "deploy the gateway", App: "Ghostty", Window: "zsh — lumi", MediaPath: "a.jpg"},
		{Kind: KindScreen, CapturedAt: now.Add(time.Second), Text: "deploy the gateway", App: "Arc", Window: "GitHub — pull request", MediaPath: "b.jpg"},
	}
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}

	// Exact app match, case-insensitive.
	got, err := s.Search(ctx, SearchOptions{Query: "deploy", App: "ghostty"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].App != "Ghostty" {
		t.Fatalf("app filter should return only the Ghostty event, got %#v", got)
	}

	// App filter must also apply with no query (recency path).
	got, err = s.Search(ctx, SearchOptions{App: "Arc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].App != "Arc" {
		t.Fatalf("app filter should apply without a query, got %#v", got)
	}

	// Partial app names must not match.
	got, err = s.Search(ctx, SearchOptions{App: "Ghost"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("app filter is exact, expected no results, got %#v", got)
	}
}

// The filters must compose with MatchAny, which is the mode ask's second
// retrieval stage uses.
func TestSearchFiltersApplyUnderMatchAny(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events := []Event{
		{Kind: KindScreen, Text: "gateway deploy notes", App: "Ghostty", MediaPath: "a.jpg"},
		{Kind: KindScreen, Text: "gateway deploy notes", App: "Arc", MediaPath: "b.jpg"},
	}
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Search(ctx, SearchOptions{Query: "gateway rollback", Match: MatchAny, App: "Arc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].App != "Arc" {
		t.Fatalf("app filter must narrow a MatchAny query, got %#v", got)
	}
}

func TestSearchFiltersByWindowSubstring(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events := []Event{
		{Kind: KindScreen, Text: "one", App: "Arc", Window: "GitHub — pull request #12", MediaPath: "a.jpg"},
		{Kind: KindScreen, Text: "two", App: "Arc", Window: "Linear — LUM-4", MediaPath: "b.jpg"},
	}
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Search(ctx, SearchOptions{Window: "pull request"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "one" {
		t.Fatalf("window substring filter failed, got %#v", got)
	}
}

// A window filter containing LIKE wildcards must be treated as literal text.
func TestSearchWindowFilterEscapesWildcards(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	e := Event{Kind: KindScreen, Text: "one", Window: "Inbox — 12 unread", MediaPath: "a.jpg"}
	if err := s.Insert(ctx, &e); err != nil {
		t.Fatal(err)
	}

	got, err := s.Search(ctx, SearchOptions{Window: "%unread"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("%% must be literal, not a wildcard, got %#v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestSearchFilters|TestSearchWindow' -v`
Expected: FAIL — `unknown field 'App' in struct literal of type SearchOptions` (compile error).

- [ ] **Step 3: Append the index migration**

In `internal/store/migrations.go`, append to the `migrations` slice, after the version 1 entry:

```go
	{
		Version: 2,
		SQL: `
CREATE INDEX IF NOT EXISTS events_app_captured_at_idx
  ON events(app COLLATE NOCASE, captured_at DESC);`,
	},
```

- [ ] **Step 4: Add the fields and filter clauses**

In `internal/store/store.go`, replace the `SearchOptions` struct (lines 35-42) with:

```go
type SearchOptions struct {
	Query  string
	Match  MatchMode
	Kind   Kind
	App    string // exact, case-insensitive
	Window string // substring, case-insensitive
	Since  *time.Time
	Until  *time.Time
	Limit  int
}
```

In `Search`, immediately after the `opts.Kind` predicate block and before the `opts.Since` block, insert:

```go
	if app := strings.TrimSpace(opts.App); app != "" {
		where = append(where, "e.app = ? COLLATE NOCASE")
		args = append(args, app)
	}
	if window := strings.TrimSpace(opts.Window); window != "" {
		where = append(where, `e.window LIKE ? ESCAPE '\' COLLATE NOCASE`)
		args = append(args, "%"+likeEscape(window)+"%")
	}
```

Add `likeEscape` to `internal/store/store.go` (not `query.go`, which is owned by the retrieval work):

```go
// likeEscape neutralises LIKE wildcards so a filter value is matched literally.
// Pair it with an ESCAPE '\' clause.
func likeEscape(input string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(input)
}
```

- [ ] **Step 5: Run the store tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS — all store tests including the four new filter tests.

- [ ] **Step 6: Thread the flags through the CLI**

In `internal/cli/root.go`, change the `searchOptions` helper (line 297) to:

```go
func searchOptions(query, kind, since, until, app, window string, limit int) (store.SearchOptions, error) {
	opts := store.SearchOptions{Query: query, App: app, Window: window, Limit: limit}
```

The rest of the function body is unchanged.

In `searchCommand`, change the var declaration (line 154) to:

```go
	var kind, since, until, app, window string
```

update the call (line 166) to:

```go
				opts, err := searchOptions(query, kind, since, until, app, window, limit)
```

and add two flags after the `--type` flag registration (line 189):

```go
	flags.StringVar(&app, "app", "", "only events captured from this application (exact, case-insensitive)")
	flags.StringVar(&window, "window", "", "only events whose window title contains this text")
```

In `askCommand`, change the var declaration (line 198) to:

```go
	var since, model, app, window string
```

update the call (line 210) to:

```go
				opts, err := searchOptions("", "all", since, "", app, window, limit)
```

and register the flags after the `--since` flag (line 236):

```go
	cmd.Flags().StringVar(&app, "app", "", "restrict activity to this application")
	cmd.Flags().StringVar(&window, "window", "", "restrict activity to windows whose title contains this text")
```

No change is needed in `internal/cli/retrieve.go`. `retrieveContext` takes `opts` by value and mutates only `Query` and `Match`, so the app/window predicates automatically apply to all three retrieval stages — same as the existing kind and time predicates.

- [ ] **Step 7: Verify the build, vet, and full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: build succeeds, no vet output, all packages pass — including the existing `internal/cli` retrieval tests, which must not regress.

- [ ] **Step 8: Commit**

```bash
git add internal/store/migrations.go internal/store/store.go internal/store/store_test.go internal/cli/root.go
git commit -m "feat(search): filter by app and window title"
```

---

### Task 3: Retention and the prune command

**Files:**
- Create: `internal/retention/retention.go`
- Create: `internal/retention/retention_test.go`
- Modify: `internal/store/store.go` (add `Expired` and `DeleteByIDs`)
- Modify: `internal/cli/root.go` (add `pruneCommand`, register it on the `AddCommand` line at 66)

**Interfaces:**
- Consumes: `store.Store`, `store.Event`, and the migration system from Task 1.
- Produces:
  - `func (s *Store) Expired(ctx context.Context, before time.Time, limit int) ([]Event, error)`
  - `func (s *Store) DeleteByIDs(ctx context.Context, ids []int64) (int64, error)`
  - `type retention.Options struct { Before *time.Time; MaxBytes int64; DryRun bool }`
  - `type retention.Result struct { Events int64; Bytes int64; MissingFiles int }`
  - `func retention.Prune(ctx context.Context, s *store.Store, opts Options) (Result, error)`

**Policy (decided, do not re-litigate):** Age pruning runs first, then size pruning deletes oldest-first until the total media footprint is at or under `MaxBytes`. A media file that is already gone from disk is not an error — the row is still deleted and `MissingFiles` is incremented. Row deletion happens *before* file unlinking, so a crash mid-prune leaves orphaned files (recoverable) rather than rows pointing at missing media (not recoverable). Deleting rows removes the FTS entries automatically via the `events_ad` trigger — do not touch `events_fts` directly.

- [ ] **Step 1: Write the failing store tests**

Append to `internal/store/store_test.go`:

```go
func TestExpiredAndDeleteByIDs(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	old := Event{Kind: KindScreen, CapturedAt: now.Add(-48 * time.Hour), Text: "ancient", MediaPath: "old.jpg"}
	fresh := Event{Kind: KindScreen, CapturedAt: now, Text: "current", MediaPath: "new.jpg"}
	for _, e := range []*Event{&old, &fresh} {
		if err := s.Insert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	expired, err := s.Expired(ctx, now.Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != old.ID {
		t.Fatalf("expected only the 48h-old event, got %#v", expired)
	}

	deleted, err := s.DeleteByIDs(ctx, []int64{old.ID})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteByIDs = %d, want 1", deleted)
	}

	// The FTS index must have been cleaned by the delete trigger.
	got, err := s.Search(ctx, SearchOptions{Query: "ancient"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted event must not remain searchable, got %#v", got)
	}

	remaining, err := s.Search(ctx, SearchOptions{Query: "current"})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("the fresh event must survive, got %#v", remaining)
	}
}

func TestDeleteByIDsWithNoIDs(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	deleted, err := s.DeleteByIDs(ctx, nil)
	if err != nil {
		t.Fatalf("deleting nothing must not error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("DeleteByIDs(nil) = %d, want 0", deleted)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestExpired|TestDeleteByIDs' -v`
Expected: FAIL — `s.Expired undefined` (compile error).

- [ ] **Step 3: Implement the store methods**

Append to `internal/store/store.go`:

```go
// Expired returns events captured strictly before the cutoff, oldest first.
// A limit of zero or less means no limit.
func (s *Store) Expired(ctx context.Context, before time.Time, limit int) ([]Event, error) {
	query := `SELECT id, kind, captured_at, text, app, window, media_path, duration_ms, metadata_json
FROM events WHERE captured_at < ? ORDER BY captured_at ASC`
	args := []any{before.UTC().Format(time.RFC3339Nano)}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select expired events: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var capturedAt, metadata string
		if err := rows.Scan(&event.ID, &event.Kind, &capturedAt, &event.Text, &event.App,
			&event.Window, &event.MediaPath, &event.DurationMS, &metadata); err != nil {
			return nil, fmt.Errorf("scan expired event: %w", err)
		}
		event.CapturedAt, err = time.Parse(time.RFC3339Nano, capturedAt)
		if err != nil {
			return nil, fmt.Errorf("parse event timestamp %q: %w", capturedAt, err)
		}
		event.Metadata = json.RawMessage(metadata)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired events: %w", err)
	}
	return events, nil
}

// DeleteByIDs removes events and, via the events_ad trigger, their FTS rows.
// It does not touch media files; callers own that.
func (s *Store) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM events WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return 0, fmt.Errorf("delete events: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted events: %w", err)
	}
	return deleted, nil
}
```

- [ ] **Step 4: Run the store tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS — all store tests.

- [ ] **Step 5: Commit the store layer**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): add Expired and DeleteByIDs for retention"
```

- [ ] **Step 6: Write the failing retention test**

Create `internal/retention/retention_test.go`:

```go
package retention

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// seed inserts an event whose media file is sizeBytes long and returns its path.
func seed(t *testing.T, ctx context.Context, s *store.Store, dir, name string, at time.Time, sizeBytes int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, sizeBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	e := store.Event{Kind: store.KindScreen, CapturedAt: at, Text: name, MediaPath: path}
	if err := s.Insert(ctx, &e); err != nil {
		t.Fatal(err)
	}
	return path
}

func newStore(t *testing.T, ctx context.Context) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestPruneByAgeDeletesRowsAndFiles(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	now := time.Now().UTC()

	oldPath := seed(t, ctx, s, dir, "old.jpg", now.Add(-48*time.Hour), 10)
	freshPath := seed(t, ctx, s, dir, "fresh.jpg", now, 10)

	cutoff := now.Add(-24 * time.Hour)
	result, err := Prune(ctx, s, Options{Before: &cutoff})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 {
		t.Fatalf("Result.Events = %d, want 1", result.Events)
	}
	if result.Bytes != 10 {
		t.Fatalf("Result.Bytes = %d, want 10", result.Bytes)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired media file should have been removed, stat err = %v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh media file must survive: %v", err)
	}

	remaining, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].MediaPath != freshPath {
		t.Fatalf("expected only the fresh event to remain, got %#v", remaining)
	}
}

func TestPruneDryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	now := time.Now().UTC()
	oldPath := seed(t, ctx, s, dir, "old.jpg", now.Add(-48*time.Hour), 10)

	cutoff := now.Add(-24 * time.Hour)
	result, err := Prune(ctx, s, Options{Before: &cutoff, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.Bytes != 10 {
		t.Fatalf("dry run should report what it would delete, got %#v", result)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("dry run must not delete files: %v", err)
	}
	remaining, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("dry run must not delete rows, got %d rows", len(remaining))
	}
}

func TestPruneBySizeDeletesOldestFirst(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	now := time.Now().UTC()

	seed(t, ctx, s, dir, "a.jpg", now.Add(-3*time.Hour), 100)
	seed(t, ctx, s, dir, "b.jpg", now.Add(-2*time.Hour), 100)
	keep := seed(t, ctx, s, dir, "c.jpg", now.Add(-1*time.Hour), 100)

	// 300 bytes on disk, cap at 150 => the two oldest must go.
	result, err := Prune(ctx, s, Options{MaxBytes: 150})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 2 {
		t.Fatalf("Result.Events = %d, want 2", result.Events)
	}
	remaining, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].MediaPath != keep {
		t.Fatalf("expected only the newest event to remain, got %#v", remaining)
	}
}

func TestPruneToleratesMissingMediaFiles(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	now := time.Now().UTC()

	oldPath := seed(t, ctx, s, dir, "old.jpg", now.Add(-48*time.Hour), 10)
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-24 * time.Hour)
	result, err := Prune(ctx, s, Options{Before: &cutoff})
	if err != nil {
		t.Fatalf("a missing media file must not fail the prune: %v", err)
	}
	if result.Events != 1 {
		t.Fatalf("Result.Events = %d, want 1", result.Events)
	}
	if result.MissingFiles != 1 {
		t.Fatalf("Result.MissingFiles = %d, want 1", result.MissingFiles)
	}
}

func TestPruneWithNoPolicyIsAnError(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, ctx)
	if _, err := Prune(ctx, s, Options{}); err == nil {
		t.Fatal("prune with neither Before nor MaxBytes must return an error")
	}
}
```

- [ ] **Step 7: Run the test to verify it fails**

Run: `go test ./internal/retention/ -v`
Expected: FAIL — `undefined: Prune` (compile error).

- [ ] **Step 8: Implement the retention package**

Create `internal/retention/retention.go`:

```go
// Package retention enforces Lumi's data-retention policy: it removes old
// events and the media files they point at.
//
// Rows are deleted before their files are unlinked. A crash between the two
// leaves orphaned files on disk, which is recoverable; the reverse order would
// leave rows referencing media that no longer exists, which is not.
package retention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

type Options struct {
	// Before deletes every event captured strictly before this instant.
	Before *time.Time
	// MaxBytes caps the total size of media files on disk. Oldest events are
	// deleted until the footprint fits. Zero disables size-based pruning.
	MaxBytes int64
	// DryRun reports what would be deleted without deleting anything.
	DryRun bool
}

type Result struct {
	Events       int64 `json:"events"`
	Bytes        int64 `json:"bytes"`
	MissingFiles int   `json:"missing_files"`
}

// Prune applies the age policy first, then the size policy.
func Prune(ctx context.Context, s *store.Store, opts Options) (Result, error) {
	var result Result
	if opts.Before == nil && opts.MaxBytes <= 0 {
		return result, errors.New("prune requires --older-than or --max-bytes")
	}

	if opts.Before != nil {
		expired, err := s.Expired(ctx, *opts.Before, 0)
		if err != nil {
			return result, err
		}
		partial, err := remove(ctx, s, expired, opts.DryRun)
		result.add(partial)
		if err != nil {
			return result, err
		}
	}

	if opts.MaxBytes > 0 {
		// After age pruning, walk everything oldest-first and drop until the
		// remaining footprint fits under the cap. A cutoff an hour in the
		// future is simply "every row".
		all, err := s.Expired(ctx, time.Now().UTC().Add(time.Hour), 0)
		if err != nil {
			return result, err
		}
		var total int64
		sizes := make([]int64, len(all))
		for i, event := range all {
			sizes[i] = fileSize(event.MediaPath)
			total += sizes[i]
		}
		overBy := total - opts.MaxBytes
		if opts.DryRun {
			// A dry run has not actually freed the age-pruned bytes yet, so
			// discount them here to report the same outcome a real run gives.
			overBy -= result.Bytes
		}
		if overBy > 0 {
			cutoff := 0
			var freed int64
			for cutoff < len(all) && freed < overBy {
				freed += sizes[cutoff]
				cutoff++
			}
			partial, err := remove(ctx, s, all[:cutoff], opts.DryRun)
			result.add(partial)
			if err != nil {
				return result, err
			}
		}
	}

	return result, nil
}

func remove(ctx context.Context, s *store.Store, events []store.Event, dryRun bool) (Result, error) {
	var result Result
	if len(events) == 0 {
		return result, nil
	}
	ids := make([]int64, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
		if size := fileSize(event.MediaPath); size > 0 {
			result.Bytes += size
		} else if !exists(event.MediaPath) {
			result.MissingFiles++
		}
	}
	if dryRun {
		result.Events = int64(len(ids))
		return result, nil
	}
	deleted, err := s.DeleteByIDs(ctx, ids)
	result.Events = deleted
	if err != nil {
		return result, err
	}
	for _, event := range events {
		if err := os.Remove(event.MediaPath); err != nil && !os.IsNotExist(err) {
			return result, fmt.Errorf("remove media %s: %w", event.MediaPath, err)
		}
	}
	return result, nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (r *Result) add(other Result) {
	r.Events += other.Events
	r.Bytes += other.Bytes
	r.MissingFiles += other.MissingFiles
}
```

- [ ] **Step 9: Run the retention tests to verify they pass**

Run: `go test ./internal/retention/ -v`
Expected: PASS — all five tests.

- [ ] **Step 10: Add the prune command**

In `internal/cli/root.go`, add the import `"github.com/puremetricsai/lumi/internal/retention"` to the import block, then append this method:

```go
func (a *app) pruneCommand() *cobra.Command {
	var olderThan string
	var maxBytes int64
	var dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete old events and their media files",
		Long: "Delete indexed events and the screenshots or audio they point at.\n" +
			"--older-than takes a Go duration (720h) or an RFC3339 timestamp; Go durations have no 'd' unit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := retention.Options{MaxBytes: maxBytes, DryRun: dryRun}
			if olderThan != "" {
				before, err := parseTime(olderThan, true)
				if err != nil {
					return fmt.Errorf("parse --older-than: %w", err)
				}
				opts.Before = before
			}
			s, _, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			result, err := retention.Prune(cmd.Context(), s, opts)
			if err != nil {
				return err
			}
			if asJSON {
				encoder := json.NewEncoder(os.Stdout)
				encoder.SetIndent("", "  ")
				return encoder.Encode(result)
			}
			verb := "deleted"
			if dryRun {
				verb = "would delete"
			}
			fmt.Fprintf(os.Stdout, "%s %d events, %.1f MiB of media\n",
				verb, result.Events, float64(result.Bytes)/(1024*1024))
			if result.MissingFiles > 0 {
				fmt.Fprintf(os.Stdout, "%d events referenced media that was already gone\n", result.MissingFiles)
			}
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&olderThan, "older-than", "", "delete events older than this duration (e.g. 720h) or RFC3339 time")
	flags.Int64Var(&maxBytes, "max-bytes", 0, "cap total media size in bytes, deleting oldest first (zero disables)")
	flags.BoolVar(&dryRun, "dry-run", false, "report what would be deleted without deleting")
	flags.BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}
```

Register it by adding `a.pruneCommand()` to the `cmd.AddCommand(...)` call for the subcommands (line 66), before `a.doctorCommand()`.

- [ ] **Step 11: Verify the build, vet, and full suite**

Run: `go build -o lumi ./cmd/lumi && go vet ./... && go test ./...`
Expected: build succeeds, no vet output, all packages pass.

- [ ] **Step 12: Manual smoke test**

Run:
```bash
./lumi prune --older-than 720h --dry-run --data-dir /tmp/lumi-smoke
```
Expected: `would delete 0 events, 0.0 MiB of media` (an empty database is created at `/tmp/lumi-smoke`). Then confirm the policy guard:
```bash
./lumi prune --data-dir /tmp/lumi-smoke
```
Expected: exits non-zero with `Error: prune requires --older-than or --max-bytes`.

- [ ] **Step 13: Commit**

```bash
git add internal/retention/ internal/cli/root.go
git commit -m "feat(cli): add prune command with age and size retention"
```

---

## Documentation

- [ ] **Update `CLAUDE.md`**

Add `prune` to the CLI command list in the `internal/cli` architecture paragraph. In "Invariants worth preserving", add:

```markdown
- **Schema changes go through `internal/store/migrations.go`.** Append a new `migration` with the next version number; never edit shipped SQL. The applied version lives in SQLite's `user_version` pragma.
- **Pruning deletes rows before files.** Orphaned files are recoverable; rows pointing at missing media are not. `lumi prune` is the only code path permitted to delete media.
```

Also correct the existing line in the `internal/store` architecture paragraph stating there is no versioned migration system.

- [ ] **Commit**

```bash
git add CLAUDE.md
git commit -m "docs: describe migrations and retention invariants"
```

---

## Self-Review Notes

- **Not covered by design:** automatic pruning on a schedule (`lumi record` does not prune) — that belongs with the Tier 2 daemon work, since a foreground recorder has nowhere sensible to hang an hourly timer.
- **Known cost:** `Prune` with `--max-bytes` calls `os.Stat` once per event in the database. At ~17k events/day this is acceptable for a manual command but would need a `size_bytes` column if it ever becomes a background task. That column is a natural migration version 3.
- **`--older-than` accepts Go durations only**, so days must be expressed in hours (`720h`, not `30d`). This mirrors `--since` on `search`, which has the same constraint via the shared `parseTime`.
