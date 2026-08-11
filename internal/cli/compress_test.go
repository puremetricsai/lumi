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

	"github.com/puremetricsai/lumi/internal/compress"
	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/store"
)

// newCompressTest runs compressCommand directly, bypassing root's
// PersistentPreRunE so these do not need Apple Silicon, the way prune's tests do.
func newCompressTest(t *testing.T) (string, func(args ...string) (string, string, error)) {
	t.Helper()
	dataDir := t.TempDir()
	a := &app{dataDir: dataDir}
	run := func(args ...string) (string, string, error) {
		cmd := a.compressCommand()
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return stdout.String(), stderr.String(), err
	}
	return dataDir, run
}

func compressPaths(t *testing.T, dataDir string) config.Paths {
	t.Helper()
	paths, err := config.FromRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	return paths
}

// seedScreenEvent indexes one screen event with a real file, old enough that the
// default cutoff includes it.
func seedScreenEvent(t *testing.T, dataDir, name string) string {
	t.Helper()
	ctx := context.Background()
	paths := compressPaths(t, dataDir)
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(paths.Screenshots, name)
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	event := store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC().Add(-72 * time.Hour),
		Text: name, MediaPath: path,
	}
	if err := s.Insert(ctx, &event); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompressRejectsHEVCByName(t *testing.T) {
	_, run := newCompressTest(t)
	_, _, err := run("--screens", "hevc")
	if err == nil {
		t.Fatal("--screens hevc was accepted")
	}
	// It has to read as "not yet", not as a typo, or the reader goes looking for
	// the right spelling of a thing that does not exist.
	if !strings.Contains(err.Error(), "not available yet") {
		t.Errorf("error does not explain that hevc is a later phase: %v", err)
	}
}

func TestCompressRejectsUnknownCodecs(t *testing.T) {
	_, run := newCompressTest(t)
	for _, args := range [][]string{{"--screens", "webp"}, {"--audio", "opus"}} {
		if _, _, err := run(args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestCompressRejectsAnUnparseableCutoff(t *testing.T) {
	_, run := newCompressTest(t)
	if _, _, err := run("--older-than", "7d"); err == nil {
		t.Error("--older-than 7d was accepted; Go durations have no 'd' unit")
	}
}

func TestCompressRefusesWhileRecording(t *testing.T) {
	dataDir, run := newCompressTest(t)
	paths := compressPaths(t, dataDir)
	// Our own pid is definitely alive.
	if err := writeRecordState(paths, recordState{PID: os.Getpid(), StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	_, _, err := run("--screens", "none", "--audio", "none")
	if err == nil {
		t.Fatal("compress ran while a recorder was live")
	}
	if !strings.Contains(err.Error(), "--while-recording") {
		t.Errorf("the refusal does not name its override: %v", err)
	}
}

func TestCompressRunsWhileRecordingWithTheOverride(t *testing.T) {
	dataDir, run := newCompressTest(t)
	paths := compressPaths(t, dataDir)
	if err := writeRecordState(paths, recordState{PID: os.Getpid(), StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("--screens", "none", "--audio", "none", "--while-recording", "--vacuum=false"); err != nil {
		t.Fatalf("--while-recording did not override the gate: %v", err)
	}
}

// A dry run writes nothing, so the recorder gate does not apply to it.
func TestCompressDryRunIgnoresTheRecordingGate(t *testing.T) {
	dataDir, run := newCompressTest(t)
	paths := compressPaths(t, dataDir)
	if err := writeRecordState(paths, recordState{PID: os.Getpid(), StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("--dry-run", "--screens", "none", "--audio", "none"); err != nil {
		t.Fatalf("a dry run was refused while recording: %v", err)
	}
}

// Two concurrent runs collide on deterministic destination paths, and the loser
// of the compare-and-swap unlinks a file the winner's row already names. The
// lock is what keeps the crash-safety claim true.
func TestCompressRefusesWhileAnotherRunHoldsTheLock(t *testing.T) {
	dataDir, run := newCompressTest(t)
	paths := compressPaths(t, dataDir)
	release, err := lockFile(filepath.Join(paths.Root, compressLockName))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, _, err = run("--screens", "none", "--audio", "none")
	if err == nil {
		t.Fatal("a second compress ran while the lock was held")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// flock is released by the kernel when the holding process dies however
// abruptly, so a lock file left behind by a killed run holds nothing. A pid file
// needed explicit stale-takeover logic here, and that logic was itself racy.
func TestCompressIgnoresALockFileNobodyHolds(t *testing.T) {
	dataDir, run := newCompressTest(t)
	paths := compressPaths(t, dataDir)
	lock := filepath.Join(paths.Root, compressLockName)
	if err := os.WriteFile(lock, []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := run("--screens", "none", "--audio", "none", "--vacuum=false"); err != nil {
		t.Fatalf("an unheld lock file blocked the run: %v", err)
	}
}

// The window a pid file could not close: a lock that exists but has not been
// written yet. flock makes the file's *contents* irrelevant, so an empty lock
// held by a live process still excludes.
func TestCompressRefusesWhileAnEmptyLockIsHeld(t *testing.T) {
	dataDir, run := newCompressTest(t)
	paths := compressPaths(t, dataDir)
	release, err := lockFile(filepath.Join(paths.Root, compressLockName))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, _, err = run("--screens", "none", "--audio", "none")
	if err == nil {
		t.Fatal("a second compress ran while an empty lock was held")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestCompressReleasesTheLock(t *testing.T) {
	dataDir, run := newCompressTest(t)
	if _, _, err := run("--screens", "none", "--audio", "none", "--vacuum=false"); err != nil {
		t.Fatal(err)
	}
	paths := compressPaths(t, dataDir)
	if _, err := os.Stat(filepath.Join(paths.Root, compressLockName)); !os.IsNotExist(err) {
		t.Fatal("the lock survived the run, so the next one would refuse")
	}
	// Which means a second run works.
	if _, _, err := run("--screens", "none", "--audio", "none", "--vacuum=false"); err != nil {
		t.Fatalf("a second run was refused: %v", err)
	}
}

// The result goes to the process stdout, matching prune, so --json has to be
// captured off the real descriptor rather than a cobra buffer.
func TestCompressJSONIsTheOnlyThingOnStdout(t *testing.T) {
	dataDir := t.TempDir()
	seedScreenEvent(t, dataDir, "frame.mov") // skipped, so no encoder runs
	a := &app{dataDir: dataDir}

	realStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = realStdout })
	captured := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		captured <- string(data)
	}()

	cmd := a.compressCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--json", "--vacuum=false"})
	runErr := cmd.ExecuteContext(context.Background())
	os.Stdout = realStdout
	writer.Close()
	stdout := <-captured
	if runErr != nil {
		t.Fatal(runErr)
	}

	var result compress.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not the JSON result alone: %v\n%s", err, stdout)
	}
	if result.Screens.Skipped != 1 {
		t.Errorf("reported %d skipped, want 1", result.Screens.Skipped)
	}
}

func TestCompressReportsAnEmptyIndex(t *testing.T) {
	_, run := newCompressTest(t)
	if _, _, err := run("--screens", "none", "--audio", "none", "--vacuum=false"); err != nil {
		t.Fatalf("an empty index failed: %v", err)
	}
}
