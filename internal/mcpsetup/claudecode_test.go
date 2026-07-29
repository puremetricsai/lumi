package mcpsetup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeRunner records invocations instead of running anything.
type fakeRunner struct {
	calls [][]string
	err   error
	out   string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.out, f.err
}

// testSpec is the entry every test in this file drives toward.
func testSpec() Spec {
	return Spec{Name: "lumi", Command: "/abs/lumi", Args: []string{"mcp", "--data-dir", "/abs/root"}}
}

// writeState writes a ~/.claude.json stand-in and returns its path.
func writeState(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return path
}

// newClaudeCode builds a target that never touches the real machine.
func newClaudeCode(statePath string, runner Runner) *ClaudeCode {
	return &ClaudeCode{Runner: runner, CLIPath: "/fake/claude", StatePath: statePath}
}

func TestClaudeCodeAddPassesArgsAfterASeparator(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	target := newClaudeCode(filepath.Join(t.TempDir(), "absent.json"), runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusAdded || !result.Changed {
		t.Fatalf("got %+v, want added and changed", result)
	}

	want := []string{
		"/fake/claude", "mcp", "add", "--scope", "user", "lumi", "--",
		"/abs/lumi", "mcp", "--data-dir", "/abs/root",
	}
	if len(runner.calls) != 1 || !slices.Equal(runner.calls[0], want) {
		t.Fatalf("invocation = %v, want %v", runner.calls, want)
	}
}

func TestClaudeCodeIsUnchangedWhenAlreadyConfigured(t *testing.T) {
	t.Parallel()
	state := writeState(t, `{"mcpServers":{"lumi":{"command":"/abs/lumi",
		"args":["mcp","--data-dir","/abs/root"]}}}`)
	runner := &fakeRunner{}
	target := newClaudeCode(state, runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusUnchanged {
		t.Fatalf("status = %q, want unchanged", result.Status)
	}
	if result.Changed {
		t.Error("Changed = true on a no-op")
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %v, want nothing", runner.calls)
	}
}

// An explicit "stdio" type means the same thing as an absent one, and must not
// register as a difference that demands --force.
func TestClaudeCodeTreatsExplicitStdioTypeAsEquivalent(t *testing.T) {
	t.Parallel()
	state := writeState(t, `{"mcpServers":{"lumi":{"type":"stdio","command":"/abs/lumi",
		"args":["mcp","--data-dir","/abs/root"]}}}`)
	target := newClaudeCode(state, &fakeRunner{})

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusUnchanged {
		t.Fatalf("status = %q, want unchanged", result.Status)
	}
}

func TestClaudeCodeConflictRequiresForce(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"different args":    `{"mcpServers":{"lumi":{"command":"/abs/lumi","args":["mcp"]}}}`,
		"different command": `{"mcpServers":{"lumi":{"command":"lumi","args":["mcp","--data-dir","/abs/root"]}}}`,
		"has env": `{"mcpServers":{"lumi":{"command":"/abs/lumi",
			"args":["mcp","--data-dir","/abs/root"],"env":{"LUMI_HOME":"/x"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeRunner{}
			target := newClaudeCode(writeState(t, body), runner)

			result, err := target.Apply(context.Background(), testSpec(), Options{})
			if err == nil {
				t.Fatal("Apply succeeded, want a conflict error")
			}
			if result.Status != StatusConflict {
				t.Fatalf("status = %q, want conflict", result.Status)
			}
			if !strings.Contains(err.Error(), "--force") {
				t.Errorf("error %q does not name --force", err)
			}
			if result.Current == "" || result.Manual == "" {
				t.Errorf("conflict result lacks a diff: %+v", result)
			}
			if len(runner.calls) != 0 {
				t.Errorf("ran %v, want nothing", runner.calls)
			}
		})
	}
}

func TestClaudeCodeForceRemovesThenAdds(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	target := newClaudeCode(
		writeState(t, `{"mcpServers":{"lumi":{"command":"lumi","args":["mcp"]}}}`), runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusReplaced {
		t.Fatalf("status = %q, want replaced", result.Status)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("got %d invocations, want remove then add: %v", len(runner.calls), runner.calls)
	}
	wantRemove := []string{"/fake/claude", "mcp", "remove", "lumi", "--scope", "user"}
	if !slices.Equal(runner.calls[0], wantRemove) {
		t.Errorf("first call = %v, want %v", runner.calls[0], wantRemove)
	}
	if got := runner.calls[1]; len(got) < 3 || got[2] != "add" {
		t.Errorf("second call = %v, want an add", got)
	}
}

// The invariant: ~/.claude.json is live application state, so Lumi reads it and
// never writes it. Every path is checked, because the one that regresses will
// be whichever is not covered.
func TestClaudeCodeNeverWritesTheStateFile(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body string
		opts Options
	}{
		"add":      {`{"mcpServers":{}}`, Options{}},
		"replace":  {`{"mcpServers":{"lumi":{"command":"lumi","args":["mcp"]}}}`, Options{Force: true}},
		"conflict": {`{"mcpServers":{"lumi":{"command":"lumi","args":["mcp"]}}}`, Options{}},
		"unchanged": {`{"mcpServers":{"lumi":{"command":"/abs/lumi",
			"args":["mcp","--data-dir","/abs/root"]}}}`, Options{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := writeState(t, tc.body)
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			target := newClaudeCode(state, &fakeRunner{})
			_, _ = target.Apply(context.Background(), testSpec(), tc.opts)

			after, err := os.ReadFile(state)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(before) != string(after) {
				t.Errorf("state file changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// The state file belongs to another application. Anything unreadable about it
// means "not configured", never a failure that blocks setup.
func TestClaudeCodeTreatsUnreadableStateAsUnconfigured(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"truncated json":      `{`,
		"empty object":        `{}`,
		"null mcpServers":     `{"mcpServers":null}`,
		"other servers":       `{"mcpServers":{"deepwiki":{"type":"http","url":"https://x"}}}`,
		"entry not an object": `{"mcpServers":{"lumi":"nonsense"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeRunner{}
			target := newClaudeCode(writeState(t, body), runner)

			result, err := target.Apply(context.Background(), testSpec(), Options{})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if result.Status != StatusAdded {
				t.Fatalf("status = %q, want added", result.Status)
			}
			if len(runner.calls) != 1 {
				t.Errorf("ran %v, want one add", runner.calls)
			}
		})
	}
}

func TestClaudeCodeMissingStateFileIsUnconfigured(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	target := newClaudeCode(filepath.Join(t.TempDir(), "nope.json"), runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusAdded {
		t.Fatalf("status = %q, want added", result.Status)
	}
}

// Not parallel: t.Setenv points HOME at an empty directory so the candidate
// lookup cannot find a claude the developer actually has installed.
func TestClaudeCodeMissingCLISkipsOrFails(t *testing.T) {
	for name, required := range map[string]bool{"implicit": false, "explicit": true} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			runner := &fakeRunner{}
			target := &ClaudeCode{
				Runner:    runner,
				LookPath:  func(string) (string, error) { return "", exec.ErrNotFound },
				StatePath: filepath.Join(t.TempDir(), "absent.json"),
				Required:  required,
			}

			result, err := target.Apply(context.Background(), testSpec(), Options{})
			if result.Status != StatusSkipped {
				t.Fatalf("status = %q, want skipped", result.Status)
			}
			if result.Manual == "" {
				t.Error("skip result carries no manual snippet")
			}
			if required && err == nil {
				t.Error("explicit target skipped without an error")
			}
			if !required && err != nil {
				t.Errorf("implicit skip returned %v, want nil", err)
			}
			if len(runner.calls) != 0 {
				t.Errorf("ran %v, want nothing", runner.calls)
			}
		})
	}
}

func TestClaudeCodeDryRunInvokesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body string
		opts Options
		want Status
	}{
		"would add":     {`{}`, Options{DryRun: true}, StatusAdded},
		"would replace": {`{"mcpServers":{"lumi":{"command":"lumi"}}}`, Options{DryRun: true, Force: true}, StatusReplaced},
		"unchanged": {`{"mcpServers":{"lumi":{"command":"/abs/lumi",
			"args":["mcp","--data-dir","/abs/root"]}}}`, Options{DryRun: true}, StatusUnchanged},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := &fakeRunner{}
			target := newClaudeCode(writeState(t, tc.body), runner)

			result, err := target.Apply(context.Background(), testSpec(), tc.opts)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if result.Status != tc.want {
				t.Errorf("status = %q, want %q", result.Status, tc.want)
			}
			if result.Changed {
				t.Error("Changed = true under --dry-run")
			}
			if len(runner.calls) != 0 {
				t.Errorf("ran %v under --dry-run, want nothing", runner.calls)
			}
		})
	}
}

// A dry run must still surface a conflict, so `mcp setup --dry-run` works as a
// health check that fails loudly.
func TestClaudeCodeDryRunStillReportsConflicts(t *testing.T) {
	t.Parallel()
	target := newClaudeCode(
		writeState(t, `{"mcpServers":{"lumi":{"command":"lumi","args":["mcp"]}}}`), &fakeRunner{})

	result, err := target.Apply(context.Background(), testSpec(), Options{DryRun: true})
	if err == nil {
		t.Fatal("dry run over a conflict succeeded, want an error")
	}
	if result.Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", result.Status)
	}
}

func TestClaudeCodeSurfacesCLIOutputOnFailure(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{err: errors.New("exit status 1"), out: "unknown command 'mcp'\n"}
	target := newClaudeCode(filepath.Join(t.TempDir(), "absent.json"), runner)

	_, err := target.Apply(context.Background(), testSpec(), Options{})
	if err == nil {
		t.Fatal("Apply succeeded, want a failure")
	}
	if !strings.Contains(err.Error(), "unknown command 'mcp'") {
		t.Errorf("error %q omits the CLI's own output", err)
	}
}

func TestManualSnippetIsValidJSON(t *testing.T) {
	t.Parallel()
	// The snippet is a fragment of an mcpServers object, so wrap it to parse.
	wrapped := "{" + ManualSnippet(Spec{
		Name:    "lumi",
		Command: `/tmp/dir with "quotes"/lumi`,
		Args:    []string{"mcp", "--data-dir", "/abs/root"},
	}) + "}"
	var parsed map[string]entry
	if err := json.Unmarshal([]byte(wrapped), &parsed); err != nil {
		t.Fatalf("snippet is not valid JSON: %v\n%s", err, wrapped)
	}
	if got := parsed["lumi"].Command; got != `/tmp/dir with "quotes"/lumi` {
		t.Errorf("command = %q, want the path escaped and round-tripped", got)
	}
}
