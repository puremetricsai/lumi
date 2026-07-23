package cli

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

func TestQuestionTerms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		question string
		want     []string
	}{
		{
			name:     "strips question words and temporal words",
			question: "what did I read about postgres indexes yesterday",
			want:     []string{"read", "postgres", "indexes"},
		},
		{
			name:     "an all-stopword question yields nothing",
			question: "what was I doing?",
			want:     nil,
		},
		{
			name:     "a broad recording overview yields nothing",
			question: "what has been recorded so far?",
			want:     nil,
		},
		{
			name:     "punctuation splits terms",
			question: "postgres's index-tuning!",
			want:     []string{"postgres", "index", "tuning"},
		},
		{
			name:     "duplicates are removed in order",
			question: "postgres postgres indexes postgres",
			want:     []string{"postgres", "indexes"},
		},
		{
			name:     "two-rune terms survive",
			question: "which db and go ui did I use",
			want:     []string{"db", "go", "ui", "use"},
		},
		{
			name:     "diacritics are preserved",
			question: "notes about café menus",
			want:     []string{"notes", "café", "menus"},
		},
		{
			name:     "a pure modality question yields nothing",
			question: "what did I say on the microphone",
			want:     nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := questionTerms(tc.question); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("questionTerms(%q) = %#v, want %#v", tc.question, got, tc.want)
			}
		})
	}
}

func TestQuestionTermsCapsTermCount(t *testing.T) {
	question := "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november"
	got := questionTerms(question)
	if len(got) != maxTerms {
		t.Fatalf("expected %d terms, got %d (%v)", maxTerms, len(got), got)
	}
}

// TestRetrieveContextStages is the integration-level regression for the
// reported defect: a real question must reach the FTS index instead of
// silently landing on "the 50 most recent events".
func TestRetrieveContextStages(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	insertAll(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: now, Text: "tuning postgres indexes for reporting", MediaPath: "a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: now.Add(time.Second), Text: "sqlite indexes explained", MediaPath: "b.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: now.Add(2 * time.Second), Text: "lunch menu", MediaPath: "c.jpg"},
	)

	for _, tc := range []struct {
		name      string
		question  string
		wantStage retrievalStage
	}{
		{"every term matches", "postgres indexes", stageAllTerms},
		{"only some terms match", "what did I read about postgres and kubernetes", stageAnyTerm},
		{"no searchable terms at all", "what was I doing?", stageRecent},
		{"searchable terms do not match", "what about kubernetes", stageRecentUnmatched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, stage, err := retrieveContext(ctx, s, tc.question, store.SearchOptions{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if stage != tc.wantStage {
				t.Fatalf("stage = %q, want %q", stage, tc.wantStage)
			}
			if len(events) == 0 {
				t.Fatal("expected at least one event")
			}
		})
	}
}

func TestRetrieveContextPreservesOptionsAcrossStages(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	insertAll(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: now, Text: "postgres indexes on screen", MediaPath: "a.jpg"},
		store.Event{Kind: store.KindAudio, CapturedAt: now.Add(time.Second), Text: "postgres indexes discussed aloud", MediaPath: "a.wav"},
		store.Event{Kind: store.KindAudio, CapturedAt: now.Add(2 * time.Second), Text: "unrelated chatter", MediaPath: "b.wav"},
	)
	since := now.Add(-time.Hour)

	// Each stage must keep the kind and time predicates. Exercise all three.
	for _, question := range []string{
		"postgres indexes",                   // stageAllTerms
		"what about postgres and kubernetes", // stageAnyTerm
		"what was I doing?",                  // stageRecent
	} {
		events, stage, err := retrieveContext(ctx, s, question, store.SearchOptions{
			Kind: store.KindAudio, Since: &since, Limit: 10,
		})
		if err != nil {
			t.Fatalf("%q: %v", question, err)
		}
		if len(events) == 0 {
			t.Fatalf("%q (stage %s): expected results", question, stage)
		}
		for _, event := range events {
			if event.Kind != store.KindAudio {
				t.Fatalf("%q (stage %s): kind filter dropped, got %s", question, stage, event.Kind)
			}
			if event.CapturedAt.Before(since) {
				t.Fatalf("%q (stage %s): since filter dropped", question, stage)
			}
		}
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertAll(t *testing.T, ctx context.Context, s *store.Store, events ...store.Event) {
	t.Helper()
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}
}
