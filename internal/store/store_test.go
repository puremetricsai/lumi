package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestInsertAndSearch(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
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
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
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
