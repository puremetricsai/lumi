package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/puremetricsai/lumi/internal/store"
	_ "modernc.org/sqlite"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, _ := testStoreWithPath(t)
	return s
}

// testStoreWithPath opens a store and reports the file it lives in, for tests
// that need to reach the database behind the store's own API.
func testStoreWithPath(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lumi.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// bumpSchemaVersion writes a user_version above what this build's migrations
// reach, standing in for an index a newer Lumi wrote.
//
// It goes around store.Store because there is deliberately no API for this: a
// build never writes a version it does not have a migration for. The pragma is
// set on a separate connection to the same file, which is also how the real skew
// arises — another process, holding its own connection, migrating the file this
// one already has open.
func bumpSchemaVersion(t *testing.T, ctx context.Context, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatal(err)
	}
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

// findToolDescription returns one advertised tool's description, so a test can
// assert that documented numbers match the ones the store enforces.
func findToolDescription(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	session := connect(t, ctx, testStore(t))
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == name {
			return tool.Description
		}
	}
	t.Fatalf("tool %q is not advertised", name)
	return ""
}

// findToolInputSchema returns one advertised tool's generated input schema as
// JSON. Parameter defaults and caps are documented in jsonschema tags rather
// than in the prose description, so that is where a test must look for them.
func findToolInputSchema(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	session := connect(t, ctx, testStore(t))
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != name {
			continue
		}
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	t.Fatalf("tool %q is not advertised", name)
	return ""
}
