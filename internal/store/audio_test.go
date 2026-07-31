package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// audioEvent builds a KindAudio event at t with the given source and text. The
// id is set explicitly so tests can pin tie-break ordering.
func audioEvent(id int64, t time.Time, source, text string) Event {
	return Event{ID: id, Kind: KindAudio, CapturedAt: t, AudioSource: source, Text: text,
		MediaPath: source + ".wav"}
}

// TestSystemAudioWinsEveryDuplicate is the test that fails if anyone reorders
// isSystem below runeLen: whenever both tracks carry text, the system row must
// survive no matter which transcript is longer or which id is lower.
func TestSystemAudioWinsEveryDuplicate(t *testing.T) {
	base := time.Date(2026, 7, 29, 9, 13, 16, 0, time.UTC)
	cases := []struct {
		name       string
		systemText string
		micText    string
	}{
		{"system shorter", "$52 million", "the revenue was fifty two million dollars this quarter"},
		{"system longer", "the revenue was fifty two million dollars this quarter", "$52m"},
		{"equal length", "hello there", "hi there yo"},
	}
	for _, tc := range cases {
		for _, order := range []struct {
			name     string
			systemID int64
			micID    int64
		}{
			{"system id lower", 1, 2},
			{"system id higher", 2, 1},
		} {
			t.Run(tc.name+"/"+order.name, func(t *testing.T) {
				events := []Event{
					audioEvent(order.systemID, base, "system", tc.systemText),
					audioEvent(order.micID, base, "microphone", tc.micText),
				}
				kept, chunks := CollapseAudioTracks(events)
				if len(kept) != 1 {
					t.Fatalf("expected one survivor, got %d", len(kept))
				}
				if kept[0].AudioSource != "system" {
					t.Fatalf("survivor = %q, want system", kept[0].AudioSource)
				}
				chunk := chunks[kept[0].ID]
				if chunk.Origin != AudioOriginBoth {
					t.Fatalf("origin = %q, want both", chunk.Origin)
				}
				if len(chunk.Tracks) != 2 {
					t.Fatalf("expected both tracks retained, got %d", len(chunk.Tracks))
				}
			})
		}
	}
}

func TestCollapseBlankSystemMicSurvives(t *testing.T) {
	base := time.Date(2026, 7, 29, 9, 13, 16, 0, time.UTC)
	events := []Event{
		audioEvent(1, base, "system", ""),
		audioEvent(2, base, "microphone", "the user is talking in the room"),
	}
	kept, chunks := CollapseAudioTracks(events)
	if len(kept) != 1 || kept[0].AudioSource != "microphone" {
		t.Fatalf("expected microphone survivor, got %#v", kept)
	}
	chunk := chunks[kept[0].ID]
	if chunk.Origin != AudioOriginMicrophone {
		t.Fatalf("origin = %q, want microphone", chunk.Origin)
	}
	if len(chunk.Tracks) != 2 {
		t.Fatalf("expected both tracks present, got %d", len(chunk.Tracks))
	}
	// The dropped blank system track must ride along at TextLength 0.
	var systemTrack *AudioTrack
	for i := range chunk.Tracks {
		if chunk.Tracks[i].AudioSource == "system" {
			systemTrack = &chunk.Tracks[i]
		}
	}
	if systemTrack == nil || systemTrack.TextLength != 0 {
		t.Fatalf("expected blank system track at TextLength 0, got %#v", systemTrack)
	}
}

func TestCollapseBothTextSystemSurvives(t *testing.T) {
	base := time.Date(2026, 7, 29, 9, 14, 18, 0, time.UTC)
	events := []Event{
		audioEvent(1, base, "system", "system transcript here"),
		audioEvent(2, base, "microphone", "microphone heard the same thing"),
	}
	kept, chunks := CollapseAudioTracks(events)
	if len(kept) != 1 || kept[0].AudioSource != "system" {
		t.Fatalf("expected system survivor, got %#v", kept)
	}
	chunk := chunks[kept[0].ID]
	if chunk.Origin != AudioOriginBoth {
		t.Fatalf("origin = %q, want both", chunk.Origin)
	}
	var micRetained bool
	for _, track := range chunk.Tracks {
		if track.AudioSource == "microphone" && track.TextLength > 0 {
			micRetained = true
		}
	}
	if !micRetained {
		t.Fatalf("expected microphone track retained with text, got %#v", chunk.Tracks)
	}
}

func TestCollapseWhitespaceOnlySystemIsSilent(t *testing.T) {
	base := time.Date(2026, 7, 29, 9, 15, 20, 0, time.UTC)
	events := []Event{
		audioEvent(1, base, "system", "   \t\n  "),
		audioEvent(2, base, "microphone", "actual speech"),
	}
	kept, chunks := CollapseAudioTracks(events)
	if len(kept) != 1 || kept[0].AudioSource != "microphone" {
		t.Fatalf("expected microphone survivor, got %#v", kept)
	}
	chunk := chunks[kept[0].ID]
	if chunk.Origin != AudioOriginMicrophone {
		t.Fatalf("origin = %q, want microphone (whitespace-only system is silence)", chunk.Origin)
	}
	for _, track := range chunk.Tracks {
		if track.AudioSource == "system" && track.TextLength != 0 {
			t.Fatalf("whitespace-only system track should trim to TextLength 0, got %d", track.TextLength)
		}
	}
}

func TestCollapseBothBlankIsSilent(t *testing.T) {
	base := time.Date(2026, 7, 29, 10, 52, 6, 0, time.UTC)
	events := []Event{
		audioEvent(1, base, "system", ""),
		audioEvent(2, base, "microphone", ""),
	}
	kept, chunks := CollapseAudioTracks(events)
	if len(kept) != 1 {
		t.Fatalf("expected one survivor, got %d", len(kept))
	}
	if chunks[kept[0].ID].Origin != AudioOriginSilent {
		t.Fatalf("origin = %q, want silent", chunks[kept[0].ID].Origin)
	}
}

func TestCollapseMicDotVsBlankSystem(t *testing.T) {
	base := time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC)
	events := []Event{
		audioEvent(1, base, "system", ""),
		audioEvent(2, base, "microphone", "."),
	}
	kept, chunks := CollapseAudioTracks(events)
	if len(kept) != 1 || kept[0].AudioSource != "microphone" {
		t.Fatalf("expected microphone survivor for a '.' transcript, got %#v", kept)
	}
	if chunks[kept[0].ID].Origin != AudioOriginMicrophone {
		t.Fatalf("origin = %q, want microphone", chunks[kept[0].ID].Origin)
	}
}

func TestCollapseIgnoresScreenAtSameTimestamp(t *testing.T) {
	base := time.Date(2026, 7, 29, 9, 13, 16, 0, time.UTC)
	screen := Event{ID: 10, Kind: KindScreen, CapturedAt: base, Text: "on screen", MediaPath: "s.jpg"}
	events := []Event{
		audioEvent(1, base, "system", "spoken"),
		screen,
		audioEvent(2, base, "microphone", "spoken"),
	}
	kept, chunks := CollapseAudioTracks(events)
	// One collapsed audio survivor plus the untouched screen row.
	if len(kept) != 2 {
		t.Fatalf("expected 2 kept (collapsed audio + screen), got %d", len(kept))
	}
	if _, ok := chunks[screen.ID]; ok {
		t.Fatalf("screen event must not get a chunk entry")
	}
	var sawScreen bool
	for _, e := range kept {
		if e.Kind == KindScreen {
			sawScreen = true
		}
	}
	if !sawScreen {
		t.Fatalf("screen event must pass through untouched")
	}
}

func TestCollapseUnpairedAudioPassesThrough(t *testing.T) {
	base := time.Date(2026, 7, 29, 9, 13, 16, 0, time.UTC)
	events := []Event{audioEvent(1, base, "system", "lonely chunk")}
	kept, chunks := CollapseAudioTracks(events)
	if len(kept) != 1 {
		t.Fatalf("expected one survivor, got %d", len(kept))
	}
	chunk := chunks[kept[0].ID]
	if len(chunk.Tracks) != 1 {
		t.Fatalf("expected one-entry Tracks, got %d", len(chunk.Tracks))
	}
	if chunk.Origin != AudioOriginSystem {
		t.Fatalf("origin = %q, want system", chunk.Origin)
	}
}

func TestCollapsePreservesInputOrderAndEarliestIndex(t *testing.T) {
	t0 := time.Date(2026, 7, 29, 9, 13, 16, 0, time.UTC)
	t1 := time.Date(2026, 7, 29, 9, 13, 47, 0, time.UTC)
	// Group A's system row appears first (index 0); its mic twin appears later,
	// after group B's rows. The survivor must stay at index 0.
	events := []Event{
		audioEvent(1, t0, "system", "group A speech"),    // index 0, group A
		audioEvent(3, t1, "system", "group B speech"),    // index 1, group B
		audioEvent(4, t1, "microphone", "group B heard"), // group B twin
		audioEvent(2, t0, "microphone", "group A heard"), // group A twin, seen last
	}
	kept, _ := CollapseAudioTracks(events)
	if len(kept) != 2 {
		t.Fatalf("expected 2 survivors, got %d", len(kept))
	}
	if kept[0].ID != 1 {
		t.Fatalf("group A survivor should sit at earliest index 0, got id %d", kept[0].ID)
	}
	if kept[1].ID != 3 {
		t.Fatalf("group B survivor should sit at index 1, got id %d", kept[1].ID)
	}
}

func TestAudioTracksAt(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Date(2026, 7, 29, 9, 13, 16, 493218000, time.UTC)
	key := FormatCapturedAt(base)
	events := []Event{
		{Kind: KindAudio, CapturedAt: base, AudioSource: "system", Text: "system heard this", MediaPath: "sys.wav"},
		{Kind: KindScreen, CapturedAt: base, Text: "screen at same time", MediaPath: "s.jpg"},
		{Kind: KindAudio, CapturedAt: base, AudioSource: "microphone", Text: "mic heard this", MediaPath: "mic.wav"},
	}
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.AudioTracksAt(ctx, []time.Time{base})
	if err != nil {
		t.Fatal(err)
	}
	tracks := got[key]
	if len(tracks) != 2 {
		t.Fatalf("expected 2 audio tracks (screen ignored), got %d: %#v", len(tracks), tracks)
	}
	sources := map[string]bool{}
	for _, tr := range tracks {
		sources[tr.AudioSource] = true
		if tr.TextLength == 0 {
			t.Fatalf("track %d should have non-zero TextLength", tr.ID)
		}
	}
	if !sources["system"] || !sources["microphone"] {
		t.Fatalf("expected both system and microphone tracks, got %#v", tracks)
	}
}

// TestAudioTracksAtBatchesPastVariableLimit hands more timestamps than a single
// IN (…) can hold to confirm the batching loop never trips SQLite's variable
// limit. The timestamps need not exist; an empty result is fine.
func TestAudioTracksAtBatchesPastVariableLimit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	times := make([]time.Time, deleteBatchSize*2+5) // > 900, forces multiple batches
	for i := range times {
		times[i] = base.Add(time.Duration(i) * time.Second)
	}
	if _, err := s.AudioTracksAt(ctx, times); err != nil {
		t.Fatalf("batched query should not trip the variable limit: %v", err)
	}
}
