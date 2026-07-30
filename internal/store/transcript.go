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
	// FailedChunks counts how many of the unattributed chunks hold no transcript
	// because recognition failed. They are a subset of Chunks-AttributedChunks
	// that no backfill can ever move, so a caller that reports the gap without
	// them ends up recommending a command that cannot change the number.
	FailedChunks int64 `json:"failed_chunks,omitempty"`
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
	// requested Until unless the transcript stopped short, either because the
	// window held more segments than one call reads or because the turn cap
	// dropped the tail.
	CoveredUntil time.Time `json:"covered_until"`
	// ResumeFrom is what a follow-up request should pass as Since; it is zero
	// when the transcript is complete.
	//
	// It is a separate field from CoveredUntil because the two need opposite
	// inclusivity and one value cannot be both. Coverage is measured over
	// [Since, CoveredUntil] and the segment read is inclusive at both ends, so a
	// caller resuming at CoveredUntil re-reads that whole chunk and sees its
	// turns a second time. ResumeFrom is the first chunk the transcript did not
	// cover — except where a single chunk is itself too large to return whole, or
	// where the turn cap fell inside one, in which case it names that chunk and
	// the overlap is unavoidable rather than accidental.
	ResumeFrom time.Time `json:"resume_from,omitempty"`
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
	segments, truncated, nextChunk := trimToWholeChunks(segments)
	coveredUntil, resumeFrom := opts.Until, time.Time{}
	if truncated && len(segments) > 0 {
		coveredUntil, resumeFrom = segments[len(segments)-1].CapturedAt, nextChunk
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

	result := TranscriptResult{Turns: make([]TranscriptTurn, 0, len(turns)), Truncated: truncated}
	// The turns behind the rows, kept in step with them: the cap is applied to
	// the filtered list, and the boundary it leaves can only be read off the
	// assembled turn, which knows the chunk it ended in.
	kept := make([]transcript.Turn, 0, len(turns))
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
		kept = append(kept, turn)
	}
	if len(result.Turns) > limit {
		result.Turns, result.Capped = result.Turns[:limit], true
		// The cap stops the transcript earlier than truncation did, so both the
		// coverage bound and the resume point move back to it. Leaving them where
		// truncation put them would count chunks past the last returned turn and
		// send a follow-up request beyond the turns that were dropped — the cap
		// would silently delete them instead of paginating them.
		coveredUntil = kept[limit-1].LastCapturedAt
		// The first dropped turn's own chunk, which may be the chunk the last kept
		// turn ended in: a chunk holding turns on both sides of the cap has to be
		// re-read, since the alternative is skipping its later turns.
		resumeFrom = kept[limit].CapturedAt
	}
	result.CoveredUntil, result.ResumeFrom = coveredUntil, resumeFrom

	// Coverage is measured over what the turns reach, not over what was asked
	// for; see TranscriptResult.Chunks.
	chunks, attributed, err := s.SegmentCoverage(ctx, opts.Since, coveredUntil)
	if err != nil {
		return TranscriptResult{}, err
	}
	result.Chunks, result.AttributedChunks = chunks, attributed
	if chunks > attributed {
		// Only asked when there is a gap to explain, so a complete transcript
		// costs nothing.
		failed, err := s.ChunksFailedTranscription(ctx, opts.Since, coveredUntil)
		if err != nil {
			return TranscriptResult{}, err
		}
		result.FailedChunks = failed
	}
	return result, nil
}

// trimToWholeChunks drops the tail of an over-fetched segment read so a
// transcript never ends inside a chunk. It reports whether anything was cut and
// the capture time of the first chunk left out.
//
// The last chunk of an over-long read is the one chunk we know we hold only part
// of, and half a chunk is worse than none of it: its final turn would end
// mid-sentence with nothing in the output saying so, which is precisely the
// silent-incompleteness this function exists to prevent. Dropping it whole is
// what makes the omitted boundary exact, so a follow-up request resumes at a
// chunk rather than somewhere inside one.
func trimToWholeChunks(segments []Segment) (kept []Segment, truncated bool, nextChunk time.Time) {
	if len(segments) <= maxTranscriptSegments {
		return segments, false, time.Time{}
	}
	last := segments[len(segments)-1].CapturedAt
	cut := len(segments)
	for cut > 0 && segments[cut-1].CapturedAt.Equal(last) {
		cut--
	}
	if cut == 0 {
		// One chunk alone exceeds the ceiling. Keeping its prefix beats returning
		// nothing, and Truncated still says the transcript is short. There is no
		// later chunk to resume from, so the resume point is this chunk itself and
		// the repeat is the price of reading the rest of it.
		return segments[:maxTranscriptSegments], true, last
	}
	// Everything sharing `last` was dropped, so `last` is exactly the first chunk
	// this transcript does not cover.
	return segments[:cut], true, last
}
