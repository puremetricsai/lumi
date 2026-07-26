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
// Temporal words are dropped deliberately: the window comes from --since (or
// parseTimeWindow upstream), and leaving "yesterday" in the terms makes the
// all-terms pass fail on almost every naturally phrased question. Words
// parseTimeWindow misses are still dropped here so they never reach FTS.
//
// Recording-modality words ("microphone", "conversation", "heard", "said") and
// capture verbs ("captured", "caught", "picked up") are dropped for a subtler
// reason: they name *how* something was recorded, not its content. A speech
// transcript never contains the word "microphone", but a terminal screenshot
// showing a prior `lumi ask` command does — so leaving them in makes the
// all-terms pass answer a question about the mic out of a screenshot of that
// very question. Modality is carried by event kind instead; see
// questionModality in modality.go, which consumes exactly these words.
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
		catch catches caught catching pick picks picking picked grab grabs grabbed
		microphone microphones mic mics audio sound sounds
		conversation conversations discussion discussions
		talk talks talked talking
		heard hear hears hearing overheard listen listens listened listening
		said say says saying spoke spoken speak speaks speaking speech
		transcript transcripts transcribed transcription transcriptions
		voice voices aloud verbally
		thing things stuff up
		activity activities logged indexed remembered far
		yesterday today tonight tomorrow morning afternoon evening night nights
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

// retrieval is what `ask` needs to describe its own answer honestly: the
// events, which stage produced them, and how the corpus was narrowed to get
// there. Every field that changed the result must be reportable, because a
// silently narrowed retrieval is as misleading as a silent recency fallback.
type retrieval struct {
	events []store.Event
	stage  retrievalStage
	// kind is the corpus actually searched, "" when unrestricted. It is the
	// question's derived modality unless the caller set one explicitly.
	kind store.Kind
	// derivedKind reports that kind came from the question's wording rather
	// than from a flag, so `ask` can say which reading it took.
	derivedKind bool
	// widened records that a derived restriction was withdrawn in favor of the
	// full corpus. Modality is inferred from wording, so it is sometimes wrong;
	// answering "nothing was recorded" on a bad inference would be a lie.
	widened bool
}

// retrieveContext runs the staged retrieval used by `ask`.
//
// Retrieval narrows in two dimensions. First the corpus: a question that names
// a recording modality ("what did I say on the mic") is answered from that
// modality's events, because the words that identify it are absent from its
// own transcripts and present in screenshots of Lumi's UI — searching text for
// them finds the opposite of what was asked. Then the terms, in stages.
//
// The all-terms pass runs first not because it ranks better than the any-term
// pass — bm25 over a disjunction already ranks "matched more and rarer terms"
// first — but because when every term matches, those events are near-certainly
// on topic and deserve the whole context budget.
//
// opts is taken by value; its kind and time predicates apply to every stage.
//
// inferModality gates the corpus inference. `ask` passes false whenever the
// user set --type explicitly — including --type all, which reaches here as an
// empty opts.Kind indistinguishable from an omitted flag. Without this gate the
// advertised "pass --type all to search everything" escape hatch is a no-op:
// the inference re-narrows exactly the request the user was overriding.
func retrieveContext(ctx context.Context, s *store.Store, question string, opts store.SearchOptions, inferModality bool) (retrieval, error) {
	if opts.Kind != "" {
		events, stage, err := stagedSearch(ctx, s, question, opts)
		return retrieval{events: events, stage: stage, kind: opts.Kind}, err
	}
	var kind store.Kind
	if inferModality {
		kind = questionModality(question)
	}
	if kind == "" {
		events, stage, err := stagedSearch(ctx, s, question, opts)
		return retrieval{events: events, stage: stage}, err
	}
	restricted := opts
	restricted.Kind = kind
	// A content question about audio is not answered by a chunk that was saved
	// without a transcript, and Lumi stores enough of those that a recency pass
	// returns little else.
	restricted.RequireText = true
	events, stage, err := stagedSearch(ctx, s, question, restricted)
	if err != nil {
		return retrieval{stage: stage, kind: kind, derivedKind: true}, err
	}
	narrowed := retrieval{events: events, stage: stage, kind: kind, derivedKind: true}
	// An inferred modality is only overridden by real term evidence elsewhere;
	// the deciding question is whether the question carried content terms.
	//
	//   matched terms   — the modality reading paid off; keep it.
	//   no usable terms — modality was the question's ONLY signal ("anything on
	//                     the mic today?"). There is nothing to contradict the
	//                     reading, so recency within that corpus is precisely
	//                     the answer — even when it is empty. Widening here would
	//                     answer a mic question from unrelated screenshots.
	//   terms, no match — the reading is suspect. "What did the deploy freeze
	//                     say" trips the audio router on a modality word but
	//                     carries content terms that miss audio; only here does
	//                     a wide term match justify overriding the corpus.
	if stage != stageRecentUnmatched {
		return narrowed, nil
	}
	wide, wideStage, err := stagedSearch(ctx, s, question, opts)
	if err != nil {
		return retrieval{stage: wideStage}, err
	}
	// Only a real term match — not a broader recency pass — outranks the
	// narrowed result. stage is stageRecentUnmatched here, so the question did
	// have terms; if the wide corpus also only reaches recency, there is no
	// evidence the modality reading was wrong, and the focused corpus stands.
	if len(wide) > 0 && (wideStage == stageAllTerms || wideStage == stageAnyTerm) {
		return retrieval{events: wide, stage: wideStage, widened: true}, nil
	}
	return narrowed, nil
}

// stagedSearch is the all-terms → any-term → recency ladder. opts is applied
// unchanged at every stage, so its corpus and time predicates never soften as
// the term match does.
func stagedSearch(ctx context.Context, s *store.Store, question string, opts store.SearchOptions) ([]store.Event, retrievalStage, error) {
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
