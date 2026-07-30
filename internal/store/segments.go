package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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

// Segment origins. These are stored as text with no CHECK constraint, so adding
// a value later — distinguishing two machine-side participants, say — is a value
// change rather than a migration.
const (
	OriginInternal = "internal"
	OriginExternal = "external"
	OriginUnknown  = "unknown"
	// OriginSilent appears only on the wordless marker row written for a chunk
	// that was attributed and found to hold no speech. It is what keeps the
	// derived work queue drainable: without a row, "attributed and empty" and
	// "never attributed" are the same absence. It is not part of the origin
	// vocabulary a caller may filter on, because it never labels any text.
	OriginSilent = "silent"
)

const segmentSelect = `SELECT id, event_id, captured_at, seq, origin, source_track, text,
started_at, ended_at, start_offset_ms, end_offset_ms, runs_json, is_bleed, confidence,
order_confidence, method FROM audio_segments`

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
	args := []any{formatTime(since), formatTime(until)}
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
	return scanSegments(rows)
}

// SegmentsForChunk returns every segment of one chunk, bleed included, in reading
// order.
func (s *Store) SegmentsForChunk(ctx context.Context, capturedAt string) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx,
		segmentSelect+" WHERE captured_at = ? ORDER BY seq ASC", capturedAt)
	if err != nil {
		return nil, fmt.Errorf("query segments at %s: %w", capturedAt, err)
	}
	defer rows.Close()
	return scanSegments(rows)
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
		args = append(args, formatTime(*since))
	}
	if until != nil {
		query += " AND e.captured_at <= ?"
		args = append(args, formatTime(*until))
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
)`, formatTime(since), formatTime(until))
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
)`, formatTime(since), formatTime(until)).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("check for transcribed segments: %w", err)
	}
	return found != 0, nil
}

func scanSegments(rows *sql.Rows) ([]Segment, error) {
	var out []Segment
	for rows.Next() {
		var (
			segment                  Segment
			capturedAt               string
			startedAt, endedAt       sql.NullString
			startOffset, endOffset   sql.NullInt64
			runs, orderConf, method  string
			isBleed                  bool
			confidence               float64
			origin, source, textBody string
		)
		if err := rows.Scan(&segment.ID, &segment.EventID, &capturedAt, &segment.Seq,
			&origin, &source, &textBody, &startedAt, &endedAt, &startOffset, &endOffset,
			&runs, &isBleed, &confidence, &orderConf, &method); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, capturedAt)
		if err != nil {
			return nil, fmt.Errorf("parse segment captured_at %q: %w", capturedAt, err)
		}
		segment.CapturedAt = parsed
		segment.Origin, segment.SourceTrack, segment.Text = origin, source, textBody
		segment.IsBleed, segment.Confidence = isBleed, confidence
		segment.OrderConfidence, segment.Method = orderConf, method
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
	return value.UTC().Format(time.RFC3339Nano)
}

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

// AudioChunkTimes returns every distinct audio capture time, newest first. It
// backs a forced re-attribution, where the work list is all chunks rather than
// only the unattributed ones.
func (s *Store) AudioChunkTimes(ctx context.Context, since, until *time.Time, limit int) ([]string, error) {
	query := `SELECT DISTINCT captured_at FROM events WHERE kind = 'audio'`
	var args []any
	if since != nil {
		query += " AND captured_at >= ?"
		args = append(args, formatTime(*since))
	}
	if until != nil {
		query += " AND captured_at <= ?"
		args = append(args, formatTime(*until))
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
// a stable order. It is what a re-attribution reads: AudioTracksAt reports only
// text lengths, which is enough for collapse provenance but not for aligning two
// transcripts against each other.
func (s *Store) AudioEventsAt(ctx context.Context, capturedAt string) ([]Event, error) {
	return s.queryEvents(ctx,
		eventSelect+" WHERE kind = 'audio' AND captured_at = ? ORDER BY id ASC", capturedAt)
}
