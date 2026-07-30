package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func segmentStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "segments.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// audioChunk inserts one chunk's two rows and returns their ids and chunk key.
func audioChunk(t *testing.T, s *Store, at time.Time) (systemID, micID int64, key string) {
	t.Helper()
	ctx := context.Background()
	system := &Event{Kind: KindAudio, CapturedAt: at, Text: "machine audio",
		MediaPath: "/tmp/system.wav", AudioSource: "system"}
	mic := &Event{Kind: KindAudio, CapturedAt: at, Text: "room audio",
		MediaPath: "/tmp/microphone.wav", AudioSource: "microphone"}
	if err := s.Insert(ctx, system); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, mic); err != nil {
		t.Fatal(err)
	}
	return system.ID, mic.ID, formatTime(at)
}

func TestReplaceChunkSegmentsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 29, 20, 58, 58, 735604000, time.UTC)
	systemID, micID, key := audioChunk(t, s, at)

	segments := []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
			Text: "machine audio", Confidence: 0.9, OrderConfidence: "exact", Method: "timed"},
		{EventID: micID, Seq: 1, Origin: OriginExternal, SourceTrack: "microphone",
			Text: "room audio", Confidence: 0.8, OrderConfidence: "exact", Method: "timed"},
	}
	for attempt := range 3 {
		if err := s.ReplaceChunkSegments(ctx, key, segments); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		stored, err := s.SegmentsForChunk(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if len(stored) != 2 {
			t.Fatalf("attempt %d stored %d segments, want 2", attempt, len(stored))
		}
		for i, segment := range stored {
			if segment.Seq != i {
				t.Errorf("attempt %d: segment %d has seq %d", attempt, i, segment.Seq)
			}
		}
	}
}

func TestReplaceChunkSegmentsReplacesRatherThanAppends(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Now().UTC()
	systemID, _, key := audioChunk(t, s, at)

	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system", Text: "first pass"},
		{EventID: systemID, Seq: 1, Origin: OriginInternal, SourceTrack: "system", Text: "also first pass"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginUnknown, SourceTrack: "system", Text: "second pass"},
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.SegmentsForChunk(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d segments after a rewrite, want 1", len(stored))
	}
	if stored[0].Text != "second pass" || stored[0].Origin != OriginUnknown {
		t.Errorf("stale row survived the rewrite: %+v", stored[0])
	}
}

func TestSegmentTimesRoundTripAndNullsStayNull(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 29, 20, 58, 58, 735604000, time.UTC)
	systemID, micID, key := audioChunk(t, s, at)

	started := at.Add(1200 * time.Millisecond)
	ended := at.Add(3400 * time.Millisecond)
	startOffset, endOffset := int64(1200), int64(3400)
	runs, _ := json.Marshal([]map[string]any{{"start_ms": 1200, "end_ms": 1800, "text": "machine"}})

	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
			Text: "machine audio", StartedAt: &started, EndedAt: &ended,
			StartOffsetMS: &startOffset, EndOffsetMS: &endOffset,
			RunsJSON: runs, Confidence: 0.91, OrderConfidence: "exact", Method: "timed"},
		// The text-only path knows no times at all; those must come back nil
		// rather than as a zero timestamp that reads like 1970.
		{EventID: micID, Seq: 1, Origin: OriginExternal, SourceTrack: "microphone",
			Text: "room audio", Confidence: 0.48, OrderConfidence: "sequence", Method: "text"},
	}); err != nil {
		t.Fatal(err)
	}

	stored, err := s.SegmentsForChunk(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	timed, untimed := stored[0], stored[1]

	if timed.StartedAt == nil || !timed.StartedAt.Equal(started) {
		t.Errorf("started_at = %v, want %v", timed.StartedAt, started)
	}
	if timed.EndedAt == nil || !timed.EndedAt.Equal(ended) {
		t.Errorf("ended_at = %v, want %v", timed.EndedAt, ended)
	}
	if timed.StartOffsetMS == nil || *timed.StartOffsetMS != startOffset {
		t.Errorf("start_offset_ms = %v", timed.StartOffsetMS)
	}
	if string(timed.RunsJSON) != string(runs) {
		t.Errorf("runs round-tripped as %s", timed.RunsJSON)
	}
	if timed.Confidence != 0.91 {
		t.Errorf("confidence = %v", timed.Confidence)
	}
	if untimed.StartedAt != nil || untimed.EndedAt != nil {
		t.Errorf("text-path segment gained times: %v..%v", untimed.StartedAt, untimed.EndedAt)
	}
	if untimed.StartOffsetMS != nil || untimed.EndOffsetMS != nil {
		t.Error("text-path segment gained offsets")
	}
	if len(untimed.RunsJSON) != 0 {
		t.Errorf("text-path segment gained runs: %s", untimed.RunsJSON)
	}
}

// TestDeletingAnEventRemovesItsSegmentsWithoutForeignKeys is the point of having
// both a foreign key and a trigger. PRAGMA foreign_keys is a *per-connection*
// setting, so if database/sql ever hands out a connection that missed it, cascade
// enforcement silently stops. The trigger is part of the schema and cannot be
// switched off, and this test proves it is the one doing the work.
func TestDeletingAnEventRemovesItsSegmentsWithoutForeignKeys(t *testing.T) {
	ctx := context.Background()
	s, path := segmentStore(t)
	at := time.Now().UTC()
	systemID, micID, key := audioChunk(t, s, at)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system", Text: "machine"},
		{EventID: micID, Seq: 1, Origin: OriginExternal, SourceTrack: "microphone", Text: "room"},
	}); err != nil {
		t.Fatal(err)
	}

	// A second connection with foreign keys explicitly disabled.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	var enabled int
	if err := raw.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Fatalf("foreign_keys is %d; the test cannot prove the trigger", enabled)
	}
	if _, err := raw.ExecContext(ctx, "DELETE FROM events WHERE id = ?", systemID); err != nil {
		t.Fatal(err)
	}

	var remaining int
	if err := raw.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM audio_segments WHERE event_id = ?", systemID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("%d segments survived their event with foreign keys off", remaining)
	}
	// The sibling track's segments must be untouched.
	if err := raw.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM audio_segments WHERE event_id = ?", micID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Errorf("deleting one event removed %d of the sibling's segments", 1-remaining)
	}
}

func TestSegmentsBetweenExcludesBleedByDefault(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)
	systemID, micID, key := audioChunk(t, s, at)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system", Text: "the original"},
		{EventID: micID, Seq: 1, Origin: OriginInternal, SourceTrack: "microphone",
			Text: "the original", IsBleed: true},
		{EventID: micID, Seq: 2, Origin: OriginExternal, SourceTrack: "microphone", Text: "the reply"},
	}); err != nil {
		t.Fatal(err)
	}

	window := func(includeBleed bool) []Segment {
		got, err := s.SegmentsBetween(ctx, at.Add(-time.Hour), at.Add(time.Hour), includeBleed, 0)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := window(false); len(got) != 2 {
		t.Errorf("default read returned %d segments, want 2 with bleed excluded", len(got))
	}
	if got := window(true); len(got) != 3 {
		t.Errorf("bleed-inclusive read returned %d segments, want 3", len(got))
	}
}

func TestSegmentsBetweenIsOrderedBySeqNotTimestamp(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 29, 21, 0, 0, 0, time.UTC)
	systemID, micID, key := audioChunk(t, s, at)
	// Deliberately store no times, as the text-only path does. seq is then the
	// only thing that can order these.
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: micID, Seq: 0, Origin: OriginExternal, SourceTrack: "microphone", Text: "first"},
		{EventID: systemID, Seq: 1, Origin: OriginInternal, SourceTrack: "system", Text: "second"},
		{EventID: micID, Seq: 2, Origin: OriginExternal, SourceTrack: "microphone", Text: "third"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.SegmentsBetween(ctx, at.Add(-time.Hour), at.Add(time.Hour), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first", "second", "third"}
	for i, segment := range got {
		if segment.Text != want[i] {
			t.Errorf("position %d is %q, want %q", i, segment.Text, want[i])
		}
	}
}

func TestChunksMissingSegmentsIsNewestFirstAndShrinks(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	keys := make([]string, 0, 3)
	ids := make([]int64, 0, 3)
	for i := range 3 {
		systemID, _, key := audioChunk(t, s, base.Add(time.Duration(i)*time.Minute))
		keys = append(keys, key)
		ids = append(ids, systemID)
	}

	missing, err := s.ChunksMissingSegments(ctx, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 3 {
		t.Fatalf("got %d unattributed chunks, want 3", len(missing))
	}
	// Newest first, so an interrupted backfill leaves the recent past attributed.
	if missing[0] != keys[2] {
		t.Errorf("queue starts at %s, want the newest chunk %s", missing[0], keys[2])
	}

	if err := s.ReplaceChunkSegments(ctx, keys[2], []Segment{
		{EventID: ids[2], Seq: 0, Origin: OriginExternal, SourceTrack: "microphone", Text: "done"},
	}); err != nil {
		t.Fatal(err)
	}
	missing, err = s.ChunksMissingSegments(ctx, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 {
		t.Errorf("attributing a chunk left %d in the queue, want 2", len(missing))
	}
	for _, key := range missing {
		if key == keys[2] {
			t.Error("an attributed chunk is still queued")
		}
	}
}

func TestSegmentCoverageCountsChunksNotRows(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC)
	var firstID int64
	var firstKey string
	for i := range 4 {
		systemID, _, key := audioChunk(t, s, base.Add(time.Duration(i)*time.Minute))
		if i == 0 {
			firstID, firstKey = systemID, key
		}
	}
	if err := s.ReplaceChunkSegments(ctx, firstKey, []Segment{
		{EventID: firstID, Seq: 0, Origin: OriginInternal, SourceTrack: "system", Text: "a"},
		{EventID: firstID, Seq: 1, Origin: OriginInternal, SourceTrack: "system", Text: "b"},
	}); err != nil {
		t.Fatal(err)
	}

	chunks, attributed, err := s.SegmentCoverage(ctx, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if chunks != 4 {
		t.Errorf("chunks = %d, want 4 — each pair of rows is one chunk", chunks)
	}
	// Two segments on one chunk must count as one attributed chunk, not two.
	if attributed != 1 {
		t.Errorf("attributed = %d, want 1", attributed)
	}
}

// TestOriginIsTextSoAdditionalValuesNeedNoMigration is the headroom contract. If
// machine-side participants are ever distinguished, that must be a value change
// rather than a schema change.
func TestOriginIsTextSoAdditionalValuesNeedNoMigration(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Now().UTC()
	systemID, _, key := audioChunk(t, s, at)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: "internal_2", SourceTrack: "system", Text: "a second participant"},
	}); err != nil {
		t.Fatalf("storing an unforeseen origin failed: %v", err)
	}
	stored, err := s.SegmentsForChunk(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Origin != "internal_2" {
		t.Errorf("origin round-tripped as %+v", stored)
	}
}

func TestPruneRemovesSegmentsWithTheirEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	systemID, micID, key := audioChunk(t, s, at)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system", Text: "old machine audio"},
		{EventID: micID, Seq: 1, Origin: OriginExternal, SourceTrack: "microphone", Text: "old room audio"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteByIDs(ctx, []int64{systemID, micID}); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.SegmentsForChunk(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d segments outlived the events they describe", len(remaining))
	}
}

func TestTranscriptClampsTurnsAndReportsCapping(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)

	// Alternating origins so nothing merges and turn count equals segment count.
	for i := range 12 {
		at := base.Add(time.Duration(i) * time.Minute)
		systemID, micID, key := audioChunk(t, s, at)
		if err := s.ReplaceChunkSegments(ctx, key, []Segment{
			{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
				Text: "machine line", Confidence: 0.9, OrderConfidence: "sequence"},
			{EventID: micID, Seq: 1, Origin: OriginExternal, SourceTrack: "microphone",
				Text: "room line", Confidence: 0.9, OrderConfidence: "sequence"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	window := TranscriptOptions{Since: base.Add(-time.Hour), Until: base.Add(time.Hour)}
	all, err := s.Transcript(ctx, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Turns) != 24 {
		t.Fatalf("got %d turns, want 24", len(all.Turns))
	}
	if all.Capped {
		t.Error("an uncapped read reported capping")
	}

	capped := window
	capped.MaxTurns = 5
	limited, err := s.Transcript(ctx, capped)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Turns) != 5 {
		t.Errorf("got %d turns, want the requested 5", len(limited.Turns))
	}
	if !limited.Capped {
		t.Error("a truncated transcript did not report capping")
	}

	// The store clamps the limit itself, so a caller cannot exceed the documented
	// ceiling by asking for more.
	over := window
	over.MaxTurns = MaxTranscriptTurns + 500
	if got := ClampTranscriptTurns(over.MaxTurns); got != MaxTranscriptTurns {
		t.Errorf("clamp allowed %d, want %d", got, MaxTranscriptTurns)
	}
}

func TestTranscriptExcludesBleedAndFiltersByOrigin(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 29, 16, 0, 0, 0, time.UTC)
	systemID, micID, key := audioChunk(t, s, at)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
			Text: "the machine spoke", Confidence: 0.9, OrderConfidence: "sequence"},
		{EventID: micID, Seq: 1, Origin: OriginInternal, SourceTrack: "microphone",
			Text: "the machine spoke", IsBleed: true, Confidence: 0.9, OrderConfidence: "sequence"},
		{EventID: micID, Seq: 2, Origin: OriginExternal, SourceTrack: "microphone",
			Text: "the room replied", Confidence: 0.9, OrderConfidence: "sequence"},
	}); err != nil {
		t.Fatal(err)
	}
	window := TranscriptOptions{Since: at.Add(-time.Hour), Until: at.Add(time.Hour)}

	result, err := s.Transcript(ctx, window)
	if err != nil {
		t.Fatal(err)
	}
	// The machine's phrase must appear once, not twice.
	occurrences := 0
	for _, turn := range result.Turns {
		if strings.Contains(turn.Text, "the machine spoke") {
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Errorf("the machine's phrase appears %d times, want once", occurrences)
	}

	onlyExternal := window
	onlyExternal.Origin = OriginExternal
	filtered, err := s.Transcript(ctx, onlyExternal)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Turns) != 1 || filtered.Turns[0].Text != "the room replied" {
		t.Errorf("origin filter returned %+v", filtered.Turns)
	}
}

func TestTranscriptReportsCoverageHoles(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	var firstID int64
	var firstKey string
	for i := range 3 {
		systemID, _, key := audioChunk(t, s, base.Add(time.Duration(i)*time.Minute))
		if i == 0 {
			firstID, firstKey = systemID, key
		}
	}
	if err := s.ReplaceChunkSegments(ctx, firstKey, []Segment{
		{EventID: firstID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
			Text: "only this chunk is attributed", Confidence: 0.9, OrderConfidence: "sequence"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := s.Transcript(ctx, TranscriptOptions{
		Since: base.Add(-time.Hour), Until: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Chunks != 3 || result.AttributedChunks != 1 {
		t.Errorf("coverage = %d/%d, want 1/3", result.AttributedChunks, result.Chunks)
	}
}

func TestTranscriptMinConfidenceDefaultsToReturningEverything(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 29, 18, 0, 0, 0, time.UTC)
	systemID, micID, key := audioChunk(t, s, at)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
			Text: "confident", Confidence: 0.95, OrderConfidence: "exact"},
		{EventID: micID, Seq: 1, Origin: OriginExternal, SourceTrack: "microphone",
			Text: "doubtful", Confidence: 0.24, OrderConfidence: "approximate"},
	}); err != nil {
		t.Fatal(err)
	}
	window := TranscriptOptions{Since: at.Add(-time.Hour), Until: at.Add(time.Hour)}

	all, err := s.Transcript(ctx, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Turns) != 2 {
		t.Errorf("got %d turns; the default must not hide low-confidence speech", len(all.Turns))
	}

	strict := window
	strict.MinConfidence = 0.5
	filtered, err := s.Transcript(ctx, strict)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Turns) != 1 || filtered.Turns[0].Text != "confident" {
		t.Errorf("min confidence returned %+v", filtered.Turns)
	}
}

// TestTranscriptTruncationIsReportedAndCoverageFollowsIt is a regression test for
// a silent truncation found in review.
//
// A window holding more segments than one read returns used to come back as an
// ordinary transcript: the tail was dropped before assembly, Capped described
// only the turn limit, and coverage still counted every chunk in the requested
// range as attributed — so the response corroborated its own omission. Because
// same-origin adjacent chunks merge, 21,000 segments assembled into a single
// turn and neither existing guard fired.
func TestTranscriptTruncationIsReportedAndCoverageFollowsIt(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	const chunks = 700
	const perChunk = 30
	for c := range chunks {
		at := base.Add(time.Duration(c) * 30 * time.Second)
		event := &Event{Kind: KindAudio, CapturedAt: at, Text: "x",
			MediaPath: "/tmp/microphone.wav", AudioSource: "microphone"}
		if err := s.Insert(ctx, event); err != nil {
			t.Fatal(err)
		}
		rows := make([]Segment, 0, perChunk)
		for i := range perChunk {
			rows = append(rows, Segment{EventID: event.ID, Seq: i, Origin: OriginExternal,
				SourceTrack: "microphone", Text: "word", Confidence: 0.9, OrderConfidence: "sequence"})
		}
		if err := s.ReplaceChunkSegments(ctx, formatTime(at), rows); err != nil {
			t.Fatal(err)
		}
	}

	result, err := s.Transcript(ctx, TranscriptOptions{
		Since: base.Add(-time.Hour), Until: base.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatalf("%d segments across %d chunks came back as a complete transcript",
			chunks*perChunk, chunks)
	}
	if !result.CoveredUntil.Before(base.Add(24 * time.Hour)) {
		t.Errorf("covered_until %s is the requested end, so it names no resume point",
			result.CoveredUntil)
	}
	// Coverage must describe the returned turns, not the requested window;
	// otherwise it vouches for chunks whose text was dropped.
	if result.Chunks >= chunks {
		t.Errorf("coverage counts %d chunks but the transcript stops at %s",
			result.Chunks, result.CoveredUntil)
	}
	// The transcript must not end inside a chunk, so a follow-up request can
	// resume from covered_until without overlapping or skipping.
	segments, err := s.SegmentsBetween(ctx, result.CoveredUntil, result.CoveredUntil, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != perChunk {
		t.Errorf("the last covered chunk holds %d of its %d segments, so the transcript ends mid-chunk",
			len(segments), perChunk)
	}
}

// TestUntruncatedTranscriptCoversTheWholeRequest guards the other direction: the
// resume point must be the requested end when nothing was dropped.
func TestUntruncatedTranscriptCoversTheWholeRequest(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	systemID, _, key := audioChunk(t, s, at)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
			Text: "Just the one phrase.", Confidence: 0.9, OrderConfidence: "sequence"},
	}); err != nil {
		t.Fatal(err)
	}
	until := at.Add(time.Hour)
	result, err := s.Transcript(ctx, TranscriptOptions{Since: at.Add(-time.Hour), Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated {
		t.Error("a one-segment transcript reported truncation")
	}
	if !result.CoveredUntil.Equal(until) {
		t.Errorf("covered_until = %s, want the requested end %s", result.CoveredUntil, until)
	}
}

// TestOriginFilterSelectsTurnsWithoutReshapingThem is a regression test for a
// fabricated adjacency found in review.
//
// Filtering segments before assembly hid the machine's interjection from
// AssembleTurns, which then saw two separate replies as consecutive and merged
// them into one turn — joining speech that was never contiguous and deleting the
// boundary between them, with nothing in the output to show it.
func TestOriginFilterSelectsTurnsWithoutReshapingThem(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	systemID, micID, key := audioChunk(t, s, at)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: micID, Seq: 0, Origin: OriginExternal, SourceTrack: "microphone",
			Text: "Room speaks first.", Confidence: 0.9, OrderConfidence: "sequence"},
		{EventID: systemID, Seq: 1, Origin: OriginInternal, SourceTrack: "system",
			Text: "Machine interjects.", Confidence: 0.9, OrderConfidence: "sequence"},
		{EventID: micID, Seq: 2, Origin: OriginExternal, SourceTrack: "microphone",
			Text: "Room speaks much later.", Confidence: 0.9, OrderConfidence: "sequence"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := s.Transcript(ctx, TranscriptOptions{
		Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Origin: OriginExternal})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 2 {
		t.Fatalf("got %d turns, want the two the unfiltered transcript holds: %+v",
			len(result.Turns), result.Turns)
	}
	for _, turn := range result.Turns {
		if turn.Origin != OriginExternal {
			t.Errorf("turn %q survived an external-only filter with origin %s", turn.Text, turn.Origin)
		}
		if strings.Contains(turn.Text, "first") && strings.Contains(turn.Text, "later") {
			t.Errorf("two turns separated by the machine were merged into %q", turn.Text)
		}
	}
}

// TestSilentChunkLeavesTheWorkQueue pins the drainability of the derived queue.
// A chunk that holds no words is still a chunk that has been attributed, and
// nothing about it should keep coming back.
func TestSilentChunkLeavesTheWorkQueue(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	systemID, _, key := audioChunk(t, s, at)

	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginSilent, SourceTrack: "system",
			OrderConfidence: "sequence", Method: "silent"},
	}); err != nil {
		t.Fatal(err)
	}

	missing, err := s.ChunksMissingSegments(ctx, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("a marked-silent chunk is still queued: %v", missing)
	}
	chunkCount, attributed, err := s.SegmentCoverage(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if attributed != chunkCount {
		t.Errorf("coverage reports %d of %d attributed, so a silent chunk reads as a hole",
			attributed, chunkCount)
	}
	// The marker must never surface as a turn.
	result, err := s.Transcript(ctx, TranscriptOptions{Since: at.Add(-time.Hour), Until: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 0 {
		t.Errorf("the silence marker became %d turns: %+v", len(result.Turns), result.Turns)
	}
	// And it is not something to point a reader at.
	speech, err := s.HasSpeechSegments(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if speech {
		t.Error("a chunk of pure silence reports transcribed speech")
	}
}
