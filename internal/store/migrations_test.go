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
