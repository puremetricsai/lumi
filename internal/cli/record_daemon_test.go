package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
)

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatalf("FromRoot: %v", err)
	}
	return paths
}

func TestRecordStateRoundtrip(t *testing.T) {
	paths := testPaths(t)

	if _, ok, err := readRecordState(paths); err != nil || ok {
		t.Fatalf("readRecordState on missing file = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	want := recordState{
		PID:       4321,
		StartedAt: time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC),
		Screen:    true,
		Audio:     false,
		Args:      []string{"record", "start", "--foreground"},
		Log:       paths.RecordLog,
	}
	if err := writeRecordState(paths, want); err != nil {
		t.Fatalf("writeRecordState: %v", err)
	}
	got, ok, err := readRecordState(paths)
	if err != nil || !ok {
		t.Fatalf("readRecordState = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
	if got.PID != want.PID || !got.StartedAt.Equal(want.StartedAt) || got.Screen != want.Screen ||
		got.Audio != want.Audio || got.Log != want.Log {
		t.Fatalf("readRecordState = %+v, want %+v", got, want)
	}

	if err := removeRecordState(paths); err != nil {
		t.Fatalf("removeRecordState: %v", err)
	}
	if _, ok, _ := readRecordState(paths); ok {
		t.Fatal("state still present after removeRecordState")
	}
	// Removing an already-absent file is a no-op, not an error.
	if err := removeRecordState(paths); err != nil {
		t.Fatalf("removeRecordState on missing file: %v", err)
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive(self) = false, want true")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("processAlive on non-positive pid = true, want false")
	}
	// A process we start and reap is definitively gone afterwards.
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid := sleeper.Process.Pid
	if !processAlive(pid) {
		t.Fatalf("processAlive(%d) = false while running", pid)
	}
	_ = sleeper.Process.Kill()
	_ = sleeper.Wait()
	if processAlive(pid) {
		t.Fatalf("processAlive(%d) = true after kill+wait", pid)
	}
}

func TestStartDetachedProcessPreservesPID(t *testing.T) {
	child := exec.Command("sleep", "30")
	pid, err := startDetachedProcess(child)
	if err != nil {
		t.Fatalf("startDetachedProcess: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("startDetachedProcess returned pid %d, want a positive pid", pid)
	}
	if !processAlive(pid) {
		t.Fatalf("processAlive(%d) = false after detach, want true", pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess(%d): %v", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("stop detached process %d: %v", pid, err)
	}
}

func TestChildArgsForwardsFlags(t *testing.T) {
	args := childArgs("/data", recordFlags{
		interval:     3 * time.Second,
		audioChunk:   45 * time.Second,
		duration:     10 * time.Minute,
		noAudio:      true,
		speechLocale: "en-GB",
		displays:     []uint{7, 12},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"record start --foreground",
		"--data-dir /data",
		"--interval 3s",
		"--audio-chunk 45s",
		"--speech-locale en-GB",
		"--duration 10m0s",
		"--no-audio",
		// Comma-joined, not pflag's own "[7 12]" rendering, which the child
		// would refuse to parse.
		"--displays 7,12",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("childArgs missing %q; got %q", want, joined)
		}
	}
	if strings.Contains(joined, "--no-screen") {
		t.Errorf("childArgs included --no-screen when noScreen is false; got %q", joined)
	}

	// No --data-dir and duration zero: those flags are omitted.
	bare := strings.Join(childArgs("", recordFlags{speechLocale: "en-US"}), " ")
	if strings.Contains(bare, "--data-dir") || strings.Contains(bare, "--duration") {
		t.Errorf("childArgs added optional flags unexpectedly; got %q", bare)
	}
	// An unset selection must forward nothing: the child's own default is
	// "every connected display", and an empty --displays would be a third
	// spelling of it.
	if strings.Contains(bare, "--displays") {
		t.Errorf("childArgs added --displays with no selection; got %q", bare)
	}
}

// runRecordCommand executes `record <args...>` through the real cobra tree with
// a fixed data dir, capturing stdout. It bypasses PersistentPreRunE (platform
// validation) by invoking the record subtree directly.
func runRecordCommand(t *testing.T, dataDir string, args ...string) (string, error) {
	t.Helper()
	a := &app{dataDir: dataDir}
	cmd := a.recordCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestRecordStatusNotRecording(t *testing.T) {
	dir := t.TempDir()
	out, err := runRecordCommand(t, dir, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "not recording") {
		t.Fatalf("status output = %q, want it to contain %q", out, "not recording")
	}
}

func TestRecordStopNoRecorder(t *testing.T) {
	dir := t.TempDir()
	out, err := runRecordCommand(t, dir, "stop")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !strings.Contains(out, "not recording") {
		t.Fatalf("stop output = %q, want it to contain %q", out, "not recording")
	}
}

// TestRecordStatusAndStop drives the status/stop machinery against a dummy
// long-lived process, so it needs no capture permissions.
func TestRecordStatusAndStop(t *testing.T) {
	paths := testPaths(t)
	if err := paths.Ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid := sleeper.Process.Pid
	// Reap in the background so a SIGTERM'd child clears the process table
	// promptly instead of lingering as a zombie that signal-0 reports alive.
	// (The real recorder is detached and reaped by init, not by `stop`.)
	waited := make(chan struct{})
	go func() { _ = sleeper.Wait(); close(waited) }()
	defer func() {
		_ = sleeper.Process.Kill()
		<-waited
	}()

	if err := writeRecordState(paths, recordState{
		PID: pid, StartedAt: time.Now().UTC(), Screen: true, Audio: true, Log: paths.RecordLog,
	}); err != nil {
		t.Fatalf("writeRecordState: %v", err)
	}

	out, err := runRecordCommand(t, paths.Root, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "recording") || strings.Contains(out, "not recording") {
		t.Fatalf("status output = %q, want it to report recording", out)
	}

	out, err = runRecordCommand(t, paths.Root, "stop")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !strings.Contains(out, "recording stopped") {
		t.Fatalf("stop output = %q, want %q", out, "recording stopped")
	}
	if !strings.Contains(out, "gracefully stopping") {
		t.Fatalf("stop output = %q, want %q", out, "gracefully stopping")
	}
	if processAlive(pid) {
		t.Fatalf("process %d still alive after stop", pid)
	}
	if _, ok, _ := readRecordState(paths); ok {
		t.Fatal("state file still present after stop")
	}
}

func TestStopProcessTimeout(t *testing.T) {
	// A process that ignores SIGTERM should trip the timeout path. `sleep`
	// does not ignore SIGTERM, so use a shell that traps and ignores it.
	proc := exec.Command("sh", "-c", "trap '' TERM; sleep 30")
	if err := proc.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := proc.Process.Pid
	defer func() {
		_ = proc.Process.Kill()
		_ = proc.Wait()
	}()
	err := stopProcess(pid, 300*time.Millisecond)
	if err == nil {
		t.Fatal("stopProcess returned nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("stopProcess error = %v, want a timeout error", err)
	}
}
