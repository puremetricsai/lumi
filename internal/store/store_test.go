package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// newTestStore opens a store in a temp directory and closes it with the test,
// mirroring internal/retention's helper of the same shape.
func newTestStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInsertAndSearch(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	events := []Event{
		{Kind: KindScreen, CapturedAt: now, Text: "Quarterly roadmap review", App: "Arc", MediaPath: "/tmp/a.jpg"},
		{Kind: KindAudio, CapturedAt: now.Add(time.Second), Text: "Discuss the launch budget", MediaPath: "/tmp/a.wav"},
	}
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Search(ctx, SearchOptions{Query: "launch budget", Kind: KindAudio, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != KindAudio || got[0].Text != events[1].Text {
		t.Fatalf("unexpected results: %#v", got)
	}

	recent, err := s.Search(ctx, SearchOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].ID != events[1].ID {
		t.Fatalf("expected newest event, got %#v", recent)
	}
}

func TestSearchTreatsFTSSyntaxAsText(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := Event{Kind: KindScreen, Text: `issue (alpha) "quoted"`, MediaPath: "test.jpg"}
	if err := s.Insert(ctx, &e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(ctx, SearchOptions{Query: `(alpha) "quoted"`}); err != nil {
		t.Fatalf("punctuation should not create an invalid MATCH query: %v", err)
	}
}

func TestSearchFiltersByApp(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
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

// The filters must compose with MatchAny, the any-term mode retained for
// `lumi mcp`.
func TestSearchFiltersApplyUnderMatchAny(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
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
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
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

func TestExpiredAndDeleteByIDs(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
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

// AllEvents returns every row oldest-first with no cutoff — including one
// timestamped far in the future that a bounded Expired cutoff would miss.
func TestAllEventsReturnsEveryRow(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	old := Event{Kind: KindScreen, CapturedAt: now.Add(-48 * time.Hour), Text: "ancient", MediaPath: "old.jpg"}
	fresh := Event{Kind: KindScreen, CapturedAt: now, Text: "current", MediaPath: "new.jpg"}
	future := Event{Kind: KindScreen, CapturedAt: time.Date(3500, 1, 1, 0, 0, 0, 0, time.UTC), Text: "future", MediaPath: "future.jpg"}
	for _, e := range []*Event{&fresh, &old, &future} {
		if err := s.Insert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.AllEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("AllEvents returned %d events, want 3", len(all))
	}
	// Oldest first: old, fresh, future.
	wantOrder := []int64{old.ID, fresh.ID, future.ID}
	for i, want := range wantOrder {
		if all[i].ID != want {
			t.Fatalf("AllEvents[%d].ID = %d, want %d (expected oldest-first order)", i, all[i].ID, want)
		}
	}
}

func TestDeleteByIDsWithNoIDs(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
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

// A window filter containing LIKE wildcards must be treated as literal text.
func TestSearchWindowFilterEscapesWildcards(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
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

func TestDeleteByIDsBatchesLargeIDSets(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Three real rows to delete.
	var realIDs []int64
	for i := 0; i < 3; i++ {
		e := Event{Kind: KindScreen, Text: "row", MediaPath: "x.jpg"}
		if err := s.Insert(ctx, &e); err != nil {
			t.Fatal(err)
		}
		realIDs = append(realIDs, e.ID)
	}

	// Far more than SQLite's SQLITE_MAX_VARIABLE_NUMBER (32766) ids in one call.
	// An unbatched IN (?, ...) fails with "too many SQL variables"; batching must
	// tolerate it and delete exactly the real rows.
	ids := append([]int64{}, realIDs...)
	for id := int64(1_000_000); len(ids) < 40000; id++ {
		ids = append(ids, id)
	}

	deleted, err := s.DeleteByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("DeleteByIDs must batch to stay under the variable limit: %v", err)
	}
	if deleted != int64(len(realIDs)) {
		t.Fatalf("DeleteByIDs = %d, want %d", deleted, len(realIDs))
	}
	got, err := s.Search(ctx, SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("all real rows should be deleted, got %d remaining", len(got))
	}
}

// RequireText is retained for `lumi mcp`: a saved-but-untranscribed audio chunk
// answers no content question, and Lumi keeps enough of them that they crowd
// real transcripts out of a recency pass. Whitespace-only text counts as absent.
func TestSearchRequireTextDropsEmptyAndBlankText(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events := []Event{
		{Kind: KindAudio, Text: "", MediaPath: "silent.wav", AudioSource: "system"},
		{Kind: KindAudio, Text: "   \n\t ", MediaPath: "blank.wav", AudioSource: "system"},
		{Kind: KindAudio, Text: "ship the retrieval fix", MediaPath: "speech.wav", AudioSource: "microphone"},
	}
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Search(ctx, SearchOptions{Kind: KindAudio, RequireText: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MediaPath != "speech.wav" {
		t.Fatalf("RequireText must keep only transcript-bearing events, got %#v", got)
	}

	got, err = s.Search(ctx, SearchOptions{Kind: KindAudio})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("RequireText must be opt-in; unfiltered search returned %d events, want 3", len(got))
	}
}

func TestEventByIDReturnsTheStoredEvent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	event := Event{
		Kind:       KindScreen,
		CapturedAt: time.Now().UTC().Truncate(time.Second),
		Text:       "Quarterly roadmap review",
		App:        "Safari",
		Window:     "Quarterly plan",
		MediaPath:  "/tmp/a.jpg",
		TextSource: "vision",
		DisplayID:  1,
		Metadata:   []byte(`{"ocr_ms":42}`),
	}
	if err := s.Insert(ctx, &event); err != nil {
		t.Fatal(err)
	}

	got, err := s.EventByID(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != event.ID || got.Text != event.Text || got.App != event.App {
		t.Fatalf("unexpected event: %#v", got)
	}
	if string(got.Metadata) != `{"ocr_ms":42}` {
		t.Fatalf("metadata was not preserved: %s", got.Metadata)
	}
	if !got.CapturedAt.Equal(event.CapturedAt) {
		t.Fatalf("captured_at = %s, want %s", got.CapturedAt, event.CapturedAt)
	}
}

// TestEventByIDUnknownIDIsNotFound pins the sentinel: `lumi mcp`'s get_event
// has to tell an agent "no such id" rather than return an empty result, and it
// distinguishes that from a real query failure with errors.Is.
func TestEventByIDUnknownIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.EventByID(ctx, 4711)
	if got != nil {
		t.Fatalf("expected no event, got %#v", got)
	}
	if !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("error = %v, want ErrEventNotFound", err)
	}
	if !strings.Contains(err.Error(), "4711") {
		t.Fatalf("error %q does not name the missing id", err)
	}
}

func TestHasEventsReportsWhetherTheIndexHoldsAnything(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	has, err := s.HasEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("a fresh index reported that it holds events")
	}
	insertAll(t, ctx, s, Event{Kind: KindScreen, Text: "hello", CapturedAt: time.Now().UTC()})
	if has, err = s.HasEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("an index holding one event reported that it is empty")
	}
}

func TestUpdateMediaPathRepointsARow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	event := Event{Kind: KindScreen, CapturedAt: time.Now().UTC(), Text: "invoice total", MediaPath: "/media/frame.jpg"}
	if err := s.Insert(ctx, &event); err != nil {
		t.Fatal(err)
	}

	updated, err := s.UpdateMediaPath(ctx, event.ID, "/media/frame.jpg", "/media/frame.heic")
	if err != nil {
		t.Fatal(err)
	}
	if updated != 1 {
		t.Fatalf("repointed %d rows, want 1", updated)
	}
	stored, err := s.EventByID(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MediaPath != "/media/frame.heic" {
		t.Errorf("media path is %q, want the new one", stored.MediaPath)
	}
}

// The UPDATE fires events_au, which deletes and reinserts the row's whole FTS
// entry naming text, app and window unconditionally — even though a media path
// rewrite changes none of them. `lumi compress` accepts that churn; this is what
// makes "it re-syncs to the same values" a checked claim rather than an assumed
// one, because a search that stopped matching afterwards would be silent.
func TestUpdateMediaPathLeavesTheRowSearchable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	event := Event{
		Kind: KindScreen, CapturedAt: time.Now().UTC(),
		Text: "quarterly revenue projection", App: "Numbers", Window: "Q3.numbers",
		MediaPath: "/media/frame.jpg",
	}
	if err := s.Insert(ctx, &event); err != nil {
		t.Fatal(err)
	}
	before, err := s.Search(ctx, SearchOptions{Query: "quarterly revenue"})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("found %d events before repointing, want 1", len(before))
	}

	if _, err := s.UpdateMediaPath(ctx, event.ID, "/media/frame.jpg", "/media/frame.heic"); err != nil {
		t.Fatal(err)
	}

	after, err := s.Search(ctx, SearchOptions{Query: "quarterly revenue"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("found %d events after repointing, want 1 — the FTS re-sync lost the row", len(after))
	}
	if after[0].App != "Numbers" || after[0].Window != "Q3.numbers" {
		t.Errorf("repointing changed app/window to %q/%q", after[0].App, after[0].Window)
	}
	if after[0].MediaPath != "/media/frame.heic" {
		t.Errorf("search returned the old media path %q", after[0].MediaPath)
	}
}

func TestUpdateMediaPathSkipsARowAnotherWriterMoved(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	event := Event{Kind: KindScreen, CapturedAt: time.Now().UTC(), Text: "frame", MediaPath: "/media/frame.jpg"}
	if err := s.Insert(ctx, &event); err != nil {
		t.Fatal(err)
	}
	// Somebody else got there first.
	if _, err := s.UpdateMediaPath(ctx, event.ID, "/media/frame.jpg", "/media/elsewhere.jpg"); err != nil {
		t.Fatal(err)
	}

	updated, err := s.UpdateMediaPath(ctx, event.ID, "/media/frame.jpg", "/media/frame.heic")
	if err != nil {
		t.Fatalf("a lost compare-and-swap must not be an error: %v", err)
	}
	if updated != 0 {
		t.Errorf("repointed %d rows, want 0", updated)
	}
	stored, err := s.EventByID(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MediaPath != "/media/elsewhere.jpg" {
		t.Errorf("the winner's path was clobbered; row now names %q", stored.MediaPath)
	}
}

func TestUpdateMediaPathIgnoresAMissingRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	updated, err := s.UpdateMediaPath(ctx, 4242, "/media/frame.jpg", "/media/frame.heic")
	if err != nil {
		t.Fatal(err)
	}
	if updated != 0 {
		t.Errorf("repointed %d rows for an id that does not exist", updated)
	}
}

func TestVacuumRebuildsAnIdleDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "lumi.db")
	s, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 200; i++ {
		event := Event{
			Kind: KindScreen, CapturedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
			Text: strings.Repeat("indexed screen text ", 64), MediaPath: "/media/frame.jpg",
		}
		if err := s.Insert(ctx, &event); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.AllEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(all))
	for _, event := range all[:150] {
		ids = append(ids, event.ID)
	}
	if _, err := s.DeleteByIDs(ctx, ids); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() > before.Size() {
		t.Errorf("vacuum grew the file from %d to %d bytes", before.Size(), after.Size())
	}
	// The rows that survived have to survive it.
	remaining, err := s.AllEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 50 {
		t.Errorf("%d events survived the vacuum, want 50", len(remaining))
	}
}

// The ids `lumi compress` and `get_event` hold across a vacuum are safe only
// because events.id is INTEGER PRIMARY KEY, which makes it an alias for the
// rowid rather than a separate column. A table without that declaration would
// have its rows renumbered here, invalidating every reference held outside it.
//
// Rows are deleted from the middle first, on purpose. Without gaps in the id
// sequence a vacuum has nothing to renumber, so the test would pass just as
// happily for a table that *is* renumbered — which is the whole thing it exists
// to rule out.
func TestVacuumPreservesEventIDs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	var ids []int64
	for i := 0; i < 10; i++ {
		event := Event{
			Kind: KindScreen, CapturedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
			Text: "frame", MediaPath: "/media/frame.jpg",
		}
		if err := s.Insert(ctx, &event); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, event.ID)
	}
	// Leave holes: drop four rows from the middle of the sequence.
	if _, err := s.DeleteByIDs(ctx, []int64{ids[2], ids[3], ids[5], ids[6]}); err != nil {
		t.Fatal(err)
	}
	survivors := []int64{ids[0], ids[1], ids[4], ids[7], ids[8], ids[9]}

	if err := s.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}

	for _, id := range survivors {
		event, err := s.EventByID(ctx, id)
		if err != nil {
			t.Fatalf("event %d no longer resolves after a vacuum: %v", id, err)
		}
		if event.ID != id {
			t.Errorf("event resolved to id %d, want %d", event.ID, id)
		}
	}
	// And the gaps stay gaps rather than being filled by renumbered survivors.
	for _, id := range []int64{ids[2], ids[3], ids[5], ids[6]} {
		if _, err := s.EventByID(ctx, id); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("deleted id %d resolves after a vacuum, so rows were renumbered", id)
		}
	}
}

func TestVacuumBusyIsRecognisable(t *testing.T) {
	// The classifier, not the lock: provoking a real SQLITE_BUSY needs a second
	// process holding a write lock, and the property worth pinning is that a busy
	// error is reported as ErrVacuumBusy rather than as a failed run.
	if !errors.Is(fmt.Errorf("%w: x", ErrVacuumBusy), ErrVacuumBusy) {
		t.Error("ErrVacuumBusy does not survive wrapping")
	}
	for _, tc := range []struct {
		name string
		code sqlite3.ExtendedErrorCode
		want bool
	}{
		{"SQLITE_BUSY", 5, true},
		// Extended busy codes carry the primary code in their low byte;
		// an equality test would miss every one of them. The driver's
		// ErrorCode is a uint8, so the truncation that isolates the primary
		// code is the type's rather than an explicit mask — which is exactly
		// what this pins, since a widened type would silently stop matching.
		{"SQLITE_BUSY_SNAPSHOT", 517, true},
		{"SQLITE_BUSY_RECOVERY", 261, true},
		// Contention inside this connection, not another process holding the file.
		{"SQLITE_LOCKED", 6, false},
		{"SQLITE_CORRUPT", 11, false},
	} {
		if got := errors.Is(tc.code, sqlite3.BUSY); got != tc.want {
			t.Errorf("%s (%d) classified as busy=%v, want %v", tc.name, tc.code, got, tc.want)
		}
	}
}

// TestSearchBreaksCapturedAtTiesByDescendingID pins the tiebreaker that makes a
// browse page walkable. captured_at is not unique — an audio chunk's two tracks
// share one by construction, and 21% of live rows share theirs — so without
// e.id the order inside a tie group is whatever the query plan produced, and two
// identical calls can return different subsets of a group the LIMIT cuts
// through. `lumi mcp` hands the oldest captured_at on a page back as `until`,
// which needs the same boundary every time.
func TestSearchBreaksCapturedAtTiesByDescendingID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	base := time.Now().UTC().Truncate(time.Second)
	// Both inserted as slices: insertAll passes the slice's own array, so the IDs
	// Insert writes back are visible here. A single Event passed by value is not.
	older := []Event{{Kind: KindScreen, CapturedAt: base.Add(-time.Hour), Text: "older row"}}
	tie := []Event{
		{Kind: KindScreen, CapturedAt: base, Text: "tie one"},
		{Kind: KindScreen, CapturedAt: base, Text: "tie two"},
		{Kind: KindScreen, CapturedAt: base, Text: "tie three"},
	}
	insertAll(t, ctx, s, older...)
	insertAll(t, ctx, s, tie...)

	all, err := s.Search(ctx, SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d events, want 4", len(all))
	}
	// The tie group comes back newest-id-first, and the older row lands last —
	// proving the tiebreaker did not disturb the primary captured_at DESC key.
	for i, want := range []int64{tie[2].ID, tie[1].ID, tie[0].ID, older[0].ID} {
		if all[i].ID != want {
			t.Fatalf("position %d = id %d, want %d (order: %v)", i, all[i].ID, want, ids(all))
		}
	}

	// A cut THROUGH the tie group is exactly where an unspecified order shows.
	first, err := s.Search(ctx, SearchOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Search(ctx, SearchOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("limited calls returned %d and %d rows, want 2 each", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("two identical calls returned different pages: %v then %v", ids(first), ids(second))
		}
	}
	if first[0].ID != tie[2].ID || first[1].ID != tie[1].ID {
		t.Fatalf("page = %v, want the tie group's two highest ids", ids(first))
	}
}

// TestSearchBreaksRankedTiesByDescendingID: identical text at an identical
// captured_at scores an identical bm25 rank, so e.id is the only key left.
func TestSearchBreaksRankedTiesByDescendingID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, ctx)
	base := time.Now().UTC().Truncate(time.Second)
	// Filler with distinct timestamps keeps FTS5's IDF term from going
	// degenerate, the same reason TestSearchMatchAnyFindsPartialMatches carries it.
	for i := range 5 {
		insertAll(t, ctx, s, Event{Kind: KindScreen, CapturedAt: base.Add(-time.Duration(i+2) * time.Hour),
			Text: fmt.Sprintf("unrelated filler text number %d", i)})
	}
	tie := []Event{
		{Kind: KindScreen, CapturedAt: base, Text: "postgres index maintenance"},
		{Kind: KindScreen, CapturedAt: base, Text: "postgres index maintenance"},
	}
	insertAll(t, ctx, s, tie...)

	got, err := s.Search(ctx, SearchOptions{Query: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d hits, want 2", len(got))
	}
	if got[0].ID != tie[1].ID || got[1].ID != tie[0].ID {
		t.Fatalf("ranked tie order = %v, want descending id %v", ids(got), []int64{tie[1].ID, tie[0].ID})
	}
}

func ids(events []Event) []int64 {
	out := make([]int64, len(events))
	for i, event := range events {
		out[i] = event.ID
	}
	return out
}
