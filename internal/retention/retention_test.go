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

// mediaDir creates a dedicated media subdirectory alongside the database,
// mirroring production where Paths.Screenshots/Paths.Audio are separate from
// the SQLite file — so the --all orphan sweep never sees the db.
func mediaDir(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "media")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
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

func TestPruneAllDeletesEverything(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	now := time.Now().UTC()

	// Include an event captured in the future to prove All is not a stale
	// "before now" cutoff in disguise.
	oldPath := seed(t, ctx, s, dir, "old.jpg", now.Add(-48*time.Hour), 10)
	freshPath := seed(t, ctx, s, dir, "fresh.jpg", now, 10)
	futurePath := seed(t, ctx, s, dir, "future.jpg", now.Add(48*time.Hour), 10)

	result, err := Prune(ctx, s, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 3 {
		t.Fatalf("Result.Events = %d, want 3", result.Events)
	}
	if result.Bytes != 30 {
		t.Fatalf("Result.Bytes = %d, want 30", result.Bytes)
	}
	for _, p := range []string{oldPath, freshPath, futurePath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("--all must remove media file %s, stat err = %v", p, err)
		}
	}
	remaining, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("--all must delete every row, got %d", len(remaining))
	}
}

func TestPruneAllDryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	now := time.Now().UTC()
	oldPath := seed(t, ctx, s, dir, "old.jpg", now.Add(-48*time.Hour), 10)
	freshPath := seed(t, ctx, s, dir, "fresh.jpg", now, 10)

	result, err := Prune(ctx, s, Options{All: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 2 || result.Bytes != 20 {
		t.Fatalf("dry run should report what --all would delete, got %#v", result)
	}
	for _, p := range []string{oldPath, freshPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry run must not delete files: %v", err)
		}
	}
	remaining, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("dry run must not delete rows, got %d", len(remaining))
	}
}

// --all must remove media files that no event row references — a failed Insert
// or a crash between a prior prune's row delete and file unlink. The wipe is a
// privacy guarantee, so a stray file left on disk is a leak.
func TestPruneAllRemovesOrphanedMedia(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	media := mediaDir(t, dir)
	now := time.Now().UTC()

	indexed := seed(t, ctx, s, media, "indexed.jpg", now, 10)
	// An orphan: a file on disk with no corresponding row.
	orphan := filepath.Join(media, "orphan.jpg")
	if err := os.WriteFile(orphan, make([]byte, 7), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Prune(ctx, s, Options{All: true, MediaDirs: []string{media}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 {
		t.Fatalf("Result.Events = %d, want 1", result.Events)
	}
	if result.OrphanFiles != 1 {
		t.Fatalf("Result.OrphanFiles = %d, want 1", result.OrphanFiles)
	}
	// Bytes must include both the indexed file (10) and the orphan (7).
	if result.Bytes != 17 {
		t.Fatalf("Result.Bytes = %d, want 17 (indexed + orphan)", result.Bytes)
	}
	for _, p := range []string{indexed, orphan} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("--all must remove %s, stat err = %v", p, err)
		}
	}
}

// --all must delete a row whose captured_at is far in the future. The former
// "now + 1000 years" cutoff used a strict `<` compare and would have missed it.
func TestPruneAllRemovesFarFutureRow(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	media := mediaDir(t, dir)

	future := time.Date(3500, 1, 1, 0, 0, 0, 0, time.UTC)
	futurePath := seed(t, ctx, s, media, "future.jpg", future, 10)

	result, err := Prune(ctx, s, Options{All: true, MediaDirs: []string{media}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 {
		t.Fatalf("Result.Events = %d, want 1", result.Events)
	}
	if _, err := os.Stat(futurePath); !os.IsNotExist(err) {
		t.Fatalf("--all must remove the far-future event's media, stat err = %v", err)
	}
	remaining, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("--all must delete the far-future row, got %d", len(remaining))
	}
}

// A dry run under --all must delete nothing — neither indexed files nor orphans
// — yet report exactly what a real run would remove.
func TestPruneAllDryRunReportsOrphansWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	s, dir := newStore(t, ctx)
	media := mediaDir(t, dir)
	now := time.Now().UTC()

	indexed := seed(t, ctx, s, media, "indexed.jpg", now, 10)
	orphan := filepath.Join(media, "orphan.jpg")
	if err := os.WriteFile(orphan, make([]byte, 7), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Prune(ctx, s, Options{All: true, MediaDirs: []string{media}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.OrphanFiles != 1 || result.Bytes != 17 {
		t.Fatalf("dry run must report what --all would delete, got %#v", result)
	}
	for _, p := range []string{indexed, orphan} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry run must not delete %s: %v", p, err)
		}
	}
	remaining, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
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
