package transcript

import (
	"strings"
	"time"
)

// span is a half-open millisecond interval on some track's own timeline.
type span struct {
	start, end int64
}

// anchorOf returns the wall clock a track's offsets are measured from, and
// whether that anchor was measured rather than assumed.
//
// A track's own StartedAt is the instant its first sample buffer arrived. The
// chunk's CapturedAt is not a substitute: it is read before ScreenCaptureKit is
// even asked for shareable content, so it precedes real audio by an unbounded
// margin. Falling back to it keeps offsets monotonic and comparable — the bias is
// shared by both tracks of the chunk — but the absolute clock is then derived
// rather than observed, which is what the returned flag records.
func anchorOf(track *Track, chunk Chunk) (time.Time, bool) {
	if track != nil && !track.StartedAt.IsZero() {
		return track.StartedAt, true
	}
	return chunk.CapturedAt, false
}

func orderConfidence(exact bool) OrderConfidence {
	if exact {
		return OrderExact
	}
	return OrderSequence
}

// speechSegments returns the segments that actually carry words. Empty-text
// segments are excluded here because they have nothing to attribute, but they are
// not worthless: internalAudible consults them as evidence that audio played.
func speechSegments(track *Track) []TimedSegment {
	if track == nil {
		return nil
	}
	out := make([]TimedSegment, 0, len(track.Segments))
	for _, segment := range track.Segments {
		if strings.TrimSpace(segment.Text) != "" {
			out = append(out, segment)
		}
	}
	return out
}

// segmentSpan returns a segment's own audible extent: the union of its word
// timings when it has them, else its declared range.
//
// Preferring runs is not a refinement. A recognizer result reports a range
// extending to its finalization boundary, and one measured pair reported
// 3000..9660 ms around a single word occupying 3000..3660 ms. Using the declared
// range would inflate machine audio by seconds and drag unrelated room speech
// into an overlap it never had.
func segmentSpan(segment TimedSegment) span {
	if len(segment.Runs) == 0 {
		return span{segment.StartMS, segment.EndMS}
	}
	start, end := segment.Runs[0].StartMS, segment.Runs[0].EndMS
	for _, run := range segment.Runs[1:] {
		if run.StartMS < start {
			start = run.StartMS
		}
		if run.EndMS > end {
			end = run.EndMS
		}
	}
	return span{start, end}
}

// audibleSpans returns every interval in which the system track was producing
// words, expressed on the microphone track's timeline so the two are directly
// comparable.
func audibleSpans(system *Track, systemBase, micBase time.Time) []span {
	if system == nil {
		return nil
	}
	shift := systemBase.Sub(micBase).Milliseconds()
	out := make([]span, 0, len(system.Segments))
	for _, segment := range system.Segments {
		if strings.TrimSpace(segment.Text) == "" {
			continue
		}
		for _, run := range segment.Runs {
			out = append(out, span{run.StartMS + shift, run.EndMS + shift})
		}
		if len(segment.Runs) == 0 {
			bounds := segmentSpan(segment)
			out = append(out, span{bounds.start + shift, bounds.end + shift})
		}
	}
	return mergeSpans(out)
}

func mergeSpans(spans []span) []span {
	if len(spans) < 2 {
		return spans
	}
	sorted := append([]span(nil), spans...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].start < sorted[j-1].start; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	out := []span{sorted[0]}
	for _, current := range sorted[1:] {
		last := &out[len(out)-1]
		if current.start <= last.end {
			if current.end > last.end {
				last.end = current.end
			}
			continue
		}
		out = append(out, current)
	}
	return out
}

// overlapMS totals how much of target coincides with any of spans, allowing lag
// of slack at each edge. The slack absorbs writer-start skew between the two
// files — measured at 61 ms — and the flight time from speaker to microphone.
//
// The widened spans are re-merged before they are summed. spans arrives disjoint,
// but growing each one by lag at both ends can make neighbours meet, and summing
// them separately would then count the shared milliseconds twice — always
// upwards, and so always towards a bleed verdict. The final clamp hides that only
// once the inflated total passes the segment's whole duration.
func overlapMS(target span, spans []span, lag int64) int64 {
	widened := make([]span, 0, len(spans))
	for _, other := range spans {
		widened = append(widened, span{other.start - lag, other.end + lag})
	}
	var total int64
	for _, other := range mergeSpans(widened) {
		start, end := other.start, other.end
		if start < target.start {
			start = target.start
		}
		if end > target.end {
			end = target.end
		}
		if end > start {
			total += end - start
		}
	}
	if limit := target.end - target.start; total > limit {
		total = limit
	}
	return total
}

// overlappingInternalText concatenates the system track's words that fall inside
// target, so the comparison sees only the machine audio that could plausibly have
// bled into this particular segment.
func overlappingInternalText(system *Track, target span, spans []span, opts Options) string {
	if system == nil {
		return ""
	}
	// spans are already on the microphone timeline; recover the shift so segment
	// offsets can be compared against target.
	var builder strings.Builder
	shift := int64(0)
	if len(spans) > 0 && len(system.Segments) > 0 {
		if first := firstAudible(system); first != nil {
			shift = spans[0].start - segmentSpan(*first).start
		}
	}
	for _, segment := range speechSegments(system) {
		bounds := segmentSpan(segment)
		bounds.start += shift
		bounds.end += shift
		if bounds.end+opts.MaxAlignLagMS < target.start || bounds.start-opts.MaxAlignLagMS > target.end {
			continue
		}
		builder.WriteString(segment.Text)
	}
	return builder.String()
}

func firstAudible(track *Track) *TimedSegment {
	for i := range track.Segments {
		if strings.TrimSpace(track.Segments[i].Text) != "" {
			return &track.Segments[i]
		}
	}
	return nil
}

// internalAudible reports whether the system track was producing sound across a
// microphone span, for a chunk whose system transcript came back empty.
//
// It asks two independent questions. The energy envelope answers "was there a
// signal here at all", which is a presence test with enormous headroom: an output
// tap reads exactly zero when nothing plays, while real content measured -46 dBFS
// and above. And an empty-text segment carrying a valid span is the recognizer
// itself saying audio was present but no words resolved.
//
// wholeTrack asks about the track rather than a span, for callers with no timings.
func internalAudible(system *Track, startMS, endMS int64, opts Options, wholeTrack bool) bool {
	if system == nil {
		return false
	}
	for _, segment := range system.Segments {
		if strings.TrimSpace(segment.Text) != "" {
			continue
		}
		bounds := span{segment.StartMS, segment.EndMS}
		if bounds.end <= bounds.start {
			continue
		}
		if wholeTrack {
			return true
		}
		if bounds.end+opts.MaxAlignLagMS >= startMS && bounds.start-opts.MaxAlignLagMS <= endMS {
			return true
		}
	}
	if !system.HasEnergyData() {
		return false
	}
	window := int64(system.EnvelopeWindowMS)
	from, to := 0, len(system.Envelope)-1
	if !wholeTrack {
		from = int((startMS - opts.MaxAlignLagMS) / window)
		to = int((endMS + opts.MaxAlignLagMS) / window)
	}
	if from < 0 {
		from = 0
	}
	if to >= len(system.Envelope) {
		to = len(system.Envelope) - 1
	}
	for i := from; i <= to; i++ {
		if system.Envelope[i] > opts.InternalAudibleDBFS {
			return true
		}
	}
	return false
}

// offsetMS maps a byte offset within a segment's text to a millisecond offset on
// its own track, using word timings when they are available and interpolating
// across the segment when they are not.
//
// Runs are not guaranteed to concatenate back to the segment text — the bridge
// drops any run whose timing would not resolve — so the walk is clamped to the
// segment's own bounds rather than trusted blindly.
func offsetMS(segment TimedSegment, offset int) int64 {
	clamp := func(value int64) int64 {
		if value < segment.StartMS {
			return segment.StartMS
		}
		if value > segment.EndMS {
			return segment.EndMS
		}
		return value
	}
	if len(segment.Runs) > 0 {
		consumed := 0
		for _, run := range segment.Runs {
			next := consumed + len(run.Text)
			if offset <= next {
				width := next - consumed
				if width <= 0 {
					return clamp(run.StartMS)
				}
				fraction := float64(offset-consumed) / float64(width)
				return clamp(run.StartMS + int64(fraction*float64(run.EndMS-run.StartMS)))
			}
			consumed = next
		}
		return clamp(segment.Runs[len(segment.Runs)-1].EndMS)
	}
	if len(segment.Text) == 0 {
		return segment.StartMS
	}
	fraction := float64(offset) / float64(len(segment.Text))
	return clamp(segment.StartMS + int64(fraction*float64(segment.EndMS-segment.StartMS)))
}

func absoluteMS(base time.Time, offsetMS int64, reference time.Time) int64 {
	return base.Add(time.Duration(offsetMS) * time.Millisecond).Sub(reference).Milliseconds()
}

// timedSegmentAt renders one attributed segment, carrying both its absolute times
// and its offsets within its own file.
func timedSegmentAt(segment TimedSegment, sourceTrack string, origin Origin, base time.Time,
	order OrderConfidence, method Method, confidence float64, bleed bool,
) Segment {
	start, end := segment.StartMS, segment.EndMS
	result := Segment{
		Origin: origin, SourceTrack: sourceTrack, Text: segment.Text,
		StartOffsetMS: &start, EndOffsetMS: &end,
		IsBleed: bleed, Confidence: clampConfidence(confidence),
		OrderConfidence: order, Method: method, Runs: segment.Runs,
	}
	if !base.IsZero() {
		result.StartedAt = base.Add(time.Duration(start) * time.Millisecond).UTC()
		result.EndedAt = base.Add(time.Duration(end) * time.Millisecond).UTC()
	}
	return result
}

// confidenceOf prefers the recognizer's own score. A missing score is not low
// confidence, so it falls back to a documented default rather than to zero.
func confidenceOf(segment TimedSegment, opts Options) float64 {
	if segment.Confidence > 0 {
		return segment.Confidence
	}
	return opts.DefaultASRConfidence
}

func clampConfidence(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// tokenRange is a half-open token index range, with the reading position to place
// it at when it has no anchor of its own.
type tokenRange struct {
	start, end int
	order      float64
}

// bleedRegion is one stretch of the microphone transcript that re-recorded the
// machine, paired with the stretch of the machine's own transcript it came from.
//
// Both spans are carried together because they must stay consistent: whatever
// pattern tokens a region *emits* are exactly the ones that must count as
// accounted for. Deriving emission from the span but coverage from the
// underlying blocks lets the tokens between blocks be emitted twice — once
// inside the region's text and again as unheard machine audio — which is the
// duplication this whole package exists to remove.
type bleedRegion struct {
	textStart, textEnd       int
	patternStart, patternEnd int
}

// maxBleedGapTokens is how many unmatched tokens may sit inside one bleed region
// before it is treated as two. A word or two the room speaker interjected
// mid-phrase should not carve the machine's sentence into fragments that read
// badly.
const maxBleedGapTokens = 3

// bleedRegions merges the alignment's matched blocks into contiguous regions,
// tracking both the microphone span and the machine span each covers.
func bleedRegions(alignment Alignment, textLen, patternLen int) []bleedRegion {
	var regions []bleedRegion
	for _, block := range alignment.Blocks {
		if block.Matches == 0 {
			continue
		}
		if len(regions) > 0 {
			last := &regions[len(regions)-1]
			if block.TextStart-last.textEnd <= maxBleedGapTokens {
				if block.TextEnd > last.textEnd {
					last.textEnd = block.TextEnd
				}
				if block.PatternEnd > last.patternEnd {
					last.patternEnd = block.PatternEnd
				}
				continue
			}
		}
		regions = append(regions, bleedRegion{
			textStart: block.TextStart, textEnd: block.TextEnd,
			patternStart: block.PatternStart, patternEnd: block.PatternEnd,
		})
	}
	for i := range regions {
		if regions[i].textEnd > textLen {
			regions[i].textEnd = textLen
		}
		if regions[i].patternEnd > patternLen {
			regions[i].patternEnd = patternLen
		}
	}
	return regions
}

// regionPlacement is where one bleed region's segments landed in the reading
// order textPath is building: first is the order of its first emitted segment,
// next the order the segment after it will take.
//
// It exists because a region emits a variable number of segments — room speech
// before it, the machine's own text, the microphone's copy — so a region's index
// among the regions says nothing about where it sits in the finished transcript.
// Anchoring against the index instead of the placement put interpolated audio
// near the top of the transcript, several turns ahead of the phrase it followed.
type regionPlacement struct {
	first, next float64
}

// unmatchedPatternRuns finds machine audio that no region accounted for — words
// the microphone never picked up — with the reading position to insert each run
// at.
//
// Coverage comes from the regions rather than from the raw blocks, so a word
// already carried inside a region's text is never emitted a second time.
func unmatchedPatternRuns(regions []bleedRegion, patternLen int, placements []regionPlacement) []tokenRange {
	covered := make([]bool, patternLen)
	// anchor carries the index of the region a token belongs to, so an unheard
	// phrase can be placed beside the audio it followed. -1 is "no region yet".
	anchor := make([]int, patternLen)
	for i := range anchor {
		anchor[i] = -1
	}
	for index, region := range regions {
		for i := region.patternStart; i < region.patternEnd && i < patternLen; i++ {
			covered[i] = true
			anchor[i] = index
		}
	}
	var runs []tokenRange
	for i := 0; i < patternLen; {
		if covered[i] {
			i++
			continue
		}
		start := i
		for i < patternLen && !covered[i] {
			i++
		}
		runs = append(runs, tokenRange{start: start, end: i, order: interpolatedOrder(anchor, placements, start)})
	}
	return runs
}

// interpolatedOrder places one run of unheard machine audio: immediately after
// the emissions of the region preceding it, or immediately before the first
// region when nothing precedes it — machine words the microphone missed were
// still spoken before the ones it caught.
func interpolatedOrder(anchor []int, placements []regionPlacement, start int) float64 {
	if start > 0 && anchor[start-1] >= 0 && anchor[start-1] < len(placements) {
		return placements[anchor[start-1]].next - 0.5
	}
	if len(placements) > 0 {
		return placements[0].first - 0.5
	}
	return 0
}

// sliceTokens cuts the verbatim source text spanned by a token range, so stored
// text keeps the recognizer's own casing, punctuation, and spacing.
func sliceTokens(source string, tokens []Token, start, end int) string {
	if start < 0 || end > len(tokens) || start >= end {
		return ""
	}
	return strings.TrimSpace(source[tokens[start].Start:tokens[end-1].End])
}

// splitSentences breaks a stretch of room speech at sentence boundaries so turns
// read as turns. It never returns empty pieces, and returns nothing at all for
// blank input.
func splitSentences(text string) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	var pieces []string
	start := 0
	runes := []rune(trimmed)
	for i, r := range runes {
		if r != '.' && r != '?' && r != '!' {
			continue
		}
		// Only break when the terminator is followed by a space, so decimals and
		// abbreviations stay intact.
		if i+1 < len(runes) && runes[i+1] != ' ' {
			continue
		}
		piece := strings.TrimSpace(string(runes[start : i+1]))
		if piece != "" {
			pieces = append(pieces, piece)
		}
		start = i + 1
	}
	if tail := strings.TrimSpace(string(runes[start:])); tail != "" {
		pieces = append(pieces, tail)
	}
	if len(pieces) == 0 {
		return []string{trimmed}
	}
	return pieces
}
