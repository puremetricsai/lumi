package store

import (
	"context"
	"time"

	"github.com/puremetricsai/lumi/internal/transcript"
)

// Transcript limits.
const (
	DefaultTranscriptTurns = 100
	MaxTranscriptTurns     = 1000
	// maxTranscriptSegments bounds the rows read before assembly. A turn limit
	// cannot be pushed into SQL, because one turn merges an unbounded number of
	// segments and the count is only known after merging — so the fetch needs its
	// own ceiling to keep a wide window from reading the whole table.
	maxTranscriptSegments = 20000
)

// TranscriptOptions selects what a transcript covers.
type TranscriptOptions struct {
	Since, Until time.Time
	// Origin restricts to "internal", "external", or "unknown". Empty returns
	// every origin.
	Origin string
	// MinConfidence drops turns below a threshold. Zero returns everything,
	// which is the right default: filtering by default would hide turns an agent
	// has no way of knowing were omitted.
	MinConfidence float64
	MaxTurns      int
	// IncludeBleed keeps the microphone's re-recording of machine audio, which is
	// otherwise excluded so a phrase appears once. For debugging only.
	IncludeBleed bool
}

// TranscriptTurn is one turn as stored data, ready to export.
type TranscriptTurn struct {
	Origin string `json:"origin"`
	Text   string `json:"text"`
	// StartedAt and EndedAt are nil for turns assembled from the text-only
	// attribution path, which recovers order but not absolute time.
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	CapturedAt time.Time  `json:"captured_at"`
	// Confidence and OrderConfidence carry no omitempty. A turn's trustworthiness
	// must never be something a reader has to infer from a missing key.
	Confidence      float64 `json:"confidence"`
	OrderConfidence string  `json:"order_confidence"`
	Overlaps        bool    `json:"overlaps,omitempty"`
	EventIDs        []int64 `json:"event_ids"`
}

// TranscriptResult is a transcript plus what it could not tell you.
type TranscriptResult struct {
	Turns []TranscriptTurn `json:"turns"`
	// Chunks and AttributedChunks describe coverage over the range the turns
	// actually reach — which is the requested window unless Truncated cut it
	// short. They are what lets a caller say a transcript has holes instead of
	// serving a partial one that looks complete, so they must never describe more
	// ground than the turns do: counting the whole window while the text stopped
	// two hours in would corroborate exactly the illusion they exist to prevent.
	Chunks           int64 `json:"chunks"`
	AttributedChunks int64 `json:"attributed_chunks"`
	// Capped reports that turns were dropped from the tail to satisfy MaxTurns.
	Capped bool `json:"capped,omitempty"`
	// Truncated reports that the window held more segments than one call reads,
	// so the transcript stops before the end of the requested range.
	//
	// It is a different fact from Capped and both can be true. Capped drops whole
	// turns after assembly; Truncated drops segments before it, and is therefore
	// the more dangerous of the two — turns dropped from a capped page are
	// missing, whereas segments dropped before assembly could otherwise cut a
	// turn off mid-sentence with nothing marking the wound.
	Truncated bool `json:"truncated,omitempty"`
	// CoveredUntil is the last capture time the turns reach. It equals the
	// requested Until unless Truncated, where it is where a follow-up request
	// should resume from.
	CoveredUntil time.Time `json:"covered_until"`
}

// Order confidence tiers, re-exported from internal/transcript so a caller can
// compare against a name rather than retyping the string. A CLI printing "~" for
// a guessed position was matching a literal, which is the drift HasSearchableTerms
// exists to prevent, one package over.
const (
	OrderExact       = string(transcript.OrderExact)
	OrderSequence    = string(transcript.OrderSequence)
	OrderApproximate = string(transcript.OrderApproximate)
)

// ClampTranscriptTurns applies the turn limit. It is exported so a caller can
// document the number that will actually be enforced rather than restating a
// limit that could drift — the same reason HasSearchableTerms is exported.
func ClampTranscriptTurns(limit int) int {
	switch {
	case limit <= 0:
		return DefaultTranscriptTurns
	case limit > MaxTranscriptTurns:
		return MaxTranscriptTurns
	default:
		return limit
	}
}

// Transcript assembles one ordered, attributed transcript for a time range.
//
// Assembly itself is pure and lives in internal/transcript; this is the only way
// callers reach it, so the rules about what a transcript contains — which
// segments count, in what order, and how many turns come back — stay in the
// store rather than being restated by every command and tool.
func (s *Store) Transcript(ctx context.Context, opts TranscriptOptions) (TranscriptResult, error) {
	limit := ClampTranscriptTurns(opts.MaxTurns)

	// One row past the ceiling, so "the window held more than we read" is
	// observed rather than inferred from a full page — a page that is exactly
	// full is otherwise indistinguishable from one that stopped at the end of
	// the range.
	segments, err := s.SegmentsBetween(ctx, opts.Since, opts.Until, opts.IncludeBleed, maxTranscriptSegments+1)
	if err != nil {
		return TranscriptResult{}, err
	}
	segments, truncated := trimToWholeChunks(segments)
	coveredUntil := opts.Until
	if truncated && len(segments) > 0 {
		coveredUntil = segments[len(segments)-1].CapturedAt
	}

	assembled := make([]transcript.TurnSegment, 0, len(segments))
	for _, segment := range segments {
		item := transcript.TurnSegment{
			ID: segment.ID, EventID: segment.EventID, CapturedAt: segment.CapturedAt,
			Seq: segment.Seq, Origin: transcript.Origin(segment.Origin),
			Text: segment.Text, Confidence: segment.Confidence,
			OrderConfidence: transcript.OrderConfidence(segment.OrderConfidence),
		}
		if segment.StartedAt != nil {
			item.StartedAt = *segment.StartedAt
		}
		if segment.EndedAt != nil {
			item.EndedAt = *segment.EndedAt
		}
		assembled = append(assembled, item)
	}

	// Assembly sees every origin, and the filter is applied to the turns it
	// produced. Filtering segments first would hand AssembleTurns a sequence in
	// which two utterances separated by the other party read as adjacent, and it
	// would merge them into one turn — inventing continuity that never happened
	// and silently deleting the boundary between them. A turn is single-origin by
	// construction, so filtering afterwards selects turns without reshaping them.
	turns := transcript.AssembleTurns(assembled, transcript.TurnOptions{})

	result := TranscriptResult{
		Turns: make([]TranscriptTurn, 0, len(turns)), Truncated: truncated, CoveredUntil: coveredUntil,
	}
	for _, turn := range turns {
		if opts.Origin != "" && string(turn.Origin) != opts.Origin {
			continue
		}
		if turn.Confidence < opts.MinConfidence {
			continue
		}
		row := TranscriptTurn{
			Origin: string(turn.Origin), Text: turn.Text, CapturedAt: turn.CapturedAt,
			Confidence: turn.Confidence, OrderConfidence: string(turn.OrderConfidence),
			Overlaps: turn.Overlaps, EventIDs: turn.EventIDs,
		}
		if row.EventIDs == nil {
			row.EventIDs = []int64{}
		}
		if !turn.StartedAt.IsZero() {
			started := turn.StartedAt
			row.StartedAt = &started
		}
		if !turn.EndedAt.IsZero() {
			ended := turn.EndedAt
			row.EndedAt = &ended
		}
		result.Turns = append(result.Turns, row)
	}
	if len(result.Turns) > limit {
		result.Turns = result.Turns[:limit]
		result.Capped = true
	}

	// Coverage is measured over what the turns reach, not over what was asked
	// for; see TranscriptResult.Chunks.
	chunks, attributed, err := s.SegmentCoverage(ctx, opts.Since, coveredUntil)
	if err != nil {
		return TranscriptResult{}, err
	}
	result.Chunks, result.AttributedChunks = chunks, attributed
	return result, nil
}

// trimToWholeChunks drops the tail of an over-fetched segment read so a
// transcript never ends inside a chunk, and reports whether anything was cut.
//
// The last chunk of an over-long read is the one chunk we know we hold only part
// of, and half a chunk is worse than none of it: its final turn would end
// mid-sentence with nothing in the output saying so, which is precisely the
// silent-incompleteness this function exists to prevent. Dropping it whole makes
// CoveredUntil a boundary a follow-up request can resume from exactly.
func trimToWholeChunks(segments []Segment) ([]Segment, bool) {
	if len(segments) <= maxTranscriptSegments {
		return segments, false
	}
	last := segments[len(segments)-1].CapturedAt
	cut := len(segments)
	for cut > 0 && segments[cut-1].CapturedAt.Equal(last) {
		cut--
	}
	if cut == 0 {
		// One chunk alone exceeds the ceiling. Keeping its prefix beats returning
		// nothing, and Truncated still says the transcript is short.
		return segments[:maxTranscriptSegments], true
	}
	return segments[:cut], true
}
