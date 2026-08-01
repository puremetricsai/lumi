package mcp

import (
	"context"
	"encoding/json"
	"reflect"
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

// TestEveryToolResultCarriesItsWholePayloadAsText is the regression test for the
// failure that motivated it: an agent asked for a meeting it had watched Lumi
// record, and got back turn counts and a time span with no words in them.
//
// structuredContent is optional for a client to read. Plenty of them — Codex CLI
// and Claude Desktop among the three `lumi mcp setup` registers — render the
// content blocks and nothing else, which is why the spec says a tool returning
// structured content SHOULD also return the serialized JSON in a TextContent
// block, and why the go-sdk does it automatically for a handler that leaves
// Content nil. A digest in Content suppressed that and left those clients with a
// summary of data they never received.
//
// The assertion is equality with structuredContent rather than "mentions the
// text", because the two must stay interchangeable: any future summary, cap, or
// elision applied to one side alone is the same bug again.
func TestEveryToolResultCarriesItsWholePayloadAsText(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	at := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	started, ended := at.Add(time.Second), at.Add(3*time.Second)
	attributedChunk(t, ctx, s, at,
		store.Segment{Origin: store.OriginExternal, SourceTrack: "microphone",
			Text: "we should ship the beta before the conference", StartedAt: &started,
			EndedAt: &ended, Confidence: 0.44, OrderConfidence: "exact"},
	)
	screen := insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: at, Text: "quarterly roadmap review",
			App: "Safari", MediaPath: "/tmp/a.jpg"},
	)[0]
	session := connect(t, ctx, s)

	for _, call := range []struct {
		tool string
		args map[string]any
		// want is a body of text the payload holds and a digest would drop. It is
		// the symptom stated directly, next to the structural check.
		want string
	}{
		{"search_events", map[string]any{}, "quarterly roadmap review"},
		{"get_event", map[string]any{"id": screen.ID}, "quarterly roadmap review"},
		{"list_apps", map[string]any{}, "Safari"},
		{"get_transcript", map[string]any{"since": "24h"}, "ship the beta before the conference"},
	} {
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: call.tool, Arguments: call.args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s reported an error: %s", call.tool, textOf(t, result))
		}
		if result.StructuredContent == nil {
			t.Fatalf("%s returned no structured content", call.tool)
		}
		content := textOf(t, result)
		if !strings.Contains(content, call.want) {
			t.Errorf("%s content never carries %q, so a client that reads only content "+
				"cannot see it: %s", call.tool, call.want, content)
		}
		var fromText any
		if err := json.Unmarshal([]byte(content), &fromText); err != nil {
			t.Errorf("%s content does not parse as the payload: %v", call.tool, err)
			continue
		}
		// Round-trip the structured side through JSON too, so this compares two
		// decoded documents rather than a document against Go types.
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var fromStructured any
		if err := json.Unmarshal(encoded, &fromStructured); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fromText, fromStructured) {
			t.Errorf("%s content and structuredContent disagree:\n text: %s\n data: %s",
				call.tool, content, encoded)
		}
	}
}

// TestContentCarriesTheNotice pins the half of the payload that is easiest to
// lose and worst to lose: the sentence saying the result is capped, filtered, or
// drawn from a range holding unattributed audio. It rides along in the payload
// now rather than in a hand-composed summary, but a client that reads only
// content still has to receive it.
func TestContentCarriesTheNotice(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: at, Text: "a", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: at.Add(time.Second), Text: "b", MediaPath: "/tmp/b.jpg"},
	)
	session := connect(t, ctx, s)

	result, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "search_events", Arguments: map[string]any{"limit": 1}})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Notice string `json:"notice"`
	}
	content := textOf(t, result)
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("content does not parse: %v", err)
	}
	if payload.Notice == "" {
		t.Fatal("a capped page must carry a notice for this test to mean anything")
	}
	if !strings.Contains(payload.Notice, "capped") {
		t.Errorf("a capped page's notice does not say so: %q", payload.Notice)
	}
}
