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
// The terms themselves come from SearchTerms, so the drop rule is stated once.
func joinTerms(input, operator string) string {
	terms := SearchTerms(input)
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, operator)
}

// SearchTerms splits query into the terms an FTS MATCH will actually be built
// from: whitespace-separated fields, minus the ones FTS5 tokenizes to nothing.
// Terms containing no letters or digits are dropped, because a query built only
// from them would be an FTS5 syntax error rather than a zero-result search.
//
// It is exported for the same reason HasSearchableTerms is. `lumi mcp` centres
// the excerpt it returns on the term that earned a row its place, and to do
// that it has to know which terms those were. Deriving them here rather than
// re-splitting at the boundary is what keeps the two from drifting — the drop
// rule has been copied out of this file once already.
//
// The terms are returned as the user wrote them, not as FTS5 tokenizes them:
// a caller matching them against raw text will miss what unicode61's diacritic
// folding matched, and a term is a quoted phrase that may span several tokens.
// Callers must treat a miss as "no excerpt to centre on", never as "this row
// did not match".
func SearchTerms(query string) []string {
	fields := strings.Fields(query)
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		if !hasAlphanumeric(field) {
			continue
		}
		terms = append(terms, field)
	}
	return terms
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
// Only screen events are counted. Audio chunks carry an app too, but counting
// them would measure a different thing badly: each chunk contributes two rows,
// so a quiet hour of recording would outvote the screen signal, and an audio
// attribution failure would be reported as a screen capture problem. The
// remedy this report exists to recommend is about the screen path.
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
		LowerCapturedAtBound(since)).
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
// dropped: events Accessibility could not attribute — plus audio captured
// before audio rows carried an app at all — are real captured activity, and a
// gap in attribution is itself information.
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
		args = append(args, LowerCapturedAtBound(*opts.Since))
	}
	if opts.Until != nil {
		where = append(where, "captured_at <= ?")
		args = append(args, UpperCapturedAtBound(*opts.Until))
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
