package vocabulary

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestParseAppliesFileFormat(t *testing.T) {
	input := []byte(strings.Join([]string{
		"# a comment",
		"",
		"  Acme Corp  ",
		"Mostafa",
		"   # indented comment",
		"Acme Corp",
		"Kubernetes",
	}, "\n"))

	terms, dropped := Parse(input)

	want := []string{"Acme Corp", "Mostafa", "Kubernetes"}
	if !slices.Equal(terms, want) {
		t.Fatalf("Parse terms = %q, want %q", terms, want)
	}
	if dropped != 0 {
		t.Fatalf("Parse dropped = %d, want 0", dropped)
	}
}

func TestParseKeepsFirstOccurrenceOrder(t *testing.T) {
	terms, _ := Parse([]byte("Beta\nAlpha\nBeta\n"))
	want := []string{"Beta", "Alpha"}
	if !slices.Equal(terms, want) {
		t.Fatalf("Parse terms = %q, want %q", terms, want)
	}
}

func TestParseCapsAtMaxTermsAndCountsTheRest(t *testing.T) {
	var lines []string
	for i := 0; i < MaxTerms+7; i++ {
		lines = append(lines, fmt.Sprintf("term-%03d", i))
	}

	terms, dropped := Parse([]byte(strings.Join(lines, "\n")))

	if len(terms) != MaxTerms {
		t.Fatalf("len(terms) = %d, want %d", len(terms), MaxTerms)
	}
	if dropped != 7 {
		t.Fatalf("dropped = %d, want 7", dropped)
	}
	// File order is priority order: the cap keeps the earliest terms.
	if terms[0] != "term-000" || terms[MaxTerms-1] != fmt.Sprintf("term-%03d", MaxTerms-1) {
		t.Fatalf("cap did not keep the earliest terms: first=%q last=%q", terms[0], terms[MaxTerms-1])
	}
}

func TestParseDoesNotCountDuplicatesAsDropped(t *testing.T) {
	var lines []string
	for i := 0; i < MaxTerms; i++ {
		lines = append(lines, fmt.Sprintf("term-%03d", i))
	}
	lines = append(lines, "term-000", "term-001")

	terms, dropped := Parse([]byte(strings.Join(lines, "\n")))

	if len(terms) != MaxTerms {
		t.Fatalf("len(terms) = %d, want %d", len(terms), MaxTerms)
	}
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 (duplicates are not cap drops)", dropped)
	}
}

func TestParseEmptyInput(t *testing.T) {
	terms, dropped := Parse(nil)
	if len(terms) != 0 || dropped != 0 {
		t.Fatalf("Parse(nil) = (%q, %d), want (empty, 0)", terms, dropped)
	}
}

func TestParseKeepsTermsAfterAnOverLongLine(t *testing.T) {
	// bufio.Scanner's default 64KB token limit would stop here and silently
	// drop every term below the long line.
	oversized := strings.Repeat("x", 128*1024)
	input := []byte("Alpha\n" + oversized + "\nBravo\n")

	terms, _ := Parse(input)

	if !slices.Contains(terms, "Alpha") {
		t.Fatalf("terms = %q, want it to contain Alpha", terms)
	}
	if !slices.Contains(terms, "Bravo") {
		t.Fatalf("terms = %q, want Bravo to survive an over-long preceding line", terms)
	}
	if !slices.Contains(terms, oversized) {
		t.Fatal("the over-long line itself was dropped; it is a valid (if silly) term")
	}
}
