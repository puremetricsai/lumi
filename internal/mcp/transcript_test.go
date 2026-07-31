package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

func callTranscript(t *testing.T, ctx context.Context, h *handlers, in getTranscriptInput) getTranscriptOutput {
	t.Helper()
	_, out, err := h.getTranscript(ctx, nil, in)
	if err != nil {
		t.Fatalf("get_transcript: %v", err)
	}
	return out
}

// attributedChunk inserts one chunk's rows plus its segments, and returns the
// chunk time.
func attributedChunk(t *testing.T, ctx context.Context, s *store.Store, at time.Time, segments ...store.Segment) time.Time {
	t.Helper()
	system := store.Event{Kind: store.KindAudio, CapturedAt: at, Text: "machine track",
		MediaPath: "/tmp/system.wav", AudioSource: "system", DurationMS: 30000}
	mic := store.Event{Kind: store.KindAudio, CapturedAt: at, Text: "microphone track",
		MediaPath: "/tmp/microphone.wav", AudioSource: "microphone", DurationMS: 30000}
	if err := s.Insert(ctx, &system); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, &mic); err != nil {
		t.Fatal(err)
	}
	for i := range segments {
		if segments[i].EventID == 0 {
			if segments[i].SourceTrack == "system" {
				segments[i].EventID = system.ID
			} else {
				segments[i].EventID = mic.ID
			}
		}
		segments[i].Seq = i
	}
	if err := s.ReplaceChunkSegments(ctx, store.FormatCapturedAt(at), segments); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestGetTranscriptReturnsOrderedAttributedTurns(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	at := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	started := at.Add(time.Second)
	ended := at.Add(3 * time.Second)

	attributedChunk(t, ctx, s, at,
		store.Segment{Origin: store.OriginInternal, SourceTrack: "system",
			Text: "the invoice went out on Monday", StartedAt: &started, EndedAt: &ended,
			Confidence: 0.92, OrderConfidence: "exact"},
		store.Segment{Origin: store.OriginInternal, SourceTrack: "microphone",
			Text: "the invoice went out on Monday", IsBleed: true,
			Confidence: 0.92, OrderConfidence: "exact"},
		store.Segment{Origin: store.OriginExternal, SourceTrack: "microphone",
			Text: "got it, I will file it today", Confidence: 0.88, OrderConfidence: "exact"},
	)
	h := &handlers{store: s}

	out := callTranscript(t, ctx, h, getTranscriptInput{Since: "1h"})
	if len(out.Turns) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(out.Turns), out.Turns)
	}
	if out.Turns[0].Origin != store.OriginInternal || out.Turns[1].Origin != store.OriginExternal {
		t.Errorf("origins = %s, %s", out.Turns[0].Origin, out.Turns[1].Origin)
	}
	// The machine's phrase must appear once. Appearing twice is the original bug.
	occurrences := 0
	for _, turn := range out.Turns {
		if strings.Contains(turn.Text, "invoice went out") {
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Errorf("the machine's phrase appears %d times, want once", occurrences)
	}
	if len(out.Turns[0].EventIDs) == 0 {
		t.Error("a turn carries no event ids, so its raw track is unreachable")
	}
}

// TestTranscriptTurnAlwaysSerializesConfidence is the omitempty trap, the same
// one TestEventRecordAlwaysSerializesTruncated guards. A turn's trustworthiness
// must be a present key on every row: if doubt arrived as an absence, an agent
// would have to infer it from a missing field.
func TestTranscriptTurnAlwaysSerializesConfidence(t *testing.T) {
	// Zero confidence and the empty order tier are the values an omitempty would
	// silently drop.
	encoded, err := json.Marshal(TranscriptTurnRecord{Origin: store.OriginExternal, Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"confidence", "order_confidence", "truncated", "text_length", "origin"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("%q was dropped from the wire form: %s", key, encoded)
		}
	}
}

func TestGetTranscriptRejectsUnknownOrigin(t *testing.T) {
	ctx := context.Background()
	h := &handlers{store: testStore(t)}
	_, _, err := h.getTranscript(ctx, nil, getTranscriptInput{Origin: "remote"})
	if err == nil {
		t.Fatal("an unrecognized origin was accepted")
	}
	// The message has to teach the vocabulary; the names only make sense once you
	// know two tracks are recorded.
	for _, want := range []string{"internal", "external", "unknown", "microphone"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not explain %q: %v", want, err)
		}
	}
}

func TestGetTranscriptValidatesRangeAndConfidence(t *testing.T) {
	ctx := context.Background()
	h := &handlers{store: testStore(t)}

	if _, _, err := h.getTranscript(ctx, nil, getTranscriptInput{
		Since: "2026-07-29T12:00:00Z", Until: "2026-07-29T11:00:00Z",
	}); err == nil {
		t.Error("an inverted range was accepted")
	}
	over := 1.5
	if _, _, err := h.getTranscript(ctx, nil, getTranscriptInput{MinConfidence: &over}); err == nil {
		t.Error("min_confidence above 1 was accepted")
	}
}

// TestGetTranscriptTurnLimitMatchesTheStore pins that the number the schema
// documents is the number enforced, rather than a second copy that can drift.
func TestGetTranscriptTurnLimitMatchesTheStore(t *testing.T) {
	if got := store.ClampTranscriptTurns(0); got != store.DefaultTranscriptTurns {
		t.Errorf("default = %d, want %d", got, store.DefaultTranscriptTurns)
	}
	if got := store.ClampTranscriptTurns(99999); got != store.MaxTranscriptTurns {
		t.Errorf("clamp = %d, want %d", got, store.MaxTranscriptTurns)
	}
	schema := findToolInputSchema(t, "get_transcript")
	for _, want := range []string{"100", "1000"} {
		if !strings.Contains(schema, want) {
			t.Errorf("max_turns is documented without the limit %s, so an agent cannot know it", want)
		}
	}
}

// TestGetTranscriptNoticeNamesTheBackfillWhenCoverageIsPartial covers the case a
// transcript cannot reveal by itself: the turns look complete, but audio in the
// range was never attributed.
func TestGetTranscriptNoticeNamesTheBackfillWhenCoverageIsPartial(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)

	attributedChunk(t, ctx, s, base,
		store.Segment{Origin: store.OriginExternal, SourceTrack: "microphone",
			Text: "this chunk was attributed", Confidence: 0.9, OrderConfidence: "sequence"})
	// A second chunk with rows but no segments.
	unattributed := store.Event{Kind: store.KindAudio, CapturedAt: base.Add(time.Minute),
		Text: "never attributed", MediaPath: "/tmp/x.wav", AudioSource: "microphone"}
	if err := s.Insert(ctx, &unattributed); err != nil {
		t.Fatal(err)
	}
	h := &handlers{store: s}

	out := callTranscript(t, ctx, h, getTranscriptInput{Since: "1h"})
	if len(out.Turns) == 0 {
		t.Fatal("no turns returned")
	}
	if !strings.Contains(out.Notice, "backfill") {
		t.Errorf("notice does not mention the backfill: %q", out.Notice)
	}
	if !strings.Contains(out.Notice, "gaps") {
		t.Errorf("notice does not warn the transcript has gaps: %q", out.Notice)
	}
}

func TestGetTranscriptEmptyRangeExplainsWhy(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	h := &handlers{store: s}

	// An entirely empty index must say so rather than reading as a quiet hour.
	out := callTranscript(t, ctx, h, getTranscriptInput{Since: "1h"})
	if len(out.Turns) != 0 {
		t.Fatalf("got %d turns from an empty index", len(out.Turns))
	}
	if !strings.Contains(out.Notice, "no events at all") {
		t.Errorf("notice = %q, want it to name the empty index", out.Notice)
	}

	// Audio present but unattributed is a different situation with a different
	// remedy.
	at := time.Now().UTC().Add(-5 * time.Minute)
	event := store.Event{Kind: store.KindAudio, CapturedAt: at, Text: "unattributed",
		MediaPath: "/tmp/y.wav", AudioSource: "microphone"}
	if err := s.Insert(ctx, &event); err != nil {
		t.Fatal(err)
	}
	out = callTranscript(t, ctx, h, getTranscriptInput{Since: "1h"})
	if !strings.Contains(out.Notice, "backfill") {
		t.Errorf("notice = %q, want it to name the backfill", out.Notice)
	}
}

func TestGetTranscriptTruncatesPerTurn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	long := strings.Repeat("word ", 400)
	attributedChunk(t, ctx, s, at,
		store.Segment{Origin: store.OriginExternal, SourceTrack: "microphone",
			Text: long, Confidence: 0.9, OrderConfidence: "sequence"})
	h := &handlers{store: s}

	out := callTranscript(t, ctx, h, getTranscriptInput{Since: "1h"})
	if len(out.Turns) != 1 {
		t.Fatalf("got %d turns", len(out.Turns))
	}
	turn := out.Turns[0]
	if !turn.Truncated {
		t.Error("a long turn was not marked truncated")
	}
	if turn.TextLength != len([]rune(strings.TrimSpace(long))) {
		t.Errorf("text_length = %d, want the full rune count", turn.TextLength)
	}

	uncapped := 0
	out = callTranscript(t, ctx, h, getTranscriptInput{Since: "1h", MaxTextChars: &uncapped})
	if out.Turns[0].Truncated {
		t.Error("max_text_chars: 0 still truncated")
	}
}

func TestSearchEventsPointsAtTranscriptOnlyWhenAudioIsAttributed(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Add(-time.Minute)
	h := &handlers{store: s}

	// Unattributed audio must not advertise a tool that would return nothing.
	event := store.Event{Kind: store.KindAudio, CapturedAt: base, Text: "budget discussion",
		MediaPath: "/tmp/z.wav", AudioSource: "microphone"}
	if err := s.Insert(ctx, &event); err != nil {
		t.Fatal(err)
	}
	out := callSearch(t, ctx, h, searchEventsInput{Query: "budget"})
	if strings.Contains(out.Notice, "get_transcript") {
		t.Errorf("notice offered get_transcript for unattributed audio: %q", out.Notice)
	}

	// attributedChunk's own rows carry "machine track"/"microphone track", so
	// search for those: the notice keys off the *events* a search returned, not
	// off segment text.
	attributedChunk(t, ctx, s, base.Add(time.Second),
		store.Segment{Origin: store.OriginExternal, SourceTrack: "microphone",
			Text: "budget discussion continued", Confidence: 0.9, OrderConfidence: "sequence"})
	out = callSearch(t, ctx, h, searchEventsInput{Query: "track"})
	if !strings.Contains(out.Notice, "get_transcript") {
		t.Errorf("notice does not mention get_transcript for attributed audio: %q", out.Notice)
	}

	// A screen-only result set has nothing to say about transcripts.
	screen := store.Event{Kind: store.KindScreen, CapturedAt: base, Text: "unrelated screen text",
		App: "Safari", MediaPath: "/tmp/s.jpg"}
	if err := s.Insert(ctx, &screen); err != nil {
		t.Fatal(err)
	}
	out = callSearch(t, ctx, h, searchEventsInput{Query: "unrelated", Kind: "screen"})
	if strings.Contains(out.Notice, "get_transcript") {
		t.Errorf("a screen-only search offered get_transcript: %q", out.Notice)
	}
}

// TestGetTranscriptEmptyRangeDistinguishesFiltersFromMissingAttribution is a
// regression test for a notice that blamed the index for the caller's filter.
//
// A fully attributed range that a filter emptied used to be reported as "none
// have been attributed yet; run `lumi transcript backfill`" — advice that cannot
// help, and which costs an agent the one round trip it had to learn otherwise.
func TestGetTranscriptEmptyRangeDistinguishesFiltersFromMissingAttribution(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	h := &handlers{store: s}
	attributedChunk(t, ctx, s, time.Now().UTC().Add(-10*time.Minute), store.Segment{
		Origin: store.OriginInternal, SourceTrack: "system", Text: "Machine speech.",
		Confidence: 0.9, OrderConfidence: "sequence"})

	_, out, err := h.getTranscript(ctx, nil, getTranscriptInput{
		Since: "1h", Origin: store.OriginExternal})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Turns) != 0 {
		t.Fatalf("got %d turns for an origin nothing matches", len(out.Turns))
	}
	if strings.Contains(out.Notice, "backfill") {
		t.Errorf("an attributed range was told to run a backfill: %s", out.Notice)
	}
	if !strings.Contains(out.Notice, "filters") {
		t.Errorf("the notice does not name the filter that emptied the result: %s", out.Notice)
	}
}

// TestGetTranscriptNoticeSeparatesUnrecoverableChunks keeps the agent from being
// sent on an errand that cannot succeed.
//
// A chunk whose recognition failed never gains segments — labelling it would mean
// calling it silent — so it sits on the derived work queue permanently. Telling
// an agent to run a backfill for it produces a loop: the tool reports a gap, the
// agent runs the command, the number does not move.
func TestGetTranscriptNoticeSeparatesUnrecoverableChunks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)

	attributedChunk(t, ctx, s, base,
		store.Segment{Origin: store.OriginExternal, SourceTrack: "microphone",
			Text: "this chunk was attributed", Confidence: 0.9, OrderConfidence: "sequence"})
	failed := store.Event{Kind: store.KindAudio, CapturedAt: base.Add(time.Minute),
		MediaPath: "/tmp/x.wav", AudioSource: "microphone",
		Metadata: json.RawMessage(`{"processor_error":"transcribe: recognizer unavailable"}`)}
	if err := s.Insert(ctx, &failed); err != nil {
		t.Fatal(err)
	}
	h := &handlers{store: s}

	out := callTranscript(t, ctx, h, getTranscriptInput{Since: "1h"})
	if !strings.Contains(out.Notice, "could not be transcribed") {
		t.Errorf("notice does not name the chunk no backfill can recover: %q", out.Notice)
	}
	if strings.Contains(out.Notice, "backfill` to fill them") {
		t.Errorf("notice still advises a backfill that cannot help: %q", out.Notice)
	}
}

// TestGetTranscriptResumePointDoesNotRepeatTheLastTurn is the paging contract an
// agent has no way to discover for itself.
//
// The notice tells it what to pass as since next time, and the segment read is
// inclusive at both ends — so naming the last covered chunk would hand back that
// chunk's turns again on every continuation, and an agent stitching pages
// together would duplicate speech that was said once.
func TestGetTranscriptResumePointDoesNotRepeatTheLastTurn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Second)

	// Distinct text per chunk: turns carry no absolute time on the text path, so
	// the words are the only way to tell one page's turns from another's.
	for c := range 6 {
		attributedChunk(t, ctx, s, base.Add(time.Duration(c)*time.Hour),
			store.Segment{Origin: store.OriginExternal, SourceTrack: "microphone",
				Text: fmt.Sprintf("phrase %d", c), Confidence: 0.9, OrderConfidence: "sequence"})
	}
	h := &handlers{store: s}

	out := callTranscript(t, ctx, h, getTranscriptInput{Since: "11h", MaxTurns: 2})
	if len(out.Turns) != 2 {
		t.Fatalf("got %d turns, want the capped 2", len(out.Turns))
	}
	if out.ResumeFrom == "" {
		t.Fatal("a capped transcript names no resume point, so its dropped turns are unreachable")
	}
	if !strings.Contains(out.Notice, out.ResumeFrom) {
		t.Errorf("the notice does not tell the agent where to continue: %q", out.Notice)
	}

	next := callTranscript(t, ctx, h, getTranscriptInput{Since: out.ResumeFrom, MaxTurns: 2})
	if len(next.Turns) == 0 {
		t.Fatal("resuming returned nothing")
	}
	seen := map[string]bool{}
	for _, turn := range out.Turns {
		seen[turn.Text] = true
	}
	if seen[next.Turns[0].Text] {
		t.Errorf("the page after the cap repeats %q", next.Turns[0].Text)
	}
	// Nor may it skip: the continuation starts at the first turn the cap dropped.
	if next.Turns[0].Text != "phrase 2" {
		t.Errorf("resuming began at %q, want the first dropped turn %q",
			next.Turns[0].Text, "phrase 2")
	}
}
