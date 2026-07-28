package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Kind string

const (
	KindScreen Kind = "screen"
	KindAudio  Kind = "audio"
)

type Event struct {
	ID          int64           `json:"id"`
	Kind        Kind            `json:"kind"`
	CapturedAt  time.Time       `json:"captured_at"`
	Text        string          `json:"text"`
	App         string          `json:"app,omitempty"`
	Window      string          `json:"window,omitempty"`
	MediaPath   string          `json:"media_path"`
	DurationMS  int64           `json:"duration_ms,omitempty"`
	TextSource  string          `json:"text_source,omitempty"`
	DisplayID   uint32          `json:"display_id,omitempty"`
	AudioSource string          `json:"audio_source,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Rank        float64         `json:"rank,omitempty"`
}

type SearchOptions struct {
	Query  string
	Match  MatchMode
	Kind   Kind
	App    string // exact, case-insensitive
	Window string // substring, case-insensitive
	Since  *time.Time
	Until  *time.Time
	Limit  int
	// RequireText drops events whose extracted text or transcript is empty (or
	// only whitespace). It has no caller today and is retained for `lumi mcp`:
	// media saved without a usable transcript answers no content question, and
	// Lumi records enough silent audio chunks that they crowd real speech out of
	// a recency pass. It stays opt-in so `search` and the JSON export still see
	// every stored row. See CLAUDE.md.
	RequireText bool
}

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure sqlite (%s): %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// likeEscape neutralises LIKE wildcards so a filter value is matched literally.
// Pair it with an ESCAPE '\' clause.
func likeEscape(input string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(input)
}

func (s *Store) migrate(ctx context.Context) error {
	return s.runMigrations(ctx)
}

func (s *Store) Insert(ctx context.Context, event *Event) error {
	if event.Kind != KindScreen && event.Kind != KindAudio {
		return fmt.Errorf("invalid event kind %q", event.Kind)
	}
	if event.CapturedAt.IsZero() {
		event.CapturedAt = time.Now().UTC()
	}
	if len(event.Metadata) == 0 {
		event.Metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(event.Metadata) {
		return errors.New("event metadata is not valid JSON")
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO events(kind, captured_at, text, app, window, media_path, duration_ms,
                   text_source, display_id, audio_source, metadata_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.Kind, event.CapturedAt.UTC().Format(time.RFC3339Nano),
		event.Text, event.App, event.Window, event.MediaPath, event.DurationMS,
		event.TextSource, event.DisplayID, event.AudioSource, string(event.Metadata))
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	event.ID, err = result.LastInsertId()
	if err != nil {
		return fmt.Errorf("read inserted event id: %w", err)
	}
	return nil
}

func (s *Store) Search(ctx context.Context, opts SearchOptions) ([]Event, error) {
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	if opts.Limit > 500 {
		opts.Limit = 500
	}
	where := make([]string, 0, 4)
	args := make([]any, 0, 5)
	from := "events e"
	rank := "0.0"
	// Guard on the built expression, not the raw query: a query of pure
	// punctuation survives TrimSpace but tokenizes to nothing.
	match := ftsExpression(opts.Query, opts.Match)
	if match != "" {
		from = "events_fts JOIN events e ON e.id = events_fts.rowid"
		where = append(where, "events_fts MATCH ?")
		args = append(args, match)
		// bm25 length-normalizes per column, so a hit in the one-word app or
		// window column scores far higher than the same term buried in a page
		// of screen text. Under MatchAll that rarely mattered; under MatchAny it would
		// let stray window titles outrank substantive content. Weights are
		// applied only here so `search` ranking is unchanged.
		//
		// Nothing selects MatchAny today; the weighted branch is retained for
		// `lumi mcp` and pinned by TestSearchMatchAnyWeightsBodyTextOverAppAndWindow.
		// See CLAUDE.md.
		if opts.Match == MatchAny {
			rank = "bm25(events_fts, 1.0, 0.4, 0.4)"
		} else {
			rank = "bm25(events_fts)"
		}
	}
	if opts.Kind != "" {
		where = append(where, "e.kind = ?")
		args = append(args, opts.Kind)
	}
	if opts.RequireText {
		// Trim the whitespace-only transcripts SpeechAnalyzer emits for a
		// near-silent chunk; they are indistinguishable from no transcript.
		where = append(where, `trim(e.text, char(32) || char(9) || char(10) || char(13)) <> ''`)
	}
	if app := strings.TrimSpace(opts.App); app != "" {
		where = append(where, "e.app = ? COLLATE NOCASE")
		args = append(args, app)
	}
	if window := strings.TrimSpace(opts.Window); window != "" {
		where = append(where, `e.window LIKE ? ESCAPE '\' COLLATE NOCASE`)
		args = append(args, "%"+likeEscape(window)+"%")
	}
	if opts.Since != nil {
		where = append(where, "e.captured_at >= ?")
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	if opts.Until != nil {
		where = append(where, "e.captured_at <= ?")
		args = append(args, opts.Until.UTC().Format(time.RFC3339Nano))
	}
	query := `SELECT e.id, e.kind, e.captured_at, e.text, e.app, e.window,
e.media_path, e.duration_ms, e.text_source, e.display_id, e.audio_source, e.metadata_json,
` + rank + ` AS rank FROM ` + from
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if match != "" {
		query += " ORDER BY rank, e.captured_at DESC"
	} else {
		query += " ORDER BY e.captured_at DESC"
	}
	query += " LIMIT ?"
	args = append(args, opts.Limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search events: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var capturedAt, metadata string
		if err := rows.Scan(&event.ID, &event.Kind, &capturedAt, &event.Text, &event.App,
			&event.Window, &event.MediaPath, &event.DurationMS, &event.TextSource, &event.DisplayID,
			&event.AudioSource, &metadata, &event.Rank); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		event.CapturedAt, err = time.Parse(time.RFC3339Nano, capturedAt)
		if err != nil {
			return nil, fmt.Errorf("parse event timestamp %q: %w", capturedAt, err)
		}
		event.Metadata = json.RawMessage(metadata)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search results: %w", err)
	}
	return events, nil
}

// Expired returns events captured strictly before the cutoff, oldest first.
// A limit of zero or less means no limit.
func (s *Store) Expired(ctx context.Context, before time.Time, limit int) ([]Event, error) {
	query := `SELECT id, kind, captured_at, text, app, window, media_path, duration_ms,
text_source, display_id, audio_source, metadata_json
FROM events WHERE captured_at < ? ORDER BY captured_at ASC`
	args := []any{before.UTC().Format(time.RFC3339Nano)}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	return s.queryEvents(ctx, query, args...)
}

// AllEvents returns every event, oldest first, with no timestamp cutoff. It
// backs the `lumi prune --all` wipe, where a bounded `Expired` cutoff could
// silently skip a row timestamped at or past the cutoff (e.g. a far-future
// captured_at). Callers own deleting the returned rows and their media.
func (s *Store) AllEvents(ctx context.Context) ([]Event, error) {
	query := `SELECT id, kind, captured_at, text, app, window, media_path, duration_ms,
text_source, display_id, audio_source, metadata_json
FROM events ORDER BY captured_at ASC`
	return s.queryEvents(ctx, query)
}

// ErrEventNotFound reports that no stored event carries the requested id.
var ErrEventNotFound = errors.New("event not found")

// EventByID returns a single event by id, with its full untruncated text and
// metadata. A missing id is reported as ErrEventNotFound rather than a nil
// event, so `lumi mcp`'s get_event can name the id in a tool error instead of
// returning an empty result an agent would read as "no content".
func (s *Store) EventByID(ctx context.Context, id int64) (*Event, error) {
	events, err := s.queryEvents(ctx, `SELECT id, kind, captured_at, text, app, window, media_path, duration_ms,
text_source, display_id, audio_source, metadata_json
FROM events WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("event %d: %w", id, ErrEventNotFound)
	}
	return &events[0], nil
}

// queryEvents runs a SELECT with the standard event column list and scans the
// rows into Events.
func (s *Store) queryEvents(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select events: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var capturedAt, metadata string
		if err := rows.Scan(&event.ID, &event.Kind, &capturedAt, &event.Text, &event.App,
			&event.Window, &event.MediaPath, &event.DurationMS, &event.TextSource, &event.DisplayID,
			&event.AudioSource, &metadata); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		event.CapturedAt, err = time.Parse(time.RFC3339Nano, capturedAt)
		if err != nil {
			return nil, fmt.Errorf("parse event timestamp %q: %w", capturedAt, err)
		}
		event.Metadata = json.RawMessage(metadata)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}

// deleteBatchSize bounds the IN (?, …) list well under SQLite's
// SQLITE_MAX_VARIABLE_NUMBER (32766) so a large prune does not exceed it.
const deleteBatchSize = 900

// DeleteByIDs removes events and, via the events_ad trigger, their FTS rows.
// It deletes in batches to stay under SQLite's bound-parameter limit, so a
// prune of an arbitrarily large expired set never trips "too many SQL
// variables". It does not touch media files; callers own that.
func (s *Store) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var total int64
	for start := 0; start < len(ids); start += deleteBatchSize {
		end := start + deleteBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		result, err := s.db.ExecContext(ctx,
			"DELETE FROM events WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
		if err != nil {
			return total, fmt.Errorf("delete events: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("count deleted events: %w", err)
		}
		total += deleted
	}
	return total, nil
}
