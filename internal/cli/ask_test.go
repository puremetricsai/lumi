package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/store"
)

type fakeAnswerer struct {
	model           string
	question        string
	activityContext string
}

func (f *fakeAnswerer) Answer(_ context.Context, question, activityContext string) (string, error) {
	f.question = question
	f.activityContext = activityContext
	return "fake answer", nil
}

// newAskTest wires an ask command against a temporary data directory seeded
// with events, and a fake answerer so nothing touches the network.
func newAskTest(t *testing.T, events ...store.Event) (*fakeAnswerer, string, func(args ...string) (string, string, error)) {
	t.Helper()
	dataDir := t.TempDir()

	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(dataDir, "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range events {
		if err := s.Insert(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	fake := &fakeAnswerer{}
	a := &app{dataDir: dataDir, newAnswerer: func(model string) answerer {
		fake.model = model
		return fake
	}}

	run := func(args ...string) (string, string, error) {
		// The ask subcommand is executed directly rather than through the root
		// command: root's PersistentPreRunE calls platform.Validate, which
		// would make these tests darwin/arm64-only.
		cmd := a.askCommand()
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		return stdout.String(), stderr.String(), err
	}
	return fake, dataDir, run
}

func TestAskRetrievesInsteadOfFallingBackToRecency(t *testing.T) {
	now := time.Now().UTC()
	events := []store.Event{
		{Kind: store.KindScreen, CapturedAt: now.Add(-time.Hour), Text: "notes on postgres index tuning", MediaPath: "a.jpg"},
	}
	// Bury the relevant event under more newer, irrelevant events than the
	// default --limit of 50, so a recency-only pass cannot reach it. This is
	// what makes the test a genuine regression for the reported defect.
	for i := 0; i < 60; i++ {
		events = append(events, store.Event{
			Kind: store.KindScreen, CapturedAt: now.Add(time.Duration(i) * time.Minute),
			Text: "unrelated inbox chatter", MediaPath: "b.jpg",
		})
	}
	fake, _, run := newAskTest(t, events...)

	_, stderr, err := run("what did I read about postgres")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.activityContext, "postgres index tuning") {
		t.Fatalf("the matching event was not retrieved:\n%s", fake.activityContext)
	}
	if strings.Contains(stderr, "no searchable terms") {
		t.Fatalf("should not have fallen back to recency: %s", stderr)
	}
	if strings.Contains(fake.activityContext, "unrelated inbox chatter") {
		t.Fatal("context is padded with recent-but-irrelevant events")
	}
}

func TestAskReportsRecencyFallback(t *testing.T) {
	fake, _, run := newAskTest(t, store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC(), Text: "roadmap review", MediaPath: "a.jpg",
	})

	stdout, stderr, err := run("what was I doing?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "broad question") {
		t.Fatalf("recency fallback must not be silent, stderr was %q", stderr)
	}
	if !strings.Contains(stdout, "fake answer") {
		t.Fatalf("answer not printed to stdout: %q", stdout)
	}
	if fake.activityContext == "" {
		t.Fatal("the model must never be handed an empty context")
	}
}

func TestAskReportsUnmatchedTermsAccurately(t *testing.T) {
	_, _, run := newAskTest(t, store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC(), Text: "roadmap review", MediaPath: "a.jpg",
	})

	_, stderr, err := run("what did I read about kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "no events matched the question terms") {
		t.Fatalf("unmatched-term fallback was misreported: %q", stderr)
	}
	if strings.Contains(stderr, "no searchable terms") {
		t.Fatalf("real query terms were incorrectly called unsearchable: %q", stderr)
	}
}

func TestAskPartialMatchIsSilent(t *testing.T) {
	_, _, run := newAskTest(t, store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC(), Text: "postgres index tuning", MediaPath: "a.jpg",
	})

	_, stderr, err := run("what did I read about postgres and kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "best partial match") {
		t.Fatalf("partial-match stage must not be reported, stderr was %q", stderr)
	}
}

func TestAskDerivesWindowFromQuestion(t *testing.T) {
	now := time.Now().UTC()
	fake, _, run := newAskTest(t,
		store.Event{Kind: store.KindScreen, CapturedAt: now.Add(-90 * time.Minute), Text: "budget spreadsheet review", MediaPath: "a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: now.Add(-10 * time.Hour), Text: "morning standup notes", MediaPath: "b.jpg"},
	)

	_, stderr, err := run("what was I doing in the last 2 hours")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "interpreting the time in your question") {
		t.Fatalf("time window must be surfaced, stderr was %q", stderr)
	}
	if !strings.Contains(fake.activityContext, "budget spreadsheet review") {
		t.Fatalf("in-window event was not retrieved:\n%s", fake.activityContext)
	}
	if strings.Contains(fake.activityContext, "morning standup notes") {
		t.Fatalf("event outside the derived window leaked in:\n%s", fake.activityContext)
	}
}

func TestAskExplicitSinceSkipsWindowDerivation(t *testing.T) {
	now := time.Now().UTC()
	_, _, run := newAskTest(t,
		store.Event{Kind: store.KindScreen, CapturedAt: now.Add(-90 * time.Minute), Text: "budget spreadsheet review", MediaPath: "a.jpg"},
	)

	_, stderr, err := run("what was I doing in the last 2 hours", "--since", "48h")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "interpreting the time in your question") {
		t.Fatalf("explicit --since must skip natural-time parsing, stderr was %q", stderr)
	}
}

func TestAskEmptyResultNamesActiveFilters(t *testing.T) {
	now := time.Now().UTC()
	// The event is inside the derived window, so the window is not empty — only
	// the --app filter excludes it. The error must not blame the time window
	// alone.
	_, _, run := newAskTest(t,
		store.Event{Kind: store.KindScreen, CapturedAt: now.Add(-30 * time.Minute), App: "Ghostty", Text: "roadmap review", MediaPath: "a.jpg"},
	)

	_, _, err := run("what was I doing in the last 2 hours", "--app", "Safari")
	if err == nil {
		t.Fatal("expected an error when the filter excludes all in-window activity")
	}
	if !strings.Contains(err.Error(), `app "Safari"`) {
		t.Fatalf("error must name the app filter, got: %v", err)
	}
	if !strings.Contains(err.Error(), "time window") {
		t.Fatalf("error must still name the time window, got: %v", err)
	}
	if strings.Contains(err.Error(), "has been indexed yet") {
		t.Fatalf("must not claim the store is empty when activity exists in the window: %v", err)
	}
}

// TestAskInfersAudioCorpus is the reported-defect regression at the command
// layer: a mic-worded question must be answered from the transcript, not from
// the terminal screenshot of the question, and must announce the reading.
func TestAskInfersAudioCorpus(t *testing.T) {
	now := time.Now().UTC()
	fake, _, run := newAskTest(t,
		store.Event{Kind: store.KindScreen, CapturedAt: now, App: "Ghostty",
			Text: `lumi ask "what did I say on the mic today"`, MediaPath: "term.jpg", TextSource: "vision"},
		store.Event{Kind: store.KindAudio, CapturedAt: now.Add(time.Second), AudioSource: "microphone",
			Text: "we agreed to ship the fix on friday", MediaPath: "a.wav"},
	)

	_, stderr, err := run("what did I say on the mic today")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "about audio recordings") {
		t.Fatalf("the inferred audio reading must be announced, stderr: %q", stderr)
	}
	if !strings.Contains(fake.activityContext, "ship the fix on friday") {
		t.Fatalf("the mic transcript was not retrieved:\n%s", fake.activityContext)
	}
	if strings.Contains(fake.activityContext, "lumi ask") {
		t.Fatalf("answered from the terminal screenshot of the question:\n%s", fake.activityContext)
	}
}

// TestAskTypeAllOverridesInference is the P1 regression: passing --type all must
// suppress modality inference entirely, so the advertised escape hatch reaches
// every corpus instead of being re-narrowed to audio.
func TestAskTypeAllOverridesInference(t *testing.T) {
	now := time.Now().UTC()
	fake, _, run := newAskTest(t,
		store.Event{Kind: store.KindScreen, CapturedAt: now, App: "Ghostty",
			Text: `lumi ask "what did I say on the mic today"`, MediaPath: "term.jpg", TextSource: "vision"},
		store.Event{Kind: store.KindAudio, CapturedAt: now.Add(time.Second), AudioSource: "microphone",
			Text: "we agreed to ship the fix on friday", MediaPath: "a.wav"},
	)

	_, stderr, err := run("what did I say on the mic today", "--type", "all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "about audio recordings") {
		t.Fatalf("--type all must bypass modality inference, stderr: %q", stderr)
	}
	if !strings.Contains(fake.activityContext, "lumi ask") {
		t.Fatalf("--type all should search everything, including screen records:\n%s", fake.activityContext)
	}
}

// TestAskAudioQuestionDoesNotSubstituteScreens is the P2 regression at the
// command layer: a mic question whose audio corpus holds no transcript must
// report the empty audio corpus, never quietly answer from recent screens.
func TestAskAudioQuestionDoesNotSubstituteScreens(t *testing.T) {
	now := time.Now().UTC()
	fake, _, run := newAskTest(t,
		store.Event{Kind: store.KindScreen, CapturedAt: now.Add(-30 * time.Minute), App: "Ghostty",
			Text: "unrelated terminal output about a go build", MediaPath: "screen.jpg", TextSource: "vision"},
		store.Event{Kind: store.KindAudio, CapturedAt: now.Add(-20 * time.Minute), AudioSource: "system",
			Text: "", MediaPath: "silent.wav"},
	)

	_, _, err := run("anything on the mic in the last 2 hours?")
	if err == nil {
		t.Fatalf("expected an empty-corpus error, but the model was fed:\n%s", fake.activityContext)
	}
	if !strings.Contains(err.Error(), "audio recordings with a transcript") {
		t.Fatalf("error must name the empty audio corpus, got: %v", err)
	}
	if strings.Contains(fake.activityContext, "go build") {
		t.Fatalf("a mic question was answered from an unrelated screen:\n%s", fake.activityContext)
	}
}

func TestAskRespectsMaxContextChars(t *testing.T) {
	var events []store.Event
	now := time.Now().UTC()
	for i := 0; i < 20; i++ {
		events = append(events, store.Event{
			Kind: store.KindScreen, CapturedAt: now.Add(time.Duration(i) * time.Minute),
			Text: strings.Repeat("postgres tuning notes ", 300), MediaPath: "a.jpg",
		})
	}
	fake, _, run := newAskTest(t, events...)

	if _, _, err := run("postgres tuning", "--max-context-chars", "3000"); err != nil {
		t.Fatal(err)
	}
	if len(fake.activityContext) > 3000 {
		t.Fatalf("context was %d bytes, over the 3000 budget", len(fake.activityContext))
	}
	if !strings.Contains(fake.activityContext, "further events omitted") {
		t.Fatal("expected an omission marker")
	}
}

func TestAskModelResolution(t *testing.T) {
	t.Run("built-in default when unconfigured", func(t *testing.T) {
		fake, _, run := newAskTest(t, store.Event{Kind: store.KindScreen, Text: "notes", MediaPath: "a.jpg"})
		if _, _, err := run("notes"); err != nil {
			t.Fatal(err)
		}
		if fake.model != config.DefaultCerebrasModel {
			t.Fatalf("model = %q, want %q", fake.model, config.DefaultCerebrasModel)
		}
	})

	t.Run("config overrides the default", func(t *testing.T) {
		fake, dataDir, run := newAskTest(t, store.Event{Kind: store.KindScreen, Text: "notes", MediaPath: "a.jpg"})
		writeTestConfig(t, dataDir, config.Config{CerebrasModel: "qwen-3-32b"})
		if _, _, err := run("notes"); err != nil {
			t.Fatal(err)
		}
		if fake.model != "qwen-3-32b" {
			t.Fatalf("model = %q, want the configured value", fake.model)
		}
	})

	t.Run("flag overrides the config", func(t *testing.T) {
		fake, dataDir, run := newAskTest(t, store.Event{Kind: store.KindScreen, Text: "notes", MediaPath: "a.jpg"})
		writeTestConfig(t, dataDir, config.Config{CerebrasModel: "qwen-3-32b"})
		if _, _, err := run("notes", "--model", "llama-4-scout"); err != nil {
			t.Fatal(err)
		}
		if fake.model != "llama-4-scout" {
			t.Fatalf("model = %q, want the flag value", fake.model)
		}
	})

	t.Run("llama.cpp provider uses its own configured model", func(t *testing.T) {
		// The test answerer override precedes provider logic, so no llama-server
		// is needed; this asserts the llama.cpp model (not the Cerebras one) is
		// resolved and handed to the backend.
		fake, dataDir, run := newAskTest(t, store.Event{Kind: store.KindScreen, Text: "notes", MediaPath: "a.jpg"})
		writeTestConfig(t, dataDir, config.Config{
			Provider:      config.ProviderLlamaCpp,
			CerebrasModel: "qwen-3-32b",
			LlamaModel:    "ggml-org/gpt-oss-20b-GGUF",
		})
		if _, _, err := run("notes"); err != nil {
			t.Fatal(err)
		}
		if fake.model != "ggml-org/gpt-oss-20b-GGUF" {
			t.Fatalf("model = %q, want the configured llama.cpp model", fake.model)
		}
	})
}

func writeTestConfig(t *testing.T, dataDir string, cfg config.Config) {
	t.Helper()
	if err := config.SaveConfig(filepath.Join(dataDir, config.ConfigFileName), cfg); err != nil {
		t.Fatal(err)
	}
}
