package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/store"
)

func transcriptRoot(t *testing.T) (string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(context.Background(), filepath.Join(root, "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return root, s
}

// audioChunkWithText inserts one chunk's two tracks and returns the chunk key.
func audioChunkWithText(t *testing.T, s *store.Store, at time.Time, systemText, micText string) string {
	t.Helper()
	ctx := context.Background()
	system := store.Event{Kind: store.KindAudio, CapturedAt: at, Text: systemText,
		MediaPath: filepath.Join(t.TempDir(), "system.wav"), AudioSource: "system", DurationMS: 30000}
	mic := store.Event{Kind: store.KindAudio, CapturedAt: at, Text: micText,
		MediaPath: filepath.Join(t.TempDir(), "microphone.wav"), AudioSource: "microphone", DurationMS: 30000}
	if err := s.Insert(ctx, &system); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, &mic); err != nil {
		t.Fatal(err)
	}
	return store.FormatCapturedAt(at)
}

// runCLISplit keeps the two streams apart, which runCLI deliberately does not.
// A test that merges them cannot tell a faithful export from one with a warning
// printed into the middle of it.
func runCLISplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCommand()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCommand()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// TestBackfillAttributesFromStoredTextAlone is the case that matters for history:
// the WAVs may be long pruned, and attribution still has to work from the two
// transcripts already in the index.
func TestBackfillAttributesFromStoredTextAlone(t *testing.T) {
	root, s := transcriptRoot(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	key := audioChunkWithText(t, s, at,
		"The migration finished cleanly.",
		"The migration finished cleanly. Good, then let us ship it.")

	out, err := runCLI(t, "--data-dir", root, "transcript", "backfill")
	if err != nil {
		t.Fatalf("backfill: %v\n%s", err, out)
	}
	if !strings.Contains(out, "attributed 1 chunks") {
		t.Errorf("unexpected output: %s", out)
	}

	segments, err := s.SegmentsForChunk(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) == 0 {
		t.Fatal("no segments were written")
	}
	var internal, external, bleed int
	for _, segment := range segments {
		switch {
		case segment.IsBleed:
			bleed++
		case segment.Origin == store.OriginInternal:
			internal++
		case segment.Origin == store.OriginExternal:
			external++
		}
	}
	if internal == 0 || external == 0 || bleed == 0 {
		t.Errorf("got internal=%d external=%d bleed=%d; all three should appear", internal, external, bleed)
	}
}

func TestBackfillIsIdempotentAndResumable(t *testing.T) {
	root, s := transcriptRoot(t)
	at := time.Now().UTC().Add(-time.Hour)
	audioChunkWithText(t, s, at, "machine said this", "machine said this and the room replied")

	first, err := runCLI(t, "--data-dir", root, "transcript", "backfill")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "attributed 1 chunks") {
		t.Errorf("first run: %s", first)
	}
	// A second run must find nothing left to do, because the queue is derived
	// from what is unattributed rather than from a stored cursor.
	second, err := runCLI(t, "--data-dir", root, "transcript", "backfill")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "already attributed") {
		t.Errorf("second run did not report an empty queue: %s", second)
	}
	// --force re-derives them.
	forced, err := runCLI(t, "--data-dir", root, "transcript", "backfill", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(forced, "attributed 1 chunks") {
		t.Errorf("--force did not re-attribute: %s", forced)
	}
}

// TestBackfillDryRunWritesNothing follows the `mcp setup --dry-run` precedent.
func TestBackfillDryRunWritesNothing(t *testing.T) {
	root, s := transcriptRoot(t)
	at := time.Now().UTC().Add(-time.Hour)
	key := audioChunkWithText(t, s, at, "machine words", "machine words plus room words")

	out, err := runCLI(t, "--data-dir", root, "transcript", "backfill", "--dry-run", "--explain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dry run") {
		t.Errorf("output does not identify itself as a dry run: %s", out)
	}
	if !strings.Contains(out, "similarity=") {
		t.Errorf("--explain did not report the evidence: %s", out)
	}
	segments, err := s.SegmentsForChunk(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Errorf("a dry run wrote %d segments", len(segments))
	}
}

func TestBackfillLimitStopsEarly(t *testing.T) {
	root, s := transcriptRoot(t)
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := range 4 {
		audioChunkWithText(t, s, base.Add(time.Duration(i)*time.Minute),
			"machine", "machine and room")
	}
	out, err := runCLI(t, "--data-dir", root, "transcript", "backfill", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "attributed 2 chunks") {
		t.Errorf("--limit was not respected: %s", out)
	}
	remaining, err := s.ChunksMissingSegments(context.Background(), nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Errorf("%d chunks left unattributed, want 2", len(remaining))
	}
}

func TestTranscriptPrintsOrderedTurnsAndFlagsGaps(t *testing.T) {
	root, s := transcriptRoot(t)
	at := time.Now().UTC().Add(-30 * time.Minute)
	// The queue runs newest-first, so an interrupted backfill leaves the recent
	// past attributed. --limit 1 therefore picks the later chunk; the earlier one
	// stays a hole.
	audioChunkWithText(t, s, at, "", "never attributed")
	audioChunkWithText(t, s, at.Add(time.Minute),
		"the machine spoke first", "the machine spoke first then the room answered")

	if _, err := runCLI(t, "--data-dir", root, "transcript", "backfill", "--limit", "1"); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "--data-dir", root, "transcript", "--since", "1h")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "internal") || !strings.Contains(out, "external") {
		t.Errorf("transcript does not label origins: %s", out)
	}
	if !strings.Contains(out, "backfill") {
		t.Errorf("a transcript with holes did not say so: %s", out)
	}
}

// TestTranscriptJSONIsABareArray matches `lumi search --json`: the export is the
// data, not a wrapper around it.
func TestTranscriptJSONIsABareArray(t *testing.T) {
	root, s := transcriptRoot(t)
	at := time.Now().UTC().Add(-10 * time.Minute)
	audioChunkWithText(t, s, at, "machine line", "machine line and a room line")
	if _, err := runCLI(t, "--data-dir", root, "transcript", "backfill"); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "--data-dir", root, "transcript", "--since", "1h", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var turns []store.TranscriptTurn
	if err := json.Unmarshal([]byte(out), &turns); err != nil {
		t.Fatalf("output is not a bare array: %v\n%s", err, out)
	}
	if len(turns) == 0 {
		t.Fatal("no turns")
	}
	// Confidence must survive the export as a present key on every turn.
	var raw []map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	for i, turn := range raw {
		for _, key := range []string{"origin", "confidence", "order_confidence"} {
			if _, ok := turn[key]; !ok {
				t.Errorf("turn %d is missing %q", i, key)
			}
		}
	}
}

func TestTranscriptRejectsBadFlags(t *testing.T) {
	root, _ := transcriptRoot(t)
	if _, err := runCLI(t, "--data-dir", root, "transcript", "--origin", "remote"); err == nil {
		t.Error("an invalid --origin was accepted")
	}
	if _, err := runCLI(t, "--data-dir", root, "transcript", "--min-confidence", "3"); err == nil {
		t.Error("an out-of-range --min-confidence was accepted")
	}
	if _, err := runCLI(t, "--data-dir", root, "transcript",
		"--since", "2026-07-29T12:00:00Z", "--until", "2026-07-29T11:00:00Z"); err == nil {
		t.Error("an inverted range was accepted")
	}
}

func TestTranscriptOriginFilter(t *testing.T) {
	root, s := transcriptRoot(t)
	at := time.Now().UTC().Add(-10 * time.Minute)
	audioChunkWithText(t, s, at, "machine only words", "machine only words plus the room speaking")
	if _, err := runCLI(t, "--data-dir", root, "transcript", "backfill"); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "--data-dir", root, "transcript", "--since", "1h", "--origin", "external", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var turns []store.TranscriptTurn
	if err := json.Unmarshal([]byte(out), &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 {
		t.Fatal("origin filter returned nothing")
	}
	for _, turn := range turns {
		if turn.Origin != store.OriginExternal {
			t.Errorf("origin filter returned a %s turn", turn.Origin)
		}
	}
}

// TestTranscriptPrintsWhatMinConfidenceRemoved covers the deletion the printed
// turns cannot reveal: a transcript that reads as a whole conversation because
// the threshold removed the other side of it.
//
// The largest attribution penalties fall on microphone turns, so
// --min-confidence sorts by origin as much as by quality. Nothing above the
// counts says a turn was dropped, and the transcript looks the same either way.
func TestTranscriptPrintsWhatMinConfidenceRemoved(t *testing.T) {
	var out strings.Builder
	printTranscript(&out, store.TranscriptResult{
		Turns: []store.TranscriptTurn{{
			Origin: store.OriginInternal, Text: "the far side of the call",
			Confidence: 0.91, OrderConfidence: store.OrderExact,
		}},
		ConfidenceFiltered: map[string]int{store.OriginExternal: 3},
	})
	printed := out.String()
	for _, want := range []string{"min-confidence", "3", store.OriginExternal} {
		if !strings.Contains(printed, want) {
			t.Errorf("printed transcript never mentions %q:\n%s", want, printed)
		}
	}

	// A transcript nothing was removed from stays quiet, so a real removal is
	// never buried in a line that prints every time.
	var quiet strings.Builder
	printTranscript(&quiet, store.TranscriptResult{
		Turns: []store.TranscriptTurn{{
			Origin: store.OriginInternal, Text: "the far side of the call",
			Confidence: 0.91, OrderConfidence: store.OrderExact,
		}},
	})
	if strings.Contains(quiet.String(), "min-confidence") {
		t.Errorf("an unfiltered transcript warns about the threshold:\n%s", quiet.String())
	}
}

// TestTranscriptJSONWarnsOnStderrWithoutBreakingTheArray covers the surface the
// renderer cannot reach: --json encodes result.Turns and never calls
// printTranscript, so a threshold that deleted one side of the conversation left
// no trace anywhere in the output.
//
// The export itself must stay a bare array — it is a faithful dump, and `lumi
// search --json` sets that contract — so the warning goes to stderr, where it
// reaches a person without touching a pipe.
func TestTranscriptJSONWarnsOnStderrWithoutBreakingTheArray(t *testing.T) {
	root, s := transcriptRoot(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	system := store.Event{Kind: store.KindAudio, CapturedAt: at, Text: "the far side of the call",
		MediaPath: filepath.Join(t.TempDir(), "system.wav"), AudioSource: "system", DurationMS: 30000}
	mic := store.Event{Kind: store.KindAudio, CapturedAt: at, Text: "somebody in the room answered",
		MediaPath: filepath.Join(t.TempDir(), "microphone.wav"), AudioSource: "microphone", DurationMS: 30000}
	if err := s.Insert(ctx, &system); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, &mic); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceChunkSegments(ctx, store.FormatCapturedAt(at), []store.Segment{
		{EventID: system.ID, Seq: 0, Origin: store.OriginInternal, SourceTrack: "system",
			Text: "the far side of the call", Confidence: 0.91, OrderConfidence: store.OrderExact},
		{EventID: mic.ID, Seq: 1, Origin: store.OriginExternal, SourceTrack: "microphone",
			Text: "somebody in the room answered", Confidence: 0.36, OrderConfidence: store.OrderExact},
	}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCLISplit(t, "--data-dir", root, "transcript",
		"--since", "1h", "--min-confidence", "0.6", "--json")
	if err != nil {
		t.Fatalf("transcript --json: %v\n%s", err, stderr)
	}

	// Still a bare array, and still parseable by whatever is on the other end.
	var turns []store.TranscriptTurn
	if err := json.Unmarshal([]byte(stdout), &turns); err != nil {
		t.Fatalf("stdout is not a bare array: %v\n%s", err, stdout)
	}
	if len(turns) != 1 || turns[0].Origin != store.OriginInternal {
		t.Fatalf("got %+v, want the internal turn alone", turns)
	}
	if strings.Contains(stdout, "min-confidence") {
		t.Errorf("the warning was written into the export:\n%s", stdout)
	}
	if !strings.Contains(stderr, "1 external turn") {
		t.Errorf("stderr does not say what the threshold removed: %q", stderr)
	}
}

// TestBackfillQueueDrainsForSilentChunks is a regression test for a queue that
// never emptied.
//
// A chunk where nobody spoke and nothing played produces no speech, and used to
// produce no row either — so the derived work queue selected it forever, every
// run re-read its audio, and `lumi transcript` permanently advised a backfill
// that could not change the number it was reporting. Silence is the common case
// in a real index, not an edge one.
func TestBackfillQueueDrainsForSilentChunks(t *testing.T) {
	root, s := transcriptRoot(t)
	at := time.Now().UTC().Add(-time.Hour)
	audioChunkWithText(t, s, at, "", "")

	first, err := runCLI(t, "--data-dir", root, "transcript", "backfill")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "1 hold no speech") {
		t.Errorf("first run does not report the chunk as silent: %s", first)
	}
	second, err := runCLI(t, "--data-dir", root, "transcript", "backfill")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "already attributed") {
		t.Errorf("the queue did not drain: %s", second)
	}

	// And the reader must not claim a gap it has no way to fill.
	view, err := runCLI(t, "--data-dir", root, "transcript", "--since", "2h")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(view, "not attributed yet") {
		t.Errorf("a fully attributed silent range still reports gaps: %s", view)
	}
}

// TestTranscriptReportsTruncationWithAResumePoint pins the CLI half of the
// segment-read ceiling: a transcript that stops short must say so and say where.
func TestTranscriptReportsTruncationWithAResumePoint(t *testing.T) {
	root, s := transcriptRoot(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-8 * time.Hour).Truncate(time.Second)
	for c := range 700 {
		at := base.Add(time.Duration(c) * 30 * time.Second)
		key := audioChunkWithText(t, s, at, "", "room speech")
		events, err := s.AudioEventsAt(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		rows := make([]store.Segment, 0, 30)
		for i := range 30 {
			rows = append(rows, store.Segment{EventID: events[0].ID, Seq: i,
				Origin: store.OriginExternal, SourceTrack: "microphone", Text: "word",
				Confidence: 0.9, OrderConfidence: "sequence"})
		}
		if err := s.ReplaceChunkSegments(ctx, key, rows); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runCLI(t, "--data-dir", root, "transcript", "--since", "9h", "--limit", "1000")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "more audio than one read returns") {
		t.Errorf("a truncated transcript did not say so: %s", out)
	}
	if !strings.Contains(out, "--since") {
		t.Errorf("the truncation notice offers no way to continue: %s", out)
	}
}

// audioChunkWithMetadata inserts one chunk whose tracks carry diagnostics, which
// is the only way to tell a failed recognition from a quiet room after the fact.
func audioChunkWithMetadata(t *testing.T, s *store.Store, at time.Time, metadata string) string {
	t.Helper()
	ctx := context.Background()
	for _, source := range []string{"system", "microphone"} {
		event := store.Event{Kind: store.KindAudio, CapturedAt: at,
			MediaPath: filepath.Join(t.TempDir(), source+".wav"), AudioSource: source,
			DurationMS: 30000, Metadata: json.RawMessage(metadata)}
		if err := s.Insert(ctx, &event); err != nil {
			t.Fatal(err)
		}
	}
	return store.FormatCapturedAt(at)
}

// TestBackfillNeverCallsAFailedTranscriptionSilent is the backfill half of the
// silence gate.
//
// Fixing only the recorder would move the mislabelling rather than remove it:
// the chunk stays on the derived queue, the next backfill re-derives the same
// empty transcripts, and writes the marker the recorder declined to write.
func TestBackfillNeverCallsAFailedTranscriptionSilent(t *testing.T) {
	root, s := transcriptRoot(t)
	at := time.Now().UTC().Add(-time.Hour)
	key := audioChunkWithMetadata(t, s, at,
		`{"audio_source":"microphone","processor_error":"transcribe: recognizer unavailable"}`)

	out, err := runCLI(t, "--data-dir", root, "transcript", "backfill")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "1 hold no speech") {
		t.Errorf("a chunk whose recognition failed was reported as silent: %s", out)
	}
	if !strings.Contains(out, "could not be transcribed") {
		t.Errorf("the backfill does not report the failure it refused to label: %s", out)
	}

	segments, err := s.SegmentsForChunk(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Errorf("a chunk whose recognition failed was attributed as %q", segments[0].Origin)
	}
}

// TestTranscriptNamesChunksNoBackfillCanRecover keeps the reader's advice honest.
//
// A failed chunk sits on the work queue permanently, so the plain "run a
// backfill" notice would point at an action that cannot change the number it is
// complaining about — the same dead end the silence marker was introduced to
// close.
func TestTranscriptNamesChunksNoBackfillCanRecover(t *testing.T) {
	root, s := transcriptRoot(t)
	at := time.Now().UTC().Add(-time.Hour)
	audioChunkWithMetadata(t, s, at,
		`{"audio_source":"microphone","processor_error":"transcribe: recognizer unavailable"}`)

	out, err := runCLI(t, "--data-dir", root, "transcript", "--since", "2h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "could not be transcribed") {
		t.Errorf("the transcript does not name the chunk a backfill cannot recover: %s", out)
	}
}

// audioChunkWithWAVs inserts one chunk whose media actually exists on disk, which
// is what --retranscribe needs before it will read anything.
func audioChunkWithWAVs(t *testing.T, s *store.Store, at time.Time, systemText, micText string) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	for _, track := range []struct{ source, text string }{{"system", systemText}, {"microphone", micText}} {
		path := filepath.Join(dir, track.source+".wav")
		if err := os.WriteFile(path, []byte("fake-wave"), 0o600); err != nil {
			t.Fatal(err)
		}
		event := store.Event{Kind: store.KindAudio, CapturedAt: at, Text: track.text,
			MediaPath: path, AudioSource: track.source, DurationMS: 30000}
		if err := s.Insert(ctx, &event); err != nil {
			t.Fatal(err)
		}
	}
	return store.FormatCapturedAt(at)
}

// TestRetranscribeSuppliesTimingsNeverText is the invariant that keeps segments
// derived from events.
//
// A re-run of recognition is a second opinion about the same audio, not a
// replacement for the transcript already indexed: it sees a possibly newer
// model, whose words may simply differ. Installing them as segment text puts
// phrases in the transcript that are absent from the event and from the search
// index, so a reader could see a sentence that no query can find.
func TestRetranscribeSuppliesTimingsNeverText(t *testing.T) {
	root, s := transcriptRoot(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	key := audioChunkWithWAVs(t, s, at, "", "the deployment finished")

	const rival = "the department finished"
	original := retranscribeChunk
	t.Cleanup(func() { retranscribeChunk = original })
	retranscribeChunk = func(context.Context, string, string) (macosnative.Transcription, error) {
		return macosnative.Transcription{
			Text: rival,
			Segments: []macosnative.SpeechSegment{{StartMS: 0, EndMS: 2000, Text: rival, Confidence: 0.9,
				Runs: []macosnative.SpeechRun{{StartMS: 0, EndMS: 2000, Text: rival, Confidence: 0.9}}}},
		}, nil
	}

	if _, err := runCLI(t, "--data-dir", root, "transcript", "backfill", "--retranscribe"); err != nil {
		t.Fatal(err)
	}

	segments, err := s.SegmentsForChunk(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) == 0 {
		t.Fatal("the chunk gained no segments at all")
	}
	for _, segment := range segments {
		if strings.Contains(segment.Text, "department") {
			t.Errorf("segment %q carries text the event never held", segment.Text)
		}
	}
}

// TestCappedTranscriptPrintsWhereToContinue is the CLI half of paging: a page cut
// by --limit must say where the rest starts, or the dropped turns are simply
// gone as far as the reader is concerned.
func TestCappedTranscriptPrintsWhereToContinue(t *testing.T) {
	root, s := transcriptRoot(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Second)
	for c := range 6 {
		at := base.Add(time.Duration(c) * time.Hour)
		key := audioChunkWithText(t, s, at, "", "room speech")
		events, err := s.AudioEventsAt(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceChunkSegments(ctx, key, []store.Segment{{
			EventID: events[0].ID, Seq: 0, Origin: store.OriginExternal,
			SourceTrack: "microphone", Text: "phrase", Confidence: 0.9,
			OrderConfidence: "sequence"}}); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runCLI(t, "--data-dir", root, "transcript", "--since", "11h", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Stopped at 2 turns") {
		t.Fatalf("the transcript was not capped: %s", out)
	}
	// The offered value must be the third chunk — the first turn the cap dropped —
	// and not the second, which the page already printed.
	// Local, like the CoveredUntil printed beside it — base is UTC here, so
	// Format alone would assert the exact drift this renders away.
	resume := "--since " + base.Add(2*time.Hour).Local().Format(time.RFC3339Nano)
	if !strings.Contains(out, resume) {
		t.Errorf("a capped transcript does not point at the first dropped turn (%s): %s", resume, out)
	}
}
