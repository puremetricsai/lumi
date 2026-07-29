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
	// failCall, when non-nil, decides per invocation instead of err, so a test
	// can fail exactly one of a remove/add/restore sequence.
	failCall func(call []string) error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	if f.failCall != nil {
		return f.out, f.failCall(call)
	}
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

// --force removes before it adds, so a failing add is the one path that can
// destroy a registration the user already had. The old entry must come back.
func TestClaudeCodeRestoresTheOldEntryWhenTheForcedAddFails(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	// Fail the add that installs the new entry, but let the restoring add
	// through: that is the sequence where rollback has to work.
	failed := false
	runner.failCall = func(call []string) error {
		if slices.Contains(call, "add") && !failed {
			failed = true
			return errors.New("exit status 1")
		}
		return nil
	}
	target := newClaudeCode(
		writeState(t, `{"mcpServers":{"lumi":{"command":"/old/lumi","args":["mcp","--data-dir","/old"]}}}`),
		runner)

	_, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err == nil {
		t.Fatal("Apply returned nil despite a failing add")
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Errorf("error does not report the rollback: %v", err)
	}

	if len(runner.calls) != 3 {
		t.Fatalf("got %d invocations, want remove, add, restoring add: %v", len(runner.calls), runner.calls)
	}
	wantRestore := []string{
		"/fake/claude", "mcp", "add", "--scope", "user", "lumi", "--",
		"/old/lumi", "mcp", "--data-dir", "/old",
	}
	if !slices.Equal(runner.calls[2], wantRestore) {
		t.Errorf("restore call = %v, want %v", runner.calls[2], wantRestore)
	}
}

// When the rollback itself fails the user has lost the entry, so the error has
// to carry enough to put it back by hand.
func TestClaudeCodeReportsAnUnrestorableEntry(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{failCall: func(call []string) error {
		if slices.Contains(call, "add") {
			return errors.New("exit status 1")
		}
		return nil
	}}
	target := newClaudeCode(
		writeState(t, `{"mcpServers":{"lumi":{"command":"/old/lumi","args":["mcp"]}}}`), runner)

	_, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err == nil {
		t.Fatal("Apply returned nil despite a failing add")
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not be restored") {
		t.Errorf("error does not say the entry is gone: %v", err)
	}
	if !strings.Contains(msg, "/old/lumi") {
		t.Errorf("error omits the lost entry, leaving nothing to re-add by hand: %v", err)
	}
}

// A restore has to run even when the add failed by exhausting the CLI timeout
// or because the user interrupted the command; both leave ctx already done.
func TestClaudeCodeRestoresUnderACancelledContext(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	failed := false
	runner.failCall = func(call []string) error {
		if slices.Contains(call, "add") && !failed {
			failed = true
			return context.Canceled
		}
		return nil
	}
	target := newClaudeCode(
		writeState(t, `{"mcpServers":{"lumi":{"command":"/old/lumi","args":["mcp"]}}}`), runner)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := target.Apply(ctx, testSpec(), Options{Force: true})
	if err == nil {
		t.Fatal("Apply returned nil despite a failing add")
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Errorf("a cancelled context skipped the rollback: %v", err)
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

// The state file belongs to another application. Anything unreadable about the
// *file* means "not configured", never a failure that blocks setup. An
// unreadable entry under Lumi's own name is different — see the conflict test
// below — because that one is a registration this run would destroy.
func TestClaudeCodeTreatsUnreadableStateAsUnconfigured(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"truncated json":  `{`,
		"empty object":    `{}`,
		"null mcpServers": `{"mcpServers":null}`,
		"other servers":   `{"mcpServers":{"deepwiki":{"type":"http","url":"https://x"}}}`,
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

// An entry present under Lumi's name that does not decode is still the user's
// registration: replacing it needs --force, exactly as a differing one does.
func TestClaudeCodeConflictsOnAnUnreadableEntry(t *testing.T) {
	t.Parallel()
	const body = `{"mcpServers":{"lumi":"nonsense"}}`

	runner := &fakeRunner{}
	target := newClaudeCode(writeState(t, body), runner)
	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err == nil {
		t.Fatal("Apply replaced an unreadable entry without --force")
	}
	if result.Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", result.Status)
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %v, want nothing", runner.calls)
	}

	forced := &fakeRunner{}
	target = newClaudeCode(writeState(t, body), forced)
	result, err = target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err != nil {
		t.Fatalf("Apply --force: %v", err)
	}
	if result.Status != StatusReplaced || !result.Changed {
		t.Fatalf("got %+v, want replaced and changed", result)
	}
	if len(forced.calls) != 2 {
		t.Errorf("ran %v, want remove then add", forced.calls)
	}
}

// A restored entry has to come back with the transport and env it had, not as
// a bare stdio command.
func TestClaudeCodeAddArgsCarryTransportAndEnv(t *testing.T) {
	t.Parallel()
	got := addArgs("lumi", entry{
		Type:    "sse",
		Command: "https://example.test/mcp",
		Env:     map[string]string{"B": "2", "A": "1"},
	})
	want := []string{
		"mcp", "add", "--scope", "user", "--transport", "sse",
		"--env", "A=1", "--env", "B=2", "lumi", "--", "https://example.test/mcp",
	}
	if !slices.Equal(got, want) {
		t.Errorf("addArgs = %v, want %v", got, want)
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
