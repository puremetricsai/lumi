package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/puremetricsai/lumi/internal/transcript"
)

// Segment is one attributed piece of an audio chunk.
//
// Segments are derived from events, never the other way round: events.text stays
// the recognizer's verbatim output, and re-deriving segments for a chunk is
// always safe. That is what makes the backfill idempotent.
type Segment struct {
	ID         int64     `json:"id"`
	EventID    int64     `json:"event_id"`
	CapturedAt time.Time `json:"captured_at"`
	// Seq is the reading position within the chunk, dense from zero and shared
	// across both of its tracks. It is the ordering key, because the text-only
	// attribution path produces no absolute times at all.
	Seq int `json:"seq"`
	// Origin is where the sound came from: "internal", "external", or "unknown".
	Origin string `json:"origin"`
	// SourceTrack is which WAV the text was read from, "system" or "microphone".
	// It differs from Origin exactly when speaker bleed was found.
	SourceTrack string `json:"source_track"`
	Text        string `json:"text"`
	// StartedAt and EndedAt are absolute; nil when the segment came from the
	// text-only path, which cannot know them.
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// StartOffsetMS and EndOffsetMS are relative to the segment's own WAV.
	StartOffsetMS *int64 `json:"start_offset_ms,omitempty"`
	EndOffsetMS   *int64 `json:"end_offset_ms,omitempty"`
	// RunsJSON holds the word timings this segment was derived from, so a later
	// pass can refine a split without re-transcribing the audio.
	RunsJSON json.RawMessage `json:"runs,omitempty"`
	// IsBleed marks a microphone segment that re-recorded machine audio. Such
	// rows are kept — nothing captured is discarded — but excluded from an
	// assembled transcript so a phrase appears once rather than twice.
	IsBleed         bool    `json:"is_bleed,omitempty"`
	Confidence      float64 `json:"confidence"`
	OrderConfidence string  `json:"order_confidence"`
	Method          string  `json:"method,omitempty"`
}

// Segment origins, re-exported from internal/transcript — which produces the
// values that get stored — so a caller compares against a name rather than
// retyping the string, exactly as the OrderConfidence tiers are one file over.
// They are stored as text with no CHECK constraint, so adding a value later —
// distinguishing two machine-side participants, say — is a value change rather
// than a migration.
const (
	OriginInternal = string(transcript.OriginInternal)
	OriginExternal = string(transcript.OriginExternal)
	OriginUnknown  = string(transcript.OriginUnknown)
	// OriginSilent appears only on the wordless marker row written for a chunk
	// that was attributed and found to hold no speech. It is what keeps the
	// derived work queue drainable: without a row, "attributed and empty" and
	// "never attributed" are the same absence. It is not part of the origin
	// vocabulary a caller may filter on, because it never labels any text — see
	// ParseOrigin, which owns that distinction.
	OriginSilent = string(transcript.OriginSilent)
)

// Capture device names, re-exported from internal/transcript for the same reason
// the origins above are: internal/capture stamps events.audio_source from
// transcript.TrackSystem/TrackMicrophone, so a second spelling here would be a
// copy that is correct only until one of them moves. audio_source is the *device*
// a row was read from, never a claim about what made the sound — Segment.Origin
// is that question, and the two differ exactly where bleed was found.
const (
	AudioSourceSystem     = transcript.TrackSystem
	AudioSourceMicrophone = transcript.TrackMicrophone
)

// ParseOrigin validates a caller's origin filter, returning the stored value or
// an error naming the vocabulary. Empty means every origin.
//
// It lives here rather than in each caller for the reason HasSearchableTerms
// does: which origins may be filtered on is a fact about the column, and the
// schema was deliberately left open to a fourth value. Two copies of the switch
// would make that value filterable over MCP and rejected as a bad flag on the
// CLI, or the reverse — and neither test suite could see the difference.
//
// The error spells the vocabulary out rather than listing valid strings, because
// the names are only obvious once you know that Lumi records two audio tracks and
// that "internal" is about which machine produced the sound rather than who was
// speaking.
func ParseOrigin(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "":
		return "", nil
	case OriginInternal:
		return OriginInternal, nil
	case OriginExternal:
		return OriginExternal, nil
	case OriginUnknown:
		return OriginUnknown, nil
	}
	return "", fmt.Errorf(`origin must be "internal" (sound this machine produced, such as the far side `+
		`of a call, a video, or a notification), "external" (sound the microphone picked up from the room, `+
		`usually the user), or "unknown" (machine audio was playing but produced no transcript, so the `+
		`origin could not be determined); got %q`, value)
}

// SegmentFrom converts an attribution verdict into the row that stores it.
//
// One converter rather than one per writer: the recorder and `lumi transcript
// backfill` both produce these rows and ReplaceChunkSegments promises all three
// write paths converge on the same ones. Two copies would make a new column a
// two-package edit, and missing one would silently degrade exactly the rows a
// --force backfill rewrites.
func SegmentFrom(segment transcript.Segment, eventID int64) Segment {
	row := Segment{
		EventID: eventID, Seq: segment.Seq, Origin: string(segment.Origin),
		SourceTrack: segment.SourceTrack, Text: segment.Text,
		StartOffsetMS: segment.StartOffsetMS, EndOffsetMS: segment.EndOffsetMS,
		IsBleed: segment.IsBleed, Confidence: segment.Confidence,
		OrderConfidence: string(segment.OrderConfidence), Method: string(segment.Method),
	}
	if !segment.StartedAt.IsZero() {
		started := segment.StartedAt
		row.StartedAt = &started
	}
	if !segment.EndedAt.IsZero() {
		ended := segment.EndedAt
		row.EndedAt = &ended
	}
	if len(segment.Runs) > 0 {
		if encoded, err := json.Marshal(segment.Runs); err == nil {
			row.RunsJSON = encoded
		}
	}
	return row
}

// SegmentRows converts a chunk's whole verdict, dropping any segment whose track
// produced no event row: such a segment cannot be stored against anything, and
// dropping it beats orphaning it.
func SegmentRows(segments []transcript.Segment, eventIDs map[string]int64) []Segment {
	rows := make([]Segment, 0, len(segments))
	for _, segment := range segments {
		eventID, ok := eventIDs[segment.SourceTrack]
		if !ok {
			continue
		}
		rows = append(rows, SegmentFrom(segment, eventID))
	}
	return rows
}

// Two projections, differing only in runs_json.
//
// The word timings are stored so a later pass can refine a split without
// re-transcribing, and a chunk read for that purpose wants them. A transcript
// does not: nothing between SegmentsBetween and an assembled turn touches them,
// and they were measured averaging 686 bytes a row against 55 bytes of text — so
// carrying them through the read behind `lumi transcript` and get_transcript
// costs twelve times the payload of the only column that read uses, over up to
// maxTranscriptSegments rows.
const (
	segmentColumns = `id, event_id, captured_at, seq, origin, source_track, text,
started_at, ended_at, start_offset_ms, end_offset_ms, is_bleed, confidence,
order_confidence, method`
	segmentSelect     = `SELECT ` + segmentColumns + ` FROM audio_segments`
	segmentSelectRuns = `SELECT ` + segmentColumns + `, runs_json FROM audio_segments`
)

// ReplaceChunkSegments makes one chunk's segments exactly segs, in a single
// transaction that deletes whatever was there first.
//
// Replacing rather than inserting is what makes attribution safe to re-run: the
// recorder writes segments after indexing a chunk, the backfill writes them for
// chunks that have none, and a forced backfill rewrites them wholesale. All three
// converge on the same rows, and an interrupted run loses at most one chunk.
//
// capturedAt is the chunk key for every row written, taken from the caller and
// not from the segments, so one call can never spread a chunk across two keys.
// Both writers derive it from the events themselves — the recorder from the same
// instant it stamped the rows with, the backfill from the events it just read —
// which is what keeps it from drifting from the rows it describes. A caller that
// invents a key would silently write segments no chunk can reach.
func (s *Store) ReplaceChunkSegments(ctx context.Context, capturedAt string, segments []Segment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin segment write: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM audio_segments WHERE captured_at = ?`, capturedAt); err != nil {
		return fmt.Errorf("clear segments at %s: %w", capturedAt, err)
	}

	statement, err := tx.PrepareContext(ctx, `INSERT INTO audio_segments
(event_id, captured_at, seq, origin, source_track, text, started_at, ended_at,
 start_offset_ms, end_offset_ms, runs_json, is_bleed, confidence, order_confidence, method)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare segment insert: %w", err)
	}
	defer statement.Close()

	for _, segment := range segments {
		runs := ""
		if len(segment.RunsJSON) > 0 {
			runs = string(segment.RunsJSON)
		}
		if _, err := statement.ExecContext(ctx,
			segment.EventID, capturedAt, segment.Seq, segment.Origin, segment.SourceTrack,
			segment.Text, nullableTime(segment.StartedAt), nullableTime(segment.EndedAt),
			nullableInt(segment.StartOffsetMS), nullableInt(segment.EndOffsetMS), runs,
			segment.IsBleed, segment.Confidence, segment.OrderConfidence, segment.Method,
		); err != nil {
			return fmt.Errorf("insert segment %d at %s: %w", segment.Seq, capturedAt, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit segments at %s: %w", capturedAt, err)
	}
	return nil
}

// SegmentsBetween returns the segments of every chunk captured in [since, until],
// in reading order.
//
// Ordering is (captured_at, seq) rather than by timestamp, because the text-only
// path leaves started_at NULL. seq is the only key present on every row.
func (s *Store) SegmentsBetween(ctx context.Context, since, until time.Time, includeBleed bool, limit int) ([]Segment, error) {
	query := segmentSelect + " WHERE captured_at >= ? AND captured_at <= ?"
	args := []any{lowerBound(since), upperBound(until)}
	if !includeBleed {
		query += " AND is_bleed = 0"
	}
	query += " ORDER BY captured_at ASC, seq ASC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query segments: %w", err)
	}
	defer rows.Close()
	return scanSegments(rows, false)
}

// SegmentsForChunk returns every segment of one chunk, bleed included, in reading
// order, with the word timings each was derived from.
func (s *Store) SegmentsForChunk(ctx context.Context, capturedAt string) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx,
		segmentSelectRuns+" WHERE captured_at = ? ORDER BY seq ASC", capturedAt)
	if err != nil {
		return nil, fmt.Errorf("query segments at %s: %w", capturedAt, err)
	}
	defer rows.Close()
	return scanSegments(rows, true)
}

// ChunksMissingSegments returns audio capture times that have no segments at all,
// newest first.
//
// The work queue is derived rather than stored, so there is no state file to go
// stale and nothing to reconcile after a crash. It also doubles as the recorder's
// retry path: a chunk whose segment write failed simply still has none, and the
// next backfill picks it up.
func (s *Store) ChunksMissingSegments(ctx context.Context, since, until *time.Time, limit int) ([]string, error) {
	query := `SELECT DISTINCT e.captured_at FROM events e
WHERE e.kind = 'audio'
  AND NOT EXISTS (SELECT 1 FROM audio_segments s WHERE s.captured_at = e.captured_at)`
	var args []any
	if since != nil {
		query += " AND e.captured_at >= ?"
		args = append(args, lowerBound(*since))
	}
	if until != nil {
		query += " AND e.captured_at <= ?"
		args = append(args, upperBound(*until))
	}
	query += " ORDER BY e.captured_at DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query unattributed chunks: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var capturedAt string
		if err := rows.Scan(&capturedAt); err != nil {
			return nil, err
		}
		out = append(out, capturedAt)
	}
	return out, rows.Err()
}

// SegmentCoverage reports how many audio chunks fall in a window and how many of
// them have been attributed, so a caller can say a transcript has holes rather
// than serving one silently.
func (s *Store) SegmentCoverage(ctx context.Context, since, until time.Time) (chunks, attributed int64, err error) {
	row := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(has_segments), 0) FROM (
  SELECT EXISTS (SELECT 1 FROM audio_segments s WHERE s.captured_at = e.captured_at) AS has_segments
  FROM (SELECT DISTINCT captured_at FROM events
        WHERE kind = 'audio' AND captured_at >= ? AND captured_at <= ?) e
)`, lowerBound(since), upperBound(until))
	if err := row.Scan(&chunks, &attributed); err != nil {
		return 0, 0, fmt.Errorf("measure segment coverage: %w", err)
	}
	return chunks, attributed, nil
}

// HasSpeechSegments reports whether any chunk in a window produced a segment
// carrying words.
//
// It is a different question from SegmentCoverage, and the difference appeared
// the moment silent chunks started writing a marker row: coverage answers "has
// this been processed", which a wordless marker satisfies, while a caller
// deciding whether to point a reader at a transcript needs "is there anything to
// read". Answering the first when you meant the second recommends an empty
// transcript over a range of silence.
func (s *Store) HasSpeechSegments(ctx context.Context, since, until time.Time) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM audio_segments
  WHERE captured_at >= ? AND captured_at <= ? AND is_bleed = 0 AND TRIM(text) <> ''
)`, lowerBound(since), upperBound(until)).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("check for transcribed segments: %w", err)
	}
	return found != 0, nil
}

// scanSegments reads rows of segmentColumns, plus runs_json when withRuns says
// the projection carried it.
func scanSegments(rows *sql.Rows, withRuns bool) ([]Segment, error) {
	var out []Segment
	for rows.Next() {
		var (
			segment                Segment
			capturedAt, runs       string
			startedAt, endedAt     sql.NullString
			startOffset, endOffset sql.NullInt64
		)
		targets := []any{&segment.ID, &segment.EventID, &capturedAt, &segment.Seq,
			&segment.Origin, &segment.SourceTrack, &segment.Text, &startedAt, &endedAt,
			&startOffset, &endOffset, &segment.IsBleed, &segment.Confidence,
			&segment.OrderConfidence, &segment.Method}
		if withRuns {
			targets = append(targets, &runs)
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, capturedAt)
		if err != nil {
			return nil, fmt.Errorf("parse segment captured_at %q: %w", capturedAt, err)
		}
		segment.CapturedAt = parsed
		if runs != "" {
			segment.RunsJSON = json.RawMessage(runs)
		}
		if startedAt.Valid {
			value, err := time.Parse(time.RFC3339Nano, startedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse segment started_at %q: %w", startedAt.String, err)
			}
			segment.StartedAt = &value
		}
		if endedAt.Valid {
			value, err := time.Parse(time.RFC3339Nano, endedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse segment ended_at %q: %w", endedAt.String, err)
			}
			segment.EndedAt = &value
		}
		if startOffset.Valid {
			value := startOffset.Int64
			segment.StartOffsetMS = &value
		}
		if endOffset.Valid {
			value := endOffset.Int64
			segment.EndOffsetMS = &value
		}
		out = append(out, segment)
	}
	return out, rows.Err()
}

// formatTime renders a timestamp the way every other time column in this schema
// is written. Timestamps are compared lexicographically as strings, so any
// deviation here would silently break range filters.
func formatTime(value time.Time) string {
	return FormatCapturedAt(value)
}

// lowerBound and upperBound render a range endpoint so it covers both renderings
// captured_at has been stored in. formatTime stays exact because it also writes
// audio_segments' own columns; only comparisons need the tolerant form. See
// LowerCapturedAtBound.
func lowerBound(value time.Time) string { return LowerCapturedAtBound(value) }
func upperBound(value time.Time) string { return UpperCapturedAtBound(value) }

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func nullableInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// HasSegmentText reports whether a segment carries anything worth showing. It
// exists so callers stop reimplementing the blank check the way internal/mcp once
// reimplemented the FTS term rule.
func HasSegmentText(segment Segment) bool {
	return strings.TrimSpace(segment.Text) != ""
}

// failedTranscriptionKey is the metadata key the recorder writes when speech
// recognition did not complete. The SQL below matches it in its quoted JSON form,
// which is exact because the metadata is produced by encoding/json and never
// hand-written.
const failedTranscriptionKey = "processor_error"

// FailedTranscription reports that an event has no transcript because recognition
// did not happen — as opposed to happening and finding nothing.
//
// The recognizer returns an empty string for both, and the two need opposite
// conclusions: silence is a fact about the room, a failure is a fact about the
// recognizer. Attribution reads the empty transcript as evidence, so anything
// deriving a verdict has to consult this first.
//
// An event that carries words is not a failed transcription however loudly its
// metadata complains: whatever went wrong, there is a transcript to attribute.
func FailedTranscription(event Event) bool {
	if strings.TrimSpace(event.Text) != "" {
		return false
	}
	if len(event.Metadata) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(event.Metadata, &fields); err != nil {
		// Metadata we cannot read is not evidence of anything. Guessing "failed"
		// here would strand chunks on the queue on the strength of a parse error.
		return false
	}
	_, ok := fields[failedTranscriptionKey]
	return ok
}

// AnyFailedTranscription reports whether any of a chunk's rows is missing its
// transcript because recognition did not happen. It is half of the gate that
// keeps a failed recognition from being recorded as silence — transcript.IsSilent
// is the other half — and both writers of attribution apply it.
func AnyFailedTranscription(events []Event) bool {
	for _, event := range events {
		if FailedTranscription(event) {
			return true
		}
	}
	return false
}

// ChunksFailedTranscription counts the audio chunks in a window that have no
// attribution and never will, because recognition failed on every track that
// might have carried words.
//
// These are the chunks the derived work queue can never drain: neither the
// recorder nor the backfill will label them, because labelling them means calling
// them silent. Counting them apart is what lets a transcript report a hole it
// cannot fix, instead of advising a backfill that would reach the same dead end.
func (s *Store) ChunksFailedTranscription(ctx context.Context, since, until time.Time) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM (
  SELECT captured_at FROM events
  WHERE kind = 'audio' AND captured_at >= ? AND captured_at <= ?
    AND NOT EXISTS (SELECT 1 FROM audio_segments s WHERE s.captured_at = events.captured_at)
  GROUP BY captured_at
  HAVING SUM(CASE WHEN TRIM(text) <> '' THEN 1 ELSE 0 END) = 0
     AND SUM(CASE WHEN metadata_json LIKE '%"`+failedTranscriptionKey+`"%' THEN 1 ELSE 0 END) > 0
)`, lowerBound(since), upperBound(until)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count chunks whose transcription failed: %w", err)
	}
	return count, nil
}

// AudioChunkTimes returns every distinct audio capture time, newest first. It
// backs a forced re-attribution, where the work list is all chunks rather than
// only the unattributed ones.
func (s *Store) AudioChunkTimes(ctx context.Context, since, until *time.Time, limit int) ([]string, error) {
	query := `SELECT DISTINCT captured_at FROM events WHERE kind = 'audio'`
	var args []any
	if since != nil {
		query += " AND captured_at >= ?"
		args = append(args, lowerBound(*since))
	}
	if until != nil {
		query += " AND captured_at <= ?"
		args = append(args, upperBound(*until))
	}
	query += " ORDER BY captured_at DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query audio chunks: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var capturedAt string
		if err := rows.Scan(&capturedAt); err != nil {
			return nil, err
		}
		out = append(out, capturedAt)
	}
	return out, rows.Err()
}

// AudioEventsAt returns both tracks of one chunk with their full transcripts, in
// a stable order. It is what a re-attribution reads, which needs the transcripts
// themselves in order to align the two tracks against each other.
func (s *Store) AudioEventsAt(ctx context.Context, capturedAt string) ([]Event, error) {
	return s.queryEvents(ctx,
		eventSelect+" WHERE kind = 'audio' AND captured_at = ? ORDER BY id ASC", capturedAt)
}
