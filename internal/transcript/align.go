package transcript

import (
	"strings"
	"unicode"
)

// MaxAlignTokens bounds the alignment matrix. A 30-second chunk transcribes to
// roughly 120 tokens, so this is a runaway guard rather than a real limit; past
// it, Align reports no blocks instead of allocating a huge matrix.
//
// The number has to be one the guard can honour. Six matrices scale with the
// product of the two token counts, so the ceiling costs about 45n² bytes at the
// limit: 1000 buys a 45 MB worst case against an expected 120, while the 4000
// this started at would have allocated over 700 MB — a limit that permits the
// runaway it names.
const MaxAlignTokens = 1000

// Token is one word of a transcript, normalized for comparison but carrying the
// byte offsets it came from.
//
// The offsets are what let every emitted segment's text be *sliced out of the
// verbatim transcript* rather than rebuilt from normalized tokens. Rebuilt text
// would lose the recognizer's own casing, punctuation, and spacing, and a stored
// transcript nobody can quote verbatim is worth much less.
type Token struct {
	Norm  string
	Start int // byte offset of the token in the source string
	End   int
}

// Tokenize splits text into comparable word tokens.
//
// Normalization is lowercasing plus dropping everything that is not a letter or
// digit. It deliberately does not do Unicode NFKC folding: that needs
// golang.org/x/text, this module carries no such dependency, and recognizer
// output is already normalized prose rather than arbitrary user input.
//
// Trimming is not incidental. Run text from the speech bridge carries its own
// leading whitespace — " Yeah.", " I" — because that is what makes the runs
// concatenate into readable prose. Tokenizing without trimming would attach that
// space to every token.
func Tokenize(text string) []Token {
	tokens := make([]Token, 0, len(text)/5)
	start := -1
	var builder strings.Builder
	for offset, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = offset
				builder.Reset()
			}
			builder.WriteRune(unicode.ToLower(r))
			continue
		}
		if start >= 0 {
			tokens = append(tokens, Token{Norm: builder.String(), Start: start, End: offset})
			start = -1
		}
	}
	if start >= 0 {
		tokens = append(tokens, Token{Norm: builder.String(), Start: start, End: len(text)})
	}
	return tokens
}

// Block is one contiguous run of aligned tokens: pattern[PatternStart:PatternEnd]
// against text[TextStart:TextEnd], with Matches counting how many of those pairs
// were identical.
type Block struct {
	PatternStart, PatternEnd int
	TextStart, TextEnd       int
	Matches                  int
}

// Alignment is the result of aligning an internal-track transcript into an
// external-track one.
type Alignment struct {
	// Blocks are the contiguous aligned runs, in text order.
	Blocks []Block
	// Matches is how many pattern tokens found an identical partner.
	Matches int
	// PatternLen is len(pattern), kept so Similarity needs no second argument.
	PatternLen int
}

// Similarity is the fraction of the pattern found, in order, inside the text.
//
// It is deliberately a match ratio rather than a normalized alignment score.
// Both would rank candidates the same way, but a ratio means a calibrated
// threshold keeps its meaning if the gap penalties are ever retuned — "0.6 of the
// machine audio's words reappear in the microphone transcript" is a statement
// about the data, whereas a score threshold is a statement about the scoring.
func (a Alignment) Similarity() float64 {
	if a.PatternLen == 0 {
		return 0
	}
	return float64(a.Matches) / float64(a.PatternLen)
}

// MatchedTextSpan returns the half-open token range of text covered by blocks
// that actually matched something, and whether any did.
//
// Callers splitting a microphone segment into bleed and room speech want this
// envelope rather than the individual blocks: a single filler word the room
// speaker dropped in mid-sentence splits one conceptual match into two blocks,
// and treating those as separate bleed spans would carve the segment into
// fragments that read badly.
func (a Alignment) MatchedTextSpan() (start, end int, ok bool) {
	for _, block := range a.Blocks {
		if block.Matches == 0 {
			continue
		}
		if !ok || block.TextStart < start {
			start = block.TextStart
		}
		if !ok || block.TextEnd > end {
			end = block.TextEnd
		}
		ok = true
	}
	return start, end, ok
}

// Alignment scores. Skipping a text token is cheap because the external track
// legitimately holds speech the internal track never had — that is the whole
// point of the comparison. Skipping a *pattern* token is expensive because every
// word of machine audio should be findable if this really is a re-recording.
const (
	scoreMatch      = 2.0
	scoreMismatch   = -1.0
	textGapOpen     = -2.0
	textGapExtend   = -0.2
	patternGapOpen  = -2.0
	patternGapExtnd = -1.0
)

const negInf = -1e18

// Align computes a semi-global token alignment of pattern into text: leading and
// trailing text is free to skip, interior text is cheap to skip, and pattern
// tokens are expensive to skip.
//
// That asymmetry is the shape of the actual problem. The microphone transcript
// contains everything audible in the room — machine audio bleeding through the
// speakers *and* whoever was talking — while the system transcript contains only
// what the machine played. So the question is never "are these two the same
// text", it is "does all of this shorter thing appear inside that longer one, in
// order". A symmetric measure answers a question nobody asked, and an
// order-blind one (Jaccard) cannot tell a re-recording from a coincidence of
// vocabulary.
//
// Character edit distance is also wrong here: recognizer drift between the two
// tracks lands on whole words — one real pair transcribed "$52000000 … 1st year"
// against "$52 million … first year" — so comparison has to happen at token
// granularity to survive it.
func Align(pattern, text []Token) Alignment {
	result := Alignment{PatternLen: len(pattern)}
	if len(pattern) == 0 || len(text) == 0 {
		return result
	}
	if len(pattern) > MaxAlignTokens || len(text) > MaxAlignTokens {
		return result
	}

	n, m := len(pattern), len(text)
	// m: pattern[i-1] aligned with text[j-1].
	// x: text[j-1] consumed against a gap (advance j).
	// y: pattern[i-1] consumed against a gap (advance i).
	match := newMatrix(n+1, m+1)
	textGap := newMatrix(n+1, m+1)
	patGap := newMatrix(n+1, m+1)
	// from* record which matrix each cell came from, for traceback.
	fromMatch := newTrace(n+1, m+1)
	fromTextGap := newTrace(n+1, m+1)
	fromPatGap := newTrace(n+1, m+1)

	match[0][0] = 0
	for j := 1; j <= m; j++ {
		// Leading text is free to skip, so an unmatched prefix costs nothing.
		textGap[0][j] = 0
		fromTextGap[0][j] = stateTextGap
	}
	for i := 1; i <= n; i++ {
		patGap[i][0] = patternGapOpen + patternGapExtnd*float64(i-1)
		fromPatGap[i][0] = statePatGap
	}

	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			step := scoreMismatch
			if pattern[i-1].Norm == text[j-1].Norm {
				step = scoreMatch
			}
			match[i][j], fromMatch[i][j] = best3(
				match[i-1][j-1], textGap[i-1][j-1], patGap[i-1][j-1])
			match[i][j] += step

			// Advance j: skip a text token.
			textGap[i][j], fromTextGap[i][j] = best3(
				match[i][j-1]+textGapOpen,
				textGap[i][j-1]+textGapExtend,
				patGap[i][j-1]+textGapOpen)

			// Advance i: skip a pattern token.
			patGap[i][j], fromPatGap[i][j] = best3(
				match[i-1][j]+patternGapOpen,
				textGap[i-1][j]+patternGapOpen,
				patGap[i-1][j]+patternGapExtnd)
		}
	}

	// Trailing text is free to skip, so end anywhere on the last pattern row.
	bestScore, bestJ, bestState := negInf, 0, stateMatch
	for j := 0; j <= m; j++ {
		if match[n][j] > bestScore {
			bestScore, bestJ, bestState = match[n][j], j, stateMatch
		}
		if textGap[n][j] > bestScore {
			bestScore, bestJ, bestState = textGap[n][j], j, stateTextGap
		}
		if patGap[n][j] > bestScore {
			bestScore, bestJ, bestState = patGap[n][j], j, statePatGap
		}
	}

	result.Blocks, result.Matches = traceback(
		pattern, text, n, bestJ, bestState,
		fromMatch, fromTextGap, fromPatGap)
	return result
}

type state uint8

const (
	stateMatch state = iota
	stateTextGap
	statePatGap
)

func best3(a, b, c float64) (float64, state) {
	value, from := a, stateMatch
	if b > value {
		value, from = b, stateTextGap
	}
	if c > value {
		value, from = c, statePatGap
	}
	return value, from
}

// traceback walks the recorded predecessors back to the origin, collecting
// maximal runs of aligned pairs. Runs are emitted in text order.
func traceback(pattern, text []Token, i, j int, current state,
	fromMatch, fromTextGap, fromPatGap [][]state,
) ([]Block, int) {
	var blocks []Block
	totalMatches := 0

	// The run currently being accumulated, in reverse.
	open := false
	var run Block

	closeRun := func() {
		if open {
			blocks = append(blocks, run)
			open = false
		}
	}

	for i > 0 || j > 0 {
		switch current {
		case stateMatch:
			if i == 0 || j == 0 {
				// Cannot have arrived here legitimately; stop rather than spin.
				closeRun()
				return reverseBlocks(blocks), totalMatches
			}
			// A block is a run of tokens that are actually *equal*, not merely
			// aligned. Diagonal steps through mismatched tokens close the current
			// run instead of extending it: the aligner will happily walk a
			// diagonal of unrelated words when that beats paying gap penalties,
			// and treating such a walk as a match would let a single shared word
			// mark an entire unrelated transcript as a re-recording.
			if pattern[i-1].Norm == text[j-1].Norm {
				totalMatches++
				if !open {
					run = Block{PatternStart: i - 1, PatternEnd: i, TextStart: j - 1, TextEnd: j, Matches: 1}
					open = true
				} else {
					run.PatternStart = i - 1
					run.TextStart = j - 1
					run.Matches++
				}
			} else {
				closeRun()
			}
			next := fromMatch[i][j]
			i, j = i-1, j-1
			current = next
		case stateTextGap:
			closeRun()
			if j == 0 {
				return reverseBlocks(blocks), totalMatches
			}
			next := fromTextGap[i][j]
			j--
			current = next
		case statePatGap:
			closeRun()
			if i == 0 {
				return reverseBlocks(blocks), totalMatches
			}
			next := fromPatGap[i][j]
			i--
			current = next
		}
	}
	closeRun()
	return reverseBlocks(blocks), totalMatches
}

func reverseBlocks(blocks []Block) []Block {
	for left, right := 0, len(blocks)-1; left < right; left, right = left+1, right-1 {
		blocks[left], blocks[right] = blocks[right], blocks[left]
	}
	return blocks
}

func newMatrix(rows, cols int) [][]float64 {
	flat := make([]float64, rows*cols)
	for i := range flat {
		flat[i] = negInf
	}
	out := make([][]float64, rows)
	for i := range out {
		out[i] = flat[i*cols : (i+1)*cols]
	}
	return out
}

func newTrace(rows, cols int) [][]state {
	flat := make([]state, rows*cols)
	out := make([][]state, rows)
	for i := range out {
		out[i] = flat[i*cols : (i+1)*cols]
	}
	return out
}
