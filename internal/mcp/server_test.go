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
