//go:build eval

// Package-level note for the eval suite (build tag `eval`, run via `task eval`).
//
// Evals are not unit tests. A unit test pins a function's contract; an eval
// scores end-to-end question answering against a corpus built to contain the
// hazards that actually break it. They are tagged out of `go test ./...`
// because the answer layer needs a live inference provider and because a
// scored report is read, not just checked.
//
// The corpus is fictional on purpose. Real transcripts are private, machine-
// specific, and would make the suite unrunnable on anyone else's laptop — so
// the hazards are reproduced from first principles instead of recorded.
package cli

import (
	"context"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// evalDay anchors the corpus. A fixed wall-clock day keeps the time-window
// evals deterministic; the store compares UTC, so the events are built in the
// local zone and converted on insert.
var evalDay = time.Date(2026, 3, 4, 0, 0, 0, 0, time.Local)

func evalAt(hour, minute int) time.Time {
	return evalDay.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// evalDaysBefore places an event on an earlier calendar day, for the cases that
// grade which *day* a question resolves to. Every existing case pins its own
// [evalDay, evalDay+24h) window, so these rows are invisible to them.
func evalDaysBefore(days, hour, minute int) time.Time {
	return evalDay.AddDate(0, 0, -days).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// evalCorpus is one workday containing every hazard that has produced a wrong
// answer. Each event's media path is its stable ID in eval expectations.
//
// The hazards, in the order they bite:
//
//  1. Self-reference. Lumi indexes the terminal Lumi is running in, so a
//     question typed at the prompt is guaranteed to be in the screen corpus,
//     word for word, and to win a conjunctive match against every transcript.
//  2. Modality words are anti-correlated with their own recordings. "mic" and
//     "microphone" appear in screenshots of Lumi's UI and never in speech.
//  3. Silent media. Most audio chunks carry no transcript; ranked by recency
//     they bury the few that do.
//  4. Junk transcripts. A near-silent chunk transcribes to a single token.
//  5. Vocabulary collisions. "say", "talk" and "sound" appear in questions
//     that are not about audio at all.
//  6. Adjacent days. "the day before yesterday" contains the word "yesterday",
//     so a question about one day is one regex away from being answered out of
//     another. The two earlier days exist to catch that off-by-one from both
//     sides: the right day must come back and the neighbouring day must not.
func evalCorpus(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s := testStore(t)
	insertAll(t, ctx, s,
		// (6) two days before the corpus day — what "the day before yesterday"
		// and "2 days ago" must resolve to.
		store.Event{
			Kind: store.KindScreen, CapturedAt: evalDaysBefore(2, 10, 0), App: "Figma",
			Window:    "Onboarding — wireframes",
			Text:      "onboarding wireframes v3: welcome step, workspace picker, invite teammates",
			MediaPath: "daybefore-figma.jpg", TextSource: "vision", DisplayID: 1,
		},
		store.Event{
			Kind: store.KindAudio, CapturedAt: evalDaysBefore(2, 10, 30), AudioSource: "microphone",
			Text:       "in standup we agreed to cut the invite step from the onboarding flow",
			MediaPath:  "daybefore-standup.wav",
			DurationMS: 30000,
		},
		// The neighbouring day, so losing a day is a visible failure rather than
		// an empty result that could be blamed on the corpus.
		store.Event{
			Kind: store.KindScreen, CapturedAt: evalDaysBefore(1, 14, 0), App: "Arc",
			Window:    "LUMI-214 — Jira",
			Text:      "LUMI-214 retention sweep: prune --all should unlink orphaned media",
			MediaPath: "yesterday-jira.jpg", TextSource: "vision", DisplayID: 1,
		},
		// (1) Lumi's own terminal, echoing the question back at the index.
		store.Event{
			Kind: store.KindScreen, CapturedAt: evalAt(9, 0), App: "Ghostty",
			Window:    "lumi — zsh",
			Text:      `~ lumi ask "what conversations have you caught on the mic today?"` + "\nnote: interpreting the time in your question as the day of 2026-03-04",
			MediaPath: "terminal-echo.jpg", TextSource: "vision", DisplayID: 1,
		},
		store.Event{
			Kind: store.KindScreen, CapturedAt: evalAt(9, 1), App: "Ghostty",
			Window:    "lumi — zsh",
			Text:      "prompt eval time: 7674.22 ms / 16355 tokens\neval time = 3636.97 ms\nllama_perf_context_print: total time",
			MediaPath: "terminal-log.jpg", TextSource: "vision", DisplayID: 1,
		},
		// A genuine screen record with substantive content.
		store.Event{
			Kind: store.KindScreen, CapturedAt: evalAt(9, 5), App: "Arc",
			Window:    "Partial indexes — PostgreSQL Documentation",
			Text:      "Partial indexes\nA partial index is an index built over a subset of a table...\nCREATE INDEX orders_unbilled ON orders (order_nr) WHERE billed is not true;",
			MediaPath: "browser-postgres.jpg", TextSource: "vision", DisplayID: 1,
		},
		// (2)+(3): a real conversation next to a silent chunk from the same instant.
		store.Event{
			Kind: store.KindAudio, CapturedAt: evalAt(9, 30), AudioSource: "microphone",
			Text:       "let's push the contract review to Thursday, I'll send the redlines tonight",
			MediaPath:  "mic-contract.wav",
			DurationMS: 30000,
		},
		store.Event{
			Kind: store.KindAudio, CapturedAt: evalAt(9, 30), AudioSource: "system",
			Text: "", MediaPath: "system-silent.wav", DurationMS: 30000,
		},
		// (4) junk transcript, and (3) more silence.
		store.Event{
			Kind: store.KindAudio, CapturedAt: evalAt(9, 45), AudioSource: "microphone",
			Text: "I", MediaPath: "mic-junk.wav", DurationMS: 30000,
		},
		store.Event{
			Kind: store.KindAudio, CapturedAt: evalAt(9, 50), AudioSource: "microphone",
			Text: "   ", MediaPath: "mic-blank.wav", DurationMS: 30000,
		},
		// System audio: a video the user watched, not something the user said.
		store.Event{
			Kind: store.KindAudio, CapturedAt: evalAt(10, 15), AudioSource: "system",
			Text:       "thirty five students failed a portion of their midterm because they used AI to generate their responses",
			MediaPath:  "system-lecture.wav",
			DurationMS: 30000,
		},
		// (5) a screen record whose natural question contains "say".
		store.Event{
			Kind: store.KindScreen, CapturedAt: evalAt(10, 20), App: "Slack",
			Window:    "#eng — Acme",
			Text:      "deploy freeze until Thursday while we cut the release branch",
			MediaPath: "slack-freeze.jpg", TextSource: "vision", DisplayID: 1,
		},
		store.Event{
			Kind: store.KindAudio, CapturedAt: evalAt(21, 15), AudioSource: "microphone",
			Text:       "alright, good night, we'll talk tomorrow about the pricing page",
			MediaPath:  "mic-evening.wav",
			DurationMS: 30000,
		},
	)
	return s
}

func mediaPaths(events []store.Event) []string {
	paths := make([]string, 0, len(events))
	for _, event := range events {
		paths = append(paths, event.MediaPath)
	}
	return paths
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
