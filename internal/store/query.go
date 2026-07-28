package store

import (
	"strings"
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
	// It has no caller until `lumi mcp` exposes it as a tool parameter; it is
	// retained deliberately rather than left over. See CLAUDE.md.
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

func hasAlphanumeric(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
