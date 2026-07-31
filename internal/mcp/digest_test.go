package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/puremetricsai/lumi/internal/store"
)

// textOf returns a tool result's Content as one string, failing the test if the
// result carries anything but text blocks — an image or audio block would mean
// this package started returning captured media.
func textOf(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	var parts []string
	for _, content := range result.Content {
		text, ok := content.(*sdk.TextContent)
		if !ok {
			t.Fatalf("tool result carried a %T, not text", content)
		}
		parts = append(parts, text.Text)
	}
	return strings.Join(parts, "\n")
}

// TestToolResultsDoNotRepeatTheStructuredPayload is the whole reason the digest
// exists. Left to itself the go-sdk copies the entire marshalled output into a
// TextContent block beside StructuredContent, so every call ships the same bytes
// twice; on a full page of events that was the largest single cost on the wire.
//
// The assertion is that Content does not parse as the payload, not that it is
// short: a digest that happened to be valid JSON of the right shape would mean
// the copy was back.
func TestToolResultsDoNotRepeatTheStructuredPayload(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: at, Text: "quarterly roadmap review",
			App: "Safari", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindAudio, CapturedAt: at, Text: "discuss the launch budget",
			AudioSource: "system", MediaPath: "/tmp/b.wav", DurationMS: 30000},
	)
	session := connect(t, ctx, s)

	for _, call := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"search_events", map[string]any{}, "2 events"},
		{"list_apps", map[string]any{}, "application"},
		{"get_transcript", map[string]any{"since": "24h"}, "turn"},
	} {
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: call.tool, Arguments: call.args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s reported an error: %s", call.tool, textOf(t, result))
		}
		content := textOf(t, result)
		if result.StructuredContent == nil {
			t.Fatalf("%s returned no structured content", call.tool)
		}
		var payload map[string]any
		if json.Unmarshal([]byte(content), &payload) == nil {
			t.Fatalf("%s still ships the payload as text: %s", call.tool, content)
		}
		if !strings.Contains(content, call.want) {
			t.Fatalf("%s digest %q does not mention %q", call.tool, content, call.want)
		}
		// A digest is a summary. If it ever approaches the payload's size the
		// duplication has crept back in some other form.
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if len(content) >= len(encoded) {
			t.Fatalf("%s digest (%d bytes) is not smaller than its payload (%d bytes)",
				call.tool, len(content), len(encoded))
		}
	}
}

// TestDigestCarriesTheNotice is the one thing the digest must not drop. A client
// that renders only Content would otherwise be told nothing about a result that
// was capped, filtered, or drawn from a range holding unattributed audio — the
// exact case where a partial answer reads as a complete one.
func TestDigestCarriesTheNotice(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: at, Text: "a", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: at.Add(time.Second), Text: "b", MediaPath: "/tmp/b.jpg"},
	)
	h := &handlers{store: s}

	res, out, err := h.searchEvents(ctx, nil, searchEventsInput{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Notice == "" {
		t.Fatal("a capped page must carry a notice for this test to mean anything")
	}
	if content := textOf(t, res); !strings.Contains(content, out.Notice) {
		t.Fatalf("digest %q dropped the notice %q", content, out.Notice)
	}
}

// TestEmptyResultsStillDigest covers the branch where there is nothing to
// summarize: the notice explaining why is the entire useful content.
func TestEmptyResultsStillDigest(t *testing.T) {
	ctx := context.Background()
	h := &handlers{store: testStore(t)}

	res, out, err := h.searchEvents(ctx, nil, searchEventsInput{})
	if err != nil {
		t.Fatal(err)
	}
	content := textOf(t, res)
	if !strings.HasPrefix(content, "no events") {
		t.Fatalf("empty digest = %q", content)
	}
	if !strings.Contains(content, out.Notice) {
		t.Fatalf("empty digest %q dropped the notice %q", content, out.Notice)
	}
}

func TestSpanReportsTheRangeNotTheOrder(t *testing.T) {
	// Search returns by relevance, so the first record is not the earliest. The
	// digest must still name the range the page actually covers.
	stamps := []string{
		"2026-07-31T09:29:14.346041-07:00",
		"2026-07-31T09:00:12.000000-07:00",
		"2026-07-31T09:15:00.000000-07:00",
	}
	if got := span(stamps); got != "2026-07-31T09:00:12 to 2026-07-31T09:29:14" {
		t.Fatalf("span = %q", got)
	}
	if got := span([]string{stamps[0], stamps[0]}); got != "2026-07-31T09:29:14" {
		t.Fatalf("a single instant must not render as a range: %q", got)
	}
	if got := span([]string{"not a timestamp"}); got != "" {
		t.Fatalf("an unparseable stamp must be skipped, got %q", got)
	}
}
