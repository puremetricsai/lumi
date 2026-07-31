package mcp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// digestLayout is short enough to read at a glance and still unambiguous across
// a range that spans midnight, which a bare clock time is not.
const digestLayout = "2006-01-02T15:04:05"

// summary wraps a digest as the tool result's Content.
//
// Returning a nil *sdk.CallToolResult would leave Content unset, and the go-sdk
// then copies the entire marshalled output into a TextContent block beside
// StructuredContent — the same bytes twice on every call, which for a full page
// of events is the largest single cost on the wire. Setting Content ourselves
// takes that branch out.
//
// The digest is a summary, never the data: an agent reads structuredContent.
// What it must carry is the notice, because a client that renders only Content
// would otherwise lose the one part of a response that says the results are
// incomplete — capped, filtered, or spanning unattributed audio.
func summary(parts ...string) *sdk.CallToolResult {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: strings.Join(kept, "; ")}},
	}
}

// countsBy renders "9 screen, 3 audio" from a tally, ordered by count and then
// by name so the same result always digests the same way.
func countsBy(tally map[string]int) string {
	if len(tally) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tally))
	for key := range tally {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if tally[keys[i]] != tally[keys[j]] {
			return tally[keys[i]] > tally[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", tally[key], key))
	}
	return strings.Join(parts, ", ")
}

// span renders the range a set of preformatted timestamps covers. The strings
// are the ones already on the wire, so the digest cannot disagree with the
// payload about when something happened. Unparseable or absent values are
// skipped rather than guessed at.
func span(stamps []string) string {
	var earliest, latest time.Time
	for _, stamp := range stamps {
		parsed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		if earliest.IsZero() || parsed.Before(earliest) {
			earliest = parsed
		}
		if parsed.After(latest) {
			latest = parsed
		}
	}
	if earliest.IsZero() {
		return ""
	}
	if earliest.Equal(latest) {
		return earliest.Format(digestLayout)
	}
	return earliest.Format(digestLayout) + " to " + latest.Format(digestLayout)
}

// plural is the difference between "1 events" and a digest that reads like a
// sentence.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, pluralForm)
}

// eventsDigest summarizes a page of search results.
func eventsDigest(events []EventRecord) string {
	if len(events) == 0 {
		return "no events"
	}
	tally := make(map[string]int, 2)
	stamps := make([]string, 0, len(events))
	for _, event := range events {
		tally[event.Kind]++
		stamps = append(stamps, event.CapturedAt)
	}
	digest := plural(len(events), "event", "events")
	if len(tally) > 1 {
		digest += " (" + countsBy(tally) + ")"
	}
	if covered := span(stamps); covered != "" {
		digest += ", " + covered
	}
	return digest
}

// eventDigest summarizes one event. It names the app the event is attributed to
// whichever field carries it, so the digest does not have to know that an audio
// row spells the pair foreground_app/foreground_window.
func eventDigest(event EventRecord) string {
	app := event.App
	if app == "" {
		app = event.ForegroundApp
	}
	digest := fmt.Sprintf("event %d: %s", event.ID, event.Kind)
	if app != "" {
		digest += ", " + app
	}
	return digest + fmt.Sprintf(", %s", plural(event.TextLength, "char", "chars"))
}

// appsDigest summarizes a list_apps inventory. In window mode the rows are
// titles for one application, which is a different thing to have counted.
func appsDigest(entries []AttributionRecord, app *string) string {
	noun, nounPlural := "application", "applications"
	if app != nil {
		noun, nounPlural = "window title", "window titles"
	}
	if len(entries) == 0 {
		return "no " + nounPlural
	}
	digest := plural(len(entries), noun, nounPlural)
	if app != nil {
		return digest + fmt.Sprintf(" for %q", *app)
	}
	return digest + ", most active first"
}

// turnsDigest summarizes an assembled transcript. Origin counts are the point:
// a transcript that is all one origin is a different thing from a conversation,
// and that is visible here without reading a turn.
func turnsDigest(turns []TranscriptTurnRecord) string {
	if len(turns) == 0 {
		return "no turns"
	}
	tally := make(map[string]int, 3)
	stamps := make([]string, 0, len(turns))
	for _, turn := range turns {
		tally[turn.Origin]++
		if turn.StartedAt != "" {
			stamps = append(stamps, turn.StartedAt)
		}
		if turn.EndedAt != "" {
			stamps = append(stamps, turn.EndedAt)
		}
	}
	digest := plural(len(turns), "turn", "turns")
	if len(tally) > 1 {
		digest += " (" + countsBy(tally) + ")"
	}
	if covered := span(stamps); covered != "" {
		digest += ", " + covered
	}
	return digest
}
