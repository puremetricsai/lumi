package store

import (
	"context"
	"fmt"
)

// migration is one forward-only schema change. Version numbers start at 1 and
// increase by one. The applied version is tracked in SQLite's user_version
// pragma, so the whole system costs no extra table.
//
// Never edit the SQL of a migration that has shipped — databases in the field
// have already applied it and will not re-run it. Append a new migration
// instead.
type migration struct {
	Version int
	SQL     string
}

var migrations = []migration{
	{
		Version: 1,
		SQL: `
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
END;`,
	},
	{
		Version: 2,
		SQL: `
CREATE INDEX IF NOT EXISTS events_app_captured_at_idx
  ON events(app COLLATE NOCASE, captured_at DESC);`,
	},
	{
		Version: 3,
		SQL: `
ALTER TABLE events ADD COLUMN text_source TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN display_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN audio_source TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS events_display_captured_at_idx
  ON events(display_id, captured_at DESC);
CREATE INDEX IF NOT EXISTS events_audio_source_captured_at_idx
  ON events(audio_source, captured_at DESC);`,
	},
	{
		Version: 4,
		SQL: `
CREATE TABLE IF NOT EXISTS audio_segments (
  id INTEGER PRIMARY KEY,
  event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  captured_at TEXT NOT NULL,
  seq INTEGER NOT NULL,
  origin TEXT NOT NULL,
  source_track TEXT NOT NULL,
  text TEXT NOT NULL DEFAULT '',
  started_at TEXT,
  ended_at TEXT,
  start_offset_ms INTEGER,
  end_offset_ms INTEGER,
  runs_json TEXT NOT NULL DEFAULT '',
  is_bleed INTEGER NOT NULL DEFAULT 0,
  confidence REAL NOT NULL DEFAULT 0,
  order_confidence TEXT NOT NULL DEFAULT 'sequence',
  method TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  UNIQUE (event_id, seq)
);
CREATE INDEX IF NOT EXISTS audio_segments_chunk_idx
  ON audio_segments(captured_at, seq);
CREATE INDEX IF NOT EXISTS audio_segments_origin_idx
  ON audio_segments(origin, captured_at);
CREATE TRIGGER IF NOT EXISTS audio_segments_event_ad AFTER DELETE ON events BEGIN
  DELETE FROM audio_segments WHERE event_id = old.id;
END;`,
	},
	{
		// audio_attribution and source_apps_json promote "which application made
		// this sound" out of metadata_json and into columns, because an agent
		// branching on it must not have to parse a blob to tell a fact from a
		// guess. They stay TEXT with no CHECK for the reason origin does: naming a
		// further class of source later is then a value change, not a migration.
		//
		// stream_offset_ms is nullable with no default on purpose. Zero is a real
		// value — every session's first chunk has it — so NOT NULL DEFAULT 0 would
		// make every row recorded before this migration claim to be a session
		// start. NULL means "not recorded", which is what those rows are.
		//
		// events_fts is untouched: it indexes text, app, and window only, and all
		// three of its sync triggers name those columns, so no new column reaches
		// it and no reindex is needed.
		Version: 5,
		SQL: `
ALTER TABLE events ADD COLUMN audio_attribution TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN source_apps_json TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN stream_offset_ms INTEGER;
CREATE INDEX IF NOT EXISTS events_audio_attribution_captured_at_idx
  ON events(audio_attribution, captured_at DESC);`,
	},
}

// runMigrations applies every migration whose version exceeds the database's
// current user_version, each in its own transaction.
func (s *Store) runMigrations(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d (SQLite must include FTS5): %w", m.Version, err)
		}
		// user_version does not accept a bound parameter.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record schema version %d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}
		current = m.Version
	}
	return nil
}
