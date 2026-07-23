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
)

func TestReadLlamaPid(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, contents string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cases := []struct {
		name    string
		path    string
		wantPid int
		wantOK  bool
	}{
		{"valid", writeFile("valid.pid", "12345\n"), 12345, true},
		{"no-trailing-newline", writeFile("bare.pid", "678"), 678, true},
		{"surrounding-whitespace", writeFile("spaced.pid", "  42  \n"), 42, true},
		{"missing", filepath.Join(dir, "does-not-exist.pid"), 0, false},
		{"empty", writeFile("empty.pid", ""), 0, false},
		{"whitespace-only", writeFile("ws.pid", "   \n\t"), 0, false},
		{"non-numeric", writeFile("abc.pid", "abc"), 0, false},
		{"zero", writeFile("zero.pid", "0"), 0, false},
		{"negative", writeFile("neg.pid", "-1"), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pid, ok := readLlamaPid(tc.path)
			if pid != tc.wantPid || ok != tc.wantOK {
				t.Fatalf("readLlamaPid(%q) = (%d, %t), want (%d, %t)", tc.path, pid, ok, tc.wantPid, tc.wantOK)
			}
		})
	}
}

// newLlamaStopTest mirrors newConfigureTest: it builds an app rooted at a fresh
// data dir and returns a runner that executes `llama stop` directly, plus the
// resolved paths so tests can seed/inspect the pid file.
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

func TestLlamaStopNoPidFile(t *testing.T) {
	paths, run := newLlamaStopTest(t)
	// Guard: no pid file exists.
	if _, err := os.Stat(paths.LlamaPid); !os.IsNotExist(err) {
		t.Fatalf("expected no pid file, stat err = %v", err)
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

	if err := os.WriteFile(paths.LlamaPid, []byte(strconv.Itoa(deadPid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run()
	if err != nil {
		t.Fatalf("llama stop with a dead pid should not error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "was not running") {
		t.Fatalf("expected output to note the process was not running; got:\n%s", out)
	}
	// The pid file must be removed after stop.
	if _, statErr := os.Stat(paths.LlamaPid); !os.IsNotExist(statErr) {
		t.Fatalf("expected pid file to be removed, stat err = %v", statErr)
	}
}
