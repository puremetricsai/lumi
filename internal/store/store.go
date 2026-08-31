package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

type Kind string

const (
	KindScreen Kind = "screen"
	KindAudio  Kind = "audio"
)

type Event struct {
	ID          int64     `json:"id"`
	Kind        Kind      `json:"kind"`
	CapturedAt  time.Time `json:"captured_at"`
	Text        string    `json:"text"`
	App         string    `json:"app,omitempty"`
	Window      string    `json:"window,omitempty"`
	MediaPath   string    `json:"media_path"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
	TextSource  string    `json:"text_source,omitempty"`
	DisplayID   uint32    `json:"display_id,omitempty"`
	AudioSource string    `json:"audio_source,omitempty"`
	// AudioAttribution says how SourceApps was earned; see AudioAttribution.
	// Empty on screen rows and on audio rows predating the column.
	AudioAttribution string `json:"audio_attribution,omitempty"`
	// SourceApps is the raw source_apps_json column. It stays raw for the same
	// reason Metadata does — the store stores it and does not interpret it — and
	// because "" and "[]" mean different things that a decoded slice cannot hold
	// apart. Read it with DecodeSourceApps.
	SourceApps string `json:"source_apps,omitempty"`
	// StreamOffsetMS is how far into the capture session this chunk began. It is
	// the exact, drift-free grid position that captured_at used to be before
	// captured_at became a measured instant; nil means it was never recorded.
	StreamOffsetMS *int64          `json:"stream_offset_ms,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	Rank           float64         `json:"rank,omitempty"`
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
	// only whitespace). Its only caller is `lumi mcp`, as the `require_text`
	// tool parameter: media saved without a usable transcript answers no
	// content question, and
	// Lumi records enough silent audio chunks that they crowd real speech out of
	// a recency pass. It stays opt-in so `search` and the JSON export still see
	// every stored row. See CLAUDE.md.
	RequireText bool
}

const (
	// DefaultSearchLimit is the page Search returns when a caller asks for no
	// particular limit. `lumi search`'s --limit default and `lumi mcp`'s limit
	// parameter both derive from it rather than restating the number.
	DefaultSearchLimit = 20
	// MaxSearchLimit is the ceiling Search clamps to. Callers that document a
	// cap to their users (the MCP tool schema does) must read it from here, or
	// the documented contract silently stops matching the clamp.
	MaxSearchLimit = 500
)

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
	if _, err := ParseAudioAttribution(event.AudioAttribution); err != nil {
		return err
	}
	// A screen row carrying an audio attribution would fork what the column means
	// by row kind, which is the defect this column exists to undo rather than
	// repeat.
	if event.Kind == KindScreen && event.AudioAttribution != "" {
		return fmt.Errorf("screen event carries audio attribution %q", event.AudioAttribution)
	}
	// Decoding, not merely json.Valid: a syntactically fine but wrongly shaped
	// payload such as `["Comet"]` parses as JSON and then fails to decode, so
	// validating only syntax stores provenance nothing can read. Downstream that
	// is worse than rejecting it — the MCP boundary silently drops an
	// undecodable list, so the row looks like it simply had no source.
	sourceApps, _, err := DecodeSourceApps(event.SourceApps)
	if err != nil {
		return err
	}
	// Microphone audio is never attributed to an application. The rule is decided
	// in internal/capture, but it is enforced here as well because it is a
	// guarantee the whole repository makes to its users — and a guarantee that
	// rests on one caller getting it right is one a second writer can break
	// silently. Attributing room audio invents a source that outlives its
	// evidence: the WAV is deleted on the retention schedule while whatever was
	// built from it survives.
	if event.AudioSource == AudioSourceMicrophone {
		if attribution := AudioAttribution(event.AudioAttribution); attribution != "" &&
			attribution != AttributionUnattributed {
			return fmt.Errorf("microphone event carries attribution %q; microphone audio has no "+
				"identifiable source", event.AudioAttribution)
		}
		if len(sourceApps) > 0 {
			return errors.New("microphone event names a source application; microphone audio has " +
				"no identifiable source")
		}
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO events(kind, captured_at, text, app, window, media_path, duration_ms,
                   text_source, display_id, audio_source, audio_attribution,
                   source_apps_json, stream_offset_ms, metadata_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.Kind, FormatCapturedAt(event.CapturedAt),
		event.Text, event.App, event.Window, event.MediaPath, event.DurationMS,
		event.TextSource, event.DisplayID, event.AudioSource, event.AudioAttribution,
		event.SourceApps, nullableInt(event.StreamOffsetMS), string(event.Metadata))
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
		opts.Limit = DefaultSearchLimit
	}
	if opts.Limit > MaxSearchLimit {
		opts.Limit = MaxSearchLimit
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
		// MatchAny's only caller is `lumi mcp`, which exposes it as the
		// `match: "any"` tool parameter; the weighting is pinned by
		// TestSearchMatchAnyWeightsBodyTextOverAppAndWindow. See CLAUDE.md.
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
		args = append(args, LowerCapturedAtBound(*opts.Since))
	}
	if opts.Until != nil {
		where = append(where, "e.captured_at <= ?")
		args = append(args, UpperCapturedAtBound(*opts.Until))
	}
	query := `SELECT ` + prefixedEventColumns("e.") + `,
` + rank + ` AS rank FROM ` + from
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// e.id closes both orderings because captured_at is not unique: an audio
	// chunk's two tracks share one by construction, and on the live index 1,936
	// of 9,009 rows (21%) share theirs with another row. Without a tiebreaker the
	// order inside a tie group is whatever the query plan happened to produce, so
	// two identical calls can return different subsets of a group the LIMIT cuts
	// through. `lumi mcp` now depends on that not happening: its browse-mode page
	// boundary is the oldest captured_at on the page, handed back as `until`, and
	// a boundary that reshuffles under the cap is one an agent cannot walk.
	// DESC, so it agrees with captured_at DESC and keeps the newest row of a tie
	// group first.
	if match != "" {
		query += " ORDER BY rank, e.captured_at DESC, e.id DESC"
	} else {
		query += " ORDER BY e.captured_at DESC, e.id DESC"
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
		if err := scanEvent(rows, &event, &event.Rank); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search results: %w", err)
	}
	return events, nil
}

// eventColumns is the column list every event query selects, in the order
// scanEvent reads them. Keeping it in one place means a new column is a single
// edit rather than several that fail at runtime if one is missed — and Search
// and queryEvents, which maintained the list separately, are exactly the pair
// that would drift.
const eventColumns = `id, kind, captured_at, text, app, window, media_path, duration_ms,
text_source, display_id, audio_source, audio_attribution, source_apps_json, stream_offset_ms,
metadata_json`

const eventSelect = `SELECT ` + eventColumns + ` FROM events`

// prefixedEventColumns qualifies every column with a table alias. Search joins
// events_fts against events, where text, app, and window are ambiguous without
// one.
func prefixedEventColumns(prefix string) string {
	parts := strings.Split(eventColumns, ",")
	for i, part := range parts {
		parts[i] = prefix + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(...any) error }

// scanEvent reads one row selected with eventColumns. Both query paths go
// through it so the column list and the scan order cannot disagree.
func scanEvent(row rowScanner, event *Event, extra ...any) error {
	var capturedAt, metadata string
	var streamOffset sql.NullInt64
	dest := []any{&event.ID, &event.Kind, &capturedAt, &event.Text, &event.App,
		&event.Window, &event.MediaPath, &event.DurationMS, &event.TextSource, &event.DisplayID,
		&event.AudioSource, &event.AudioAttribution, &event.SourceApps, &streamOffset, &metadata}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, capturedAt)
	if err != nil {
		return fmt.Errorf("parse event timestamp %q: %w", capturedAt, err)
	}
	event.CapturedAt = parsed
	if streamOffset.Valid {
		value := streamOffset.Int64
		event.StreamOffsetMS = &value
	}
	event.Metadata = json.RawMessage(metadata)
	return nil
}

// Expired returns events captured strictly before the cutoff, oldest first.
// A limit of zero or less means no limit.
func (s *Store) Expired(ctx context.Context, before time.Time, limit int) ([]Event, error) {
	query := eventSelect + ` WHERE captured_at < ? ORDER BY captured_at ASC`
	// Strictly-before, so the endpoint itself must be excluded under either
	// rendering: the lower bound is the one that does that.
	args := []any{LowerCapturedAtBound(before)}
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
	return s.queryEvents(ctx, eventSelect+` ORDER BY captured_at ASC`)
}

// HasEvents reports whether the index holds any event at all. It answers the
// question with EXISTS rather than a one-row Search so callers that only need
// the boolean do not materialize an event — a screen row carries a full page of
// OCR text and its metadata blob.
func (s *Store) HasEvents(ctx context.Context) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM events)").Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check for stored events: %w", err)
	}
	return exists != 0, nil
}

// ErrEventNotFound reports that no stored event carries the requested id.
var ErrEventNotFound = errors.New("event not found")

// EventByID returns a single event by id, with its full untruncated text and
// metadata. A missing id is reported as ErrEventNotFound rather than a nil
// event, so `lumi mcp`'s get_event can name the id in a tool error instead of
// returning an empty result an agent would read as "no content".
func (s *Store) EventByID(ctx context.Context, id int64) (*Event, error) {
	events, err := s.queryEvents(ctx, eventSelect+` WHERE id = ?`, id)
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
		if err := scanEvent(rows, &event); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
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

// UpdateMediaPath repoints one event at a file that has replaced its media,
// but only while the row still names the file the caller worked from.
//
// The predicate is a compare-and-swap, not defensiveness. Every reader of
// media_path resolves the path and *then* opens the file — retention and the
// backfill both do — so a writer that moved a row in between would be silently
// clobbered by an unconditional UPDATE. A zero return means someone got there
// first and is not an error; `internal/compress` treats it as "skip this row"
// and deletes the file it had written.
//
// This is the only statement in this package that rewrites an existing row, and
// it fires the events_au trigger, which deletes and reinserts the row's whole
// FTS entry even though text, app and window did not change. That churn is an
// accepted cost here rather than an unnoticed one — it re-syncs to the same
// values, and it is one of the two reasons `lumi compress` runs VACUUM last.
func (s *Store) UpdateMediaPath(ctx context.Context, id int64, from, to string) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE events SET media_path = ? WHERE id = ? AND media_path = ?`, to, id, from)
	if err != nil {
		return 0, fmt.Errorf("repoint event %d at %s: %w", id, to, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count repointed events: %w", err)
	}
	return updated, nil
}

// ErrVacuumBusy reports that VACUUM could not take the exclusive lock it needs.
//
// It is a skipped step rather than a failed run: whatever ran before it has
// already committed, and reclaiming free pages is maintenance that the next
// invocation can do just as well.
var ErrVacuumBusy = errors.New("vacuum could not acquire an exclusive lock")

// sqliteBusy is SQLITE_BUSY. Only the primary result code is compared, because
// SQLite reports extended codes in the high bits (SQLITE_BUSY_SNAPSHOT is 261)
// that an equality test would miss.
//
// SQLITE_LOCKED (6) is deliberately not treated as busy: it reports contention
// within this connection or cache rather than another process holding the file,
// so reporting it as "the database is in use" would name the wrong cause.
const sqliteBusy = 5

// Vacuum rebuilds the database file, returning free pages to the filesystem.
//
// SQLite never releases them on its own, and auto_vacuum cannot be enabled on an
// existing database without a full rebuild anyway, so this is the only way an
// index that has been pruned gets smaller. It needs up to twice the file size in
// scratch space, cannot run inside a transaction, and needs an exclusive lock —
// all of which are the caller's to surface rather than this function's to
// prevent. Open already sets busy_timeout, so a contended vacuum waits before
// reporting ErrVacuumBusy.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteBusy {
			return fmt.Errorf("%w: %v", ErrVacuumBusy, err)
		}
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}
