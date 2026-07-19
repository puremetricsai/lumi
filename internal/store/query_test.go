package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFtsExpression(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		mode  MatchMode
		want  string
	}{
		{"all joins with AND", "postgres indexes", MatchAll, `"postgres" AND "indexes"`},
		{"any joins with OR", "postgres indexes", MatchAny, `"postgres" OR "indexes"`},
		{"single term has no operator", "postgres", MatchAny, `"postgres"`},
		{"double quotes are escaped", `say "hi"`, MatchAll, `"say" AND """hi"""`},
		{"terms keep their punctuation as a phrase", "(alpha)", MatchAll, `"(alpha)"`},
		{"empty input", "", MatchAny, ""},
		{"whitespace only", "   \t\n ", MatchAll, ""},
		{"tokens with no alphanumerics are dropped", "??? ... !!!", MatchAny, ""},
		{"mixed tokens drop only the unusable ones", "??? alpha ...", MatchAny, `"alpha"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ftsExpression(tc.input, tc.mode); got != tc.want {
				t.Fatalf("ftsExpression(%q, %v) = %q, want %q", tc.input, tc.mode, got, tc.want)
			}
		})
	}
}

// TestSearchDefaultMatchModeIsAll guards `lumi search`: the zero value of
// SearchOptions.Match must keep today's conjunctive semantics.
func TestSearchDefaultMatchModeIsAll(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertAll(t, ctx, s,
		Event{Kind: KindScreen, Text: "postgres index tuning notes", MediaPath: "a.jpg"},
		Event{Kind: KindScreen, Text: "unrelated grocery list", MediaPath: "b.jpg"},
	)

	got, err := s.Search(ctx, SearchOptions{Query: "postgres tuning"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the both-terms event, got %d results", len(got))
	}
	got, err = s.Search(ctx, SearchOptions{Query: "postgres grocery"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("MatchAll must require every term, got %d results", len(got))
	}
}

// TestSearchMatchAnyFindsPartialMatches is the regression for the reported
// defect: a natural-language question ANDed together matches nothing, so `ask`
// silently degraded to recency.
func TestSearchMatchAnyFindsPartialMatches(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	events := []Event{
		{Kind: KindScreen, Text: "tuning postgres indexes for the reporting query", MediaPath: "a.jpg"},
		{Kind: KindScreen, Text: "sqlite indexes explained", MediaPath: "b.jpg"},
		{Kind: KindScreen, Text: "lunch menu for the week", MediaPath: "c.jpg"},
		// Filler so term-frequency statistics are not degenerate: with a
		// three-document corpus FTS5's IDF term goes negative for anything
		// appearing in more than half the rows.
		{Kind: KindScreen, Text: "standup notes about the mobile release", MediaPath: "d.jpg"},
		{Kind: KindScreen, Text: "expense report submission", MediaPath: "e.jpg"},
		{Kind: KindScreen, Text: "design review of the onboarding flow", MediaPath: "f.jpg"},
		{Kind: KindScreen, Text: "flight itinerary confirmation", MediaPath: "g.jpg"},
	}
	for i := range events {
		events[i].CapturedAt = now.Add(time.Duration(i) * time.Second)
	}
	insertAll(t, ctx, s, events...)

	question := "what did i read about postgres indexes yesterday"

	all, err := s.Search(ctx, SearchOptions{Query: question, Match: MatchAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("MatchAll on a full question should match nothing, got %d", len(all))
	}

	partial, err := s.Search(ctx, SearchOptions{Query: question, Match: MatchAny})
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) < 2 {
		t.Fatalf("MatchAny should surface partial matches, got %d", len(partial))
	}
	if !strings.Contains(partial[0].Text, "postgres") {
		t.Fatalf("event matching the most terms should rank first, got %q", partial[0].Text)
	}
}

func TestSearchMatchAnyTreatsFTSSyntaxAsText(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertAll(t, ctx, s, Event{Kind: KindScreen, Text: `issue (alpha) "quoted"`, MediaPath: "a.jpg"})

	if _, err := s.Search(ctx, SearchOptions{Query: `(alpha) OR NEAR "quoted"`, Match: MatchAny}); err != nil {
		t.Fatalf("FTS5 syntax in user text must not reach the parser: %v", err)
	}
}

func TestSearchPunctuationOnlyQueryFallsBackToRecency(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertAll(t, ctx, s, Event{Kind: KindScreen, Text: "roadmap review", MediaPath: "a.jpg"})

	got, err := s.Search(ctx, SearchOptions{Query: "???"})
	if err != nil {
		t.Fatalf("a query with no usable tokens must not error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected recency fallback to return the event, got %d", len(got))
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertAll(t *testing.T, ctx context.Context, s *Store, events ...Event) {
	t.Helper()
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}
}
