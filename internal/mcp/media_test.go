package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// TestMediaDirAndFileRejoinToTheStoredPath is the contract the split rests on.
// Splitting a path a caller is told they can open is only safe if the two halves
// put it back together exactly, for every kind on the page.
func TestMediaDirAndFileRejoinToTheStoredPath(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	stored := map[string]string{
		"screen": "/var/lumi/screenshots/20260731T162914Z-346041-display-5.jpg",
		"audio":  "/var/lumi/audio/20260731T162832Z-798705-system.wav",
	}
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: at, Text: "a", MediaPath: stored["screen"]},
		store.Event{Kind: store.KindAudio, CapturedAt: at, Text: "b",
			MediaPath: stored["audio"], AudioSource: "system", DurationMS: 30000},
	)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{})
	if len(out.MediaDir) != 2 {
		t.Fatalf("a mixed page must hoist both kinds: %#v", out.MediaDir)
	}
	for _, event := range out.Events {
		rejoined := filepath.Join(out.MediaDir[event.Kind], event.MediaFile)
		if rejoined != stored[event.Kind] {
			t.Fatalf("%s rejoined to %q, want %q", event.Kind, rejoined, stored[event.Kind])
		}
	}
}

// TestMediaDirCarriesOnlyTheKindsOnThePage keeps the map from advertising a
// directory for a kind the caller did not receive.
func TestMediaDirCarriesOnlyTheKindsOnThePage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: at, Text: "a", MediaPath: "/var/lumi/screenshots/a.jpg"},
		store.Event{Kind: store.KindAudio, CapturedAt: at, Text: "b",
			MediaPath: "/var/lumi/audio/b.wav", AudioSource: "system", DurationMS: 30000},
	)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Kind: "screen"})
	if len(out.MediaDir) != 1 || out.MediaDir["screen"] != "/var/lumi/screenshots" {
		t.Fatalf("a screen-only page must hoist only screen: %#v", out.MediaDir)
	}
	if out.Events[0].MediaFile != "a.jpg" {
		t.Fatalf("media_file = %q", out.Events[0].MediaFile)
	}
}

// TestMediaFileIsTheStoredNameNotAComposedOne is why the basename is taken from
// the path rather than rebuilt from captured_at. The recorder's rename to the
// timestamped form is best-effort: a chunk whose rename fell through keeps the
// native temp name, and a caller composing the name from the formula would ask
// for a file that does not exist.
func TestMediaFileIsTheStoredNameNotAComposedOne(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	at := time.Date(2026, 7, 31, 16, 28, 32, 798705920, time.UTC)
	insertEvents(t, ctx, s, store.Event{
		Kind: store.KindAudio, CapturedAt: at, Text: "b", AudioSource: "system",
		MediaPath: "/var/lumi/audio/lumi-chunk-0417.wav", DurationMS: 30000,
	})
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{})
	if out.Events[0].MediaFile != "lumi-chunk-0417.wav" {
		t.Fatalf("media_file = %q, want the stored name", out.Events[0].MediaFile)
	}
	if rejoined := filepath.Join(out.MediaDir["audio"], out.Events[0].MediaFile); rejoined !=
		"/var/lumi/audio/lumi-chunk-0417.wav" {
		t.Fatalf("a fallback-named file must still rejoin: %q", rejoined)
	}
}

// TestSplitMediaDirLeavesWholePaths covers an index carried between data dirs,
// where one kind's files sit in two directories. Hoisting either would silently
// point half the events at the wrong place, so that kind keeps whole paths and
// gets no media_dir entry.
func TestSplitMediaDirLeavesWholePaths(t *testing.T) {
	records := []EventRecord{
		{Kind: "screen", MediaFile: "/old/screenshots/a.jpg"},
		{Kind: "screen", MediaFile: "/new/screenshots/b.jpg"},
		{Kind: "audio", MediaFile: "/new/audio/c.wav"},
	}
	dirs := hoistMediaDir(records)

	if _, present := dirs["screen"]; present {
		t.Fatalf("a split kind must not be hoisted: %#v", dirs)
	}
	if dirs["audio"] != "/new/audio" {
		t.Fatalf("the unsplit kind must still hoist: %#v", dirs)
	}
	if records[0].MediaFile != "/old/screenshots/a.jpg" || records[1].MediaFile != "/new/screenshots/b.jpg" {
		t.Fatalf("split-kind records must keep whole paths: %#v", records[:2])
	}
	if records[2].MediaFile != "c.wav" {
		t.Fatalf("the unsplit kind must be shortened: %q", records[2].MediaFile)
	}
}

// TestEventWithNoMediaHoistsNothing keeps an event that never had a file — a
// row whose capture failed before anything was written — from inventing one.
func TestEventWithNoMediaHoistsNothing(t *testing.T) {
	records := []EventRecord{{Kind: "screen", MediaFile: ""}}
	if dirs := hoistMediaDir(records); dirs != nil {
		t.Fatalf("media_dir = %#v, want none", dirs)
	}
	if records[0].MediaFile != "" {
		t.Fatalf("media_file = %q, want empty", records[0].MediaFile)
	}
}
