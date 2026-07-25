package cli

import (
	"context"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

func TestQuestionModality(t *testing.T) {
	for _, tc := range []struct {
		name     string
		question string
		want     store.Kind
	}{
		{
			// The reported defect, verbatim.
			name:     "mic question routes to audio",
			question: "what conversations have you caught on the mic today?",
			want:     store.KindAudio,
		},
		{
			name:     "microphone question routes to audio",
			question: "what conversations have you caught on the microphone today?",
			want:     store.KindAudio,
		},
		{
			name:     "spoken-word phrasing routes to audio",
			question: "what did I talk about with the team",
			want:     store.KindAudio,
		},
		{
			name:     "transcript phrasing routes to audio",
			question: "any transcripts from this morning",
			want:     store.KindAudio,
		},
		{
			name:     "a content question stays unrestricted",
			question: "what did I read about postgres indexes yesterday",
			want:     "",
		},
		{
			name:     "a broad overview stays unrestricted",
			question: "what was I doing this afternoon?",
			want:     "",
		},
		{
			// Vetoed: the user is explicitly relating the two modalities, so
			// dropping every screen record would answer a different question.
			name:     "a mixed-modality question stays unrestricted",
			question: "what was on screen while I was talking",
			want:     "",
		},
		{
			// A false positive: the subject is a settings pane, not a
			// recording. Routing is a preference, not a hard filter — it widens
			// back on a real screen term match (TestRetrieveContextWidensOnTermEvidence).
			name:     "audio as a subject noun still routes to audio",
			question: "what audio settings did I change",
			want:     store.KindAudio,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := questionModality(tc.question); got != tc.want {
				t.Fatalf("questionModality(%q) = %q, want %q", tc.question, got, tc.want)
			}
		})
	}
}

// TestAudioModalityWordsAreStopwords protects the property that makes modality
// routing work: a word that selects the audio corpus must never also be an FTS
// term. "microphone" appears in a terminal screenshot of `lumi ask`, never in a
// speech transcript, so leaving one in the term list re-creates the original
// defect the moment someone adds a trigger.
func TestAudioModalityWordsAreStopwords(t *testing.T) {
	for word := range audioModalityWords {
		if _, stop := questionStopwords[word]; !stop {
			t.Errorf("audio modality word %q is not a stopword; it would leak into the FTS query", word)
		}
	}
}

// selfReferentialCorpus mirrors the shape of the real defect: Lumi indexes its
// own terminal, so a question typed into that terminal appears verbatim in the
// screen corpus and wins the all-terms stage against every transcript.
func selfReferentialCorpus(t *testing.T, ctx context.Context, s *store.Store, now time.Time) {
	t.Helper()
	insertAll(t, ctx, s,
		store.Event{
			Kind: store.KindScreen, CapturedAt: now, App: "Ghostty",
			Text:      `lumi ask "what conversations have you caught on the mic today?"`,
			MediaPath: "terminal.jpg", TextSource: "vision",
		},
		store.Event{
			Kind: store.KindAudio, CapturedAt: now.Add(time.Second), AudioSource: "microphone",
			Text: "yeah I think we should ship the retrieval fix before the demo", MediaPath: "a.wav",
		},
		store.Event{
			Kind: store.KindAudio, CapturedAt: now.Add(2 * time.Second), AudioSource: "system",
			Text: "", MediaPath: "silent.wav",
		},
	)
}

// TestRetrieveContextSelfReferentialTerminal is the regression test for the
// reported defect: asking about the mic must not be answered from a screenshot
// of the question.
func TestRetrieveContextSelfReferentialTerminal(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	selfReferentialCorpus(t, ctx, s, now)

	got, err := retrieveContext(ctx, s, "what conversations have you caught on the mic today?",
		store.SearchOptions{Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != store.KindAudio {
		t.Fatalf("modality = %q, want audio", got.kind)
	}
	if got.widened {
		t.Fatal("retrieval widened despite a matching audio transcript")
	}
	if len(got.events) == 0 {
		t.Fatal("expected the mic transcript to be retrieved")
	}
	for _, event := range got.events {
		if event.Kind != store.KindAudio {
			t.Fatalf("retrieved a %s event (%q); the question was about the microphone", event.Kind, event.Text)
		}
		if event.Text == "" {
			t.Fatalf("retrieved a transcript-less audio event (%q); it carries no answer", event.MediaPath)
		}
	}
}

// TestRetrieveContextWidensOnTermEvidence covers the false-positive escape
// hatch: a misread modality ("what audio settings did I change" is about a
// settings pane, not a recording) widens back to the full corpus — but only
// because a screen event actually matches the content terms. Widening is
// earned by term evidence, never by an empty audio corpus alone (see
// TestRetrieveContextPureModalityKeepsEmptyCorpus for the other side).
func TestRetrieveContextWidensOnTermEvidence(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	insertAll(t, ctx, s,
		store.Event{
			Kind: store.KindScreen, CapturedAt: now, App: "System Settings",
			Text: "audio output settings — remember to change the input level", MediaPath: "settings.jpg",
		},
	)

	got, err := retrieveContext(ctx, s, "what audio settings did I change", store.SearchOptions{Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.widened {
		t.Fatal("expected the audio restriction to widen on a real screen term match")
	}
	if got.stage != stageAllTerms {
		t.Fatalf("widening must ride a term match, stage = %q", got.stage)
	}
	if len(got.events) != 1 || got.events[0].Kind != store.KindScreen {
		t.Fatalf("expected the screen event after widening, got %+v", got.events)
	}
}

// TestRetrieveContextPureModalityKeepsEmptyCorpus is the P2 regression. When a
// question's only signal is the modality ("anything on the mic today?") and the
// audio corpus holds nothing transcribable, the answer must stay empty within
// that corpus, not silently reach for whatever screen was captured recently.
// There is no term evidence the modality reading was wrong, so substituting
// screens would answer a mic question from a screenshot.
func TestRetrieveContextPureModalityKeepsEmptyCorpus(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	insertAll(t, ctx, s,
		store.Event{
			Kind: store.KindScreen, CapturedAt: now, App: "Ghostty",
			Text: "unrelated terminal output about a go build", MediaPath: "screen.jpg",
		},
		store.Event{
			Kind: store.KindAudio, CapturedAt: now.Add(time.Second), AudioSource: "system",
			Text: "", MediaPath: "silent.wav",
		},
	)

	got, err := retrieveContext(ctx, s, "anything on the mic today?", store.SearchOptions{Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.widened {
		t.Fatal("a pure-modality question must not widen to screens on an empty audio corpus")
	}
	if got.kind != store.KindAudio || !got.derivedKind {
		t.Fatalf("expected a derived audio corpus, got kind=%q derived=%v", got.kind, got.derivedKind)
	}
	for _, event := range got.events {
		if event.Kind == store.KindScreen {
			t.Fatalf("mic question answered from a screen record %q", event.MediaPath)
		}
	}
}

// TestRetrieveContextInferenceOptOut is the retrieval-level half of the P1 fix:
// with inference disabled (the caller's --type all), a modality-worded question
// searches every corpus instead of being re-narrowed to audio.
func TestRetrieveContextInferenceOptOut(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	selfReferentialCorpus(t, ctx, s, now)

	got, err := retrieveContext(ctx, s, "what conversations have you caught on the mic today?",
		store.SearchOptions{Limit: 10}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != "" || got.derivedKind {
		t.Fatalf("inference opt-out must leave the corpus unrestricted, got kind=%q derived=%v", got.kind, got.derivedKind)
	}
	sawScreen := false
	for _, event := range got.events {
		if event.Kind == store.KindScreen {
			sawScreen = true
		}
	}
	if !sawScreen {
		t.Fatal("searching everything should reach the screen records the router would have hidden")
	}
}

// TestRetrieveContextExplicitKindWins keeps `--type` authoritative: a user who
// names the corpus must not have it re-derived from their wording.
func TestRetrieveContextExplicitKindWins(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	selfReferentialCorpus(t, ctx, s, now)

	got, err := retrieveContext(ctx, s, "what conversations have you caught on the mic today?",
		store.SearchOptions{Kind: store.KindScreen, Limit: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != store.KindScreen {
		t.Fatalf("modality = %q, want the explicit screen kind", got.kind)
	}
	if len(got.events) != 1 || got.events[0].Kind != store.KindScreen {
		t.Fatalf("expected the screen event, got %+v", got.events)
	}
}
