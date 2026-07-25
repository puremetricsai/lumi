//go:build eval

package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// retrievalCase scores one question against evalCorpus. Retrieval is graded
// separately from the answer because it is the layer that decides what the
// model can possibly know: a wrong answer built on the right records is a
// prompt problem, and the right answer built on the wrong records is luck.
type retrievalCase struct {
	name     string
	question string

	// wantKind is the corpus the question should be answered from; "" means
	// retrieval must stay unrestricted.
	wantKind store.Kind
	// wantStage, when set, pins which retrieval stage should have produced the
	// events. Leave empty when several are acceptable.
	wantStage retrievalStage
	// wantWidened asserts that an inferred modality was withdrawn.
	wantWidened bool

	// mustInclude and mustExclude are media paths. mustExclude is where the
	// reported defects live: the failures were never "found nothing", they
	// were "found the wrong thing and sounded certain about it".
	mustInclude []string
	mustExclude []string

	// knownGap marks a case that documents behavior we have not built yet. It
	// is reported as GAP and does not fail the suite, so the eval can carry an
	// honest backlog instead of pretending the suite is complete.
	knownGap string
}

func retrievalCases() []retrievalCase {
	return []retrievalCase{
		{
			// The reported defect, verbatim. Answered from a screenshot of
			// itself before the fix.
			name:        "mic conversations are not answered from Lumi's own terminal",
			question:    "what conversations have you caught on the mic today?",
			wantKind:    store.KindAudio,
			mustInclude: []string{"mic-contract.wav", "mic-evening.wav"},
			mustExclude: []string{"terminal-echo.jpg", "terminal-log.jpg", "browser-postgres.jpg"},
		},
		{
			// The second reported phrasing. It produced a *different* wrong
			// answer than the first, which is why both are pinned.
			name:        "microphone phrasing behaves identically to mic",
			question:    "what conversations have you caught on the microphone today?",
			wantKind:    store.KindAudio,
			mustInclude: []string{"mic-contract.wav"},
			mustExclude: []string{"terminal-echo.jpg", "terminal-log.jpg"},
		},
		{
			name:        "transcript-less audio never reaches the model as content",
			question:    "what did I say today",
			wantKind:    store.KindAudio,
			mustExclude: []string{"system-silent.wav", "mic-blank.wav"},
		},
		{
			name:        "a topical audio question matches on terms, not recency",
			question:    "what did I say about the contract",
			wantKind:    store.KindAudio,
			wantStage:   stageAllTerms,
			mustInclude: []string{"mic-contract.wav"},
			mustExclude: []string{"mic-evening.wav", "mic-junk.wav"},
		},
		{
			// "say" is an audio trigger, but the subject is a Slack message.
			// The audio corpus has no term match, so the reading is withdrawn.
			name:        "a modality false positive yields to real term evidence",
			question:    "what did the deploy freeze say",
			wantWidened: true,
			mustInclude: []string{"slack-freeze.jpg"},
		},
		{
			// "read" vetoes the audio router even though nothing else would.
			name:        "a screen-worded question is never routed to audio",
			question:    "what did I read about postgres partial indexes",
			wantKind:    "",
			mustInclude: []string{"browser-postgres.jpg"},
		},
		{
			name:      "a broad question stays unrestricted and admits it",
			question:  "what was I doing today?",
			wantKind:  "",
			wantStage: stageRecent,
		},
		{
			// Modality is the only signal, so recency inside the audio corpus
			// is the answer — not a reason to widen back to screens.
			name:        "a pure modality question keeps its corpus",
			question:    "anything on the mic today?",
			wantKind:    store.KindAudio,
			wantStage:   stageRecent,
			mustExclude: []string{"terminal-echo.jpg", "slack-freeze.jpg"},
		},
		{
			name:     "system audio is distinguished from what the user said",
			question: "what did I say on the mic today",
			wantKind: store.KindAudio,
			// The router narrows to kind=audio but not to audio_source, so a
			// video's soundtrack still reaches a question about the mic and the
			// model may attribute it to the user.
			mustExclude: []string{"system-lecture.wav"},
			knownGap:    "modality routes on kind, not audio_source",
		},
	}
}

// TestEvalRetrieval is the hermetic half of the suite: no provider, no network,
// no permissions. It is the half that must stay green.
func TestEvalRetrieval(t *testing.T) {
	ctx := context.Background()
	s := evalCorpus(t)
	since := evalDay
	until := evalDay.Add(24 * time.Hour)

	var report evalReport
	for _, tc := range retrievalCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := retrieveContext(ctx, s, tc.question, store.SearchOptions{
				Since: &since, Until: &until, Limit: 50,
			}, true)
			if err != nil {
				t.Fatal(err)
			}
			paths := mediaPaths(got.events)
			var failures []string
			if got.kind != tc.wantKind {
				failures = append(failures, fmt.Sprintf("corpus = %q, want %q", got.kind, tc.wantKind))
			}
			if tc.wantStage != "" && got.stage != tc.wantStage {
				failures = append(failures, fmt.Sprintf("stage = %q, want %q", got.stage, tc.wantStage))
			}
			if got.widened != tc.wantWidened {
				failures = append(failures, fmt.Sprintf("widened = %v, want %v", got.widened, tc.wantWidened))
			}
			for _, want := range tc.mustInclude {
				if !contains(paths, want) {
					failures = append(failures, fmt.Sprintf("missing required record %s", want))
				}
			}
			for _, unwanted := range tc.mustExclude {
				if contains(paths, unwanted) {
					failures = append(failures, fmt.Sprintf("retrieved forbidden record %s", unwanted))
				}
			}
			report.record(t, tc.name, tc.knownGap, failures, paths)
		})
	}
	report.summarize(t)
}

// evalReport turns per-case outcomes into a readable scoreboard. Evals are read
// as a report; a bare FAIL line loses the shape of the regression.
type evalReport struct {
	passed, failed, gaps int
	lines                []string
}

func (r *evalReport) record(t *testing.T, name, knownGap string, failures, retrieved []string) {
	t.Helper()
	switch {
	case len(failures) == 0:
		r.passed++
		r.lines = append(r.lines, "PASS  "+name)
	case knownGap != "":
		r.gaps++
		r.lines = append(r.lines, "GAP   "+name+" ("+knownGap+")")
		t.Logf("known gap (%s): %s\n  retrieved: %s", knownGap, strings.Join(failures, "; "), strings.Join(retrieved, ", "))
	default:
		r.failed++
		r.lines = append(r.lines, "FAIL  "+name)
		t.Errorf("%s\n  retrieved: %s", strings.Join(failures, "\n  "), strings.Join(retrieved, ", "))
	}
}

func (r *evalReport) summarize(t *testing.T) {
	t.Helper()
	t.Logf("\n%s\n%d passed, %d failed, %d known gaps",
		strings.Join(r.lines, "\n"), r.passed, r.failed, r.gaps)
}
