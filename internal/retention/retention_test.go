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

// A dry run that combines an age policy and a size policy must report exactly
// what a real run would delete — no double-counting of the age-expired events
// that the size stage re-sees only because dry run never deleted them.
func TestPruneDryRunCombinedAgeAndSize(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	now := time.Now().UTC()

	oldPath := seed(t, ctx, s, dir, "old.jpg", now.Add(-48*time.Hour), 10)
	freshPath := seed(t, ctx, s, dir, "fresh.jpg", now, 100)

	cutoff := now.Add(-24 * time.Hour)
	// Age prunes old (10B). Size cap 50 then forces fresh (100B) out too.
	// A real run deletes 2 events / 110 bytes; the dry run must report the same.
	result, err := Prune(ctx, s, Options{Before: &cutoff, MaxBytes: 50, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 2 || result.Bytes != 110 {
		t.Fatalf("dry run must report what a real run would delete, got %#v", result)
	}
	// Dry run must not have deleted anything.
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("dry run must not delete files: %v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("dry run must not delete files: %v", err)
	}
	remaining, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("dry run must not delete rows, got %d", len(remaining))
	}
}

func TestPruneWithNoPolicyIsAnError(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, ctx)
	if _, err := Prune(ctx, s, Options{}); err == nil {
		t.Fatal("prune with neither Before nor MaxBytes must return an error")
	}
}
