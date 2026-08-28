package mcpsetup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// codexRunner answers `codex mcp get` from a canned response and records every
// invocation. It is separate from fakeRunner because this target reads through
// the CLI as well as writing through it, so one blanket output would make the
// detection call and the write calls indistinguishable.
type codexRunner struct {
	calls [][]string
	// getOut and getErr are the response to `mcp get`. A non-nil getErr is how
	// the real CLI reports a name it has never heard of — and also how it
	// reports a config it cannot load, which is why the probe below exists.
	getOut string
	getErr error
	// listErr is the response to the `mcp list` health probe. nil means codex
	// itself is fine; non-nil means it cannot read its own configuration.
	listErr error
	// out is the output of the write calls, shown in failure messages.
	out string
	// failCall, when non-nil, decides per invocation, so a test can fail
	// exactly one of a remove/add/restore sequence.
	failCall func(call []string) error
}

func (c *codexRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	c.calls = append(c.calls, call)
	if len(args) >= 2 && args[0] == "mcp" && args[1] == "get" {
		return c.getOut, c.getErr
	}
	if len(args) >= 2 && args[0] == "mcp" && args[1] == "list" {
		return "[]", c.listErr
	}
	if c.failCall != nil {
		return c.out, c.failCall(call)
	}
	return c.out, nil
}

// writeCalls are the invocations that could have changed config.toml. The
// detection read and the health probe are not among them, and no test should
// have to filter them out by hand.
func (c *codexRunner) writeCalls() [][]string {
	var out [][]string
	for _, call := range c.calls {
		if len(call) >= 3 && call[1] == "mcp" && (call[2] == "get" || call[2] == "list") {
			continue
		}
		out = append(out, call)
	}
	return out
}

// notConfigured is a healthy codex that has never heard of the name: `mcp get`
// exits non-zero, `mcp list` succeeds.
func notConfigured() *codexRunner {
	return &codexRunner{getErr: errors.New("exit status 1")}
}

// configured builds a runner whose `mcp get` reports a stdio entry.
func configured(command string, args ...string) *codexRunner {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, tomlString(arg))
	}
	return &codexRunner{getOut: `{"name":"lumi","enabled":true,"transport":{"type":"stdio",` +
		`"command":` + tomlString(command) + `,"args":[` + strings.Join(quoted, ",") + `],` +
		`"env":{},"env_vars":[],"cwd":null}}`}
}

// newCodex builds a target that never touches the real machine.
func newCodex(runner Runner) *Codex {
	return &Codex{Runner: runner, CLIPath: "/fake/codex"}
}

func TestCodexAddPassesArgsAfterASeparator(t *testing.T) {
	t.Parallel()
	runner := notConfigured()
	target := newCodex(runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusAdded || !result.Changed {
		t.Fatalf("got %+v, want added and changed", result)
	}

	// The -- is what keeps codex's own parser off --data-dir. No --scope: codex
	// has none.
	want := []string{
		"/fake/codex", "mcp", "add", "lumi", "--",
		"/abs/lumi", "mcp", "--data-dir", "/abs/root",
	}
	writes := runner.writeCalls()
	if len(writes) != 1 || !slices.Equal(writes[0], want) {
		t.Fatalf("invocation = %v, want %v", writes, want)
	}
}

func TestCodexIsUnchangedWhenAlreadyConfigured(t *testing.T) {
	t.Parallel()
	runner := configured("/abs/lumi", "mcp", "--data-dir", "/abs/root")
	target := newCodex(runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusUnchanged {
		t.Fatalf("status = %q, want unchanged", result.Status)
	}
	if result.Changed {
		t.Error("an unchanged run reported a change")
	}
	if writes := runner.writeCalls(); len(writes) != 0 {
		t.Errorf("an already-configured client was written to: %v", writes)
	}
}

func TestCodexRefusesToOverwriteADifferingEntry(t *testing.T) {
	t.Parallel()
	runner := configured("/other/lumi", "mcp", "--data-dir", "/other/root")
	target := newCodex(runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err == nil {
		t.Fatal("Apply succeeded, want a conflict error")
	}
	if result.Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", result.Status)
	}
	if result.Current != "/other/lumi mcp --data-dir /other/root" {
		t.Errorf("current = %q, want the existing command line", result.Current)
	}
	if writes := runner.writeCalls(); len(writes) != 0 {
		t.Errorf("a conflict still wrote: %v", writes)
	}
	// The paste-able answer has to be in the format this client actually reads.
	if !strings.Contains(result.Manual, "[mcp_servers.lumi]") {
		t.Errorf("manual snippet is not TOML:\n%s", result.Manual)
	}
	if result.ManualHint != tomlManualHint {
		t.Errorf("hint = %q, want %q", result.ManualHint, tomlManualHint)
	}
}

func TestCodexForceRemovesBeforeAdding(t *testing.T) {
	t.Parallel()
	runner := configured("/other/lumi", "mcp")
	target := newCodex(runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusReplaced || !result.Changed {
		t.Fatalf("got %+v, want replaced and changed", result)
	}

	writes := runner.writeCalls()
	if len(writes) != 2 {
		t.Fatalf("invocations = %v, want a remove then an add", writes)
	}
	// Order matters: relying on add-to-overwrite would make --force a silent
	// no-op against a codex that rejects duplicates instead.
	if !slices.Equal(writes[0], []string{"/fake/codex", "mcp", "remove", "lumi"}) {
		t.Errorf("first invocation = %v, want a remove", writes[0])
	}
	if !slices.Contains(writes[1], "add") {
		t.Errorf("second invocation = %v, want an add", writes[1])
	}
}

// An entry codex describes as something other than a plain stdio command is the
// user's registration all the same. Treating it as absent would replace it with
// neither --force nor any record of what was there.
func TestCodexTreatsAnUnreadableEntryAsAConflict(t *testing.T) {
	t.Parallel()
	for name, getOut := range map[string]string{
		"not JSON at all": `Server: lumi`,
		"http transport":  `{"name":"lumi","transport":{"type":"streamable_http","url":"https://x"}}`,
		"no command":      `{"name":"lumi","transport":{"type":"stdio","args":["mcp"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := &codexRunner{getOut: getOut}
			target := newCodex(runner)

			result, err := target.Apply(context.Background(), testSpec(), Options{})
			if err == nil {
				t.Fatal("Apply succeeded, want a conflict error")
			}
			if result.Status != StatusConflict {
				t.Fatalf("status = %q, want conflict", result.Status)
			}
			if result.Current != unreadableEntry {
				t.Errorf("current = %q, want %q", result.Current, unreadableEntry)
			}
			if writes := runner.writeCalls(); len(writes) != 0 {
				t.Errorf("a conflict still wrote: %v", writes)
			}
		})
	}
}

// An env block is a difference: nobody sets one by accident, and dropping it
// silently is exactly what --force exists to gate.
func TestCodexTreatsAnEnvBlockAsADifference(t *testing.T) {
	t.Parallel()
	runner := &codexRunner{getOut: `{"name":"lumi","transport":{"type":"stdio",
		"command":"/abs/lumi","args":["mcp","--data-dir","/abs/root"],
		"env":{"B":"2","A":"1"}}}`}
	target := newCodex(runner)

	if _, err := target.Apply(context.Background(), testSpec(), Options{}); err == nil {
		t.Fatal("an entry carrying env was accepted as identical")
	}

	// Under --force the replacement drops it, which is the point of the gate.
	runner.calls = nil
	result, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err != nil {
		t.Fatalf("Apply --force: %v", err)
	}
	if result.Status != StatusReplaced {
		t.Fatalf("status = %q, want replaced", result.Status)
	}
	add := runner.writeCalls()[1]
	if slices.Contains(add, "--env") {
		t.Errorf("the replacement carried the old env: %v", add)
	}
}

// A failed add after a successful remove leaves the user with nothing. The
// rollback puts theirs back, and the error has to say which way it went.
func TestCodexRestoresTheOldEntryWhenAddFails(t *testing.T) {
	t.Parallel()
	runner := configured("/other/lumi", "mcp", "--data-dir", "/other/root")
	adds := 0
	runner.failCall = func(call []string) error {
		if !slices.Contains(call, "add") {
			return nil
		}
		adds++
		if adds == 1 {
			return errors.New("exit status 1")
		}
		return nil // the rollback succeeds
	}
	target := newCodex(runner)

	_, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err == nil {
		t.Fatal("Apply succeeded, want the add failure")
	}
	if !strings.Contains(err.Error(), "was restored") {
		t.Errorf("error does not report the rollback:\n%v", err)
	}

	writes := runner.writeCalls()
	if len(writes) != 3 {
		t.Fatalf("invocations = %v, want remove, add, restore", writes)
	}
	// The restore has to re-add what was there, not what setup wanted.
	if !slices.Contains(writes[2], "/other/lumi") {
		t.Errorf("restore = %v, want the previous entry re-added", writes[2])
	}
}

// When the rollback fails too, the entry is gone and the error is the only copy
// of it left — so it has to carry one.
func TestCodexReportsTheLostEntryWhenRestoreFails(t *testing.T) {
	t.Parallel()
	runner := configured("/other/lumi", "mcp", "--data-dir", "/other/root")
	runner.failCall = func(call []string) error {
		if slices.Contains(call, "add") {
			return errors.New("exit status 1")
		}
		return nil
	}
	target := newCodex(runner)

	_, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err == nil {
		t.Fatal("Apply succeeded, want the add failure")
	}
	if !strings.Contains(err.Error(), "/other/lumi") ||
		!strings.Contains(err.Error(), "[mcp_servers.lumi]") {
		t.Errorf("error does not hand back the lost entry as TOML:\n%v", err)
	}
}

// An unreadable entry cannot be re-added through `codex mcp add`'s stdio form.
// The raw description is then the only complete record of what was removed.
func TestCodexReportsTheRawEntryWhenAnUnreadableOneIsLost(t *testing.T) {
	t.Parallel()
	const raw = `{"name":"lumi","transport":{"type":"streamable_http","url":"https://example.test"}}`
	runner := &codexRunner{getOut: raw}
	runner.failCall = func(call []string) error {
		if slices.Contains(call, "add") {
			return errors.New("exit status 1")
		}
		return nil
	}
	target := newCodex(runner)

	_, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err == nil {
		t.Fatal("Apply succeeded, want the add failure")
	}
	if !strings.Contains(err.Error(), "https://example.test") {
		t.Errorf("error does not carry what was removed:\n%v", err)
	}
	// One remove and one add, and no second add: there is nothing to restore
	// the entry with, so retrying would only obscure that.
	if len(runner.writeCalls()) != 2 {
		t.Errorf("invocations = %v, want a remove and one add", runner.writeCalls())
	}
}

// A dry run may read — that is the only way to know what it would do — but it
// must issue nothing that could change config.toml, and must not claim a change.
func TestCodexDryRunReadsButNeverWrites(t *testing.T) {
	t.Parallel()
	runner := configured("/other/lumi", "mcp")
	target := newCodex(runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{Force: true, DryRun: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusReplaced {
		t.Fatalf("status = %q, want replaced", result.Status)
	}
	if result.Changed {
		t.Error("a dry run reported a change")
	}
	if writes := runner.writeCalls(); len(writes) != 0 {
		t.Errorf("a dry run wrote: %v", writes)
	}
}

func TestCodexSkipsWhenTheCLIIsMissing(t *testing.T) {
	t.Parallel()
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	// Candidates is emptied rather than left to the default: the real list
	// holds absolute paths like /opt/homebrew/bin/codex, so on a developer's
	// own Mac the default would find a codex and this test never see a skip.
	none := []string{}

	target := &Codex{LookPath: missing, Candidates: none}
	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("an implicitly reached client failed instead of skipping: %v", err)
	}
	if result.Status != StatusSkipped {
		t.Fatalf("status = %q, want skipped", result.Status)
	}
	if !strings.Contains(result.Manual, "[mcp_servers.lumi]") {
		t.Errorf("the skip did not hand back a TOML snippet:\n%s", result.Manual)
	}
	if result.ManualHint != tomlManualHint {
		t.Errorf("hint = %q, want %q", result.ManualHint, tomlManualHint)
	}

	// Named explicitly, the same condition is an error.
	required := &Codex{LookPath: missing, Candidates: none, Required: true}
	if _, err := required.Apply(context.Background(), testSpec(), Options{}); err == nil {
		t.Error("--client codex succeeded with no codex installed")
	}
}

// `codex mcp get` exits non-zero both for an absent name and for a config it
// cannot load. A healthy `mcp list` is what separates them, and a name codex
// genuinely does not know is an ordinary add.
func TestCodexTreatsAnAbsentNameAsUnconfigured(t *testing.T) {
	t.Parallel()
	runner := notConfigured()
	target := newCodex(runner)

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusAdded {
		t.Fatalf("status = %q, want added", result.Status)
	}
	// No remove: nothing was believed to be there.
	if writes := runner.writeCalls(); len(writes) != 1 || slices.Contains(writes[0], "remove") {
		t.Errorf("invocations = %v, want a single add", writes)
	}
}

// A read Lumi cannot trust must not read as "not configured". Under --dry-run
// there is no write to fail later, so swallowing it here would make the
// documented health check print "would add" and exit 0 on a machine where a
// real run cannot get that far.
func TestCodexFailsWhenItCannotReadTheConfig(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]Options{
		"real run": {},
		"dry run":  {DryRun: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := &codexRunner{
				getOut:  "Error: failed to load configuration",
				getErr:  errors.New("exit status 1"),
				listErr: errors.New("exit status 1"),
			}
			target := newCodex(runner)

			result, err := target.Apply(context.Background(), testSpec(), opts)
			if err == nil {
				t.Fatal("Apply succeeded despite an unreadable configuration")
			}
			// Not a conflict: nothing is in the way, and offering --force
			// would be advice that cannot possibly work.
			if result.Status != StatusFailed {
				t.Errorf("status = %q, want failed", result.Status)
			}
			// The CLI's own diagnostic is the actionable part.
			if !strings.Contains(err.Error(), "failed to load configuration") {
				t.Errorf("error drops codex's diagnostic:\n%v", err)
			}
			if writes := runner.writeCalls(); len(writes) != 0 {
				t.Errorf("an unreadable configuration was written to: %v", writes)
			}
		})
	}
}

// A disabled entry has the right command and args and still does not work:
// codex will not launch it. Reporting "unchanged" would tell the user setup
// succeeded while the agent silently never sees Lumi.
func TestCodexTreatsADisabledEntryAsADifference(t *testing.T) {
	t.Parallel()
	disabled := func() *codexRunner {
		return &codexRunner{getOut: `{"name":"lumi","enabled":false,"transport":{"type":"stdio",
			"command":"/abs/lumi","args":["mcp","--data-dir","/abs/root"]}}`}
	}

	runner := disabled()
	result, err := newCodex(runner).Apply(context.Background(), testSpec(), Options{})
	if err == nil {
		t.Fatal("a disabled entry was accepted as already configured")
	}
	if result.Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", result.Status)
	}
	// Current and desired would otherwise print identically and the conflict
	// would read as a bug.
	if !strings.Contains(result.Current, "(disabled)") {
		t.Errorf("current = %q, does not say why it differs", result.Current)
	}
	if writes := runner.writeCalls(); len(writes) != 0 {
		t.Errorf("a conflict still wrote: %v", writes)
	}

	// --force is the way out, and it re-adds the entry enabled.
	runner = disabled()
	result, err = newCodex(runner).Apply(context.Background(), testSpec(), Options{Force: true})
	if err != nil {
		t.Fatalf("Apply --force: %v", err)
	}
	if result.Status != StatusReplaced || !result.Changed {
		t.Fatalf("got %+v, want replaced and changed", result)
	}
}

// `codex mcp add` has no way to re-add an entry disabled, so a rollback would
// hand back a server the user had deliberately switched off and call it
// restored. It must say so instead.
func TestCodexWillNotSilentlyReEnableAnEntryItRestores(t *testing.T) {
	t.Parallel()
	runner := &codexRunner{getOut: `{"name":"lumi","enabled":false,"transport":{"type":"stdio",
		"command":"/other/lumi","args":["mcp"]}}`}
	runner.failCall = func(call []string) error {
		if slices.Contains(call, "add") {
			return errors.New("exit status 1")
		}
		return nil
	}
	target := newCodex(runner)

	_, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err == nil {
		t.Fatal("Apply succeeded, want the add failure")
	}
	if !strings.Contains(err.Error(), "re-add it disabled") {
		t.Errorf("error does not explain why it could not restore:\n%v", err)
	}
	// One add attempted, and no second one dressed up as a restore.
	if len(runner.writeCalls()) != 2 {
		t.Errorf("invocations = %v, want a remove and one add", runner.writeCalls())
	}
}

// Lumi.app is launched by launchd, whose PATH is /usr/bin:/bin:/usr/sbin:/sbin
// and nothing more, so a PATH-only lookup reported "not installed" for every
// user whose codex lives anywhere else — the app's Set up MCP button silently
// skipped Codex. A candidate that exists is used exactly as a PATH hit is.
func TestCodexFallsBackToACandidateWhenPATHHasNone(t *testing.T) {
	t.Parallel()
	installed := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(installed, nil, 0o755); err != nil {
		t.Fatal(err)
	}

	runner := notConfigured()
	target := &Codex{
		Runner:     runner,
		LookPath:   func(string) (string, error) { return "", exec.ErrNotFound },
		Candidates: []string{filepath.Join(t.TempDir(), "absent"), installed},
	}

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusAdded {
		t.Fatalf("status = %q, want added", result.Status)
	}
	writes := runner.writeCalls()
	if len(writes) != 1 || writes[0][0] != installed {
		t.Fatalf("wrote through %v, want the candidate %q", writes, installed)
	}
}

// The ChatGPT desktop app and the codex CLI share $CODEX_HOME/config.toml, so
// the app's bundled binary registers both and belongs in the list. Nothing in
// the list probes ~/.codex, which the desktop app creates whether or not a CLI
// is installed.
func TestCodexCandidatesIncludeTheDesktopAppsBinary(t *testing.T) {
	t.Parallel()
	got := codexCLICandidates("/home/u")
	if !slices.Contains(got, "/Applications/ChatGPT.app/Contents/Resources/codex") {
		t.Errorf("candidates omit the desktop app's binary: %v", got)
	}
	if !slices.Contains(got, "/home/u/.local/bin/codex") {
		t.Errorf("candidates omit the home-relative install: %v", got)
	}
	for _, candidate := range got {
		if strings.Contains(candidate, "/.codex/") {
			t.Errorf("candidate %q probes ~/.codex, which is no evidence of a CLI", candidate)
		}
	}
}
