// Package mcp serves Lumi's activity index to MCP-capable agents over stdio.
//
// It depends on internal/store and nothing else of Lumi's: internal/cli opens
// the store and calls Serve, never the other way round.
//
// Two rules hold everywhere in this package. Nothing may write to os.Stdout
// except JSON-RPC protocol frames — the stdio transport owns that stream, and a
// stray write silently corrupts the session. And no tool returns screenshot or
// audio bytes: media_dir joined to an event's media_file is a path the user can
// open themselves.
package mcp

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/puremetricsai/lumi/internal/store"
)

// localStamp renders an instant the way every timestamp this package returns is
// rendered: the machine's local zone with its offset, at nanosecond precision.
//
// One function rather than a convention, because the two halves of the rule are
// not equally obvious. The precision is what lets a value round-trip when it is
// handed back as a since or until bound. The *zone* matters for a reason that
// only shows up between fields: an agent has no way to know that two timestamps
// in one response are the same kind of thing, so it compares them as strings —
// and resume_from was rendered UTC while captured_at, started_at, ended_at and
// last_seen were local. Both parse, both round-trip, and the notice that offers
// resume_from names covered_until in the same sentence, so the two read seven
// hours apart while describing adjacent moments.
//
// Storage and range comparison stay UTC; this is a rendering rule only.
func localStamp(at time.Time) string {
	return at.Local().Format(time.RFC3339Nano)
}

// parseTimestamp accepts an RFC3339 timestamp or a Go duration such as "2h",
// which reads as "that long ago". An empty value means "no bound".
//
// field names the offending parameter in the error because these tools face an
// agent, not a person: the reply is the only documentation it gets mid-task, so
// it has to say which parameter was wrong and what the accepted forms are.
// Negative durations are rejected rather than silently read as future times.
func parseTimestamp(field, value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return &parsed, nil
	}
	if duration, err := time.ParseDuration(value); err == nil && duration >= 0 {
		parsed := time.Now().Add(-duration)
		return &parsed, nil
	}
	return nil, fmt.Errorf("%s must be an RFC3339 timestamp such as 2026-07-27T14:02:11-07:00, "+
		"or a non-negative duration such as 2h or 45m meaning that long ago; got %q", field, value)
}

// parseKind maps the tool's kind parameter onto a store.Kind. An empty value
// (or "all") means both modalities.
func parseKind(value string) (store.Kind, error) {
	switch strings.TrimSpace(value) {
	case "", "all":
		return "", nil
	case "screen":
		return store.KindScreen, nil
	case "audio":
		return store.KindAudio, nil
	}
	return "", fmt.Errorf(`kind must be "screen" or "audio", or omitted for both; got %q`, value)
}

// parseMatch maps the tool's match parameter onto a store.MatchMode. The zero
// value is MatchAll, so an omitted match keeps conjunctive semantics.
func parseMatch(value string) (store.MatchMode, error) {
	switch strings.TrimSpace(value) {
	case "", "all":
		return store.MatchAll, nil
	case "any":
		return store.MatchAny, nil
	}
	return store.MatchAll, fmt.Errorf(`match must be "all" or "any"; got %q`, value)
}

// clampLimit resolves a caller-supplied page size: zero or less takes the
// default, anything above the ceiling is capped. Handlers must clamp before
// calling the store so the value they report back in a "capped at N" notice is
// the limit that was actually applied.
func clampLimit(limit, fallback, max int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > max {
		return max
	}
	return limit
}

// truncateText caps text at maxChars runes, reporting whether it cut and the
// untruncated length in runes. maxChars of zero or less means no cap.
//
// Counting runes rather than bytes keeps a cut from splitting a multi-byte
// character. Returning the true length is what makes truncation safe at all: an
// agent that sees a shortened body can tell it is looking at a fragment and
// fetch the rest with get_event, instead of reasoning over half a transcript
// believing it has the whole thing.
func truncateText(text string, maxChars int) (string, bool, int) {
	length := utf8.RuneCountInString(text)
	if maxChars <= 0 || length <= maxChars {
		return text, false, length
	}
	return string([]rune(text)[:maxChars]), true, length
}

// excerptAround returns the maxChars-rune window of text centred on the
// earliest occurrence of any term, reporting whether anything was elided and
// the rune length of the WHOLE text. The return triple means exactly what
// truncateText's does — only which window is shown changes.
//
// It exists because the blind prefix cut it wraps usually did not contain the
// match. Full-display OCR averages 1,747 chars and runs to 8,643, so the first
// 600 runes are the menu bar and the tab strip: sampled on the live index, the
// first occurrence of the query term sits past char 600 in 22% of rows for
// "claude", 38% for "meeting", and 100% for "invoice". A real call for "claude"
// came back 842 chars long, beginning "Comet\nFile\nEdit\nView\nAssistant…",
// with the word nowhere in it — on 20 of 20 rows marked truncated. The agent
// paid for chrome and then followed the description into twenty get_event
// calls to answer one question.
//
// Two details are load-bearing. The search runs in RUNE space: strings.Index is
// never called, so the multi-byte split truncateText counts runes to prevent
// cannot occur here either — the byte offset is removed rather than converted.
// And a token-boundary hit beats every substring hit, so "class" centres on a
// standalone class and not on a classification 2,000 runes earlier.
//
// Go-side matching cannot reproduce FTS5's, and it does not try. events_fts
// folds diacritics (unicode61 remove_diacritics 2), a whitespace-separated term
// becomes a quoted phrase that can span tokens, and the index covers app and
// window as well as text — so a row can legitimately match on its window title
// with the term absent from text entirely. A miss here means "no excerpt to
// centre on", never "this row did not match": every miss falls through to
// truncateText, so the floor is exactly the behaviour this replaces and no row
// can come back worse off.
func excerptAround(text string, terms []string, maxChars int) (string, bool, int) {
	if maxChars <= 0 || len(terms) == 0 {
		return truncateText(text, maxChars)
	}
	runes := []rune(text)
	full := len(runes)
	if full <= maxChars {
		return truncateText(text, maxChars)
	}

	lowered := lowerRunes(text)
	match, found := earliestMatch(lowered, terms)
	if !found {
		return truncateText(text, maxChars)
	}

	// maxChars/4 of leading context rather than dead-centring: what follows a
	// search term is generally what answers the question, and the text before it
	// is chrome. The clamp is what makes a match in the first or last stretch pin
	// to that end instead of running past the text.
	start := match - maxChars/4
	if start < 0 {
		start = 0
	}
	if start > full-maxChars {
		start = full - maxChars
	}
	end := start + maxChars

	// The … markers sit OUTSIDE the maxChars budget, matching how maxChars is
	// documented on truncateText.
	window := string(runes[start:end])
	if start > 0 {
		window = "…" + window
	}
	if end < full {
		window += "…"
	}
	return window, true, full
}

// earliestMatch returns the rune offset to centre on: the earliest hit of ANY
// term, not of the first term. Under match: "any" the first-listed term need
// not appear at all, so the earliest hit of any of them is the one that earned
// the row its place.
//
// A token-boundary hit anywhere in the text wins over every bare substring hit,
// which is what keeps "class" off a classification earlier in the page.
//
// ponytail: naive rune scan — a handful of terms over at most ~9k runes of OCR
// text. Swap in a real substring search only if a page of results shows up in a
// profile.
func earliestMatch(lowered []rune, terms []string) (int, bool) {
	bestBoundary, haveBoundary := 0, false
	bestSubstring, haveSubstring := 0, false

	for _, term := range terms {
		needle := lowerRunes(term)
		if len(needle) == 0 {
			continue
		}
		for at := 0; at+len(needle) <= len(lowered); at++ {
			if !matchAt(lowered, needle, at) {
				continue
			}
			if !haveSubstring || at < bestSubstring {
				bestSubstring, haveSubstring = at, true
			}
			if isTokenBoundary(lowered, at, len(needle)) {
				if !haveBoundary || at < bestBoundary {
					bestBoundary, haveBoundary = at, true
				}
				break
			}
		}
	}

	if haveBoundary {
		return bestBoundary, true
	}
	if haveSubstring {
		return bestSubstring, true
	}
	return 0, false
}

// lowerRunes lowercases per rune rather than with strings.ToLower, which is not
// length-preserving — U+0130 lowercases to two runes and would slide every
// offset after it out of step with the original text.
func lowerRunes(s string) []rune {
	runes := []rune(s)
	for i, r := range runes {
		runes[i] = unicode.ToLower(r)
	}
	return runes
}

func matchAt(haystack, needle []rune, at int) bool {
	for i, r := range needle {
		if haystack[at+i] != r {
			return false
		}
	}
	return true
}

// isTokenBoundary reports whether the run at [at, at+length) is flanked by
// non-word runes. Deliberately not a \b regexp: OCR text is any script.
func isTokenBoundary(runes []rune, at, length int) bool {
	if at > 0 && isWordRune(runes[at-1]) {
		return false
	}
	if end := at + length; end < len(runes) && isWordRune(runes[end]) {
		return false
	}
	return true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
