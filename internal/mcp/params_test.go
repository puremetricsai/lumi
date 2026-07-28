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
