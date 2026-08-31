package mcp

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/puremetricsai/lumi/internal/store"
)

func TestParseTimestampAcceptsRFC3339AndDurations(t *testing.T) {
	got, err := parseTimestamp("since", "2026-07-27T14:02:11-07:00")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 27, 21, 2, 11, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("parsed %s, want %s", got.UTC(), want)
	}

	got, err = parseTimestamp("since", "2h")
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(*got)
	if elapsed < 2*time.Hour-time.Minute || elapsed > 2*time.Hour+time.Minute {
		t.Fatalf("a duration must mean that long ago, got %s ago", elapsed)
	}

	got, err = parseTimestamp("until", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("an empty value must mean no bound, got %v", got)
	}
}

// TestParseTimestampErrorNamesTheFieldAndForms pins the contract that makes the
// tool self-correcting: an agent that mis-formats a time must be able to read
// the reply and retry without guessing.
func TestParseTimestampErrorNamesTheFieldAndForms(t *testing.T) {
	for _, value := range []string{"yesterday", "-2h", "2026-13-45"} {
		got, err := parseTimestamp("since", value)
		if err == nil {
			t.Fatalf("parseTimestamp(%q) returned %v, want an error", value, got)
		}
		message := err.Error()
		for _, want := range []string{"since", "RFC3339", "duration", value} {
			if !strings.Contains(message, want) {
				t.Fatalf("error %q does not mention %q", message, want)
			}
		}
	}
}

func TestParseKind(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want store.Kind
	}{
		{"", ""},
		{"all", ""},
		{"screen", store.KindScreen},
		{"audio", store.KindAudio},
	} {
		got, err := parseKind(tc.in)
		if err != nil {
			t.Fatalf("parseKind(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("parseKind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	_, err := parseKind("video")
	if err == nil {
		t.Fatal("parseKind(\"video\") must fail")
	}
	for _, want := range []string{"screen", "audio", "video"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestParseMatch(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want store.MatchMode
	}{
		{"", store.MatchAll},
		{"all", store.MatchAll},
		{"any", store.MatchAny},
	} {
		got, err := parseMatch(tc.in)
		if err != nil {
			t.Fatalf("parseMatch(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("parseMatch(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	_, err := parseMatch("some")
	if err == nil {
		t.Fatal("parseMatch(\"some\") must fail")
	}
	for _, want := range []string{"all", "any", "some"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestTruncateText(t *testing.T) {
	for _, tc := range []struct {
		name          string
		in            string
		max           int
		wantOut       string
		wantTruncated bool
		wantLength    int
	}{
		{"under the cap is untouched", "hello", 10, "hello", false, 5},
		{"exactly at the cap is untouched", "hello", 5, "hello", false, 5},
		{"over the cap is cut", "hello world", 5, "hello", true, 11},
		{"a zero cap means no cap", "hello world", 0, "hello world", false, 11},
		{"a negative cap means no cap", "hello world", -1, "hello world", false, 11},
		{"empty text", "", 600, "", false, 0},
		{"multibyte text is cut on a rune boundary", strings.Repeat("日", 50), 20, strings.Repeat("日", 20), true, 50},
		{"emoji are cut on a rune boundary", strings.Repeat("🙂", 50), 3, strings.Repeat("🙂", 3), true, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, truncated, length := truncateText(tc.in, tc.max)
			if out != tc.wantOut {
				t.Fatalf("text = %q, want %q", out, tc.wantOut)
			}
			if truncated != tc.wantTruncated {
				t.Fatalf("truncated = %v, want %v", truncated, tc.wantTruncated)
			}
			if length != tc.wantLength {
				t.Fatalf("length = %d, want %d", length, tc.wantLength)
			}
			if !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) {
				t.Fatalf("truncation broke UTF-8: %q", out)
			}
		})
	}
}

// TestSearchLimitDescriptionMatchesStoreBounds guards the one place where the
// agent-facing contract is a literal rather than a constant: the limit
// parameter's jsonschema tag names the default and cap that store.Search
// actually enforces, and a struct tag cannot interpolate them.
func TestSearchLimitDescriptionMatchesStoreBounds(t *testing.T) {
	field, ok := reflect.TypeOf(searchEventsInput{}).FieldByName("Limit")
	if !ok {
		t.Fatal("searchEventsInput has no Limit field")
	}
	description := field.Tag.Get("jsonschema")
	want := fmt.Sprintf("defaults to %d and is capped at %d", store.DefaultSearchLimit, store.MaxSearchLimit)
	if !strings.Contains(description, want) {
		t.Fatalf("limit description %q does not state %q", description, want)
	}
}

// TestExcerptAroundFallsThroughToTruncateText pins the floor. Every case here
// is one excerptAround refuses to centre, and on each of them it must return
// exactly what shipped before it existed — compared against a live truncateText
// call rather than a literal, so the two cannot drift.
func TestExcerptAroundFallsThroughToTruncateText(t *testing.T) {
	const body = "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi"
	cases := []struct {
		name  string
		text  string
		terms []string
		max   int
	}{
		{"no cap", body, []string{"kappa"}, 0},
		{"negative cap", body, []string{"kappa"}, -1},
		{"no terms", body, nil, 20},
		{"empty terms", body, []string{}, 20},
		{"text shorter than the cap", "short", []string{"short"}, 600},
		{"term absent", body, []string{"kubernetes"}, 20},
		// FTS5 folds diacritics (unicode61 remove_diacritics 2) and would have
		// matched this row; the Go scan does not. That is the documented floor:
		// a miss means "no excerpt to centre on", never "this row did not match".
		{"diacritic-folded miss", "a long line of filler text mentioning cafe somewhere inside it", []string{"café"}, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotText, gotTrunc, gotLen := excerptAround(tc.text, tc.terms, tc.max)
			wantText, wantTrunc, wantLen := truncateText(tc.text, tc.max)
			if gotText != wantText || gotTrunc != wantTrunc || gotLen != wantLen {
				t.Fatalf("excerptAround = (%q, %v, %d), want truncateText's (%q, %v, %d)",
					gotText, gotTrunc, gotLen, wantText, wantTrunc, wantLen)
			}
		})
	}
}

// TestExcerptAroundCentersOnTheMatch is the whole point: a term past the cap
// must survive into the window, and the reported length must stay the length of
// the WHOLE text so truncated/text_length keep meaning what they meant.
func TestExcerptAroundCentersOnTheMatch(t *testing.T) {
	text := strings.Repeat("menu bar chrome ", 100) + "INVOICE 4471 due friday" + strings.Repeat(" trailing filler", 100)
	got, truncated, length := excerptAround(text, []string{"invoice"}, 120)
	if !strings.Contains(strings.ToLower(got), "invoice") {
		t.Fatalf("excerpt does not contain the match: %q", got)
	}
	if !truncated {
		t.Fatal("truncated = false on a text longer than the cap")
	}
	if want := utf8.RuneCountInString(text); length != want {
		t.Fatalf("length = %d, want the whole text's %d", length, want)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("a mid-text window should be elided at both ends: %q", got)
	}
}

// TestExcerptAroundClampsAtBothEnds pins the edge the windowing gets wrong:
// a match near either end must pin to that end rather than run past the text.
func TestExcerptAroundClampsAtBothEnds(t *testing.T) {
	const max = 40
	head := "invoice at the very start " + strings.Repeat("filler ", 60)
	got, _, _ := excerptAround(head, []string{"invoice"}, max)
	if strings.HasPrefix(got, "…") {
		t.Fatalf("a match at rune 0 must not be elided at the head: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a clamped head window still elides its tail: %q", got)
	}
	if n := utf8.RuneCountInString(strings.Trim(got, "…")); n != max {
		t.Fatalf("window is %d runes, want %d", n, max)
	}

	tail := strings.Repeat("filler ", 60) + "trailing invoice"
	got, _, _ = excerptAround(tail, []string{"invoice"}, max)
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("a clamped tail window still elides its head: %q", got)
	}
	if strings.HasSuffix(got, "…") {
		t.Fatalf("a match at the very end must not be elided at the tail: %q", got)
	}
	if !strings.Contains(got, "invoice") {
		t.Fatalf("clamping lost the match: %q", got)
	}
}

// TestExcerptAroundNeverSplitsAMultiByteRune is why the search runs in rune
// space and strings.Index is never called: a byte offset windowed as if it were
// a rune offset is exactly the corruption truncateText counts runes to prevent.
func TestExcerptAroundNeverSplitsAMultiByteRune(t *testing.T) {
	const max = 30
	text := strings.Repeat("日本語のテキスト", 40) + "café" + strings.Repeat("のあとにもっと", 40)
	got, _, _ := excerptAround(text, []string{"café"}, max)
	if !utf8.ValidString(got) {
		t.Fatalf("excerpt is not valid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("excerpt contains a replacement rune: %q", got)
	}
	if !strings.Contains(got, "café") {
		t.Fatalf("excerpt lost the multi-byte match: %q", got)
	}
	if n := utf8.RuneCountInString(strings.Trim(got, "…")); n != max {
		t.Fatalf("window is %d runes, want %d", n, max)
	}
}

// TestExcerptAroundPrefersATokenBoundaryHit is what keeps "class" off a
// classification thousands of runes earlier in the same page.
func TestExcerptAroundPrefersATokenBoundaryHit(t *testing.T) {
	text := "classification " + strings.Repeat("filler ", 100) + "the class began"
	got, _, _ := excerptAround(text, []string{"class"}, 40)
	if !strings.Contains(got, "the class began") {
		t.Fatalf("window did not centre on the standalone token: %q", got)
	}
	if strings.Contains(got, "classification") {
		t.Fatalf("window centred on the substring hit instead: %q", got)
	}
}

// TestExcerptAroundTakesTheEarliestHitOfAnyTerm: under match "any" the
// first-listed term need not appear at all, so the earliest hit of any of them
// is the one that earned the row its place.
func TestExcerptAroundTakesTheEarliestHitOfAnyTerm(t *testing.T) {
	text := strings.Repeat("head ", 20) + "beta marker " + strings.Repeat("mid ", 100) + "alpha marker" + strings.Repeat(" tail", 50)
	got, _, _ := excerptAround(text, []string{"alpha", "beta"}, 40)
	if !strings.Contains(got, "beta marker") {
		t.Fatalf("window did not take the earliest hit of any term: %q", got)
	}
}
