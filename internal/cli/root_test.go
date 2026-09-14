package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/store"
)

// A mistyped --data-dir must read as "there is nothing here", not be silently
// created and then reported as a healthy index.
func TestReportAttributionHealthDoesNotCreateAnIndex(t *testing.T) {
	root := filepath.Join(t.TempDir(), "typo")
	paths, err := config.FromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := reportAttributionHealth(context.Background(), &out, paths); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no screen history") {
		t.Errorf("output = %q, want a no-screen-history notice", out.String())
	}
	if _, err := os.Stat(paths.Database); !os.IsNotExist(err) {
		t.Errorf("doctor created an index at %s", paths.Database)
	}
}

func TestReportAttributionHealthWarnsOnUnattributedEvents(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, event := range []store.Event{
		{Kind: store.KindScreen, CapturedAt: now.Add(-time.Minute), App: "Ghostty", MediaPath: "a.jpg"},
		{Kind: store.KindScreen, CapturedAt: now, MediaPath: "b.jpg"},
	} {
		if err := s.Insert(ctx, &event); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	var out strings.Builder
	if err := reportAttributionHealth(ctx, &out, paths); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "attribution\twarn\t1 of 2 screen events") {
		t.Errorf("output = %q, want a warn row counting 1 of 2", out.String())
	}
}

// An hour holds thousands of events at the default interval, so a real gap is a
// fraction of a percent. Integer division reported it as "warn 0% have no app",
// which reads as "nothing is wrong".
func TestFormatPercentNeverRoundsARealGapToZero(t *testing.T) {
	for _, testCase := range []struct {
		part, total int64
		want        string
	}{
		{part: 5, total: 505, want: "<1%"},
		{part: 1, total: 3600, want: "<1%"},
		{part: 1, total: 2, want: "50%"},
		{part: 3407, total: 3407, want: "100%"},
		{part: 0, total: 0, want: "0%"},
	} {
		if got := formatPercent(testCase.part, testCase.total); got != testCase.want {
			t.Errorf("formatPercent(%d, %d) = %s, want %s",
				testCase.part, testCase.total, got, testCase.want)
		}
	}
}

// TestTruncateRunes covers the helper behind `lumi search`'s human-readable
// output. A byte-indexed cut through multi-byte text is the reason it exists,
// so the multibyte cases carry the weight: the result must stay under the cap,
// stay valid UTF-8, and never contain a replacement character.
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
		{"emoji cut", strings.Repeat("🙂", 50), 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateRunes(tc.in, tc.max)
			if len(got) > tc.max && len(tc.in) > tc.max {
				t.Fatalf("truncateRunes returned %d bytes, over the %d cap", len(got), tc.max)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Fatalf("truncation produced a replacement character: %q", got)
			}
		})
	}
	if got := truncateRunes("hello", 100); got != "hello" {
		t.Fatalf("under-cap input was modified: %q", got)
	}
	if got := truncateRunes("hello", 100); strings.Contains(got, ellipsis) {
		t.Fatalf("under-cap input gained an ellipsis: %q", got)
	}
	// A cut with room for the marker has to be visible as a cut, otherwise a
	// truncated search row reads as complete text.
	if got := truncateRunes(strings.Repeat("x", 100), 20); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("truncated output lost its ellipsis: %q", got)
	}
}

// TestSearchOptions pins the whole flag→store.SearchOptions mapping behind
// `lumi search`, whose behavior — including `--json` — must not change.
func TestSearchOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		want store.Kind
	}{
		{"empty type means every kind", "", ""},
		{"all means every kind", "all", ""},
		{"screen", "screen", store.KindScreen},
		{"audio", "audio", store.KindAudio},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := searchOptions("roadmap", tc.kind, "", "", "", "", 0)
			if err != nil {
				t.Fatalf("searchOptions(%q) returned %v", tc.kind, err)
			}
			if opts.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q", opts.Kind, tc.want)
			}
		})
	}

	t.Run("invalid type is rejected", func(t *testing.T) {
		if _, err := searchOptions("", "video", "", "", "", "", 0); err == nil {
			t.Fatal("an unknown --type must be an error")
		}
	})

	t.Run("query, app, window, and limit pass through unchanged", func(t *testing.T) {
		opts, err := searchOptions("quarterly roadmap", "all", "", "", "Safari", "Quarterly Plan", 7)
		if err != nil {
			t.Fatal(err)
		}
		if opts.Query != "quarterly roadmap" {
			t.Errorf("Query = %q", opts.Query)
		}
		if opts.App != "Safari" {
			t.Errorf("App = %q", opts.App)
		}
		if opts.Window != "Quarterly Plan" {
			t.Errorf("Window = %q", opts.Window)
		}
		if opts.Limit != 7 {
			t.Errorf("Limit = %d", opts.Limit)
		}
		// No time flags means no window at all, not a zero-valued one.
		if opts.Since != nil || opts.Until != nil {
			t.Errorf("Since = %v, Until = %v, want both nil", opts.Since, opts.Until)
		}
		// searchOptions never chooses a match mode; MatchAll is the zero value.
		if opts.Match != store.MatchAll {
			t.Errorf("Match = %v, want MatchAll", opts.Match)
		}
	})

	t.Run("--since accepts RFC3339", func(t *testing.T) {
		stamp := "2026-07-18T09:00:00-07:00"
		opts, err := searchOptions("", "", stamp, "", "", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := time.Parse(time.RFC3339, stamp)
		if opts.Since == nil || !opts.Since.Equal(want) {
			t.Fatalf("Since = %v, want %v", opts.Since, want)
		}
	})

	t.Run("--since accepts a duration", func(t *testing.T) {
		before := time.Now()
		opts, err := searchOptions("", "", "8h", "", "", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		after := time.Now()
		if opts.Since == nil {
			t.Fatal("Since = nil, want a resolved timestamp")
		}
		// A duration is an offset back from now, so it must land in the window
		// the call itself spanned.
		if opts.Since.Before(before.Add(-8*time.Hour)) || opts.Since.After(after.Add(-8*time.Hour)) {
			t.Fatalf("Since = %v, want roughly 8h before now", opts.Since)
		}
	})

	t.Run("--until accepts RFC3339", func(t *testing.T) {
		stamp := "2026-07-18T17:30:00Z"
		opts, err := searchOptions("", "", "", stamp, "", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := time.Parse(time.RFC3339, stamp)
		if opts.Until == nil || !opts.Until.Equal(want) {
			t.Fatalf("Until = %v, want %v", opts.Until, want)
		}
	})

	// The asymmetry is deliberate: parseTime is called with allowDuration=false
	// for --until, so "8h" is an error rather than eight hours ago.
	t.Run("--until rejects a duration", func(t *testing.T) {
		if _, err := searchOptions("", "", "", "8h", "", "", 0); err == nil {
			t.Fatal("--until must not accept a duration")
		}
	})

	t.Run("an unparseable time is an error", func(t *testing.T) {
		if _, err := searchOptions("", "", "yesterday", "", "", "", 0); err == nil {
			t.Fatal("--since must reject text that is neither RFC3339 nor a duration")
		}
	})
}

// TestParseTime pins the helper directly, including the allowDuration switch
// that gives --since and --until different vocabularies.
func TestParseTime(t *testing.T) {
	stamp := "2026-07-18T09:00:00Z"
	want, _ := time.Parse(time.RFC3339, stamp)
	for _, allow := range []bool{true, false} {
		got, err := parseTime(stamp, allow)
		if err != nil {
			t.Fatalf("parseTime(%q, %v) returned %v", stamp, allow, err)
		}
		if !got.Equal(want) {
			t.Fatalf("parseTime(%q, %v) = %v, want %v", stamp, allow, got, want)
		}
	}

	if _, err := parseTime("90m", true); err != nil {
		t.Fatalf("a duration must parse when allowed: %v", err)
	}
	_, err := parseTime("90m", false)
	if err == nil {
		t.Fatal("a duration must not parse when disallowed")
	}
	// The message has to name the accepted vocabulary, since that is the only
	// hint the user gets about the asymmetry.
	if !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("error %q does not mention RFC3339", err)
	}
	if strings.Contains(err.Error(), "duration") {
		t.Fatalf("error %q offers a duration that is not accepted", err)
	}
	if _, err := parseTime("not a time", true); err == nil {
		t.Fatal("unparseable text must be an error")
	} else if !strings.Contains(err.Error(), "duration") {
		t.Fatalf("error %q should mention that a duration was accepted", err)
	}
}
