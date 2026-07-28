package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

func TestSearchEventsFiltersCombine(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: base, Text: "quarterly roadmap review", App: "Safari", Window: "Quarterly plan", MediaPath: "/tmp/a.jpg", TextSource: "vision", DisplayID: 1},
		store.Event{Kind: store.KindScreen, CapturedAt: base.Add(-72 * time.Hour), Text: "quarterly roadmap review", App: "Safari", Window: "Quarterly plan", MediaPath: "/tmp/b.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: base, Text: "quarterly roadmap review", App: "Ghostty", Window: "zsh", MediaPath: "/tmp/c.jpg"},
		store.Event{Kind: store.KindAudio, CapturedAt: base, Text: "discuss the launch budget", MediaPath: "/tmp/d.wav", AudioSource: "microphone", DurationMS: 30000},
	)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{})
	if len(out.Events) != 4 {
		t.Fatalf("no filters must return everything, got %d", len(out.Events))
	}

	out = callSearch(t, ctx, h, searchEventsInput{Kind: "audio"})
	if len(out.Events) != 1 || out.Events[0].Kind != "audio" {
		t.Fatalf("kind filter failed: %#v", out.Events)
	}

	out = callSearch(t, ctx, h, searchEventsInput{Query: "quarterly roadmap", App: "safari", Since: "24h"})
	if len(out.Events) != 1 {
		t.Fatalf("query+app+since must isolate one event, got %d", len(out.Events))
	}
	got := out.Events[0]
	if got.App != "Safari" || got.Window != "Quarterly plan" || got.MediaPath != "/tmp/a.jpg" {
		t.Fatalf("unexpected record: %#v", got)
	}
	if got.TextSource != "vision" || got.DisplayID != 1 {
		t.Fatalf("provenance columns were dropped: %#v", got)
	}
	if _, err := time.Parse(time.RFC3339, got.CapturedAt); err != nil {
		t.Fatalf("captured_at %q is not RFC3339: %v", got.CapturedAt, err)
	}
	if !strings.Contains(got.CapturedAt, offsetOf(base)) {
		t.Fatalf("captured_at %q is not rendered in the local zone", got.CapturedAt)
	}

	out = callSearch(t, ctx, h, searchEventsInput{Window: "quarterly"})
	if len(out.Events) != 2 {
		t.Fatalf("window substring filter failed: %#v", out.Events)
	}

	out = callSearch(t, ctx, h, searchEventsInput{Limit: 2})
	if len(out.Events) != 2 {
		t.Fatalf("limit failed, got %d", len(out.Events))
	}
}

// offsetOf renders the machine's local UTC offset the way RFC3339 does, so the
// timezone assertion holds on any machine (including a UTC CI box, where it is
// the literal "Z").
func offsetOf(at time.Time) string {
	rendered := at.Local().Format(time.RFC3339)
	return rendered[len(rendered)-6:]
}

func TestSearchEventsMatchAnyFindsWhatAllMisses(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, Text: "postgres index tuning notes", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, Text: "an unrelated grocery list", MediaPath: "/tmp/b.jpg"},
	)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Query: "postgres grocery"})
	if len(out.Events) != 0 {
		t.Fatalf(`the default match must be "all", got %d results`, len(out.Events))
	}
	out = callSearch(t, ctx, h, searchEventsInput{Query: "postgres grocery", Match: "any"})
	if len(out.Events) != 2 {
		t.Fatalf(`match "any" must return both, got %d`, len(out.Events))
	}
}

// TestSearchEventsMatchAnyRanksBodyTextAboveAWindowTitle pins the bm25 column
// weights through the tool boundary: without them a one-word window title
// outranks a page of relevant screen text, and an agent's first page of results
// is all title noise.
func TestSearchEventsMatchAnyRanksBodyTextAboveAWindowTitle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, Text: "budget", App: "Safari", Window: "budget", MediaPath: "/tmp/title.jpg"},
		store.Event{Kind: store.KindScreen, Text: "the launch budget covers contractor time and hosting", App: "Notes", Window: "notes", MediaPath: "/tmp/body.jpg"},
	)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Query: "launch budget", Match: "any"})
	if len(out.Events) != 2 {
		t.Fatalf("expected both events, got %d", len(out.Events))
	}
	if out.Events[0].MediaPath != "/tmp/body.jpg" {
		t.Fatalf("the body-text hit must rank first, got %#v", out.Events[0])
	}
}

func TestSearchEventsRequireTextDropsBlankTranscripts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindAudio, Text: "   \n\t ", MediaPath: "/tmp/silent.wav"},
		store.Event{Kind: store.KindAudio, Text: "discuss the launch budget", MediaPath: "/tmp/speech.wav"},
	)
	h := &handlers{store: s}

	if out := callSearch(t, ctx, h, searchEventsInput{}); len(out.Events) != 2 {
		t.Fatalf("require_text defaults to off, got %d", len(out.Events))
	}
	out := callSearch(t, ctx, h, searchEventsInput{RequireText: true})
	if len(out.Events) != 1 || out.Events[0].MediaPath != "/tmp/speech.wav" {
		t.Fatalf("require_text failed: %#v", out.Events)
	}
}

func TestSearchEventsTruncation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	long := strings.Repeat("a", 1000)
	multibyte := strings.Repeat("日", 800)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, Text: long, App: "Long", MediaPath: "/tmp/long.jpg"},
		store.Event{Kind: store.KindScreen, Text: multibyte, App: "Multi", MediaPath: "/tmp/multi.jpg"},
		store.Event{Kind: store.KindScreen, Text: "short", App: "Short", MediaPath: "/tmp/short.jpg"},
	)
	h := &handlers{store: s}

	byApp := func(out searchEventsOutput, app string) EventRecord {
		t.Helper()
		for _, record := range out.Events {
			if record.App == app {
				return record
			}
		}
		t.Fatalf("no record for app %q", app)
		return EventRecord{}
	}

	out := callSearch(t, ctx, h, searchEventsInput{})
	short := byApp(out, "Short")
	if short.Truncated || short.Text != "short" || short.TextLength != 5 {
		t.Fatalf("under-cap text must be untouched: %#v", short)
	}
	defaulted := byApp(out, "Long")
	if !defaulted.Truncated || len([]rune(defaulted.Text)) != defaultMaxTextChars || defaulted.TextLength != 1000 {
		t.Fatalf("default cap failed: truncated=%v runes=%d length=%d",
			defaulted.Truncated, len([]rune(defaulted.Text)), defaulted.TextLength)
	}
	multi := byApp(out, "Multi")
	if !strings.HasSuffix(multi.Text, "日") || len([]rune(multi.Text)) != defaultMaxTextChars {
		t.Fatalf("multibyte text was cut mid-character: %q", multi.Text)
	}

	atCap := 5
	out = callSearch(t, ctx, h, searchEventsInput{App: "Short", MaxTextChars: &atCap})
	if record := byApp(out, "Short"); record.Truncated {
		t.Fatalf("text exactly at the cap must not be marked truncated: %#v", record)
	}

	none := 0
	out = callSearch(t, ctx, h, searchEventsInput{App: "Long", MaxTextChars: &none})
	if record := byApp(out, "Long"); record.Truncated || record.Text != long {
		t.Fatalf("max_text_chars 0 must mean no cap: truncated=%v len=%d", record.Truncated, len(record.Text))
	}
}

func TestSearchEventsRejectsInvalidEnumsAndTimes(t *testing.T) {
	ctx := context.Background()
	h := &handlers{store: testStore(t)}

	for _, tc := range []struct {
		name  string
		in    searchEventsInput
		wants []string
	}{
		{"kind", searchEventsInput{Kind: "video"}, []string{"screen", "audio", "video"}},
		{"match", searchEventsInput{Match: "some"}, []string{"all", "any", "some"}},
		{"since", searchEventsInput{Since: "yesterday"}, []string{"since", "RFC3339", "duration"}},
		{"until", searchEventsInput{Until: "yesterday"}, []string{"until", "RFC3339", "duration"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := h.searchEvents(ctx, nil, tc.in)
			if err == nil {
				t.Fatal("expected a tool error")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestSearchEventsDistinguishesEmptyStoreFromNoMatch is the difference between
// an agent reporting "you have not recorded anything yet" and "nothing matched"
// — the two need different follow-up actions.
func TestSearchEventsDistinguishesEmptyStoreFromNoMatch(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Query: "anything"})
	if len(out.Events) != 0 {
		t.Fatalf("an empty store must return no events, got %d", len(out.Events))
	}
	if !strings.Contains(out.Notice, "no events") {
		t.Fatalf("empty-store notice = %q, want it to say the store holds no events", out.Notice)
	}

	insertEvents(t, ctx, s, store.Event{Kind: store.KindScreen, Text: "roadmap", MediaPath: "/tmp/a.jpg"})
	out = callSearch(t, ctx, h, searchEventsInput{Query: "kubernetes"})
	if len(out.Events) != 0 {
		t.Fatalf("expected no matches, got %d", len(out.Events))
	}
	if !strings.Contains(out.Notice, "matched") {
		t.Fatalf("no-match notice = %q, want it to say nothing matched the filters", out.Notice)
	}
}
