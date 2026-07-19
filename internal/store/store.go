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
	ID         int64           `json:"id"`
	Kind       Kind            `json:"kind"`
	CapturedAt time.Time       `json:"captured_at"`
	Text       string          `json:"text"`
	App        string          `json:"app,omitempty"`
	Window     string          `json:"window,omitempty"`
	MediaPath  string          `json:"media_path"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Rank       float64         `json:"rank,omitempty"`
}

type SearchOptions struct {
	Query string
	Kind  Kind
	Since *time.Time
	Until *time.Time
	Limit int
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

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('screen', 'audio')),
  captured_at TEXT NOT NULL,
  text TEXT NOT NULL DEFAULT '',
  app TEXT NOT NULL DEFAULT '',
  window TEXT NOT NULL DEFAULT '',
  media_path TEXT NOT NULL,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  metadata_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS events_captured_at_idx ON events(captured_at DESC);
CREATE INDEX IF NOT EXISTS events_kind_captured_at_idx ON events(kind, captured_at DESC);
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(
  text, app, window,
  content='events', content_rowid='id',
  tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS events_ai AFTER INSERT ON events BEGIN
  INSERT INTO events_fts(rowid, text, app, window)
  VALUES (new.id, new.text, new.app, new.window);
END;
CREATE TRIGGER IF NOT EXISTS events_ad AFTER DELETE ON events BEGIN
  INSERT INTO events_fts(events_fts, rowid, text, app, window)
  VALUES ('delete', old.id, old.text, old.app, old.window);
END;
CREATE TRIGGER IF NOT EXISTS events_au AFTER UPDATE ON events BEGIN
  INSERT INTO events_fts(events_fts, rowid, text, app, window)
  VALUES ('delete', old.id, old.text, old.app, old.window);
  INSERT INTO events_fts(rowid, text, app, window)
  VALUES (new.id, new.text, new.app, new.window);
END;`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate database (SQLite must include FTS5): %w", err)
	}
	return nil
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
INSERT INTO events(kind, captured_at, text, app, window, media_path, duration_ms, metadata_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, event.Kind, event.CapturedAt.UTC().Format(time.RFC3339Nano),
		event.Text, event.App, event.Window, event.MediaPath, event.DurationMS, string(event.Metadata))
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
	if strings.TrimSpace(opts.Query) != "" {
		from = "events_fts JOIN events e ON e.id = events_fts.rowid"
		where = append(where, "events_fts MATCH ?")
		args = append(args, ftsQuery(opts.Query))
		rank = "bm25(events_fts)"
	}
	if opts.Kind != "" {
		where = append(where, "e.kind = ?")
		args = append(args, opts.Kind)
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
e.media_path, e.duration_ms, e.metadata_json, ` + rank + ` AS rank FROM ` + from
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if strings.TrimSpace(opts.Query) != "" {
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
			&event.Window, &event.MediaPath, &event.DurationMS, &metadata, &event.Rank); err != nil {
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

func ftsQuery(input string) string {
	words := strings.Fields(input)
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		word = strings.ReplaceAll(word, `"`, `""`)
		quoted = append(quoted, `"`+word+`"`)
	}
	return strings.Join(quoted, " AND ")
}
