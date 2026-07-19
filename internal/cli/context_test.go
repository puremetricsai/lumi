package cli

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/puremetricsai/lumi/internal/store"
)

// TestContextForRespectsBudget is the regression for the reported defect:
// contextFor used to concatenate every event's full OCR text with no cap.
func TestContextForRespectsBudget(t *testing.T) {
	events := make([]store.Event, 50)
	for i := range events {
		events[i] = store.Event{
			Kind:       store.KindScreen,
			CapturedAt: time.Now().UTC(),
			Text:       strings.Repeat("lorem ipsum dolor sit amet ", 200), // ~5400 chars
			MediaPath:  "a.jpg",
		}
	}

	got := contextFor(events, defaultContextChars)

	if len(got) > defaultContextChars {
		t.Fatalf("context is %d bytes, over the %d budget", len(got), defaultContextChars)
	}
	if !strings.Contains(got, "further events omitted") {
		t.Fatal("expected an omission marker when events are dropped")
	}
	// 50 events capped at maxEventChars each cannot fit in the budget, so the
	// marker must report a real count.
	if strings.Contains(got, "0 further events omitted") {
		t.Fatal("omission marker reported zero omitted events")
	}
}

func TestContextForTruncatesPerEvent(t *testing.T) {
	event := store.Event{
		Kind:       store.KindScreen,
		CapturedAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		Text:       strings.Repeat("x", 10000),
		App:        "Arc",
		Window:     "Roadmap",
		MediaPath:  "a.jpg",
	}

	got := contextFor([]store.Event{event}, defaultContextChars)

	if len(got) > maxEventChars*2 {
		t.Fatalf("single event rendered %d bytes, expected a per-event cap near %d", len(got), maxEventChars)
	}
	for _, want := range []string{"2026-07-19T12:00:00Z", "screen", "Arc", "Roadmap", "a.jpg"} {
		if !strings.Contains(got, want) {
			t.Fatalf("header lost %q:\n%s", want, got)
		}
	}
}

func TestContextForIsRuneSafe(t *testing.T) {
	// Multibyte runes so a byte-indexed cut would land mid-rune.
	event := store.Event{Kind: store.KindScreen, Text: strings.Repeat("日", 5000), MediaPath: "a.jpg"}

	got := contextFor([]store.Event{event}, defaultContextChars)

	if !utf8.ValidString(got) {
		t.Fatal("truncation split a multi-byte rune")
	}
}

func TestTruncateRunes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		max  int
	}{
		{"shorter than the cap is unchanged", "hello", 100},
		{"multibyte cut", strings.Repeat("日", 50), 20},
		{"cap smaller than the ellipsis", strings.Repeat("日", 50), 1},
		{"zero cap", "hello", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateRunes(tc.in, tc.max)
			if len(got) > tc.max && len(tc.in) > tc.max {
				t.Fatalf("truncateRunes returned %d bytes, over the %d cap", len(got), tc.max)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
		})
	}
	if got := truncateRunes("hello", 100); got != "hello" {
		t.Fatalf("under-cap input was modified: %q", got)
	}
}

// TestContextForAlwaysIncludesOneEvent: an empty context would make the model
// answer from nothing at all, which is worse than exceeding a soft budget.
func TestContextForAlwaysIncludesOneEvent(t *testing.T) {
	event := store.Event{Kind: store.KindScreen, Text: strings.Repeat("x", 10000), MediaPath: "a.jpg"}

	got := contextFor([]store.Event{event}, 10)

	if got == "" {
		t.Fatal("expected at least one event even under an unreachable budget")
	}
	if !utf8.ValidString(got) {
		t.Fatal("invalid UTF-8")
	}
}

func TestContextForEmpty(t *testing.T) {
	if got := contextFor(nil, defaultContextChars); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestCompactOCR(t *testing.T) {
	in := "  File   Edit  View  \n\n\n   \n  main.go — lumi  \n\n  func main() {  \n"
	want := "File   Edit  View\nmain.go — lumi\nfunc main() {"
	if got := compactOCR(in); got != want {
		t.Fatalf("compactOCR:\n got %q\nwant %q", got, want)
	}
}
