package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// MatchMode selects how the terms of a search query are combined.
//
// MatchAll is the zero value, so any caller that does not set it — notably
// `lumi search` — keeps the original conjunctive behavior.
type MatchMode int

const (
	// MatchAll requires every term to appear in an event.
	MatchAll MatchMode = iota
	// MatchAny requires only one term, letting bm25 rank events by how many
	// (and how rare) the terms they matched are. This is what makes
	// natural-language questions retrievable; see ftsExpression.
	//
	// Its only caller is `lumi mcp`, which exposes it as the `match: "any"`
	// tool parameter; `lumi search` stays conjunctive. See CLAUDE.md.
	MatchAny
)

// ftsQuery quotes every term and joins them with AND.
func ftsQuery(input string) string { return joinTerms(input, " AND ") }

// ftsQueryAny quotes every term and joins them with OR.
func ftsQueryAny(input string) string { return joinTerms(input, " OR ") }

// ftsExpression builds the MATCH expression for input under mode. It returns
// the empty string when input contains no usable terms, which callers must
// treat as "do not run an FTS query at all" — an empty or token-free
// expression is not valid FTS5.
func ftsExpression(input string, mode MatchMode) string {
	if mode == MatchAny {
		return ftsQueryAny(input)
	}
	return ftsQuery(input)
}

// joinTerms is the single place where user text is escaped into FTS5 syntax.
// Each whitespace-separated term is wrapped in double quotes (and any embedded
// quote doubled), which makes it a literal phrase rather than something the
// FTS5 parser can interpret as an operator.
//
// Terms containing no letters or digits are dropped: FTS5 tokenizes them to
// nothing, so a query built only from them would be a syntax error rather than
// a zero-result search.
func joinTerms(input, operator string) string {
	fields := strings.Fields(input)
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		if !hasAlphanumeric(field) {
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(field, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, operator)
}

// HasSearchableTerms reports whether query still yields a MATCH expression once
// FTS5's drop rule is applied. It is derived from ftsExpression rather than
// restating the rule, so a caller that must distinguish "no query" from "a query
// that tokenizes to nothing" cannot drift out of step with joinTerms.
//
// Its caller is `lumi mcp`: when a query survives TrimSpace but empties the
// expression, Search runs no MATCH clause at all and returns the most recent
// events, which an agent cannot tell apart from real hits.
func HasSearchableTerms(query string) bool {
	return ftsExpression(query, MatchAll) != ""
}

func hasAlphanumeric(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// Attribution is one row of the app/window inventory ListAttribution returns.
// In app mode Window is empty; in window mode App carries the requested app.
type Attribution struct {
	App      string    `json:"app"`
	Window   string    `json:"window,omitempty"`
	Events   int64     `json:"events"`
	LastSeen time.Time `json:"last_seen"`
}

// AttributionOptions selects what ListAttribution groups by and over what span.
type AttributionOptions struct {
	// App selects the mode. Nil groups every event by application. Non-nil
	// groups the events of exactly that application (matched case-insensitively)
	// by window title — including a pointer to the empty string, which lists the
	// windows of events that carry no app attribution.
	App   *string
	Since *time.Time
	Until *time.Time
	// Kind restricts the rows considered. The zero value spans every kind.
	// Audio chunks are legitimately unattributed, so any caller reasoning about
	// how much attribution is *missing* must narrow to KindScreen or the ratio
	// is meaningless.
	Kind Kind
	// Limit caps the returned rows. Zero or less means no limit, matching
	// Expired's convention; callers that face an agent set their own ceiling.
	Limit int
}

// AttributionHealthReport answers "is screen capture still recording which
// application the user was in". Unattributed counts screen events whose app is
// empty; LastAttributed is the newest screen event that did carry an app.
type AttributionHealthReport struct {
	Total          int64
	Unattributed   int64
	LastAttributed time.Time
	// HasLastAttributed is false when no screen event has ever been attributed,
	// which is different from "the last one was long ago".
	HasLastAttributed bool
}

// AttributionHealth measures observed attribution over a window, which is what
// diagnoses a live capture problem: TCC can report Accessibility as granted from
// a fresh process while a long-running recorder has been failing for a day.
//
// Only screen events are counted — audio chunks carry no app by design and would
// otherwise drag the ratio toward "broken" on a healthy index.
//
// LastAttributed is deliberately unbounded by `since`. Scoping it to the window
// would make it unknown exactly when the outage is longer than the window, which
// is the case this report exists to explain.
func (s *Store) AttributionHealth(ctx context.Context, since time.Time) (AttributionHealthReport, error) {
	const query = `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN app = '' THEN 1 ELSE 0 END), 0),
       (SELECT MAX(captured_at) FROM events WHERE kind = ? AND app != '')
FROM events WHERE kind = ? AND captured_at >= ?`
	var report AttributionHealthReport
	var lastAttributed sql.NullString
	// COUNT is 0 over an empty set but SUM and MAX are NULL, so both need a
	// destination that tolerates it.
	err := s.db.QueryRowContext(ctx, query, string(KindScreen), string(KindScreen),
		since.UTC().Format(time.RFC3339Nano)).
		Scan(&report.Total, &report.Unattributed, &lastAttributed)
	if err != nil {
		return AttributionHealthReport{}, fmt.Errorf("read attribution health: %w", err)
	}
	if lastAttributed.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, lastAttributed.String)
		if err != nil {
			return AttributionHealthReport{}, fmt.Errorf(
				"parse attribution timestamp %q: %w", lastAttributed.String, err)
		}
		report.LastAttributed, report.HasLastAttributed = parsed, true
	}
	return report, nil
}

// ListAttribution reports which applications (or, within one application, which
// windows) the index actually holds, most events first. It exists because an
// agent that cannot discover the real values of `app` will guess them and
// filter everything away.
//
// Rows with an empty app are grouped under an explicit empty entry rather than
// dropped: audio chunks and screens Accessibility could not attribute are real
// captured activity, and a gap in attribution is itself information.
func (s *Store) ListAttribution(ctx context.Context, opts AttributionOptions) ([]Attribution, error) {
	group := "app"
	where := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if opts.App != nil {
		group = "window"
		where = append(where, "app = ? COLLATE NOCASE")
		args = append(args, *opts.App)
	}
	if opts.Since != nil {
		where = append(where, "captured_at >= ?")
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	if opts.Until != nil {
		where = append(where, "captured_at <= ?")
		args = append(args, opts.Until.UTC().Format(time.RFC3339Nano))
	}
	if opts.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, string(opts.Kind))
	}
	query := "SELECT " + group + " AS label, COUNT(*) AS events, MAX(captured_at) AS last_seen FROM events"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " GROUP BY " + group + " ORDER BY events DESC, " + group + " ASC"
	if opts.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list attribution: %w", err)
	}
	defer rows.Close()
	results := make([]Attribution, 0)
	for rows.Next() {
		var label, lastSeen string
		var count int64
		if err := rows.Scan(&label, &count, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan attribution row: %w", err)
		}
		row := Attribution{Events: count}
		if opts.App != nil {
			row.App, row.Window = *opts.App, label
		} else {
			row.App = label
		}
		row.LastSeen, err = time.Parse(time.RFC3339Nano, lastSeen)
		if err != nil {
			return nil, fmt.Errorf("parse attribution timestamp %q: %w", lastSeen, err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attribution rows: %w", err)
	}
	return results, nil
}
