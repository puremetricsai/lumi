package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/puremetricsai/lumi/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertEvents(t *testing.T, ctx context.Context, s *store.Store, events ...store.Event) []store.Event {
	t.Helper()
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

// callSearch runs the search_events handler the way the SDK would, failing the
// test on a tool error. Tests that assert on the error call the handler
// directly instead.
func callSearch(t *testing.T, ctx context.Context, h *handlers, in searchEventsInput) searchEventsOutput {
	t.Helper()
	_, out, err := h.searchEvents(ctx, nil, in)
	if err != nil {
		t.Fatalf("search_events: %v", err)
	}
	return out
}
