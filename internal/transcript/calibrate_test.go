package transcript

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"
)

// calibrationPair is one chunk's two transcripts.
type calibrationPair struct {
	CapturedAt string `json:"captured_at"`
	Internal   string `json:"internal"`
	External   string `json:"external"`
}

// TestCalibrateBleedThresholds measures the similarity distribution over real
// captured pairs, which is how BleedSimilarity and CrosstalkSimilarity were
// chosen rather than guessed.
//
// It reads its input from a path given in the environment and is skipped
// otherwise. That is deliberate and not merely convenience: the pairs are real
// captured conversation, and committing them as a fixture would put private
// speech into a source repository that gets pushed. The measured *numbers* belong
// in the repo; the words do not.
//
// Regenerate the extract with:
//
//	sqlite3 -json "file:$HOME/Library/Application Support/Lumi/lumi.db?immutable=1" "
//	  with p as (select captured_at,
//	     max(case when audio_source='system'     then text end) internal,
//	     max(case when audio_source='microphone' then text end) external
//	   from events where kind='audio' group by captured_at having count(*)=2)
//	  select captured_at, internal, external from p
//	  where trim(coalesce(internal,''))<>'' and trim(coalesce(external,''))<>''
//	  order by captured_at;" > /tmp/pairs.json
//
// then run:
//
//	LUMI_CALIBRATION_PAIRS=/tmp/pairs.json go test ./internal/transcript -run Calibrate -v
//
// What it must show is a *bimodal* distribution with an empty valley: pairs where
// the microphone re-recorded the machine cluster near 1.0, pairs where both
// parties spoke at once cluster near 0. If the histogram ever stops being bimodal
// the thresholds are meaningless and the timed path will mislabel too, so this
// prints rather than merely asserts.
func TestCalibrateBleedThresholds(t *testing.T) {
	path := os.Getenv("LUMI_CALIBRATION_PAIRS")
	if path == "" {
		t.Skip("set LUMI_CALIBRATION_PAIRS to a JSON extract of both-track chunks")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pairs []calibrationPair
	if err := json.Unmarshal(raw, &pairs); err != nil {
		t.Fatal(err)
	}
	if len(pairs) == 0 {
		t.Fatal("extract holds no pairs")
	}

	scores := make([]float64, 0, len(pairs))
	// coverage is what fraction of the *microphone* transcript the matched span
	// accounts for. It answers a different question from similarity and both are
	// needed: similarity says whether bleed happened at all, coverage says how
	// much of the microphone track it consumed. A chunk can score 1.0 similarity
	// — every word of machine audio reappears — while covering only a third of
	// the microphone transcript, because the room speaker kept talking. Those are
	// exactly the chunks a segment must be split across rather than labelled
	// wholesale.
	coverage := make([]float64, 0, len(pairs))
	for _, pair := range pairs {
		external := Tokenize(pair.External)
		result := Align(Tokenize(pair.Internal), external)
		scores = append(scores, result.Similarity())
		if start, end, ok := result.MatchedTextSpan(); ok && len(external) > 0 {
			coverage = append(coverage, float64(end-start)/float64(len(external)))
		} else {
			coverage = append(coverage, 0)
		}
	}

	histogram := make([]int, 10)
	for _, score := range scores {
		bucket := int(score * 10)
		if bucket > 9 {
			bucket = 9
		}
		histogram[bucket]++
	}
	t.Logf("%d pairs", len(pairs))
	for i, count := range histogram {
		bar := ""
		for range count {
			bar += "#"
		}
		t.Logf("  %.1f-%.1f  %3d %s", float64(i)/10, float64(i+1)/10, count, bar)
	}

	// How much of the microphone track the bleed accounted for, among chunks where
	// bleed was detected at all. A mode well below 1.0 is the case for splitting.
	partial, whole := 0, 0
	for i, score := range scores {
		if score < BleedSimilarity {
			continue
		}
		if coverage[i] < 0.8 {
			partial++
		} else {
			whole++
		}
	}
	t.Logf("of %d bleed chunks: %d cover <80%% of the microphone transcript (need splitting), %d cover most of it",
		partial+whole, partial, whole)

	sorted := append([]float64(nil), scores...)
	sort.Float64s(sorted)
	quantile := func(q float64) float64 { return sorted[int(q*float64(len(sorted)-1))] }
	t.Logf("min %.3f  p25 %.3f  median %.3f  p75 %.3f  max %.3f",
		sorted[0], quantile(0.25), quantile(0.5), quantile(0.75), sorted[len(sorted)-1])

	// Report the widest empty run of buckets — the valley a threshold should sit
	// in. A calibrated threshold belongs in the middle of the largest gap, not at
	// a round number that happens to look tidy.
	bestStart, bestLen, runStart, runLen := -1, 0, -1, 0
	for i, count := range histogram {
		if count == 0 {
			if runStart < 0 {
				runStart = i
			}
			runLen++
			if runLen > bestLen {
				bestStart, bestLen = runStart, runLen
			}
			continue
		}
		runStart, runLen = -1, 0
	}
	if bestLen > 0 {
		low := float64(bestStart) / 10
		high := float64(bestStart+bestLen) / 10
		t.Logf("widest empty band: %.1f-%.1f, midpoint %.2f", low, high, (low+high)/2)
	} else {
		t.Log("no empty band — the distribution is not cleanly separated")
	}

	// Guard the property the thresholds depend on rather than any specific value:
	// both modes must be populated, or there is nothing to separate.
	low, high := 0, 0
	for _, score := range scores {
		switch {
		case score < CrosstalkSimilarity:
			low++
		case score >= BleedSimilarity:
			high++
		}
	}
	t.Logf("below CrosstalkSimilarity(%.2f): %d, at/above BleedSimilarity(%.2f): %d, between: %d",
		CrosstalkSimilarity, low, BleedSimilarity, high, len(scores)-low-high)
	if low == 0 {
		t.Error("no pair scored as simultaneous speech; CrosstalkSimilarity may be too low")
	}
	if high == 0 {
		t.Error("no pair scored as bleed; BleedSimilarity may be too high")
	}
}

// TestAttributeRealPairs runs the full text-only path over the same extract and
// reports what it decided, which is the end-to-end check the unit tests cannot
// give: synthetic fixtures only prove the branches work on inputs shaped the way
// the author imagined.
//
// Reads its input the same way and for the same reason as
// TestCalibrateBleedThresholds — see that comment.
func TestAttributeRealPairs(t *testing.T) {
	path := os.Getenv("LUMI_CALIBRATION_PAIRS")
	if path == "" {
		t.Skip("set LUMI_CALIBRATION_PAIRS to a JSON extract of both-track chunks")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pairs []calibrationPair
	if err := json.Unmarshal(raw, &pairs); err != nil {
		t.Fatal(err)
	}

	counts := map[Origin]int{}
	var bleed, chunks, emptyChunks int
	var internalChars, externalChars, bleedChars int
	for _, pair := range pairs {
		segments := Attribute(Chunk{
			CapturedAt: time.Now(),
			System:     &Track{Source: "system", Text: pair.Internal},
			Microphone: &Track{Source: "microphone", Text: pair.External},
		}, Options{})
		if len(segments) == 0 {
			emptyChunks++
			continue
		}
		chunks++
		for i, segment := range segments {
			if segment.Seq != i {
				t.Fatalf("chunk %s: seq %d out of order", pair.CapturedAt, segment.Seq)
			}
			counts[segment.Origin]++
			switch {
			case segment.IsBleed:
				bleed++
				bleedChars += len(segment.Text)
			case segment.Origin == OriginInternal:
				internalChars += len(segment.Text)
			case segment.Origin == OriginExternal:
				externalChars += len(segment.Text)
			}
		}
	}

	t.Logf("%d chunks attributed, %d produced nothing", chunks, emptyChunks)
	t.Logf("segments: internal=%d external=%d unknown=%d (of which %d are bleed)",
		counts[OriginInternal], counts[OriginExternal], counts[OriginUnknown], bleed)
	t.Logf("characters kept for a transcript: internal=%d external=%d; bleed excluded=%d",
		internalChars, externalChars, bleedChars)

	if chunks == 0 {
		t.Fatal("no chunk produced any segment")
	}
	// Both sides of a real conversation must survive. If either count collapses
	// to zero the labels are not describing a conversation at all.
	if counts[OriginInternal] == 0 {
		t.Error("no machine audio was labelled internal")
	}
	if counts[OriginExternal] == 0 {
		t.Error("no room speech survived as external")
	}
	// The point of the whole exercise: the machine's words must be excluded from
	// the microphone track so a phrase appears once rather than twice.
	if bleed == 0 {
		t.Error("no bleed was detected across a call recorded through speakers")
	}
}
