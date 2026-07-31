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
	"unicode/utf8"

	"github.com/puremetricsai/lumi/internal/store"
)

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
