package cli

import (
	"context"
	"strings"
	"unicode"

	"github.com/puremetricsai/lumi/internal/store"
)

// retrievalStage records which pass produced the events handed to the model,
// so `ask` can say out loud when it fell back rather than answering from
// recent activity as if it had retrieved something.
type retrievalStage string

const (
	stageAllTerms retrievalStage = "all-terms"
	stageAnyTerm  retrievalStage = "any-term"
	// stageRecent is a deliberate broad-question fallback: nothing in the
	// question was useful as an FTS term.
	stageRecent retrievalStage = "recent"
	// stageRecentUnmatched is materially different from stageRecent. The
	// question did contain useful terms, but none occurred in the selected
	// activity window. Keeping the reasons separate prevents ask from telling
	// users that a real query had "no searchable terms".
	stageRecentUnmatched retrievalStage = "recent-unmatched"
)

const (
	// minTermRunes is 2, not 3: dropping two-rune tokens would lose db, go,
	// ai, ui and similar.
	minTermRunes = 2
	maxTerms     = 12
)

// questionStopwords are words that carry no retrieval signal in a question.
//
// Temporal words are included deliberately: the time window is already
// expressed by --since, and leaving "yesterday" in the term list makes the
// all-terms pass fail on almost every naturally phrased question. Mapping time
// expressions onto --since properly is a worthwhile follow-up.
//
// "go" is deliberately absent: on a developer's machine it is far more often
// the language than the verb.
var questionStopwords = map[string]struct{}{}

func init() {
	words := strings.Fields(`
		a an the this that these those
		i me my mine we us our ours you your yours it its
		what when where who whom whose why how which
		is am are was were be been being do does did done doing
		have has had having will would shall should can could may might must
		get got getting
		of in on at to for from with about into over after before during
		by as and or but not no nor if then than so such
		tell show remind find search look give list
		anything something everything some any all
		record records recorded recording save saved capture captured
		activity activities logged indexed remembered far
		yesterday today tonight tomorrow morning afternoon evening
		day days week weeks month months year years
		ago last recent recently earlier ago past now
		time times
	`)
	for _, word := range words {
		questionStopwords[word] = struct{}{}
	}
}

// questionTerms reduces a natural-language question to the terms worth
// searching for: lowercased, split on non-alphanumeric runes, stopwords and
// very short tokens removed, deduplicated in order, capped at maxTerms.
//
// It returns nil when nothing searchable remains ("what was I doing?"), which
// callers must treat as a signal to fall back to recency rather than as an
// empty query.
func questionTerms(question string) []string {
	fields := strings.FieldsFunc(strings.ToLower(question), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var terms []string
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if len([]rune(field)) < minTermRunes {
			continue
		}
		if _, stop := questionStopwords[field]; stop {
			continue
		}
		if _, dup := seen[field]; dup {
			continue
		}
		seen[field] = struct{}{}
		terms = append(terms, field)
		if len(terms) == maxTerms {
			break
		}
	}
	return terms
}

// retrieveContext runs the staged retrieval used by `ask` and reports which
// stage produced the returned events.
//
// The all-terms pass runs first not because it ranks better than the any-term
// pass — bm25 over a disjunction already ranks "matched more and rarer terms"
// first — but because when every term matches, those events are near-certainly
// on topic and deserve the whole context budget.
//
// opts is taken by value; its kind and time predicates apply to every stage.
func retrieveContext(ctx context.Context, s *store.Store, question string, opts store.SearchOptions) ([]store.Event, retrievalStage, error) {
	if terms := questionTerms(question); len(terms) > 0 {
		opts.Query = strings.Join(terms, " ")
		for _, stage := range []struct {
			mode store.MatchMode
			name retrievalStage
		}{
			{store.MatchAll, stageAllTerms},
			{store.MatchAny, stageAnyTerm},
		} {
			opts.Match = stage.mode
			events, err := s.Search(ctx, opts)
			if err != nil {
				return nil, stage.name, err
			}
			if len(events) > 0 {
				return events, stage.name, nil
			}
		}
		opts.Query = ""
		opts.Match = store.MatchAll
		events, err := s.Search(ctx, opts)
		if err != nil {
			return nil, stageRecentUnmatched, err
		}
		return events, stageRecentUnmatched, nil
	}
	opts.Query = ""
	opts.Match = store.MatchAll
	events, err := s.Search(ctx, opts)
	if err != nil {
		return nil, stageRecent, err
	}
	return events, stageRecent, nil
}
