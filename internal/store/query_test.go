package store

import (
	"context"
	"path/filepath"
	"slices"
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
// defect: a natural-language question ANDed together matches nothing, so a
// caller relying on MatchAll silently degraded to recency.
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

// TestSearchMatchAnyWeightsBodyTextOverAppAndWindow pins the column weights in
// `bm25(events_fts, 1.0, 0.4, 0.4)`. bm25 length-normalizes per column, so a
// term that is the whole of a one-word app or window title scores far higher
// than the same term buried in a page of screen text — which would let a stray
// window title outrank the event that actually discusses the subject. Removing
// the three weights must turn this test red.
func TestSearchMatchAnyWeightsBodyTextOverAppAndWindow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	body := "The gateway rollout is blocked on the staging certificate rotation, " +
		"which the platform team scheduled for Thursday after the load test finished. " +
		"Notes from the review call cover the gateway retry budget, the timeout defaults, " +
		"and the plan to shift gateway traffic in ten percent increments once the " +
		"certificate lands and the canary has been observed for a full business day."
	events := []Event{
		// The term is the entire window title, with a body that never mentions
		// it: the shortest possible column hit.
		{Kind: KindScreen, Text: "lunch order for the offsite", App: "Ghostty", Window: "Gateway", MediaPath: "a.jpg"},
		// The term appears once, inside substantive screen text.
		{Kind: KindScreen, Text: body, App: "Ghostty", Window: "notes", MediaPath: "b.jpg"},
		// Filler so FTS5's IDF term is not degenerate on a two-row corpus.
		{Kind: KindScreen, Text: "standup notes about the mobile release", MediaPath: "c.jpg"},
		{Kind: KindScreen, Text: "expense report submission", MediaPath: "d.jpg"},
		{Kind: KindScreen, Text: "design review of the onboarding flow", MediaPath: "e.jpg"},
		{Kind: KindScreen, Text: "flight itinerary confirmation", MediaPath: "f.jpg"},
	}
	for i := range events {
		events[i].CapturedAt = now.Add(time.Duration(i) * time.Second)
	}
	insertAll(t, ctx, s, events...)

	got, err := s.Search(ctx, SearchOptions{Query: "gateway", Match: MatchAny})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both gateway events, got %d", len(got))
	}
	if got[0].MediaPath != "b.jpg" {
		t.Fatalf("the body-text match must outrank the title-only match; got %q first (ranks %v, %v)",
			got[0].MediaPath, got[0].Rank, got[1].Rank)
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
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "lumi.db"), nil)
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

func TestListAttributionGroupsByApp(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertAll(t, ctx, s,
		Event{Kind: KindScreen, CapturedAt: base, App: "Safari", Window: "Plan", Text: "a", MediaPath: "a.jpg"},
		Event{Kind: KindScreen, CapturedAt: base.Add(time.Minute), App: "Safari", Window: "Docs", Text: "b", MediaPath: "b.jpg"},
		Event{Kind: KindScreen, CapturedAt: base.Add(2 * time.Minute), App: "Ghostty", Window: "zsh", Text: "c", MediaPath: "c.jpg"},
		// An audio chunk has no app attribution at all.
		Event{Kind: KindAudio, CapturedAt: base.Add(3 * time.Minute), Text: "d", MediaPath: "d.wav"},
	)

	got, err := s.ListAttribution(ctx, AttributionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected Safari, Ghostty and the empty bucket, got %#v", got)
	}
	if got[0].App != "Safari" || got[0].Events != 2 {
		t.Fatalf("expected Safari with 2 events first, got %#v", got[0])
	}
	if !got[0].LastSeen.Equal(base.Add(time.Minute)) {
		t.Fatalf("Safari last_seen = %s, want %s", got[0].LastSeen, base.Add(time.Minute))
	}
	// An unattributed row is information, not noise: it must be reported under
	// an explicit empty app rather than dropped.
	var sawEmpty bool
	for _, row := range got {
		if row.App == "" {
			sawEmpty = true
			if row.Events != 1 {
				t.Fatalf("empty-app bucket = %d events, want 1", row.Events)
			}
		}
	}
	if !sawEmpty {
		t.Fatalf("the empty-app bucket was dropped: %#v", got)
	}
}

func TestListAttributionFiltersByKind(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertAll(t, ctx, s,
		Event{Kind: KindScreen, CapturedAt: base, App: "Safari", Text: "a", MediaPath: "a.jpg"},
		Event{Kind: KindAudio, CapturedAt: base.Add(time.Minute), Text: "b", MediaPath: "b.wav"},
	)

	got, err := s.ListAttribution(ctx, AttributionOptions{Kind: KindScreen})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].App != "Safari" {
		t.Fatalf("kind filter did not exclude the audio chunk: %#v", got)
	}
}

// The ratio must not be diluted by audio, which carries no app by design.
func TestAttributionHealthCountsScreenEventsOnly(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertAll(t, ctx, s,
		Event{Kind: KindScreen, CapturedAt: base, App: "Ghostty", Text: "a", MediaPath: "a.jpg"},
		Event{Kind: KindScreen, CapturedAt: base.Add(time.Minute), Text: "b", MediaPath: "b.jpg"},
		Event{Kind: KindScreen, CapturedAt: base.Add(2 * time.Minute), Text: "c", MediaPath: "c.jpg"},
		Event{Kind: KindAudio, CapturedAt: base.Add(3 * time.Minute), Text: "d", MediaPath: "d.wav"},
	)

	got, err := s.AttributionHealth(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 3 {
		t.Errorf("Total = %d, want 3 screen events (audio excluded)", got.Total)
	}
	if got.Unattributed != 2 {
		t.Errorf("Unattributed = %d, want 2", got.Unattributed)
	}
	if !got.HasLastAttributed || !got.LastAttributed.Equal(base) {
		t.Errorf("LastAttributed = %v (ok=%t), want %s", got.LastAttributed, got.HasLastAttributed, base)
	}
}

// The outage this report explains is usually longer than the window it is asked
// about. Scoping LastAttributed to `since` would blank it out precisely then.
func TestAttributionHealthReportsLastAttributedFromBeforeTheWindow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	attributed := base.Add(-24 * time.Hour)
	insertAll(t, ctx, s,
		Event{Kind: KindScreen, CapturedAt: attributed, App: "Ghostty", Text: "a", MediaPath: "a.jpg"},
		Event{Kind: KindScreen, CapturedAt: base, Text: "b", MediaPath: "b.jpg"},
	)

	got, err := s.AttributionHealth(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || got.Unattributed != 1 {
		t.Fatalf("window should hold exactly the one unattributed event: %#v", got)
	}
	if !got.HasLastAttributed || !got.LastAttributed.Equal(attributed) {
		t.Errorf("LastAttributed = %v (ok=%t), want the event from before the window (%s)",
			got.LastAttributed, got.HasLastAttributed, attributed)
	}
}

// SUM and MAX are NULL over an empty set; scanning them into plain integers and
// strings would fail rather than report an empty index.
func TestAttributionHealthOverEmptySet(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	got, err := s.AttributionHealth(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("empty index must report cleanly, got %v", err)
	}
	if got.Total != 0 || got.Unattributed != 0 || got.HasLastAttributed {
		t.Errorf("empty index reported %#v", got)
	}
}

func TestListAttributionGroupsByWindowForOneApp(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertAll(t, ctx, s,
		Event{Kind: KindScreen, CapturedAt: base, App: "Safari", Window: "Plan", Text: "a", MediaPath: "a.jpg"},
		Event{Kind: KindScreen, CapturedAt: base.Add(time.Minute), App: "Safari", Window: "Plan", Text: "b", MediaPath: "b.jpg"},
		Event{Kind: KindScreen, CapturedAt: base.Add(2 * time.Minute), App: "Safari", Window: "Docs", Text: "c", MediaPath: "c.jpg"},
		Event{Kind: KindScreen, CapturedAt: base.Add(3 * time.Minute), App: "Ghostty", Window: "zsh", Text: "d", MediaPath: "d.jpg"},
	)

	// The app filter matches case-insensitively, exactly like Search's.
	app := "safari"
	got, err := s.ListAttribution(ctx, AttributionOptions{App: &app})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected two Safari windows, got %#v", got)
	}
	if got[0].Window != "Plan" || got[0].Events != 2 {
		t.Fatalf("expected Plan with 2 events first, got %#v", got[0])
	}
	if got[0].App != "safari" {
		t.Fatalf("window rows must carry the requested app, got %q", got[0].App)
	}
}

func TestListAttributionAppliesTimeRangeAndLimit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertAll(t, ctx, s,
		Event{Kind: KindScreen, CapturedAt: base.Add(-48 * time.Hour), App: "Old", Text: "a", MediaPath: "a.jpg"},
		Event{Kind: KindScreen, CapturedAt: base, App: "Safari", Text: "b", MediaPath: "b.jpg"},
		Event{Kind: KindScreen, CapturedAt: base.Add(time.Minute), App: "Ghostty", Text: "c", MediaPath: "c.jpg"},
	)

	since := base.Add(-time.Hour)
	got, err := s.ListAttribution(ctx, AttributionOptions{Since: &since})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("since must exclude the 48h-old app, got %#v", got)
	}
	got, err = s.ListAttribution(ctx, AttributionOptions{Since: &since, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("limit was ignored, got %#v", got)
	}
	until := base.Add(-24 * time.Hour)
	got, err = s.ListAttribution(ctx, AttributionOptions{Until: &until})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].App != "Old" {
		t.Fatalf("until must keep only the old app, got %#v", got)
	}
}

func TestHasSearchableTermsTracksTheExpressionBuilder(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"meeting notes", true},
		{"lumi2", true},
		{"", false},
		{"   ", false},
		{"!!! ??? ...", false},
		{"🎉", false},
		{"!!! notes", true},
	} {
		if got := HasSearchableTerms(tc.query); got != tc.want {
			t.Errorf("HasSearchableTerms(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// TestSearchTermsAreTheTermsJoinTermsQuotes pins the written contract of the
// exported splitter: terms come back AS THE USER WROTE THEM, minus the ones
// FTS5 tokenizes to nothing.
//
// It is exported for the same reason HasSearchableTerms is — `lumi mcp` centres
// its excerpt on the term that earned a row its place, and deriving the terms
// here rather than re-splitting at the boundary is what keeps the two from
// drifting; the drop rule has been copied out of this file once already. A
// caller matching these against raw text may still miss what FTS5 matched, so a
// miss is "no excerpt to centre on", never "this row did not match".
func TestSearchTermsAreTheTermsJoinTermsQuotes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"plain terms", "postgres indexes", []string{"postgres", "indexes"}},
		{"collapses whitespace", "  spaced \t out\n", []string{"spaced", "out"}},
		{"keeps punctuation inside a term", "(alpha)", []string{"(alpha)"}},
		{"quotes are escaped later, not here", `say "hi"`, []string{"say", `"hi"`}},
		{"digits count as alphanumeric", "lumi2", []string{"lumi2"}},
		{"non-ascii letters are kept", "café", []string{"café"}},
		{"empty", "", nil},
		{"only whitespace", "   ", nil},
		{"only punctuation", "??? ...", nil},
		{"only emoji", "🎉", nil},
		{"drops the unsearchable term only", "??? alpha ...", []string{"alpha"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SearchTerms(tc.query); !slices.Equal(got, tc.want) {
				t.Fatalf("SearchTerms(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}
