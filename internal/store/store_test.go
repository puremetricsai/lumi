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
