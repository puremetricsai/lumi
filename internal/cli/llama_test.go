package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/llamacpp"
)

// newLlamaStopTest mirrors newConfigureTest: it builds an app rooted at a fresh
// data dir and returns a runner that executes `llama stop` directly, plus the
// resolved paths so tests can seed/inspect the state file.
func newLlamaStopTest(t *testing.T) (config.Paths, func() (string, error)) {
	t.Helper()
	dataDir := t.TempDir()
	a := &app{dataDir: dataDir}
	paths, err := config.FromRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	run := func() (string, error) {
		cmd := a.llamaStopCommand()
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stdout)
		cmd.SetArgs(nil)
		err := cmd.ExecuteContext(context.Background())
		return stdout.String(), err
	}
	return paths, run
}

func TestLlamaStopNoStateFile(t *testing.T) {
	paths, run := newLlamaStopTest(t)
	// Guard: no state file exists.
	if _, err := os.Stat(paths.LlamaState); !os.IsNotExist(err) {
		t.Fatalf("expected no state file, stat err = %v", err)
	}
	out, err := run()
	if err == nil {
		t.Fatalf("expected an error with no pid file recorded; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no lumi-launched llama-server recorded") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLlamaStopStaleDeadPid(t *testing.T) {
	paths, run := newLlamaStopTest(t)

	// Deterministically obtain a pid that is guaranteed dead: spawn a trivial
	// process and wait for it to exit. We must NEVER write our own pid, since
	// stop would SIGTERM the test runner.
	proc := exec.Command("true")
	if err := proc.Run(); err != nil {
		t.Fatalf("run trivial process: %v", err)
	}
	deadPid := proc.Process.Pid
	if processAlive(deadPid) {
		t.Skipf("pid %d unexpectedly still alive; cannot exercise the stale-pid branch deterministically", deadPid)
	}

	if err := llamacpp.WriteState(paths.LlamaState, llamacpp.State{PID: deadPid, Model: "some/repo"}); err != nil {
		t.Fatal(err)
	}

	out, err := run()
	if err != nil {
		t.Fatalf("llama stop with a dead pid should not error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "was not running") {
		t.Fatalf("expected output to note the process was not running; got:\n%s", out)
	}
	// The state file must be removed after stop.
	if _, statErr := os.Stat(paths.LlamaState); !os.IsNotExist(statErr) {
		t.Fatalf("expected state file to be removed, stat err = %v", statErr)
	}
}

// A server launched by the pid-file-only build must still be stoppable.
func TestLlamaStopAdoptsLegacyPidFile(t *testing.T) {
	paths, run := newLlamaStopTest(t)
	proc := exec.Command("true")
	if err := proc.Run(); err != nil {
		t.Fatalf("run trivial process: %v", err)
	}
	deadPid := proc.Process.Pid
	if processAlive(deadPid) {
		t.Skipf("pid %d unexpectedly still alive", deadPid)
	}
	legacy := filepath.Join(paths.Root, "llama-server.pid")
	if err := os.WriteFile(legacy, []byte(strconv.Itoa(deadPid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run()
	if err != nil {
		t.Fatalf("llama stop with a legacy pid file: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, strconv.Itoa(deadPid)) {
		t.Fatalf("expected the adopted pid in the output; got:\n%s", out)
	}
	if _, statErr := os.Stat(legacy); !os.IsNotExist(statErr) {
		t.Fatalf("expected the legacy pid file to be removed, stat err = %v", statErr)
	}
}
