package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/puremetricsai/lumi/internal/store"
)

// connect wires a server and client over the SDK's in-memory transport and
// returns the initialized client session.
func connect(t *testing.T, ctx context.Context, s *store.Store) *sdk.ClientSession {
	t.Helper()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	serverSession, err := newServer(s, Options{Name: "lumi", Version: "test"}).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	clientSession, err := sdk.NewClient(&sdk.Implementation{Name: "test-agent", Version: "0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clientSession.Close() })
	return clientSession
}

func TestServerAdvertisesEveryToolWithSchemas(t *testing.T) {
	ctx := context.Background()
	session := connect(t, ctx, testStore(t))

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tool := range tools.Tools {
		found[tool.Name] = true
		if tool.Description == "" {
			t.Fatalf("tool %q has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Fatalf("tool %q has no generated input schema", tool.Name)
		}
	}
	for _, want := range []string{"search_events", "get_event", "list_apps", "get_transcript"} {
		if !found[want] {
			t.Fatalf("tool %q was not advertised; got %v", want, found)
		}
	}
}

func TestCallToolSearchEventsOverTheProtocol(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, Text: "quarterly roadmap review", App: "Safari", MediaPath: "/tmp/a.jpg"},
	)
	session := connect(t, ctx, s)

	result, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "search_events",
		Arguments: map[string]any{"query": "roadmap", "app": "Safari"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %#v", result.Content)
	}
	var out searchEventsOutput
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 || out.Events[0].App != "Safari" {
		t.Fatalf("unexpected structured content: %#v", out.Events)
	}
}

// TestInvalidArgumentsAreToolErrorsNotProtocolErrors pins the spec's error
// rule: a bad filter value must come back as a readable tool result the agent
// can retry from, never as a JSON-RPC fault that looks like Lumi crashed.
func TestInvalidArgumentsAreToolErrorsNotProtocolErrors(t *testing.T) {
	ctx := context.Background()
	session := connect(t, ctx, testStore(t))

	result, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "search_events",
		Arguments: map[string]any{"kind": "video"},
	})
	if err != nil {
		t.Fatalf("a bad value must not be a protocol error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError on an invalid kind")
	}
	var message strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			message.WriteString(text.Text)
		}
	}
	for _, want := range []string{"screen", "audio"} {
		if !strings.Contains(message.String(), want) {
			t.Fatalf("error text %q does not enumerate the valid kinds", message.String())
		}
	}
}

// TestInitializeCarriesRoutingInstructions pins the channel itself: the SDK
// returns ServerOptions.Instructions in the initialize result and clients
// render it once per session, so a nil ServerOptions is a silent regression —
// nothing errors, the agent just goes back to opening audio questions with
// search_events.
func TestInitializeCarriesRoutingInstructions(t *testing.T) {
	ctx := context.Background()
	session := connect(t, ctx, testStore(t))

	got := session.InitializeResult().Instructions
	if strings.TrimSpace(got) == "" {
		t.Fatal("the initialize result carries no instructions")
	}
	for _, want := range []string{"get_transcript", "search_events", "list_apps", "get_event", "max_text_chars", "notice"} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions never mention %q: %s", want, got)
		}
	}
}

// TestInstructionsDoNotRestateTheToolDescriptions guards the reason these are
// short. Instructions are cross-tool ROUTING; every rule about what a field
// means lives in the tool description that returns it. A copy here would be a
// second home for a rule that no test could keep in step with the first, and it
// would be the copy an agent reads first — so the budget and the vocabulary ban
// are both deliberate.
func TestInstructionsDoNotRestateTheToolDescriptions(t *testing.T) {
	const budget = 1400
	if n := len(serverInstructions); n > budget {
		t.Fatalf("instructions are %d chars, over the %d-char routing budget; a rule about a field belongs in that tool's description", n, budget)
	}
	for _, forbidden := range []string{
		"emitting_process", "foreground_inferred", "unattributed",
		"audio_source", "source_app", "foreground_app", "attribution",
		"resume_from", "media_dir", "media_file", "order_confidence",
	} {
		if strings.Contains(serverInstructions, forbidden) {
			t.Fatalf("instructions restate %q, which the tool descriptions already carry", forbidden)
		}
	}
}
