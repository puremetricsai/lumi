package cli

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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
