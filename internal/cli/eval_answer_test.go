//go:build eval

package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// answerCase grades the model's reply, not the retrieval that fed it.
//
// The reported defect was visible only at this layer: retrieval handed back
// screen records and the model said, fluently and wrongly, "I did not catch any
// audio conversations on the mic today." Grading is deliberately keyword-based
// rather than judged by a second model — the failure mode is a *claim of
// absence* over records that were present, and that is detectable by substring
// far more cheaply and reproducibly than by a judge.
type answerCase struct {
	name     string
	question string

	// mustMention are substrings the answer has to contain, case-insensitively.
	// Keep them to content words that appear in the corpus transcripts, never
	// to phrasing — the model is free to word things however it likes.
	mustMention []string
	// mustNotMention catches the specific lie: denying what was retrieved.
	mustNotMention []string
}

func answerCases() []answerCase {
	return []answerCase{
		{
			// The reported defect. The old answer opened "I did not catch any
			// audio conversations on the mic today".
			name:           "mic question is answered from transcripts, not denied",
			question:       "what conversations have you caught on the mic today?",
			mustMention:    []string{"contract"},
			mustNotMention: []string{"did not catch any", "no audio recordings", "only include screen"},
		},
		{
			name:        "a topical audio question surfaces the right conversation",
			question:    "what did I say about the contract",
			mustMention: []string{"thursday"},
		},
		{
			name:        "a screen question is still answered from screen records",
			question:    "what did I read about postgres partial indexes",
			mustMention: []string{"partial index"},
		},
	}
}

// TestEvalAnswer runs the real configured provider end to end. It is opt-in
// because it needs a reachable backend (llama-server or a Cerebras key) and
// because a model's wording is not a build-breaking contract — it is a signal
// to read. Enable with LUMI_EVAL_LLM=1.
func TestEvalAnswer(t *testing.T) {
	if os.Getenv("LUMI_EVAL_LLM") != "1" {
		t.Skip("set LUMI_EVAL_LLM=1 to grade real model answers (needs a configured provider)")
	}
	ctx := context.Background()
	s := evalCorpus(t)
	since := evalDay
	until := evalDay.Add(24 * time.Hour)

	a := &app{}
	cfg, err := a.loadConfig()
	if err != nil {
		t.Fatalf("load config (run `lumi configure`): %v", err)
	}
	model := cfg.ResolvedModel()
	if cfg.ResolvedProvider() == "llama.cpp" {
		model = cfg.ResolvedLlamaModel()
	}
	ans, err := a.answerer(ctx, model, os.Stderr)
	if err != nil {
		t.Fatalf("build answerer: %v", err)
	}

	var report evalReport
	for _, tc := range answerCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := retrieveContext(ctx, s, tc.question, store.SearchOptions{
				Since: &since, Until: &until, Limit: 50,
			}, true)
			if err != nil {
				t.Fatal(err)
			}
			answer, err := ans.Answer(ctx, tc.question, contextFor(got.events, defaultContextChars))
			if err != nil {
				t.Fatalf("answer: %v", err)
			}
			lower := strings.ToLower(answer)
			var failures []string
			for _, want := range tc.mustMention {
				if !strings.Contains(lower, strings.ToLower(want)) {
					failures = append(failures, "answer never mentions "+want)
				}
			}
			for _, unwanted := range tc.mustNotMention {
				if strings.Contains(lower, strings.ToLower(unwanted)) {
					failures = append(failures, "answer denies retrieved activity: "+unwanted)
				}
			}
			if len(failures) > 0 {
				t.Logf("answer was:\n%s", answer)
			}
			report.record(t, tc.name, "", failures, mediaPaths(got.events))
		})
	}
	report.summarize(t)
}
