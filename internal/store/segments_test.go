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

// TestTranscriptReportsWhatMinConfidenceRemoved is a regression test for a
// silent, one-sided deletion found in review.
//
// Confidence is not comparable across origins: internal/transcript's crosstalk
// and ambiguity multipliers reach microphone-derived turns alone, so on a live
// index internal turns scored 0.682-0.983 while external turns scored
// 0.331-0.592. One threshold at 0.6
// therefore deletes one whole side of the conversation and returns a transcript
// that looks complete. The counts are the only thing that can reveal a removal
// the caller did not know it was asking for.
func TestTranscriptReportsWhatMinConfidenceRemoved(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 29, 19, 0, 0, 0, time.UTC)
	systemID, micID, key := audioChunk(t, s, at)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
			Text: "the machine spoke", Confidence: 0.9, OrderConfidence: "exact"},
		{EventID: micID, Seq: 1, Origin: OriginExternal, SourceTrack: "microphone",
			Text: "the room replied", Confidence: 0.36, OrderConfidence: "exact"},
	}); err != nil {
		t.Fatal(err)
	}
	window := TranscriptOptions{Since: at.Add(-time.Hour), Until: at.Add(time.Hour)}

	all, err := s.Transcript(ctx, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.ConfidenceFiltered) != 0 {
		t.Errorf("the default threshold reported %v removed; it removes nothing", all.ConfidenceFiltered)
	}

	strict := window
	strict.MinConfidence = 0.6
	filtered, err := s.Transcript(ctx, strict)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Turns) != 1 || filtered.Turns[0].Origin != OriginInternal {
		t.Fatalf("threshold returned %+v, want the internal turn alone", filtered.Turns)
	}
	if got := filtered.ConfidenceFiltered[OriginExternal]; got != 1 {
		t.Errorf("removed %d external turns, want 1: %v", got, filtered.ConfidenceFiltered)
	}
	if _, ok := filtered.ConfidenceFiltered[OriginInternal]; ok {
		t.Errorf("an origin nothing was removed from is reported: %v", filtered.ConfidenceFiltered)
	}

	// A turn the caller excluded by naming an origin is not a confidence removal.
	// Reporting it as one would blame the threshold for a filter the caller can
	// already see it set, and bury the asymmetry this count exists to expose.
	byOrigin := window
	byOrigin.Origin = OriginInternal
	byOrigin.MinConfidence = 0.6
	narrowed, err := s.Transcript(ctx, byOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed.ConfidenceFiltered) != 0 {
		t.Errorf("the origin filter was counted as a confidence removal: %v", narrowed.ConfidenceFiltered)
	}
}

// TestConfidenceRemovalsStopAtTheCappedPage pins the counts to what this page
// covers: everything before ResumeFrom, which is where the next page begins. The
// chunk sitting exactly on that bound is the one exception, covered separately by
// TestConfidenceRemovalsStopAtTheBoundaryEvenWhenItOverlaps.
//
// The cap is applied after filtering and drags CoveredUntil and ResumeFrom back
// with it, so a tally taken over every assembled turn describes removals from
// past the point this page stops — and the next page, resuming at ResumeFrom,
// reads those same turns and counts them again. A number that both overstates
// the page and double-counts across pages is worse than no number.
func TestConfidenceRemovalsStopAtTheCappedPage(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 29, 20, 0, 0, 0, time.UTC)
	for i := range 4 {
		at := base.Add(time.Duration(i) * time.Minute)
		systemID, micID, key := audioChunk(t, s, at)
		if err := s.ReplaceChunkSegments(ctx, key, []Segment{
			{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
				Text: "machine line", Confidence: 0.9, OrderConfidence: "exact"},
			{EventID: micID, Seq: 1, Origin: OriginExternal, SourceTrack: "microphone",
				Text: "room line", Confidence: 0.36, OrderConfidence: "exact"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Every external turn fails the threshold; the cap then keeps two of the four
	// internal turns that passed it.
	page, err := s.Transcript(ctx, TranscriptOptions{
		Since: base.Add(-time.Hour), Until: base.Add(time.Hour),
		MinConfidence: 0.6, MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Capped || len(page.Turns) != 2 {
		t.Fatalf("got %d turns, capped=%v; want a 2-turn capped page", len(page.Turns), page.Capped)
	}
	if got := page.ConfidenceFiltered[OriginExternal]; got != 2 {
		t.Errorf("page reports %d external turns removed, want the 2 inside its own coverage: %v",
			got, page.ConfidenceFiltered)
	}

	// The turns the cap deferred are counted by the page that actually returns
	// them, so paging through the range totals each removal once.
	rest, err := s.Transcript(ctx, TranscriptOptions{
		Since: page.ResumeFrom, Until: base.Add(time.Hour),
		MinConfidence: 0.6, MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rest.ConfidenceFiltered[OriginExternal]; got != 2 {
		t.Errorf("the second page reports %d external turns removed, want 2: %v",
			got, rest.ConfidenceFiltered)
	}
}

// TestConfidenceRemovalsSurviveAChunkThatOnlyHeldRejectedTurns is the gap
// between the two boundaries a capped page carries.
//
// CoveredUntil is the last turn this page *returned*; ResumeFrom is the first
// turn it *deferred*. A chunk between them holding nothing but rejected turns
// belongs to neither if the count is taken against CoveredUntil: page one skips
// it as beyond its coverage, and page two starts after it and never reads it. The
// removal then exists in no page's accounting at all, which is the failure the
// counts were added to prevent, reproduced one level up.
func TestConfidenceRemovalsSurviveAChunkThatOnlyHeldRejectedTurns(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 29, 21, 0, 0, 0, time.UTC)

	// Alternating origins so nothing merges across the chunk boundaries, and the
	// middle chunk holds a rejected turn alone.
	for i, segment := range []Segment{
		{Origin: OriginInternal, SourceTrack: "system", Text: "machine line", Confidence: 0.9},
		{Origin: OriginExternal, SourceTrack: "microphone", Text: "room line", Confidence: 0.36},
		{Origin: OriginInternal, SourceTrack: "system", Text: "machine again", Confidence: 0.9},
	} {
		at := base.Add(time.Duration(i) * time.Minute)
		systemID, micID, key := audioChunk(t, s, at)
		segment.EventID = systemID
		if segment.SourceTrack == "microphone" {
			segment.EventID = micID
		}
		segment.OrderConfidence = "exact"
		if err := s.ReplaceChunkSegments(ctx, key, []Segment{segment}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.Transcript(ctx, TranscriptOptions{
		Since: base.Add(-time.Hour), Until: base.Add(time.Hour),
		MinConfidence: 0.6, MaxTurns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Capped || len(page.Turns) != 1 {
		t.Fatalf("got %d turns, capped=%v; want a 1-turn capped page", len(page.Turns), page.Capped)
	}
	// The rejected chunk sits before ResumeFrom, so page two will never read it.
	// This page is the only one that can report it.
	if got := page.ConfidenceFiltered[OriginExternal]; got != 1 {
		t.Errorf("page reports %d external turns removed, want 1: the chunk between CoveredUntil "+
			"and ResumeFrom is reported by no page at all", got)
	}

	rest, err := s.Transcript(ctx, TranscriptOptions{
		Since: page.ResumeFrom, Until: base.Add(time.Hour),
		MinConfidence: 0.6, MaxTurns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rest.ConfidenceFiltered[OriginExternal]; got != 0 {
		t.Errorf("the second page counts %d external removals it never read", got)
	}
}

// TestConfidenceRemovalsCountTheChunkTooLargeToReturnWhole is the degenerate
// boundary, where ResumeFrom and CoveredUntil name the same chunk.
//
// One chunk over the segment ceiling is the case trimToWholeChunks cannot
// paginate: it keeps the prefix and points ResumeFrom back at the very chunk it
// just served, so the overlap is unavoidable rather than accidental. A removal
// bound that excludes everything at or after ResumeFrom therefore excludes the
// only chunk on the page, and the next request — landing on the same chunk and
// the same ceiling — excludes it again. The page reports nothing removed while
// having removed something, which is the whole defect these counts exist to
// prevent, reproduced in the one place it is hardest to see.
func TestConfidenceRemovalsCountTheChunkTooLargeToReturnWhole(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	at := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	event := &Event{Kind: KindAudio, CapturedAt: at, Text: "x",
		MediaPath: "/tmp/microphone.wav", AudioSource: "microphone"}
	if err := s.Insert(ctx, event); err != nil {
		t.Fatal(err)
	}
	// One segment past the ceiling, all in this chunk, all below any threshold.
	rows := make([]Segment, 0, maxTranscriptSegments+1)
	for i := range maxTranscriptSegments + 1 {
		rows = append(rows, Segment{EventID: event.ID, Seq: i, Origin: OriginExternal,
			SourceTrack: "microphone", Text: "word", Confidence: 0.36, OrderConfidence: "sequence"})
	}
	if err := s.ReplaceChunkSegments(ctx, formatTime(at), rows); err != nil {
		t.Fatal(err)
	}

	result, err := s.Transcript(ctx, TranscriptOptions{
		Since: at.Add(-time.Hour), Until: at.Add(time.Hour), MinConfidence: 0.6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatalf("a chunk past the segment ceiling came back as a complete transcript")
	}
	if !result.ResumeFrom.Equal(result.CoveredUntil) {
		t.Fatalf("ResumeFrom %s and CoveredUntil %s differ; this is not the degenerate case",
			result.ResumeFrom, result.CoveredUntil)
	}
	if got := result.ConfidenceFiltered[OriginExternal]; got == 0 {
		t.Error("the page removed every turn it held and reported nothing removed")
	}
}

// TestConfidenceRemovalsStopAtTheBoundaryEvenWhenItOverlaps is the other half of
// the equal-boundary case.
//
// A cap falling inside a chunk leaves ResumeFrom and CoveredUntil pointing at the
// same chunk, and that chunk's own removals are counted here because the next
// page re-reads it either way. That accepted overlap must not become a licence to
// count *everything*: a rejected turn in a later chunk was never part of this
// page's ground, and the next page — starting inclusively at ResumeFrom — reads
// and reports it. Counting it here too makes it the one thing the overlap is not
// supposed to produce, a removal attributed to a page that never covered it.
func TestConfidenceRemovalsStopAtTheBoundaryEvenWhenItOverlaps(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)

	// Two passing turns in the first chunk, so the cap falls inside it and both
	// boundaries land on the same chunk.
	systemID, micID, key := audioChunk(t, s, base)
	if err := s.ReplaceChunkSegments(ctx, key, []Segment{
		{EventID: systemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
			Text: "machine line", Confidence: 0.9, OrderConfidence: "exact"},
		{EventID: micID, Seq: 1, Origin: OriginExternal, SourceTrack: "microphone",
			Text: "room line", Confidence: 0.9, OrderConfidence: "exact"},
	}); err != nil {
		t.Fatal(err)
	}
	// A later chunk holding one rejected turn, internal so it cannot merge with
	// the external turn before it.
	laterSystemID, _, laterKey := audioChunk(t, s, base.Add(time.Minute))
	if err := s.ReplaceChunkSegments(ctx, laterKey, []Segment{
		{EventID: laterSystemID, Seq: 0, Origin: OriginInternal, SourceTrack: "system",
			Text: "faint machine line", Confidence: 0.36, OrderConfidence: "exact"},
	}); err != nil {
		t.Fatal(err)
	}

	page, err := s.Transcript(ctx, TranscriptOptions{
		Since: base.Add(-time.Hour), Until: base.Add(time.Hour),
		MinConfidence: 0.6, MaxTurns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Capped {
		t.Fatal("the cap did not fire")
	}
	if page.ResumeFrom.After(page.CoveredUntil) {
		t.Fatalf("ResumeFrom %s is past CoveredUntil %s; this is not the overlapping case",
			page.ResumeFrom, page.CoveredUntil)
	}
	if got := page.ConfidenceFiltered[OriginInternal]; got != 0 {
		t.Errorf("page counts %d internal removals from a chunk past its own ResumeFrom", got)
	}

	// The page that actually reaches that chunk is the one that reports it.
	rest, err := s.Transcript(ctx, TranscriptOptions{
		Since: page.ResumeFrom, Until: base.Add(time.Hour), MinConfidence: 0.6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rest.ConfidenceFiltered[OriginInternal]; got != 1 {
		t.Errorf("the page covering the later chunk reports %d internal removals, want 1", got)
	}
}

// TestConfidenceRemovalsReadAsASentence covers the rendering every consumer
// shares. Ordering is fixed rather than map order so one removal never reads two
// ways between calls, and the counts are pluralized because the string is shown
// to a person.
func TestConfidenceRemovalsReadAsASentence(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		filtered map[string]int
		want     string
	}{
		{"nothing removed", nil, ""},
		{"one turn is singular", map[string]int{OriginExternal: 1}, "1 external turn"},
		{"several turns are plural", map[string]int{OriginExternal: 3}, "3 external turns"},
		{"two origins are joined and sorted", map[string]int{
			OriginUnknown: 1, OriginExternal: 3}, "3 external turns and 1 unknown turn"},
		{"three origins keep the serial comma out of the final join", map[string]int{
			OriginUnknown: 1, OriginExternal: 3, OriginInternal: 2},
			"3 external turns, 2 internal turns and 1 unknown turn"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := TranscriptResult{ConfidenceFiltered: testCase.filtered}.ConfidenceRemovals()
			if got != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
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

// TestFailedTranscriptionIsDistinguishedFromSilence pins the rule that separates
// a chunk nobody could transcribe from one that held nothing to transcribe.
//
// The recognizer returns an empty string for both, so the only evidence is the
// diagnostic the recorder stored beside the row. The rule lives here rather than
// in the backfill for the same reason HasSearchableTerms does: the SQL that finds
// these chunks and the Go check that recognises one must not be able to drift.
func TestFailedTranscriptionIsDistinguishedFromSilence(t *testing.T) {
	cases := []struct {
		name  string
		event Event
		want  bool
	}{
		{"recognition failed", Event{Text: "", Metadata: json.RawMessage(
			`{"audio_source":"microphone","processor_error":"recognizer unavailable"}`)}, true},
		{"silence", Event{Text: "", Metadata: json.RawMessage(`{"audio_source":"microphone"}`)}, false},
		{"blank but not empty", Event{Text: "   ", Metadata: json.RawMessage(
			`{"processor_error":"recognizer unavailable"}`)}, true},
		// A recognizer that returned words alongside an error still produced a
		// transcript, and attribution reads the words, not the error.
		{"partial transcript", Event{Text: "half a sentence", Metadata: json.RawMessage(
			`{"processor_error":"recognizer gave up"}`)}, false},
		{"no metadata", Event{Text: ""}, false},
		{"unreadable metadata", Event{Text: "", Metadata: json.RawMessage(`not json`)}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := FailedTranscription(test.event); got != test.want {
				t.Errorf("FailedTranscription = %v, want %v", got, test.want)
			}
		})
	}
}

// TestChunksFailedTranscriptionCountsOnlyWhatCannotBeAttributed backs the report
// that keeps `lumi transcript` from advising a backfill that cannot help.
//
// A chunk whose recognition failed never gains segments — the recorder and the
// backfill both refuse to call it silent — so it sits in the work queue forever.
// Counting those apart is what lets the transcript say "this cannot be recovered"
// instead of "run a backfill", which is the advice the queue would otherwise
// imply.
func TestChunksFailedTranscriptionCountsOnlyWhatCannotBeAttributed(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)

	insert := func(at time.Time, text string, metadata string) int64 {
		t.Helper()
		event := &Event{Kind: KindAudio, CapturedAt: at, Text: text,
			MediaPath: "/tmp/microphone.wav", AudioSource: "microphone",
			Metadata: json.RawMessage(metadata)}
		if err := s.Insert(ctx, event); err != nil {
			t.Fatal(err)
		}
		return event.ID
	}
	const failed = `{"audio_source":"microphone","processor_error":"recognizer unavailable"}`
	const clean = `{"audio_source":"microphone"}`

	insert(base, "", failed)                        // unattributed failure: counted
	insert(base.Add(time.Minute), "", clean)        // silence: not a failure
	insert(base.Add(2*time.Minute), "words", clean) // speech: not a failure

	// A failure that somehow did gain segments is not on the queue, so it is not
	// something to report as unrecoverable.
	attributed := insert(base.Add(3*time.Minute), "", failed)
	if err := s.ReplaceChunkSegments(ctx, formatTime(base.Add(3*time.Minute)), []Segment{
		{EventID: attributed, Seq: 0, Origin: OriginSilent, SourceTrack: "microphone",
			OrderConfidence: "sequence", Method: "silent"},
	}); err != nil {
		t.Fatal(err)
	}

	count, err := s.ChunksFailedTranscription(ctx, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("ChunksFailedTranscription = %d, want 1", count)
	}

	// The window is honoured, so a transcript reports only its own range.
	count, err = s.ChunksFailedTranscription(ctx, base.Add(time.Minute), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("ChunksFailedTranscription outside the failure's window = %d, want 0", count)
	}
}

// TestCappedTranscriptCoversOnlyTheTurnsItReturned is a regression test for a
// coverage claim that outran its own transcript.
//
// The cap drops turns from the tail after assembly, but the coverage boundary was
// fixed before it — so a capped page counted every chunk in the requested window
// as attributed, vouching for ground its text never reached. That is the same
// defect truncation already had, arriving by the other door.
func TestCappedTranscriptCoversOnlyTheTurnsItReturned(t *testing.T) {
	ctx := context.Background()
	s, _ := segmentStore(t)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	// Chunks far enough apart that no two turns merge, so the cap lands on a
	// chunk boundary and the arithmetic is unambiguous.
	const chunks = 10
	for c := range chunks {
		at := base.Add(time.Duration(c) * time.Hour)
		event := &Event{Kind: KindAudio, CapturedAt: at, Text: "x",
			MediaPath: "/tmp/microphone.wav", AudioSource: "microphone"}
		if err := s.Insert(ctx, event); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceChunkSegments(ctx, formatTime(at), []Segment{{
			EventID: event.ID, Seq: 0, Origin: OriginExternal, SourceTrack: "microphone",
			Text: "phrase", Confidence: 0.9, OrderConfidence: "sequence"}}); err != nil {
			t.Fatal(err)
		}
	}

	until := base.Add(24 * time.Hour)
	result, err := s.Transcript(ctx, TranscriptOptions{
		Since: base.Add(-time.Hour), Until: until, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Capped {
		t.Fatalf("%d turns came back uncapped at a limit of 3", len(result.Turns))
	}
	if result.CoveredUntil.After(base.Add(2 * time.Hour)) {
		t.Errorf("covered_until %s runs past the last returned turn", result.CoveredUntil)
	}
	if result.Chunks != 3 {
		t.Errorf("coverage counts %d chunks for a transcript holding 3 turns", result.Chunks)
	}
	// And it must name where to continue, or the capped turns are simply lost.
	if result.ResumeFrom.IsZero() {
		t.Fatal("a capped transcript offers no resume point, so its dropped turns are unreachable")
	}
	next, err := s.Transcript(ctx, TranscriptOptions{
		Since: result.ResumeFrom, Until: until, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Turns) == 0 {
		t.Fatal("resuming a capped transcript returned nothing")
	}
	if next.Turns[0].CapturedAt.Equal(result.Turns[len(result.Turns)-1].CapturedAt) {
		t.Errorf("the page after the cap repeats the turn at %s", next.Turns[0].CapturedAt)
	}
	if !next.Turns[0].CapturedAt.Equal(base.Add(3 * time.Hour)) {
		t.Errorf("resuming skipped to %s, want the first dropped turn at %s",
			next.Turns[0].CapturedAt, base.Add(3*time.Hour))
	}
}

// TestTruncatedTranscriptResumesWithoutRepeatingAChunk pins the other half of
// paging.
//
// CoveredUntil is the last chunk the turns reach, and SegmentsBetween is
// inclusive at both ends — so a caller told to resume from it re-reads that whole
// chunk and sees its turns twice. The resume point has to be the first chunk
// *not* covered.
func TestTruncatedTranscriptResumesWithoutRepeatingAChunk(t *testing.T) {
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
		t.Fatal("the transcript was not truncated, so the test proves nothing")
	}
	if !result.ResumeFrom.After(result.CoveredUntil) {
		t.Errorf("resume_from %s does not start after covered_until %s, so the last chunk repeats",
			result.ResumeFrom, result.CoveredUntil)
	}
	// Nothing may be skipped either: the resume point must be the very next chunk.
	segments, err := s.SegmentsBetween(ctx, result.CoveredUntil, result.ResumeFrom, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2*perChunk {
		t.Errorf("covered_until and resume_from span %d segments, want two adjacent chunks (%d)",
			len(segments), 2*perChunk)
	}
}

// TestCompleteTranscriptOffersNoResumePoint keeps the field from reading as
// "there is more" when there is not.
func TestCompleteTranscriptOffersNoResumePoint(t *testing.T) {
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
	result, err := s.Transcript(ctx, TranscriptOptions{
		Since: at.Add(-time.Hour), Until: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ResumeFrom.IsZero() {
		t.Errorf("a complete transcript names a resume point at %s", result.ResumeFrom)
	}
}
