package transcript

import (
	"strings"
	"time"
)

// TurnSegment is a stored segment as turn assembly needs to see it.
//
// It exists so this package can stay free of any storage dependency while still
// carrying the identifiers a reader needs to get back to the underlying rows.
// The store converts into it.
type TurnSegment struct {
	ID      int64
	EventID int64
	// CapturedAt is the chunk key. Turn assembly needs it to tell "the next
	// phrase in this chunk" from "the first phrase of the next chunk", which are
	// governed by different rules.
	CapturedAt time.Time
	Seq        int
	Origin     Origin
	Text       string
	// StartedAt and EndedAt are zero when the text-only path produced them.
	StartedAt, EndedAt time.Time
	Confidence         float64
	OrderConfidence    OrderConfidence
}

// Turn is one continuous stretch of speech from one origin.
type Turn struct {
	Origin Origin
	Text   string
	// CapturedAt is the chunk the turn began in.
	CapturedAt time.Time
	// LastCapturedAt is the chunk it ended in, which is a different chunk
	// whenever a turn continued across a boundary. A caller bounding a page of
	// turns needs how far they reached, and CapturedAt answers where they
	// started — using it as the bound would exclude chunks the text already
	// covers.
	LastCapturedAt time.Time
	// StartedAt and EndedAt are zero when no constituent segment carried times.
	StartedAt, EndedAt time.Time
	// Confidence is the least confident constituent's, and OrderConfidence the
	// weakest. A turn is only as trustworthy as its worst part, so both
	// aggregate pessimistically rather than averaging — an average would let one
	// confident segment launder a doubtful one.
	Confidence      float64
	OrderConfidence OrderConfidence
	// Overlaps marks a turn that coincides in time with the one before it, which
	// happens when both parties spoke at once. Such turns are emitted as they
	// are rather than serialized into a false sequence.
	Overlaps   bool
	SegmentIDs []int64
	EventIDs   []int64
}

// Turn assembly defaults.
const (
	// DefaultMaxTurnGap is how long a silence inside one chunk may be before it
	// separates two turns by the same speaker.
	DefaultMaxTurnGap = 2 * time.Second
	// DefaultMaxChunkGap is how far apart two chunks may be captured and still
	// count as consecutive.
	//
	// Continuation across a chunk boundary is decided *structurally*, on whether
	// the chunks are adjacent, and not on how much silence sits between them.
	// The capture stream now stays open across a boundary, so consecutive chunks
	// abut and there is no dead air to measure at all; in the index recorded
	// before that fix, the recorder stopped the stream, finalized both files,
	// and transcribed before starting again, and the resulting hole measured 0.4
	// to 2.9 seconds — straddling the length of a natural pause. Either way,
	// silence at a boundary carries no information.
	//
	// What does carry information is adjacency: of 3,040 consecutive audio
	// chunks in a real index, 3,028 were captured 30-33 s apart, and the next
	// largest gaps were 39 s, 48 s, 91 s, and then overnight stops. A
	// continuously open stream lands at the bottom of that band rather than
	// scattered through it — chunks now sit exactly one chunk duration apart —
	// so 35 s still sits in the empty space above both, and separates "the next
	// chunk" from "the recorder was restarted". A turn must never be stitched
	// across a restart.
	DefaultMaxChunkGap = 35 * time.Second
)

// TurnOptions tunes assembly. Zero fields take the defaults above.
type TurnOptions struct {
	MaxTurnGap    time.Duration
	MaxChunkGap   time.Duration
	JoinSeparator string
}

func (o TurnOptions) withDefaults() TurnOptions {
	if o.MaxTurnGap == 0 {
		o.MaxTurnGap = DefaultMaxTurnGap
	}
	if o.MaxChunkGap == 0 {
		o.MaxChunkGap = DefaultMaxChunkGap
	}
	if o.JoinSeparator == "" {
		o.JoinSeparator = " "
	}
	return o
}

// AssembleTurns merges consecutive segments into turns.
//
// Segments must already be in reading order — (captured_at, seq) — which is what
// the store returns. Bleed segments must already be excluded; a turn is what a
// reader should see, and the whole point of marking bleed is to keep the
// machine's words from appearing twice.
//
// Text is joined with a space *between* segments, never with the empty string.
// That is the opposite of how a segment's own text was built from recognizer
// runs, and both rules matter: runs carry their own leading whitespace, whereas
// segments were split at sentence and phrase boundaries that consumed it.
func AssembleTurns(segments []TurnSegment, opts TurnOptions) []Turn {
	opts = opts.withDefaults()
	var turns []Turn
	var parts []string
	var current *Turn
	var previous TurnSegment

	flush := func() {
		if current == nil {
			return
		}
		current.Text = strings.Join(parts, opts.JoinSeparator)
		turns = append(turns, *current)
		current, parts = nil, nil
	}

	for _, segment := range segments {
		if strings.TrimSpace(segment.Text) == "" {
			continue
		}
		if current != nil && !canMerge(previous, segment, opts) {
			flush()
		}
		if current == nil {
			current = &Turn{
				Origin: segment.Origin, CapturedAt: segment.CapturedAt,
				LastCapturedAt: segment.CapturedAt,
				StartedAt:      segment.StartedAt, EndedAt: segment.EndedAt,
				Confidence: segment.Confidence, OrderConfidence: segment.OrderConfidence,
			}
			parts = nil
		} else {
			current.LastCapturedAt = segment.CapturedAt
			if current.StartedAt.IsZero() && !segment.StartedAt.IsZero() {
				current.StartedAt = segment.StartedAt
			}
			if !segment.EndedAt.IsZero() && segment.EndedAt.After(current.EndedAt) {
				current.EndedAt = segment.EndedAt
			}
			if segment.Confidence < current.Confidence {
				current.Confidence = segment.Confidence
			}
			current.OrderConfidence = weakestOrder(current.OrderConfidence, segment.OrderConfidence)
		}
		parts = append(parts, strings.TrimSpace(segment.Text))
		if segment.ID != 0 {
			current.SegmentIDs = append(current.SegmentIDs, segment.ID)
		}
		current.EventIDs = appendUnique(current.EventIDs, segment.EventID)
		previous = segment
	}
	flush()

	markOverlaps(turns)
	return turns
}

// canMerge decides whether next continues the turn that prev belongs to.
func canMerge(prev, next TurnSegment, opts TurnOptions) bool {
	if prev.Origin != next.Origin {
		return false
	}
	// An undetermined origin never joins anything. Merging it into a confident
	// turn would launder the doubt away, and merging two of them would assert
	// that unrelated uncertain speech came from the same source.
	if prev.Origin == OriginUnknown || next.Origin == OriginUnknown {
		return false
	}
	// Interpolated speech does not join anchored speech, for the same reason.
	// Machine audio the microphone never captured has no position of its own and
	// was placed beside its nearest neighbour; folding it into a well-anchored
	// turn would put text at a guessed point inside a passage the reader is being
	// told is sequenced. Keeping it as its own turn makes the guess visible and
	// skippable, and leaves the anchored turn's own tier intact.
	if (prev.OrderConfidence == OrderApproximate) != (next.OrderConfidence == OrderApproximate) {
		return false
	}

	if next.CapturedAt.Equal(prev.CapturedAt) {
		// Within one chunk, a measurable silence is the only thing that
		// separates turns. Without times, consecutive same-origin segments of
		// one 30-second window are treated as continuous — they were split at
		// sentence boundaries, not at speaker changes.
		if prev.EndedAt.IsZero() || next.StartedAt.IsZero() {
			return true
		}
		return next.StartedAt.Sub(prev.EndedAt) <= opts.MaxTurnGap
	}

	// Across chunks, adjacency is the whole test; see DefaultMaxChunkGap.
	gap := next.CapturedAt.Sub(prev.CapturedAt)
	return gap > 0 && gap <= opts.MaxChunkGap
}

// markOverlaps flags turns that coincide in time with the previous one, which is
// what genuine simultaneous speech produces. They are reported rather than
// reordered: forcing them into a sequence would assert an order that did not
// happen.
func markOverlaps(turns []Turn) {
	for i := 1; i < len(turns); i++ {
		current, previous := &turns[i], turns[i-1]
		if current.StartedAt.IsZero() || previous.EndedAt.IsZero() {
			continue
		}
		if current.StartedAt.Before(previous.EndedAt) {
			current.Overlaps = true
		}
	}
}

// orderRank ranks the tiers from most to least trustworthy.
func orderRank(value OrderConfidence) int {
	switch value {
	case OrderExact:
		return 2
	case OrderSequence:
		return 1
	default:
		return 0
	}
}

func weakestOrder(a, b OrderConfidence) OrderConfidence {
	if orderRank(b) < orderRank(a) {
		return b
	}
	return a
}

func appendUnique(values []int64, value int64) []int64 {
	if value == 0 {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
