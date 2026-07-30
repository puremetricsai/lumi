package transcript

import (
	"strings"
	"testing"
	"time"
)

// Fixtures are synthetic. The shapes they exercise come from real captured
// chunks, but the wording is invented — real chunks are private conversation and
// do not belong in a source repository.

var anchor = time.Date(2026, 7, 29, 20, 58, 58, 0, time.UTC)

// words builds runs covering text evenly across a span, which is what the speech
// bridge produces for real audio.
func words(text string, startMS, endMS int64) []TimedRun {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	step := (endMS - startMS) / int64(len(fields))
	runs := make([]TimedRun, 0, len(fields))
	at := startMS
	for i, field := range fields {
		prefix := " "
		if i == 0 {
			prefix = ""
		}
		runs = append(runs, TimedRun{
			StartMS: at, EndMS: at + step, Text: prefix + field, Confidence: 0.9,
		})
		at += step
	}
	runs[len(runs)-1].EndMS = endMS
	return runs
}

func seg(text string, startMS, endMS int64) TimedSegment {
	return TimedSegment{
		StartMS: startMS, EndMS: endMS, Text: text,
		Confidence: 0.9, Runs: words(text, startMS, endMS),
	}
}

func track(source string, startedAt time.Time, segments ...TimedSegment) *Track {
	text := ""
	for i, segment := range segments {
		if i > 0 {
			text += " "
		}
		text += segment.Text
	}
	return &Track{Source: source, StartedAt: startedAt, Text: text, Segments: segments}
}

// silentEnvelope is a track that played nothing: an output tap reads exactly zero.
func silentEnvelope(windows int) ([]float64, int) {
	envelope := make([]float64, windows)
	for i := range envelope {
		envelope[i] = -120
	}
	return envelope, 100
}

func originsOf(segments []Segment) []Origin {
	out := make([]Origin, len(segments))
	for i, segment := range segments {
		out[i] = segment.Origin
	}
	return out
}

func findText(t *testing.T, segments []Segment, substring string) Segment {
	t.Helper()
	for _, segment := range segments {
		if strings.Contains(segment.Text, substring) {
			return segment
		}
	}
	t.Fatalf("no segment containing %q; got %+v", substring, segments)
	return Segment{}
}

// TestEverySystemTrackSegmentIsInternalWithoutVerification pins the asymmetry the
// whole package rests on. The system track is a tap on the audio output graph, so
// room sound has no path into it — its contents need no checking against
// anything.
func TestEverySystemTrackSegmentIsInternalWithoutVerification(t *testing.T) {
	system := track("system", anchor, seg("The build finished successfully.", 0, 2000))
	// Deliberately give the microphone the very same words. Even a perfect match
	// must not turn a system segment into room audio.
	mic := track("microphone", anchor, seg("The build finished successfully.", 0, 2000))

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	found := false
	for _, segment := range segments {
		if segment.SourceTrack != "system" {
			continue
		}
		found = true
		if segment.Origin != OriginInternal {
			t.Errorf("system-track segment %q labelled %s", segment.Text, segment.Origin)
		}
		if segment.IsBleed {
			t.Errorf("system-track segment %q marked as bleed", segment.Text)
		}
	}
	if !found {
		t.Fatal("no system-track segment was emitted at all")
	}
}

// TestBleedIsLabelledInternalAndMarked is the core case: the microphone
// re-recorded the machine through the speakers, so the same words appear twice
// and only one copy should reach a transcript.
func TestBleedIsLabelledInternalAndMarked(t *testing.T) {
	phrase := "The deployment finished with no warnings at all."
	system := track("system", anchor, seg(phrase, 0, 3000))
	mic := track("microphone", anchor, seg(phrase, 60, 3060))

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	var bleed, original int
	for _, segment := range segments {
		if segment.Origin != OriginInternal {
			t.Errorf("segment %q labelled %s, want internal", segment.Text, segment.Origin)
		}
		if segment.SourceTrack == "microphone" {
			if !segment.IsBleed {
				t.Errorf("microphone copy of %q was not marked as bleed", segment.Text)
			}
			bleed++
		} else {
			original++
		}
	}
	if bleed != 1 || original != 1 {
		t.Errorf("got %d bleed and %d original segments, want 1 of each", bleed, original)
	}
}

// TestMicSegmentWithNoOverlappingInternalAudioIsExternal covers the ordinary case
// where the machine was quiet and the user simply spoke.
func TestMicSegmentWithNoOverlappingInternalAudioIsExternal(t *testing.T) {
	system := track("system", anchor, seg("Ping.", 0, 300))
	mic := track("microphone", anchor, seg("I think we should ship it on Thursday.", 5000, 9000))

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	segment := findText(t, segments, "ship it on Thursday")
	if segment.Origin != OriginExternal {
		t.Errorf("origin = %s, want external", segment.Origin)
	}
	if segment.Confidence < 0.85 {
		t.Errorf("confidence = %.2f; a clean, unambiguous segment should not be penalised", segment.Confidence)
	}
	if segment.IsBleed {
		t.Error("room speech was marked as bleed")
	}
}

// TestSimultaneousSpeechIsExternalAtLoweredConfidence is the requirement that both
// parties talking must yield partial attribution rather than a confident wrong
// label. The lowered confidence is the load-bearing half.
func TestSimultaneousSpeechIsExternalAtLoweredConfidence(t *testing.T) {
	system := track("system", anchor, seg("Right, and the invoice went out on Monday.", 0, 4000))
	mic := track("microphone", anchor, seg("Sorry, could you repeat the last part?", 0, 4000))

	overlapping := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})
	spoken := findText(t, overlapping, "repeat the last part")
	if spoken.Origin != OriginExternal {
		t.Errorf("origin = %s, want external", spoken.Origin)
	}

	// The same words with nothing playing are the clean baseline.
	quiet := Attribute(Chunk{
		CapturedAt: anchor,
		System:     track("system", anchor, seg("Mm.", 20000, 20100)),
		Microphone: mic,
	}, Options{})
	clean := findText(t, quiet, "repeat the last part")

	if spoken.Confidence >= clean.Confidence {
		t.Errorf("simultaneous speech scored %.2f, not below the clean case's %.2f",
			spoken.Confidence, clean.Confidence)
	}
}

// TestAmbiguousMicSegmentSplitsIntoBleedAndExternal is why the aligner returns
// spans rather than a score. A third of real bleed chunks hold room speech
// alongside the re-recording in one window; labelling wholesale would either bury
// the user's words or index the machine's twice.
func TestAmbiguousMicSegmentSplitsIntoBleedAndExternal(t *testing.T) {
	system := track("system", anchor, seg("Teams usually land on that option.", 1000, 3000))
	mic := track("microphone", anchor,
		seg("Yeah I like that. Teams usually land on that option. I'm open to it.", 900, 3600))

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	var micSegments []Segment
	for _, segment := range segments {
		if segment.SourceTrack == "microphone" {
			micSegments = append(micSegments, segment)
		}
	}
	if len(micSegments) < 2 {
		t.Fatalf("microphone segment was not split; got %d parts: %+v", len(micSegments), micSegments)
	}

	var bleed, external int
	rebuilt := ""
	for _, segment := range micSegments {
		rebuilt += segment.Text
		if segment.IsBleed {
			bleed++
			if !strings.Contains(segment.Text, "Teams usually land") {
				t.Errorf("bleed part is %q, want the machine's phrase", segment.Text)
			}
		} else {
			external++
			if segment.Origin != OriginExternal {
				t.Errorf("non-bleed part %q labelled %s", segment.Text, segment.Origin)
			}
		}
	}
	if bleed != 1 {
		t.Errorf("got %d bleed parts, want exactly 1", bleed)
	}
	if external < 1 {
		t.Error("the room's own words were not preserved as external")
	}
	// Splitting must not lose words.
	for _, word := range []string{"Yeah", "Teams", "open"} {
		if !strings.Contains(rebuilt, word) {
			t.Errorf("splitting dropped %q; rebuilt text is %q", word, rebuilt)
		}
	}
}

// TestAbsentSystemTrackIsAllExternalWithoutError is a stated requirement: a
// microphone chunk with no system counterpart is entirely the room's, and must
// not error.
func TestAbsentSystemTrackIsAllExternalWithoutError(t *testing.T) {
	mic := track("microphone", anchor,
		seg("Just thinking out loud here.", 0, 2000),
		seg("Let me try the other approach.", 2000, 4000))

	segments := Attribute(Chunk{CapturedAt: anchor, Microphone: mic}, Options{})

	if len(segments) != 2 {
		t.Fatalf("got %d segments, want 2", len(segments))
	}
	for _, segment := range segments {
		if segment.Origin != OriginExternal {
			t.Errorf("segment %q labelled %s, want external", segment.Text, segment.Origin)
		}
		if segment.Method != MethodExternalOnly {
			t.Errorf("method = %s, want external_only", segment.Method)
		}
		if segment.Confidence < 0.85 {
			t.Errorf("confidence = %.2f; nothing was playing, so this is not a guess", segment.Confidence)
		}
	}
}

// TestSilentInternalTrackIsExternalButUntranscribedInternalIsUnknown is the
// distinction the energy gate exists for. The recognizer returns nothing in both
// cases; only energy separates them, and they need opposite conclusions.
func TestSilentInternalTrackIsExternalButUntranscribedInternalIsUnknown(t *testing.T) {
	mic := track("microphone", anchor, seg("So that is the plan for next week.", 0, 3000))

	t.Run("silent machine", func(t *testing.T) {
		envelope, window := silentEnvelope(300)
		system := &Track{Source: "system", StartedAt: anchor, Envelope: envelope, EnvelopeWindowMS: window}
		segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})
		got := findText(t, segments, "plan for next week")
		if got.Origin != OriginExternal {
			t.Errorf("origin = %s, want external — nothing was playing", got.Origin)
		}
		if got.Confidence < 0.85 {
			t.Errorf("confidence = %.2f, want full: silence is evidence, not absence of it", got.Confidence)
		}
	})

	t.Run("machine played something untranscribed", func(t *testing.T) {
		envelope, window := silentEnvelope(300)
		for i := 5; i < 20; i++ {
			envelope[i] = -46 // the level real untranscribed machine audio measured
		}
		system := &Track{Source: "system", StartedAt: anchor, Envelope: envelope, EnvelopeWindowMS: window}
		segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})
		got := findText(t, segments, "plan for next week")
		if got.Origin != OriginUnknown {
			t.Errorf("origin = %s, want unknown — audio played but produced no words", got.Origin)
		}
		if got.Confidence >= 0.85 {
			t.Errorf("confidence = %.2f, want reduced", got.Confidence)
		}
	})
}

// TestSilenceGateIsLocalNotWholeTrack is the notification-blip case, found in real
// data: one 100 ms window of machine audio in an otherwise silent thirty seconds.
// A whole-track gate would mark every microphone segment unknown.
func TestSilenceGateIsLocalNotWholeTrack(t *testing.T) {
	envelope, window := silentEnvelope(300)
	envelope[12] = -40 // a blip at ~1.2s
	system := &Track{Source: "system", StartedAt: anchor, Envelope: envelope, EnvelopeWindowMS: window}
	mic := track("microphone", anchor,
		seg("This one overlaps the blip.", 1000, 1500),
		seg("This one is far away from it.", 20000, 23000))

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	near := findText(t, segments, "overlaps the blip")
	if near.Origin != OriginUnknown {
		t.Errorf("segment over the blip = %s, want unknown", near.Origin)
	}
	far := findText(t, segments, "far away from it")
	if far.Origin != OriginExternal {
		t.Errorf("segment away from the blip = %s, want external — one blip must not taint the chunk", far.Origin)
	}
}

// TestTextOnlyPathRecoversOrderAndLabels covers backfill over chunks whose WAVs
// are gone. The microphone transcript is the ordered spine, so sequence survives
// even with no timings at all.
func TestTextOnlyPathRecoversOrderAndLabels(t *testing.T) {
	system := &Track{Source: "system", Text: "Teams usually land on that option."}
	mic := &Track{Source: "microphone",
		Text: "Teams usually land on that option. Yeah, I am definitely open to that."}

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})
	if len(segments) == 0 {
		t.Fatal("no segments")
	}
	for _, segment := range segments {
		if segment.Method != MethodText {
			t.Errorf("method = %s, want text", segment.Method)
		}
		if !segment.StartedAt.IsZero() {
			t.Errorf("segment %q carries an absolute time it cannot know", segment.Text)
		}
	}

	own := findText(t, segments, "definitely open to that")
	if own.Origin != OriginExternal {
		t.Errorf("the room's own words labelled %s", own.Origin)
	}
	if own.OrderConfidence != OrderSequence {
		t.Errorf("order confidence = %s, want sequence", own.OrderConfidence)
	}
	machine := findText(t, segments, "Teams usually land")
	if machine.Origin != OriginInternal {
		t.Errorf("the machine's words labelled %s", machine.Origin)
	}
	// Reading order must survive: the machine spoke before the room answered.
	if machine.Seq > own.Seq {
		t.Errorf("machine segment at seq %d follows the reply at seq %d", machine.Seq, own.Seq)
	}
}

// TestTextPathMarksUnheardMachineAudioApproximate covers the one tier that admits
// guessing: machine audio the microphone never picked up has no anchor in the
// spine, so its position is inferred and says so.
func TestTextPathMarksUnheardMachineAudioApproximate(t *testing.T) {
	system := &Track{Source: "system", Text: "Understood. The archive was never uploaded."}
	mic := &Track{Source: "microphone", Text: "Understood. Let me check on that now."}

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	unheard := findText(t, segments, "archive was never uploaded")
	if unheard.Origin != OriginInternal {
		t.Errorf("origin = %s, want internal", unheard.Origin)
	}
	if unheard.OrderConfidence != OrderApproximate {
		t.Errorf("order confidence = %s, want approximate — nothing anchors this phrase",
			unheard.OrderConfidence)
	}
}

// TestNothingCapturedIsDiscarded guards the invariant that every word of both
// transcripts survives into some segment, even when excluded from a transcript.
func TestNothingCapturedIsDiscarded(t *testing.T) {
	phrase := "The report is attached to the ticket."
	system := track("system", anchor, seg(phrase, 0, 3000))
	mic := track("microphone", anchor, seg(phrase+" Thanks, I will read it.", 60, 5000))

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	all := ""
	for _, segment := range segments {
		all += segment.Text + " "
	}
	for _, word := range []string{"report", "attached", "ticket", "Thanks", "read"} {
		if !strings.Contains(all, word) {
			t.Errorf("%q vanished; segments hold %q", word, all)
		}
	}
}

// TestSeqIsDenseAndChunkGlobal pins the ordering contract readers depend on.
func TestSeqIsDenseAndChunkGlobal(t *testing.T) {
	system := track("system", anchor, seg("First the machine speaks.", 0, 1000))
	mic := track("microphone", anchor,
		seg("Then the room replies.", 2000, 3000),
		seg("And adds something else.", 3000, 4000))

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})
	if len(segments) < 3 {
		t.Fatalf("got %d segments, want at least 3", len(segments))
	}
	for i, segment := range segments {
		if segment.Seq != i {
			t.Errorf("segment %d has seq %d; seq must be dense from zero", i, segment.Seq)
		}
	}
	if segments[0].SourceTrack != "system" {
		t.Errorf("first segment came from %s; the machine spoke first", segments[0].SourceTrack)
	}
}

// TestAnchorFallbackDowngradesOrderConfidence covers chunks captured before the
// recorder reported a first-sample-buffer wall clock. Offsets stay comparable
// because both tracks share the same fallback, but the absolute clock is derived
// rather than observed and must not claim otherwise.
func TestAnchorFallbackDowngradesOrderConfidence(t *testing.T) {
	system := track("system", time.Time{}, seg("Machine audio here.", 0, 1000))
	mic := track("microphone", time.Time{}, seg("Room audio over here.", 4000, 6000))

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})
	for _, segment := range segments {
		if segment.OrderConfidence != OrderSequence {
			t.Errorf("segment %q claims %s order without a measured anchor",
				segment.Text, segment.OrderConfidence)
		}
	}

	anchored := Attribute(Chunk{
		CapturedAt: anchor,
		System:     track("system", anchor, seg("Machine audio here.", 0, 1000)),
		Microphone: track("microphone", anchor, seg("Room audio over here.", 4000, 6000)),
	}, Options{})
	for _, segment := range anchored {
		if segment.OrderConfidence != OrderExact {
			t.Errorf("segment %q reports %s despite measured anchors",
				segment.Text, segment.OrderConfidence)
		}
	}
}

func TestAttributeNeverPanicsOnDegenerateChunks(t *testing.T) {
	cases := map[string]Chunk{
		"empty":              {CapturedAt: anchor},
		"both blank":         {CapturedAt: anchor, System: &Track{Source: "system"}, Microphone: &Track{Source: "microphone"}},
		"system text only":   {CapturedAt: anchor, System: &Track{Source: "system", Text: "Only the machine."}},
		"mic text only":      {CapturedAt: anchor, Microphone: &Track{Source: "microphone", Text: "Only the room."}},
		"segments no text":   {CapturedAt: anchor, System: track("system", anchor, TimedSegment{StartMS: 0, EndMS: 500})},
		"zero-width segment": {CapturedAt: anchor, Microphone: track("microphone", anchor, seg("Hi.", 100, 100))},
	}
	for name, chunk := range cases {
		t.Run(name, func(t *testing.T) {
			segments := Attribute(chunk, Options{})
			for i, segment := range segments {
				if segment.Seq != i {
					t.Errorf("seq %d out of order", segment.Seq)
				}
				if segment.Confidence < 0 || segment.Confidence > 1 {
					t.Errorf("confidence %.2f out of range", segment.Confidence)
				}
			}
		})
	}
}

// TestOriginsAreOnlyTheThreeKnownValues pins the vocabulary a reader can be
// asked to interpret. OriginSilent is deliberately not in it: it labels a
// wordless marker row, never speech, so any text carrying it would be a segment
// claiming to be silence.
func TestOriginsAreOnlyTheThreeKnownValues(t *testing.T) {
	system := track("system", anchor, seg("Some machine audio.", 0, 2000))
	mic := track("microphone", anchor, seg("Some room audio.", 0, 2000))
	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})
	for _, origin := range originsOf(segments) {
		switch origin {
		case OriginInternal, OriginExternal, OriginUnknown:
		default:
			t.Errorf("unexpected origin %q", origin)
		}
	}
	for _, segment := range segments {
		if segment.Origin == OriginSilent && segment.Text != "" {
			t.Errorf("segment %q is labelled silent", segment.Text)
		}
	}
}

// TestTextPathNeverEmitsTheSameMachineWordsTwice is a regression test for a real
// duplication found by running the text path over captured audio.
//
// A bleed region's text is sliced from the whole machine span it covers,
// including the words sitting between individual matched blocks. Coverage used to
// be computed from the blocks alone, so those in-between words were emitted a
// second time as "machine audio the microphone never heard" — reintroducing, in
// the transcript, exactly the duplication this package removes.
func TestTextPathNeverEmitsTheSameMachineWordsTwice(t *testing.T) {
	// Drift in the middle ("figures" vs "numbers") splits the alignment into two
	// blocks with an unmatched pattern token between them.
	system := &Track{Source: "system",
		Text: "I reviewed the quarterly figures and they look fine to me."}
	mic := &Track{Source: "microphone",
		Text: "I reviewed the quarterly numbers and they look fine to me. Great, thanks."}

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	// Every machine-origin word must appear exactly once across all non-bleed
	// segments from the system track.
	counts := map[string]int{}
	for _, segment := range segments {
		if segment.SourceTrack != "system" || segment.IsBleed {
			continue
		}
		for _, token := range Tokenize(segment.Text) {
			counts[token.Norm]++
		}
	}
	for word, count := range counts {
		if count > 1 {
			t.Errorf("machine word %q was emitted %d times; it must appear once", word, count)
		}
	}
	if counts["figures"] == 0 {
		t.Error("the drifted machine word was dropped entirely")
	}
	// And the room's own words must survive.
	external := ""
	for _, segment := range segments {
		if segment.Origin == OriginExternal {
			external += segment.Text
		}
	}
	if !strings.Contains(external, "Great") {
		t.Errorf("the room's reply was lost; external text is %q", external)
	}
}

// TestTextPathMarksOnlyTrulyUnheardAudioApproximate guards the other half: words
// the microphone genuinely never captured must still be emitted, and marked.
func TestTextPathMarksOnlyTrulyUnheardAudioApproximate(t *testing.T) {
	system := &Track{Source: "system",
		Text: "The archive uploaded cleanly. A second sentence nobody heard."}
	mic := &Track{Source: "microphone", Text: "The archive uploaded cleanly. Good."}

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	var approximate, sequence int
	for _, segment := range segments {
		if segment.SourceTrack != "system" {
			continue
		}
		switch segment.OrderConfidence {
		case OrderApproximate:
			approximate++
			if !strings.Contains(segment.Text, "nobody heard") {
				t.Errorf("approximate segment is %q, want the unheard sentence", segment.Text)
			}
		case OrderSequence:
			sequence++
		}
	}
	if approximate != 1 {
		t.Errorf("got %d approximate segments, want exactly the one unheard sentence", approximate)
	}
	if sequence == 0 {
		t.Error("the anchored machine audio was not marked as sequence-ordered")
	}
}

// TestInterpolatedAudioLandsBesideThePhraseItFollowed is a regression test for a
// misplacement found in review.
//
// The reading position of a bleed region is not its index among the regions: a
// region emits room speech, the machine's text, and the microphone's copy, so
// several emitted segments separate one region from the next. Anchoring
// interpolated audio on the index put it near the top of the transcript —
// "Extra words nobody heard" arrived at seq 1 of 7, four turns ahead of the
// phrase it actually followed. Being marked approximate excuses an imprecise
// position, not a wrong one.
func TestInterpolatedAudioLandsBesideThePhraseItFollowed(t *testing.T) {
	system := &Track{Source: "system", Text: "Hello there. Extra words nobody heard."}
	mic := &Track{Source: "microphone",
		Text: "One thing. Two things. Three things. Four things. Hello there."}

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	unheard := findText(t, segments, "nobody heard")
	if unheard.OrderConfidence != OrderApproximate {
		t.Fatalf("order confidence = %s, want approximate", unheard.OrderConfidence)
	}
	anchorSegment := findText(t, segments, "Hello there")
	if unheard.Seq < anchorSegment.Seq {
		t.Errorf("interpolated audio at seq %d precedes the phrase it followed at seq %d; segments: %v",
			unheard.Seq, anchorSegment.Seq, textsOf(segments))
	}
	// And it must not have been dropped into the middle of the room's own run.
	for _, segment := range segments {
		if segment.Origin != OriginExternal {
			continue
		}
		if segment.Seq > unheard.Seq {
			t.Errorf("room speech %q at seq %d follows interpolated audio at seq %d; segments: %v",
				segment.Text, segment.Seq, unheard.Seq, textsOf(segments))
		}
	}
}

// TestUnheardAudioBeforeAnyMatchLeadsTheTranscript covers the other end: machine
// words with no matched region before them were still spoken before the ones the
// microphone did catch.
func TestUnheardAudioBeforeAnyMatchLeadsTheTranscript(t *testing.T) {
	system := &Track{Source: "system", Text: "Nobody caught this part. Hello there."}
	mic := &Track{Source: "microphone", Text: "Hello there. Right, understood."}

	segments := Attribute(Chunk{CapturedAt: anchor, System: system, Microphone: mic}, Options{})

	unheard := findText(t, segments, "Nobody caught")
	anchorSegment := findText(t, segments, "Hello there")
	if unheard.Seq > anchorSegment.Seq {
		t.Errorf("unheard opening at seq %d follows the matched phrase at seq %d; segments: %v",
			unheard.Seq, anchorSegment.Seq, textsOf(segments))
	}
}

// TestSilentChunkStillProducesAMarker pins the row that keeps a derived work
// queue drainable. Without it, "attributed and empty" and "never attributed" are
// the same absence, so every silent chunk — the common case, not an edge one —
// stays queued forever and counts as a permanent hole in coverage.
func TestSilentChunkStillProducesAMarker(t *testing.T) {
	chunk := Chunk{CapturedAt: anchor,
		System:     &Track{Source: "system"},
		Microphone: &Track{Source: "microphone"}}

	segments := Attribute(chunk, Options{})
	if len(segments) != 1 {
		t.Fatalf("got %d segments, want exactly one marker: %v", len(segments), textsOf(segments))
	}
	marker := segments[0]
	if marker.Origin != OriginSilent {
		t.Errorf("origin = %q, want %q — silence is not the same claim as unknown, which warns "+
			"that hidden machine speech may be present", marker.Origin, OriginSilent)
	}
	if marker.Text != "" {
		t.Errorf("the marker carries text %q; it must never reach a transcript", marker.Text)
	}
	if marker.SourceTrack == "" {
		t.Error("the marker has no source track, so no event can own it")
	}
	if marker.Method != MethodSilent {
		t.Errorf("method = %q, want %q", marker.Method, MethodSilent)
	}
}

// TestChunkWithNoTracksAtAllProducesNothing guards the other side of the marker:
// with no track there is no event to attach a row to, and an unattributable chunk
// must stay in the queue rather than be marked done.
func TestChunkWithNoTracksAtAllProducesNothing(t *testing.T) {
	if segments := Attribute(Chunk{CapturedAt: anchor}, Options{}); len(segments) != 0 {
		t.Errorf("got %d segments for a chunk with no tracks, want none", len(segments))
	}
}

func textsOf(segments []Segment) []string {
	out := make([]string, len(segments))
	for i, segment := range segments {
		out[i] = segment.Text
	}
	return out
}
