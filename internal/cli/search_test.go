package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// seedAudioPair inserts a system+microphone pair sharing one captured_at.
func seedAudioPair(t *testing.T, dataDir string, at time.Time, systemText, micText string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(dataDir, "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events := []store.Event{
		{Kind: store.KindAudio, CapturedAt: at, AudioSource: "system", Text: systemText, MediaPath: "/tmp/sys.wav", DurationMS: 30000},
		{Kind: store.KindAudio, CapturedAt: at, AudioSource: "microphone", Text: micText, MediaPath: "/tmp/mic.wav", DurationMS: 30000},
	}
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}
}

// runSearchCapturingStdout executes `lumi search` with the given args, capturing
// the process's real os.Stdout (the search command writes results there, not to
// the cobra buffer).
func runSearchCapturingStdout(t *testing.T, dataDir string, args ...string) string {
	t.Helper()
	realStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = realStdout })

	captured := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		captured <- string(data)
	}()

	a := &app{dataDir: dataDir}
	cmd := a.searchCommand()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	execErr := cmd.ExecuteContext(context.Background())
	os.Stdout = realStdout
	w.Close()
	out := <-captured
	if execErr != nil {
		t.Fatalf("search %v: %v (stderr=%q)", args, execErr, stderr.String())
	}
	return out
}

// TestSearchJSONWithoutFlagStaysBareArray pins the faithful-export contract:
// with no --collapse-audio, `lumi search --json` must emit a bare []store.Event
// array, byte-for-byte what it emitted before this feature.
func TestSearchJSONWithoutFlagStaysBareArray(t *testing.T) {
	dataDir := t.TempDir()
	base := time.Now().UTC().Truncate(time.Second)
	seedAudioPair(t, dataDir, base, "system heard this", "microphone heard this")

	out := runSearchCapturingStdout(t, dataDir, "--type", "audio", "--json")

	// A bare array decodes into []store.Event and both rows survive unmerged.
	var events []store.Event
	if err := json.Unmarshal([]byte(out), &events); err != nil {
		t.Fatalf("default --json must be a bare array: %v\nout=%q", err, out)
	}
	if len(events) != 2 {
		t.Fatalf("default export must keep both tracks, got %d", len(events))
	}
	// It must not be an object with an "events" key.
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "{") {
		t.Fatalf("default --json must be an array, not an object: %q", trimmed)
	}
}

// TestSearchCollapseAudioJSONWrapsWithChunks pins that --collapse-audio --json
// emits the wrapped {events, audio_chunks} view instead of the bare array.
func TestSearchCollapseAudioJSONWrapsWithChunks(t *testing.T) {
	dataDir := t.TempDir()
	base := time.Now().UTC().Truncate(time.Second)
	seedAudioPair(t, dataDir, base, "system heard this", "microphone heard this too")

	out := runSearchCapturingStdout(t, dataDir, "--type", "audio", "--collapse-audio", "--json")

	var view collapsedSearchView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("collapsed --json must decode into the wrapped view: %v\nout=%q", err, out)
	}
	if len(view.Events) != 1 {
		t.Fatalf("expected one collapsed survivor, got %d", len(view.Events))
	}
	if view.Events[0].AudioSource != "system" {
		t.Fatalf("survivor should be the system row, got %q", view.Events[0].AudioSource)
	}
	if len(view.AudioChunks) != 1 {
		t.Fatalf("expected one audio_chunk, got %d", len(view.AudioChunks))
	}
	chunk := view.AudioChunks[0]
	if chunk.EventID != view.Events[0].ID {
		t.Fatalf("audio_chunk event_id %d must match survivor id %d", chunk.EventID, view.Events[0].ID)
	}
	if chunk.Origin != "both" {
		t.Fatalf("origin = %q, want both", chunk.Origin)
	}
	if len(chunk.Tracks) != 2 {
		t.Fatalf("expected both tracks in the chunk, got %d", len(chunk.Tracks))
	}
}

// TestSearchCollapseAudioHumanShowsOrigin pins that the text output annotates a
// collapsed audio row with its origin and merged ids.
func TestSearchCollapseAudioHumanShowsOrigin(t *testing.T) {
	dataDir := t.TempDir()
	base := time.Now().UTC().Truncate(time.Second)
	seedAudioPair(t, dataDir, base, "system heard this", "microphone heard this")

	out := runSearchCapturingStdout(t, dataDir, "--type", "audio", "--collapse-audio")

	if !strings.Contains(out, "origin=both") {
		t.Fatalf("collapsed human output must show origin=both, got %q", out)
	}
	if !strings.Contains(out, "merged ") {
		t.Fatalf("collapsed human output must list merged track ids, got %q", out)
	}
}
