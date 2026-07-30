package transcript

import (
	"sort"
	"strings"
	"time"
	"unicode"
)

// Attribute decides where every piece of a chunk's audio came from.
//
// It never returns an error. Missing data lowers labels and confidence instead:
// no system track means the microphone is confidently the room, no timings means
// the transcripts are aligned as text, and machine audio that played without
// transcribing produces OriginUnknown rather than a confident guess. Failing
// would be worse than degrading — the audio is already captured and indexed, and
// attribution is a second opinion about it.
func Attribute(chunk Chunk, opts Options) []Segment {
	opts = opts.withDefaults()
	system, mic := chunk.System, chunk.Microphone

	switch {
	case !hasSpeech(mic) && !hasSpeech(system):
		return silentMarker(chunk)
	case !hasSpeech(mic):
		return finalize(internalOnly(system, chunk, opts))
	case !hasSpeech(system):
		return finalize(externalOnly(mic, system, chunk, opts))
	case timingsUsable(system) && timingsUsable(mic):
		return finalize(timedPath(system, mic, chunk, opts))
	default:
		return finalize(textPath(system, mic, chunk, opts))
	}
}

func hasSpeech(track *Track) bool {
	return track != nil && strings.TrimSpace(track.Text) != ""
}

// silentMarker records that a chunk holding no words was attributed anyway.
//
// Returning nothing here would be the honest-looking answer and is the wrong
// one. A caller's work queue is derived — the chunks with no segments are the
// chunks still to do — so a verdict that writes no row is indistinguishable from
// a verdict that was never reached. Silent chunks are the common case, not an
// edge one, so every one of them would sit in the queue forever, be re-derived
// on every backfill, and be counted as an unattributed hole by coverage. The
// transcript would then advise a backfill that could not change the number it
// was complaining about.
//
// The row carries no text, so it never reaches a turn; it exists to say the
// question was asked and answered. It is attached to whichever track has a row
// to hang it on, since a segment with no source track cannot be stored against
// an event.
func silentMarker(chunk Chunk) []Segment {
	source := ""
	switch {
	case chunk.System != nil && chunk.System.Source != "":
		source = chunk.System.Source
	case chunk.Microphone != nil && chunk.Microphone.Source != "":
		source = chunk.Microphone.Source
	default:
		return nil
	}
	return []Segment{{
		Origin: OriginSilent, SourceTrack: source,
		OrderConfidence: OrderSequence, Method: MethodSilent,
	}}
}

// timingsUsable reports whether a track carried at least one segment with real
// text and a positive span. A track transcribed before the bridge reported
// timings has text but no segments, which is what sends a chunk down the text
// path.
func timingsUsable(track *Track) bool {
	if track == nil {
		return false
	}
	for _, segment := range track.Segments {
		if strings.TrimSpace(segment.Text) != "" && segment.EndMS > segment.StartMS {
			return true
		}
	}
	return false
}

// placed pairs a segment with its position in the chunk's reading order, so
// segments can be emitted in any order and sorted once at the end.
type placed struct {
	order   float64
	segment Segment
}

// finalize drops wordless fragments, sorts by reading order, and assigns dense
// Seq values. Seq is chunk-global rather than per-track because the text path
// produces no absolute times, leaving Seq as the only ordering key a reader can
// rely on.
//
// The wordless filter lives here so every path inherits it. Splitting a segment
// around a matched span routinely leaves a stray "." or "," on one side, and a
// segment whose entire content is punctuation is not a piece of speech — storing
// it would put rows in the index that no query can ever match and that read as
// noise in a transcript.
func finalize(items []placed) []Segment {
	kept := items[:0]
	for _, item := range items {
		if hasWord(item.segment.Text) {
			kept = append(kept, item)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].order < kept[j].order })
	out := make([]Segment, 0, len(kept))
	for i, item := range kept {
		item.segment.Seq = i
		out = append(out, item.segment)
	}
	return out
}

// hasWord reports whether text holds anything a reader or an index could use.
// It matches Tokenize's notion of a word, so a segment survives exactly when it
// would produce at least one token.
func hasWord(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// internalOnly handles a chunk whose microphone track said nothing. Everything
// the system track holds is machine audio; there is nothing to compare it to and
// nothing to disambiguate.
func internalOnly(system *Track, chunk Chunk, opts Options) []placed {
	base, exact := anchorOf(system, chunk)
	order := orderConfidence(exact)
	var out []placed
	for _, segment := range system.Segments {
		if strings.TrimSpace(segment.Text) == "" {
			continue
		}
		out = append(out, placed{
			order:   float64(segment.StartMS),
			segment: timedSegmentAt(segment, system.Source, OriginInternal, base, order, MethodInternalOnly, confidenceOf(segment, opts), false),
		})
	}
	if len(out) == 0 {
		// Text without segments: the whole track is one span.
		out = append(out, placed{segment: Segment{
			Origin: OriginInternal, SourceTrack: system.Source, Text: system.Text,
			Confidence:      opts.DefaultASRConfidence * opts.TextPathPenalty,
			OrderConfidence: OrderSequence, Method: MethodInternalOnly,
		}})
	}
	return out
}

// externalOnly handles a chunk with no usable system transcript, which is the
// common case: only 58 of 2795 system rows in a two-day index carried text.
//
// The verdict turns on *why* the system transcript is empty, and the recognizer
// cannot say — it returns nothing both for a silent track and for one that played
// audio it could not transcribe. Only energy separates them, and they need
// opposite conclusions. Silence means nothing was playing, so microphone speech
// is confidently the room's. Audio without words means a re-recording of unseen
// machine speech is possible, so the honest answer is OriginUnknown.
func externalOnly(mic, system *Track, chunk Chunk, opts Options) []placed {
	base, exact := anchorOf(mic, chunk)
	order := orderConfidence(exact)

	var out []placed
	segments := speechSegments(mic)
	if len(segments) == 0 {
		origin, penalty := OriginExternal, 1.0
		if internalAudible(system, 0, 0, opts, true) {
			origin, penalty = OriginUnknown, opts.UntranscribedInternalPenalty
		}
		return []placed{{segment: Segment{
			Origin: origin, SourceTrack: mic.Source, Text: mic.Text,
			Confidence:      opts.DefaultASRConfidence * opts.TextPathPenalty * penalty,
			OrderConfidence: OrderSequence, Method: MethodExternalOnly,
		}}}
	}
	for _, segment := range segments {
		origin, penalty := OriginExternal, 1.0
		if internalAudible(system, segment.StartMS, segment.EndMS, opts, false) {
			origin, penalty = OriginUnknown, opts.UntranscribedInternalPenalty
		}
		out = append(out, placed{
			order: float64(segment.StartMS),
			segment: timedSegmentAt(segment, mic.Source, origin, base, order,
				MethodExternalOnly, confidenceOf(segment, opts)*penalty, false),
		})
	}
	return out
}

// timedPath attributes a chunk where both tracks carried word timings.
func timedPath(system, mic *Track, chunk Chunk, opts Options) []placed {
	systemBase, systemExact := anchorOf(system, chunk)
	micBase, micExact := anchorOf(mic, chunk)
	// Anchors have to agree about exactness. With only one real anchor the two
	// tracks would be placed on different axes, which is worse than placing both
	// approximately.
	exact := systemExact && micExact
	if !exact {
		systemBase, micBase = chunk.CapturedAt, chunk.CapturedAt
	}
	order := orderConfidence(exact)

	var out []placed

	// Every system-track segment is machine audio, unconditionally. There is no
	// acoustic path from the room into an output tap, so no verification is
	// possible or needed — this is the asymmetry the whole package rests on.
	for _, segment := range speechSegments(system) {
		out = append(out, placed{
			order:   float64(absoluteMS(systemBase, segment.StartMS, micBase)),
			segment: timedSegmentAt(segment, system.Source, OriginInternal, systemBase, order, MethodTimed, confidenceOf(segment, opts), false),
		})
	}

	// Machine-audio intervals on the microphone's axis, from unioned word timings
	// rather than segment ranges: a recognizer result extends to its finalization
	// boundary and was measured overstating speech extent tenfold, which would
	// mark neighbouring room speech as coinciding when it merely sat nearby.
	internalSpans := audibleSpans(system, systemBase, micBase)

	for _, segment := range speechSegments(mic) {
		out = append(out, attributeMicSegment(segment, mic, system, internalSpans, micBase, order, opts)...)
	}
	return out
}

// attributeMicSegment decides one microphone segment, splitting it when only part
// is a re-recording.
func attributeMicSegment(segment TimedSegment, mic, system *Track,
	internalSpans []span, micBase time.Time, order OrderConfidence, opts Options,
) []placed {
	base := confidenceOf(segment, opts)
	duration := segment.EndMS - segment.StartMS
	whole := func(origin Origin, confidence float64, bleed bool) []placed {
		return []placed{{
			order:   float64(segment.StartMS),
			segment: timedSegmentAt(segment, mic.Source, origin, micBase, order, MethodTimed, confidence, bleed),
		}}
	}

	if duration <= 0 {
		return whole(OriginExternal, base, false)
	}
	overlap := overlapMS(span{segment.StartMS, segment.EndMS}, internalSpans, opts.MaxAlignLagMS)
	if float64(overlap)/float64(duration) < opts.MinOverlapFraction {
		// No machine audio was playing across most of this segment, so it is the
		// room's regardless of what the words look like.
		return whole(OriginExternal, base, false)
	}

	internalText := overlappingInternalText(system, span{segment.StartMS, segment.EndMS}, internalSpans, opts)
	external := Tokenize(segment.Text)
	alignment := Align(Tokenize(internalText), external)
	similarity := alignment.Similarity()

	if similarity < opts.CrosstalkSimilarity {
		// Machine audio and room speech coincided but say different things:
		// genuinely simultaneous speech. The label is still external — these
		// words are not in the machine's transcript — but it is less certain,
		// because a re-recording buried under the room's voice would look the
		// same.
		return whole(OriginExternal, base*opts.CrosstalkPenalty, false)
	}

	penalty := 1.0
	if similarity < opts.BleedSimilarity {
		penalty = opts.AmbiguousPenalty
	}

	start, end, ok := alignment.MatchedTextSpan()
	if !ok || len(external) == 0 {
		return whole(OriginInternal, base*penalty, true)
	}
	prefix := strings.TrimSpace(segment.Text[:external[start].Start])
	matched := segment.Text[external[start].Start:external[end-1].End]
	suffix := strings.TrimSpace(segment.Text[external[end-1].End:])
	if prefix == "" && suffix == "" {
		return whole(OriginInternal, base*penalty, true)
	}

	// The matched span is the machine's; whatever brackets it is the room's. A
	// third of real bleed chunks look like this.
	var out []placed
	emit := func(text string, from, to int, origin Origin, bleed bool) {
		if strings.TrimSpace(text) == "" {
			return
		}
		startMS := offsetMS(segment, from)
		endMS := offsetMS(segment, to)
		if endMS < startMS {
			endMS = startMS
		}
		part := TimedSegment{StartMS: startMS, EndMS: endMS, Text: text, Confidence: segment.Confidence}
		out = append(out, placed{
			order:   float64(startMS),
			segment: timedSegmentAt(part, mic.Source, origin, micBase, order, MethodTimed, base*penalty, bleed),
		})
	}
	emit(prefix, 0, external[start].Start, OriginExternal, false)
	emit(matched, external[start].Start, external[end-1].End, OriginInternal, true)
	emit(suffix, external[end-1].End, len(segment.Text), OriginExternal, false)
	if len(out) == 0 {
		return whole(OriginInternal, base*penalty, true)
	}
	return out
}

// textPath attributes a chunk with no usable timings, which is every chunk
// captured before the bridge reported them.
//
// The microphone transcript is the ordered spine and the system transcript is the
// labelling oracle. That works because the microphone WAV is a single stream
// holding everything audible in the room, so one recognizer pass over it is
// chronological by construction — reading order *is* conversational order. Only
// absolute time is lost, not sequence.
func textPath(system, mic *Track, chunk Chunk, opts Options) []placed {
	external := Tokenize(mic.Text)
	internal := Tokenize(system.Text)
	alignment := Align(internal, external)

	regions := bleedRegions(alignment, len(external), len(internal))
	var out []placed
	position := 0.0
	// Where each region's segments land, so machine audio the microphone never
	// heard can be anchored to the emitted transcript rather than to a region
	// index that has no relationship to it. See regionPlacement.
	placements := make([]regionPlacement, len(regions))

	// Walk the microphone transcript left to right, alternating between what the
	// machine played and what the room said.
	cursor := 0
	for index, region := range regions {
		if region.textStart > cursor {
			for _, piece := range splitSentences(sliceTokens(mic.Text, external, cursor, region.textStart)) {
				out = append(out, placed{order: position, segment: Segment{
					Origin: OriginExternal, SourceTrack: mic.Source, Text: piece,
					Confidence:      opts.DefaultASRConfidence * opts.TextPathPenalty,
					OrderConfidence: OrderSequence, Method: MethodText,
				}})
				position++
			}
		}
		placements[index].first = position
		// Text comes from the system side: it is the original rather than a
		// re-recording, so it is the cleaner of the two transcriptions.
		if text := sliceTokens(system.Text, internal, region.patternStart, region.patternEnd); text != "" {
			out = append(out, placed{order: position, segment: Segment{
				Origin: OriginInternal, SourceTrack: system.Source, Text: text,
				Confidence:      opts.DefaultASRConfidence * opts.TextPathPenalty,
				OrderConfidence: OrderSequence, Method: MethodText,
			}})
			position++
		}
		// The microphone's copy is kept too, marked as bleed. Nothing captured is
		// discarded; it is simply excluded from an assembled transcript.
		out = append(out, placed{order: position, segment: Segment{
			Origin: OriginInternal, SourceTrack: mic.Source,
			Text:            sliceTokens(mic.Text, external, region.textStart, region.textEnd),
			IsBleed:         true,
			Confidence:      opts.DefaultASRConfidence * opts.TextPathPenalty,
			OrderConfidence: OrderSequence, Method: MethodText,
		}})
		position++
		placements[index].next = position
		cursor = region.textEnd
	}
	if cursor < len(external) {
		for _, piece := range splitSentences(sliceTokens(mic.Text, external, cursor, len(external))) {
			out = append(out, placed{order: position, segment: Segment{
				Origin: OriginExternal, SourceTrack: mic.Source, Text: piece,
				Confidence:      opts.DefaultASRConfidence * opts.TextPathPenalty,
				OrderConfidence: OrderSequence, Method: MethodText,
			}})
			position++
		}
	}

	// Machine audio no region accounted for — words the microphone never picked
	// up — has no anchor in the spine, so it is placed after whatever did match
	// before it. This is the only tier that admits guessing, and it says so.
	for _, gap := range unmatchedPatternRuns(regions, len(internal), placements) {
		text := sliceTokens(system.Text, internal, gap.start, gap.end)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, placed{order: gap.order, segment: Segment{
			Origin: OriginInternal, SourceTrack: system.Source, Text: text,
			Confidence:      opts.DefaultASRConfidence * opts.TextPathPenalty,
			OrderConfidence: OrderApproximate, Method: MethodText,
		}})
	}
	return out
}
