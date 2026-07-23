package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// newPruneTest returns the data dir and a runner that executes the prune
// subcommand directly (bypassing root's platform.Validate) with the given
// stdin.
func newPruneTest(t *testing.T) (string, func(stdin string, args ...string) (string, error)) {
	t.Helper()
	dataDir := t.TempDir()
	a := &app{dataDir: dataDir}
	run := func(stdin string, args ...string) (string, error) {
		cmd := a.pruneCommand()
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stdout)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return stdout.String(), err
	}
	return dataDir, run
}

// seedEvent inserts one screen event with a real media file into the store at
// the data dir's database, so a prune has both a row and a file to delete.
func seedEvent(t *testing.T, dataDir, name string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(dataDir, "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(dataDir, name)
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := store.Event{Kind: store.KindScreen, CapturedAt: time.Now().UTC(), Text: name, MediaPath: path}
	if err := s.Insert(ctx, &e); err != nil {
		t.Fatal(err)
	}
}

func remainingEvents(t *testing.T, dataDir string) int {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(dataDir, "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}

func TestPruneAllConfirmedDeletesEverything(t *testing.T) {
	dataDir, run := newPruneTest(t)
	seedEvent(t, dataDir, "a.jpg")
	seedEvent(t, dataDir, "b.jpg")

	if _, err := run("yes\n", "--all"); err != nil {
		t.Fatal(err)
	}
	if got := remainingEvents(t, dataDir); got != 0 {
		t.Fatalf("confirmed --all should delete every row, %d remain", got)
	}
}

func TestPruneAllAbortsWithoutYes(t *testing.T) {
	dataDir, run := newPruneTest(t)
	seedEvent(t, dataDir, "a.jpg")

	out, err := run("no\n", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "aborted") {
		t.Fatalf("declined --all should report it aborted, got %q", out)
	}
	if got := remainingEvents(t, dataDir); got != 1 {
		t.Fatalf("declined --all must not delete rows, %d remain", got)
	}
}

func TestPruneAllYesFlagSkipsPrompt(t *testing.T) {
	dataDir, run := newPruneTest(t)
	seedEvent(t, dataDir, "a.jpg")

	// No stdin at all: --yes must not block on a prompt.
	if _, err := run("", "--all", "--yes"); err != nil {
		t.Fatal(err)
	}
	if got := remainingEvents(t, dataDir); got != 0 {
		t.Fatalf("--all --yes should delete every row, %d remain", got)
	}
}

func TestPruneAllDryRunReportsWithoutDeleting(t *testing.T) {
	dataDir, run := newPruneTest(t)
	seedEvent(t, dataDir, "a.jpg")
	seedEvent(t, dataDir, "b.jpg")

	// Dry run needs no confirmation and must not touch the store.
	if _, err := run("", "--all", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if got := remainingEvents(t, dataDir); got != 2 {
		t.Fatalf("--all --dry-run must not delete rows, %d remain", got)
	}
}
