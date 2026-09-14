package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/retention"
	"github.com/puremetricsai/lumi/internal/store"
)

// newPruneTest returns the data dir and a runner that executes the prune
// subcommand directly (bypassing root's platform.Validate) with the given
// stdin. The runner returns the combined stdout+stderr so existing callers can
// assert on either stream; use runPruneStreams when the split matters.
func newPruneTest(t *testing.T) (string, func(stdin string, args ...string) (string, error)) {
	t.Helper()
	dataDir, runStreams := newPruneStreamTest(t)
	run := func(stdin string, args ...string) (string, error) {
		stdout, stderr, err := runStreams(stdin, args...)
		return stdout + stderr, err
	}
	return dataDir, run
}

// newPruneStreamTest is like newPruneTest but keeps stdout and stderr separate,
// so a test can prove that machine-readable results (stdout) are not polluted
// by interactive confirmation text (stderr).
func newPruneStreamTest(t *testing.T) (string, func(stdin string, args ...string) (string, string, error)) {
	t.Helper()
	dataDir := t.TempDir()
	a := &app{dataDir: dataDir}
	run := func(stdin string, args ...string) (string, string, error) {
		cmd := a.pruneCommand()
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return stdout.String(), stderr.String(), err
	}
	return dataDir, run
}

// seedEvent inserts one screen event with a real media file into the store at
// the data dir's database, so a prune has both a row and a file to delete.
func seedEvent(t *testing.T, dataDir, name string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(dataDir, "lumi.db"), nil)
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
	s, err := store.Open(ctx, filepath.Join(dataDir, "lumi.db"), nil)
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

// TestPruneAllJSONKeepsConfirmationOffStdout proves that when `--all --json` is
// confirmed interactively (stdin "yes", no --yes), stdout carries only the
// machine-readable JSON result and the human-facing confirmation banner/prompt
// goes to stderr. The prune result is written to the process's real os.Stdout,
// so the test captures that file descriptor rather than the cobra out buffer.
func TestPruneAllJSONKeepsConfirmationOffStdout(t *testing.T) {
	dataDir := t.TempDir()
	seedEvent(t, dataDir, "a.jpg")
	seedEvent(t, dataDir, "b.jpg")

	realStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = realStdout })

	captured := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		captured <- string(data)
	}()

	a := &app{dataDir: dataDir}
	cmd := a.pruneCommand()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetIn(strings.NewReader("yes\n"))
	cmd.SetArgs([]string{"--all", "--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		os.Stdout = realStdout
		w.Close()
		t.Fatal(err)
	}
	os.Stdout = realStdout
	w.Close()
	stdout := <-captured

	// stdout must be exactly the JSON result: parseable into retention.Result
	// and nothing else (no banner, no prompt).
	var result retention.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not clean JSON: %v\nstdout=%q", err, stdout)
	}
	if result.Events != 2 {
		t.Fatalf("expected 2 events wiped in JSON result, got %d", result.Events)
	}
	if strings.Contains(stdout, "Type 'yes'") || strings.Contains(stdout, "permanently delete") {
		t.Fatalf("confirmation text leaked into stdout: %q", stdout)
	}

	// The banner and prompt must be on stderr.
	if !strings.Contains(stderr.String(), "permanently delete") ||
		!strings.Contains(stderr.String(), "Type 'yes'") {
		t.Fatalf("confirmation banner/prompt should be on stderr, got %q", stderr.String())
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
