package transcript

import (
	"strings"
	"testing"
)

func norms(tokens []Token) []string {
	out := make([]string, len(tokens))
	for i, token := range tokens {
		out[i] = token.Norm
	}
	return out
}

// Fixtures here are synthetic. The structural properties they exercise — filler
// words, interior interjections, recognizer drift on numbers — are taken from
// real captured pairs, but the wording is invented: real chunks are private
// conversation and have no business in a source repository. The one verbatim
// exception is the drift case below, which is promotional video audio.
func TestTokenizeKeepsByteOffsetsIntoTheSource(t *testing.T) {
	source := "Um, the retry limits are more aggressive."
	tokens := Tokenize(source)
	want := []string{"um", "the", "retry", "limits", "are", "more", "aggressive"}
	if got := norms(tokens); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i, token := range tokens {
		slice := source[token.Start:token.End]
		if strings.ToLower(slice) != token.Norm {
			t.Errorf("token %d offsets %d..%d slice %q, normalized %q",
				i, token.Start, token.End, slice, token.Norm)
		}
	}
	// The offsets must be usable to cut verbatim text back out, punctuation and
	// casing intact.
	first, last := tokens[0], tokens[len(tokens)-1]
	if got := source[first.Start:last.End]; got != "Um, the retry limits are more aggressive" {
		t.Errorf("span slice = %q", got)
	}
}

// TestTokenizeTrimsRunWhitespace pins the detail that run text from the speech
// bridge arrives with its own leading space, because that is what makes runs
// concatenate into readable prose.
func TestTokenizeTrimsRunWhitespace(t *testing.T) {
	tokens := Tokenize(" Yeah.")
	if len(tokens) != 1 || tokens[0].Norm != "yeah" {
		t.Fatalf("tokens = %v", norms(tokens))
	}
	if tokens[0].Start != 1 {
		t.Errorf("token starts at %d, want 1 — the leading space is not part of the word", tokens[0].Start)
	}
}

func TestTokenizeHandlesEmptyAndPunctuationOnly(t *testing.T) {
	if got := Tokenize(""); len(got) != 0 {
		t.Errorf("empty string produced %v", norms(got))
	}
	if got := Tokenize("... -- ?!"); len(got) != 0 {
		t.Errorf("punctuation produced %v", norms(got))
	}
}

func TestTokenizeKeepsDigitsAndUnicodeLetters(t *testing.T) {
	if got := norms(Tokenize("$52 million in 2026")); strings.Join(got, "|") != "52|million|in|2026" {
		t.Errorf("tokens = %v", got)
	}
	if got := norms(Tokenize("naïve café")); strings.Join(got, "|") != "naïve|café" {
		t.Errorf("tokens = %v", got)
	}
}

func align(pattern, text string) Alignment {
	return Align(Tokenize(pattern), Tokenize(text))
}

// TestVerbatimReRecordingScoresNearOne is the bleed case: the microphone captured
// the machine's audio through the speakers, so every word reappears.
func TestVerbatimReRecordingScoresNearOne(t *testing.T) {
	internal := "The migration finished with no warnings. Like there were no failures at all."
	external := "The migration finished with no warnings. Like there were no failures at all. Right, exactly."
	got := align(internal, external).Similarity()
	if got < 0.95 {
		t.Errorf("similarity = %.3f, want ~1.0 for a verbatim re-recording", got)
	}
}

// TestUnrelatedSpeechScoresNearZero is simultaneous speech: both parties talking,
// so the internal words are simply not in the external transcript.
func TestUnrelatedSpeechScoresNearZero(t *testing.T) {
	internal := "Mm-hmm. Yeah. I would have to."
	external := "Let me pull the logs off the staging box first."
	got := align(internal, external).Similarity()
	if got > 0.35 {
		t.Errorf("similarity = %.3f, want a low score for unrelated speech", got)
	}
}

// TestRecognizerDriftStillMatches is why comparison is token-level and tolerant.
// This is a real pair from the index — the same audio, transcribed differently by
// each track — and it is quoted verbatim because it is a promotional video's
// marketing copy rather than anyone's conversation.
func TestRecognizerDriftStillMatches(t *testing.T) {
	internal := "is the most expressive real-time voice model out there. We've raised $52 million " +
		"and serve more than 8 million creators, developers, and enterprises in our first year."
	external := "Most expressive real-time voice model out there. We've raised $52000000 " +
		"and serve more than 8000000 creators, developers, and enterprises in our 1st year."
	got := align(internal, external).Similarity()
	if got < 0.6 {
		t.Errorf("similarity = %.3f; recognizer drift on numbers should not sink a real match", got)
	}
	if got > 0.95 {
		t.Errorf("similarity = %.3f; the drifted tokens should not count as matches", got)
	}
}

// TestOrderMatters separates this from a bag-of-words measure. The same words in
// a scrambled order are not a re-recording.
func TestOrderMatters(t *testing.T) {
	forward := align("the cost of the second option", "the cost of the second option").Similarity()
	scrambled := align("the cost of the second option", "option second the of cost the").Similarity()
	if forward <= scrambled {
		t.Errorf("in-order %.3f did not beat scrambled %.3f", forward, scrambled)
	}
	if scrambled > 0.75 {
		t.Errorf("scrambled text scored %.3f; order is not constraining the alignment", scrambled)
	}
}

// TestMatchedTextSpanLocatesBleedInsideALongerSegment is what lets a microphone
// segment be split into the part that is a re-recording and the part that is not.
func TestMatchedTextSpanLocatesBleedInsideALongerSegment(t *testing.T) {
	internal := "Teams usually land on it"
	external := "Yeah, no, I really like that. Teams usually land on it. I'm definitely open to that."
	tokens := Tokenize(external)
	result := Align(Tokenize(internal), tokens)

	start, end, ok := result.MatchedTextSpan()
	if !ok {
		t.Fatal("no matched span found")
	}
	span := external[tokens[start].Start:tokens[end-1].End]
	if span != "Teams usually land on it" {
		t.Errorf("matched span = %q, want the bleed phrase", span)
	}
	if start == 0 {
		t.Error("matched span began at the first token; the prefix should be outside it")
	}
	if end == len(tokens) {
		t.Error("matched span ran to the last token; the suffix should be outside it")
	}
}

// TestInteriorFillerDoesNotFragmentTheSpan covers why callers use MatchedTextSpan
// instead of iterating blocks: a word the room speaker interjected mid-phrase
// splits the alignment into two blocks, but the bleed is still one region.
func TestInteriorFillerDoesNotFragmentTheSpan(t *testing.T) {
	internal := "review the config file too"
	external := "could you review the, uh, config file too, if that's okay"
	tokens := Tokenize(external)
	result := Align(Tokenize(internal), tokens)
	start, end, ok := result.MatchedTextSpan()
	if !ok {
		t.Fatal("no matched span found")
	}
	span := external[tokens[start].Start:tokens[end-1].End]
	if !strings.Contains(span, "review the") || !strings.Contains(span, "config file too") {
		t.Errorf("span = %q, want to cover the whole interrupted phrase", span)
	}
}

func TestAlignHandlesDegenerateInput(t *testing.T) {
	if got := Align(nil, Tokenize("anything")).Similarity(); got != 0 {
		t.Errorf("empty pattern similarity = %v, want 0", got)
	}
	if got := Align(Tokenize("anything"), nil).Similarity(); got != 0 {
		t.Errorf("empty text similarity = %v, want 0", got)
	}
	if _, _, ok := Align(nil, nil).MatchedTextSpan(); ok {
		t.Error("empty alignment reported a matched span")
	}
}

// TestAlignRefusesRunawayInput keeps a pathological transcript from allocating a
// vast matrix. Reporting nothing is the right degradation: no blocks means no
// bleed detected, which lands on the conservative side.
func TestAlignRefusesRunawayInput(t *testing.T) {
	huge := make([]Token, MaxAlignTokens+1)
	for i := range huge {
		huge[i] = Token{Norm: "word"}
	}
	result := Align(huge, huge)
	if len(result.Blocks) != 0 || result.Matches != 0 {
		t.Error("oversized input was aligned anyway")
	}
}

func TestBlocksAreInTextOrder(t *testing.T) {
	internal := "alpha beta gamma delta"
	external := "alpha beta something gamma delta"
	result := Align(Tokenize(internal), Tokenize(external))
	if len(result.Blocks) == 0 {
		t.Fatal("no blocks")
	}
	previous := -1
	for i, block := range result.Blocks {
		if block.TextStart < previous {
			t.Errorf("block %d starts at %d, before the previous block ended at %d",
				i, block.TextStart, previous)
		}
		if block.PatternEnd <= block.PatternStart || block.TextEnd <= block.TextStart {
			t.Errorf("block %d is empty: %+v", i, block)
		}
		previous = block.TextEnd
	}
	if result.Matches != 4 {
		t.Errorf("matches = %d, want all 4 pattern tokens", result.Matches)
	}
}
