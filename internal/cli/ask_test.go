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

func TestAskReportsPartialMatch(t *testing.T) {
	_, _, run := newAskTest(t, store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC(), Text: "postgres index tuning", MediaPath: "a.jpg",
	})

	_, stderr, err := run("what did I read about postgres and kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "best partial match") {
		t.Fatalf("partial-match stage must be reported, stderr was %q", stderr)
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
}

func writeTestConfig(t *testing.T, dataDir string, cfg config.Config) {
	t.Helper()
	if err := config.SaveConfig(filepath.Join(dataDir, config.ConfigFileName), cfg); err != nil {
		t.Fatal(err)
	}
}
