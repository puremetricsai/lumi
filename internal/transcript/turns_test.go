package transcript

import (
	"testing"
	"time"
)

var chunkA = time.Date(2026, 7, 29, 20, 58, 58, 0, time.UTC)

// turnSeg builds a timed segment within a chunk.
func turnSeg(chunk time.Time, seq int, origin Origin, text string, startMS, endMS int64) TurnSegment {
	return TurnSegment{
		ID: int64(seq + 1), EventID: 100, CapturedAt: chunk, Seq: seq,
		Origin: origin, Text: text,
		StartedAt:  chunk.Add(time.Duration(startMS) * time.Millisecond),
		EndedAt:    chunk.Add(time.Duration(endMS) * time.Millisecond),
		Confidence: 0.9, OrderConfidence: OrderExact,
	}
}

// untimedSeg is what the text-only path produces: order but no clock.
func untimedSeg(chunk time.Time, seq int, origin Origin, text string) TurnSegment {
	return TurnSegment{
		ID: int64(seq + 1), EventID: 100, CapturedAt: chunk, Seq: seq,
		Origin: origin, Text: text, Confidence: 0.5, OrderConfidence: OrderSequence,
	}
}

func texts(turns []Turn) []string {
	out := make([]string, len(turns))
	for i, turn := range turns {
		out[i] = turn.Text
	}
	return out
}

func TestConsecutiveSegmentsOfOneSpeakerBecomeOneTurn(t *testing.T) {
	turns := AssembleTurns([]TurnSegment{
		turnSeg(chunkA, 0, OriginExternal, "I think we should ship it.", 0, 2000),
		turnSeg(chunkA, 1, OriginExternal, "Maybe on Thursday.", 2200, 4000),
	}, TurnOptions{})

	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1: %v", len(turns), texts(turns))
	}
	if turns[0].Text != "I think we should ship it. Maybe on Thursday." {
		t.Errorf("text = %q", turns[0].Text)
	}
	if len(turns[0].SegmentIDs) != 2 {
		t.Errorf("segment ids = %v, want both", turns[0].SegmentIDs)
	}
}

func TestALongSilenceInsideAChunkSplitsTurns(t *testing.T) {
	turns := AssembleTurns([]TurnSegment{
		turnSeg(chunkA, 0, OriginExternal, "First thought.", 0, 2000),
		turnSeg(chunkA, 1, OriginExternal, "Second thought.", 9000, 11000),
	}, TurnOptions{})
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 across a seven-second silence: %v", len(turns), texts(turns))
	}
}

func TestOriginChangeAlwaysSplits(t *testing.T) {
	turns := AssembleTurns([]TurnSegment{
		turnSeg(chunkA, 0, OriginInternal, "The build failed.", 0, 1000),
		turnSeg(chunkA, 1, OriginExternal, "Let me look.", 1100, 2000),
	}, TurnOptions{})
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 across an origin change", len(turns))
	}
	if turns[0].Origin != OriginInternal || turns[1].Origin != OriginExternal {
		t.Errorf("origins = %s, %s", turns[0].Origin, turns[1].Origin)
	}
}

// TestUnknownNeverMerges guards against laundering doubt. An undetermined origin
// absorbed into a confident turn would make the whole turn read as certain.
func TestUnknownNeverMerges(t *testing.T) {
	turns := AssembleTurns([]TurnSegment{
		turnSeg(chunkA, 0, OriginUnknown, "Something was playing.", 0, 1000),
		turnSeg(chunkA, 1, OriginUnknown, "Something else too.", 1100, 2000),
	}, TurnOptions{})
	if len(turns) != 2 {
		t.Fatalf("got %d turns; two unknown segments must not merge into one", len(turns))
	}
}

// TestTurnContinuesAcrossAdjacentChunks is the requirement that a turn spanning
// consecutive capture windows is reassembled into one.
func TestTurnContinuesAcrossAdjacentChunks(t *testing.T) {
	chunkB := chunkA.Add(31 * time.Second) // the real modal spacing
	turns := AssembleTurns([]TurnSegment{
		turnSeg(chunkA, 0, OriginExternal, "so what I was getting at is", 28000, 30000),
		turnSeg(chunkB, 0, OriginExternal, "that we should wait a week.", 0, 2000),
	}, TurnOptions{})

	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1 across a 31s chunk boundary: %v", len(turns), texts(turns))
	}
	if turns[0].Text != "so what I was getting at is that we should wait a week." {
		t.Errorf("text = %q", turns[0].Text)
	}
}

// TestTurnDoesNotBridgeARecorderRestart is the other half of the structural rule.
// A 91-second gap is a restart, not a pause, and no turn may span one.
func TestTurnDoesNotBridgeARecorderRestart(t *testing.T) {
	for _, gap := range []time.Duration{39 * time.Second, 48 * time.Second, 91 * time.Second, 9 * time.Hour} {
		t.Run(gap.String(), func(t *testing.T) {
			later := chunkA.Add(gap)
			turns := AssembleTurns([]TurnSegment{
				turnSeg(chunkA, 0, OriginExternal, "before the gap", 28000, 30000),
				turnSeg(later, 0, OriginExternal, "after the gap", 0, 2000),
			}, TurnOptions{})
			if len(turns) != 2 {
				t.Errorf("a %v gap produced %d turns, want 2", gap, len(turns))
			}
		})
	}
}

// TestChunkBoundaryIgnoresSilenceLength pins that continuation is decided
// structurally. The recorded silence at a boundary far exceeds MaxTurnGap, so a
// gap-based rule would refuse every cross-chunk turn.
func TestChunkBoundaryIgnoresSilenceLength(t *testing.T) {
	chunkB := chunkA.Add(32 * time.Second)
	// Ends at 30s into chunk A, resumes at 0s into chunk B: two seconds of
	// unobservable dead air, right at the edge of a natural pause.
	turns := AssembleTurns([]TurnSegment{
		turnSeg(chunkA, 0, OriginExternal, "first half", 25000, 30000),
		turnSeg(chunkB, 0, OriginExternal, "second half", 0, 3000),
	}, TurnOptions{})
	if len(turns) != 1 {
		t.Fatalf("got %d turns; the boundary rule must not consult silence length", len(turns))
	}
}

func TestUntimedSegmentsStillFormTurns(t *testing.T) {
	turns := AssembleTurns([]TurnSegment{
		untimedSeg(chunkA, 0, OriginInternal, "Teams usually land on that."),
		untimedSeg(chunkA, 1, OriginExternal, "Yeah, I like that."),
		untimedSeg(chunkA, 2, OriginExternal, "I am open to it."),
	}, TurnOptions{})

	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %v", len(turns), texts(turns))
	}
	if turns[1].Text != "Yeah, I like that. I am open to it." {
		t.Errorf("text = %q", turns[1].Text)
	}
	for _, turn := range turns {
		if !turn.StartedAt.IsZero() {
			t.Error("a turn with no timed segments claims a start time")
		}
	}
}

// TestConfidenceAggregatesPessimistically pins that a turn is only as trustworthy
// as its least trustworthy constituent.
func TestConfidenceAggregatesPessimistically(t *testing.T) {
	high := turnSeg(chunkA, 0, OriginExternal, "confident part", 0, 1000)
	low := turnSeg(chunkA, 1, OriginExternal, "doubtful part", 1100, 2000)
	low.Confidence = 0.3

	turns := AssembleTurns([]TurnSegment{high, low}, TurnOptions{})
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if turns[0].Confidence != 0.3 {
		t.Errorf("confidence = %.2f, want the minimum 0.30", turns[0].Confidence)
	}
}

// TestInterpolatedSpeechStaysItsOwnTurn covers a real problem found by running
// the text path over captured audio. Machine audio the microphone never picked up
// is placed beside its nearest neighbour rather than measured, and folding those
// fragments into anchored turns dragged large, well-positioned passages down to
// "approximate" — while also inserting guessed text mid-passage.
func TestInterpolatedSpeechStaysItsOwnTurn(t *testing.T) {
	anchored := turnSeg(chunkA, 0, OriginInternal, "a long, well anchored passage", 0, 4000)
	guessed := turnSeg(chunkA, 1, OriginInternal, "mm", 4100, 4200)
	guessed.OrderConfidence = OrderApproximate

	turns := AssembleTurns([]TurnSegment{anchored, guessed}, TurnOptions{})
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want the interpolated fragment kept separate", len(turns))
	}
	if turns[0].OrderConfidence != OrderExact {
		t.Errorf("the anchored turn was downgraded to %s", turns[0].OrderConfidence)
	}
	if turns[1].OrderConfidence != OrderApproximate {
		t.Errorf("the interpolated turn reports %s", turns[1].OrderConfidence)
	}
	// Two interpolated fragments in a row are the same kind of claim, so those do
	// still merge.
	second := turnSeg(chunkA, 2, OriginInternal, "hmm", 4300, 4400)
	second.OrderConfidence = OrderApproximate
	turns = AssembleTurns([]TurnSegment{guessed, second}, TurnOptions{})
	if len(turns) != 1 {
		t.Errorf("two interpolated fragments produced %d turns, want 1", len(turns))
	}
}

// TestSimultaneousSpeechProducesOverlappingTurns covers the case a linear
// transcript cannot honestly represent.
func TestSimultaneousSpeechProducesOverlappingTurns(t *testing.T) {
	turns := AssembleTurns([]TurnSegment{
		turnSeg(chunkA, 0, OriginInternal, "and the invoice went out Monday", 0, 4000),
		turnSeg(chunkA, 1, OriginExternal, "sorry, say that again?", 2000, 5000),
	}, TurnOptions{})

	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	if turns[0].Overlaps {
		t.Error("the first turn was marked as overlapping")
	}
	if !turns[1].Overlaps {
		t.Error("the second turn coincides with the first but was not marked")
	}
}

func TestBlankSegmentsAreSkipped(t *testing.T) {
	turns := AssembleTurns([]TurnSegment{
		turnSeg(chunkA, 0, OriginExternal, "real words", 0, 1000),
		turnSeg(chunkA, 1, OriginExternal, "   ", 1100, 1200),
		turnSeg(chunkA, 2, OriginExternal, "more words", 1300, 2000),
	}, TurnOptions{})
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if turns[0].Text != "real words more words" {
		t.Errorf("text = %q; a blank segment should not leave a double space", turns[0].Text)
	}
}

func TestEmptyInputProducesNoTurns(t *testing.T) {
	if turns := AssembleTurns(nil, TurnOptions{}); len(turns) != 0 {
		t.Errorf("got %d turns from no segments", len(turns))
	}
}

func TestEventIDsAreDeduplicatedPerTurn(t *testing.T) {
	first := turnSeg(chunkA, 0, OriginExternal, "one", 0, 500)
	second := turnSeg(chunkA, 1, OriginExternal, "two", 600, 1000)
	third := turnSeg(chunkA, 2, OriginExternal, "three", 1100, 1500)
	third.EventID = 200

	turns := AssembleTurns([]TurnSegment{first, second, third}, TurnOptions{})
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if len(turns[0].EventIDs) != 2 {
		t.Errorf("event ids = %v, want the two distinct rows", turns[0].EventIDs)
	}
}
