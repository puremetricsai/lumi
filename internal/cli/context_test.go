package cli

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/puremetricsai/lumi/internal/store"
)

// TestDescribeTimeWindow locks in the interpretation-note phrasing, including
// the directional forms that keep "after"/"before" from reading like the old
// centered band. Times are built in a fixed zone and rendered via .Local(), so
// assertions target the day/clock structure rather than an absolute offset.
func TestDescribeTimeWindow(t *testing.T) {
	mk := func(y int, mo time.Month, d, h, mi int) time.Time {
		return time.Date(y, mo, d, h, mi, 0, 0, time.Local)
	}
	for _, tc := range []struct {
		name  string
		since time.Time
		until time.Time
		dir   clockDir
		want  string
	}{
		{
			name:  "forward reads as onward",
			since: mk(2026, 7, 22, 22, 15),
			until: mk(2026, 7, 23, 0, 0),
			dir:   dirForward,
			want:  "from 10:15 PM onward (2026-07-22)",
		},
		{
			name:  "backward reads as up to",
			since: mk(2026, 7, 22, 0, 0),
			until: mk(2026, 7, 22, 9, 0),
			dir:   dirBackward,
			want:  "up to 9:00 AM (2026-07-22)",
		},
		{
			name:  "centered same-day range",
			since: mk(2026, 7, 22, 21, 0),
			until: mk(2026, 7, 22, 21, 30),
			dir:   dirCentered,
			want:  "2026-07-22, 9:00 PM to 9:30 PM",
		},
		{
			name:  "centered full day",
			since: mk(2026, 7, 21, 0, 0),
			until: mk(2026, 7, 22, 0, 0),
			dir:   dirCentered,
			want:  "the day of 2026-07-21",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeTimeWindow(tc.since, tc.until, tc.dir); got != tc.want {
				t.Fatalf("describeTimeWindow = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestContextForRespectsBudget is the regression for the reported defect:
// contextFor used to concatenate every event's full screen text with no cap.
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
	localTime := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC).Local().Format(time.RFC3339)
	for _, want := range []string{localTime, "screen", "Arc", "Roadmap", "a.jpg"} {
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

func TestContextForRendersSelectedEventsChronologically(t *testing.T) {
	base := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	events := []store.Event{
		{Kind: store.KindScreen, CapturedAt: base.Add(2 * time.Minute), Text: "third", MediaPath: "c.jpg"},
		{Kind: store.KindScreen, CapturedAt: base, Text: "first", MediaPath: "a.jpg"},
		{Kind: store.KindAudio, CapturedAt: base.Add(time.Minute), Text: "second", MediaPath: "b.wav"},
	}

	got := contextFor(events, defaultContextChars)
	first := strings.Index(got, "first")
	second := strings.Index(got, "second")
	third := strings.Index(got, "third")
	if first < 0 || second < 0 || third < 0 || !(first < second && second < third) {
		t.Fatalf("events were not chronological:\n%s", got)
	}
}

func TestContextForConsolidatesAdjacentIdenticalScreenText(t *testing.T) {
	base := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	events := []store.Event{
		{Kind: store.KindScreen, CapturedAt: base, App: "Zed", Window: "lumi — README.md", TextSource: "accessibility", Text: "lumi — README.md", MediaPath: "a.jpg"},
		{Kind: store.KindScreen, CapturedAt: base.Add(2 * time.Second), App: "Zed", Window: "lumi — README.md", TextSource: "accessibility", Text: "lumi — README.md", MediaPath: "b.jpg"},
	}

	got := contextFor(events, defaultContextChars)
	if !strings.Contains(got, "captures=2") || !strings.Contains(got, "media_files=2") {
		t.Fatalf("repeated captures were not consolidated:\n%s", got)
	}
	if strings.Count(got, "kind=screen") != 1 {
		t.Fatalf("expected one consolidated screen block:\n%s", got)
	}
}

func TestContextForDoesNotConsolidateAcrossOtherActivity(t *testing.T) {
	base := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	screen := store.Event{Kind: store.KindScreen, CapturedAt: base, App: "Zed", Window: "lumi", Text: "lumi", MediaPath: "a.jpg"}
	audio := store.Event{Kind: store.KindAudio, CapturedAt: base.Add(time.Second), AudioSource: "system", Text: "spoken words", MediaPath: "a.wav"}
	screenAgain := screen
	screenAgain.CapturedAt = base.Add(2 * time.Second)
	screenAgain.MediaPath = "b.jpg"

	got := contextFor([]store.Event{screen, audio, screenAgain}, defaultContextChars)
	if strings.Contains(got, "captures=2") || strings.Count(got, "kind=screen") != 2 {
		t.Fatalf("screen captures separated by audio were incorrectly consolidated:\n%s", got)
	}
}

// TestContextForRendersLocalTime is the regression for `ask` echoing UTC (04:24Z)
// when asked about a local time (9:15 pm). Stored-UTC timestamps must render local
// so the model interprets and reports times the user recognizes.
func TestContextForRendersLocalTime(t *testing.T) {
	captured := time.Date(2026, 7, 23, 4, 24, 16, 0, time.UTC)
	event := store.Event{
		Kind: store.KindScreen, CapturedAt: captured,
		App: "Zed", Window: "lumi", Text: "some visible text", MediaPath: "a.jpg",
	}

	got := contextFor([]store.Event{event}, defaultContextChars)
	want := captured.Local().Format(time.RFC3339)
	if !strings.Contains(got, want) {
		t.Fatalf("context did not render local timestamp %q:\n%s", want, got)
	}
	if _, offset := captured.Local().Zone(); offset != 0 && strings.Contains(got, "T04:24:16Z") {
		t.Fatalf("context leaked the UTC timestamp instead of local time:\n%s", got)
	}
}

func TestContextForLabelsUntranscribedAudioExplicitly(t *testing.T) {
	event := store.Event{
		Kind: store.KindAudio, CapturedAt: time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		AudioSource: "microphone", MediaPath: "mic.wav",
	}

	got := contextFor([]store.Event{event}, defaultContextChars)
	for _, want := range []string{
		`audio_source="microphone"`,
		"transcript_status=unavailable",
		"no searchable transcript was produced",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("audio context omitted %q:\n%s", want, got)
		}
	}
}

func TestContextForLabelsWindowTitleOnlyScreenEvidence(t *testing.T) {
	event := store.Event{
		Kind: store.KindScreen, CapturedAt: time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		App: "Zed", Window: "lumi — .env", TextSource: "accessibility",
		Text: "lumi — .env", MediaPath: "screen.jpg",
	}

	got := contextFor([]store.Event{event}, defaultContextChars)
	for _, want := range []string{
		"observation=window_title_only",
		"no file contents or user action captured",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("screen context omitted %q:\n%s", want, got)
		}
	}
}

func TestCompactScreenText(t *testing.T) {
	in := "  File   Edit  View  \n\n\n   \n  main.go — lumi  \n\n  func main() {  \n"
	want := "File   Edit  View\nmain.go — lumi\nfunc main() {"
	if got := compactScreenText(in); got != want {
		t.Fatalf("compactScreenText:\n got %q\nwant %q", got, want)
	}
}
