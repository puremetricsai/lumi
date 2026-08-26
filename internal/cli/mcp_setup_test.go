package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/puremetricsai/lumi/internal/mcpsetup"
)

// fakeTarget stands in for a real MCP client so tests never read or write the
// developer's own Claude configuration.
type fakeTarget struct {
	name   string
	result mcpsetup.Result
	err    error
	// spec records what the command asked for, so a test can assert on the
	// generated entry without inspecting any file.
	spec mcpsetup.Spec
	opts mcpsetup.Options
}

func (f *fakeTarget) Name() string { return f.name }

func (f *fakeTarget) Apply(_ context.Context, spec mcpsetup.Spec, opts mcpsetup.Options) (mcpsetup.Result, error) {
	f.spec = spec
	f.opts = opts
	result := f.result
	result.Target = f.name
	return result, f.err
}

// newSetupTest returns the data dir and a runner for `mcp setup`, with all
// three seams stubbed: a plausible installed binary path, no verification
// subprocess, and the supplied fake targets.
func newSetupTest(t *testing.T, targets ...mcpsetup.Target) (string, func(args ...string) (string, string, error)) {
	t.Helper()
	dataDir := t.TempDir()

	swap(t, &resolveLumiBinary, func() (string, error) { return "/usr/local/bin/lumi", nil })
	swap(t, &verifyLumiBinary, func(context.Context, string) error { return nil })
	swap(t, &newSetupTargets, func(clientSelection) []mcpsetup.Target { return targets })

	a := &app{dataDir: dataDir}
	run := func(args ...string) (string, string, error) {
		cmd := a.mcpSetupCommand()
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		err := cmd.ExecuteContext(context.Background())
		return stdout.String(), stderr.String(), err
	}
	return dataDir, run
}

// swap replaces a package-level seam for the duration of one test.
func swap[T any](t *testing.T, target *T, replacement T) {
	t.Helper()
	original := *target
	t.Cleanup(func() { *target = original })
	*target = replacement
}

// `lumi mcp` must stay the server. Turning it into a bare parent that prints
// help would dump text onto the JSON-RPC stream and break every existing client
// config, so both facts are pinned here rather than left to review.
func TestMCPCommandStillServesAndGainedSetup(t *testing.T) {
	a := &app{}
	cmd := a.mcpCommand()

	if cmd.RunE == nil {
		t.Error("mcp lost its RunE; it must remain the server itself")
	}
	if !cmd.SilenceUsage || !cmd.SilenceErrors {
		t.Error("mcp must silence usage and errors: stdout is the JSON-RPC stream")
	}

	var names []string
	for _, sub := range cmd.Commands() {
		names = append(names, sub.Name())
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"setup"}) {
		t.Errorf("subcommands = %v, want exactly [setup]", names)
	}
}

func TestMCPSetupRejectsUnknownClient(t *testing.T) {
	_, run := newSetupTest(t)
	_, _, err := run("--client", "bogus")
	if err == nil {
		t.Fatal("unknown --client accepted")
	}
	for _, want := range []string{"code", "desktop", "all"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestMCPSetupRejectsEmptyName(t *testing.T) {
	_, run := newSetupTest(t)
	if _, _, err := run("--name", "  "); err == nil {
		t.Fatal("empty --name accepted")
	}
}

// The generated argv must carry an absolute data dir, because a client spawns
// the server with a bare environment and LUMI_HOME never reaches it.
func TestMCPSetupBakesAnAbsoluteDataDir(t *testing.T) {
	target := &fakeTarget{name: "fake", result: mcpsetup.Result{Status: mcpsetup.StatusAdded}}
	dataDir, run := newSetupTest(t, target)

	if _, _, err := run(); err != nil {
		t.Fatalf("setup: %v", err)
	}

	want := []string{"mcp", "--data-dir", dataDir}
	if !slices.Equal(target.spec.Args, want) {
		t.Errorf("args = %v, want %v", target.spec.Args, want)
	}
	if !filepath.IsAbs(target.spec.Command) {
		t.Errorf("command = %q, want an absolute path", target.spec.Command)
	}
	if target.spec.Name != "lumi" {
		t.Errorf("name = %q, want the default lumi", target.spec.Name)
	}
	// The command must create the directory it points clients at.
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Errorf("data dir was not created: %v", err)
	}
}

func TestMCPSetupForwardsNameAndOptions(t *testing.T) {
	target := &fakeTarget{name: "fake", result: mcpsetup.Result{Status: mcpsetup.StatusAdded}}
	_, run := newSetupTest(t, target)

	if _, _, err := run("--name", "lumi-work", "--force", "--dry-run"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if target.spec.Name != "lumi-work" {
		t.Errorf("name = %q, want lumi-work", target.spec.Name)
	}
	if !target.opts.Force || !target.opts.DryRun {
		t.Errorf("options = %+v, want Force and DryRun", target.opts)
	}
}

// os.Executable() under `go run` points into the build cache, and that path is
// unlinked the moment the process exits.
func TestMCPSetupRefusesATemporaryBinary(t *testing.T) {
	target := &fakeTarget{name: "fake"}
	_, run := newSetupTest(t, target)
	swap(t, &resolveLumiBinary, func() (string, error) {
		return filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "lumi"), nil
	})

	_, _, err := run()
	if err == nil {
		t.Fatal("setup accepted a temporary build path")
	}
	if !strings.Contains(err.Error(), "task build") {
		t.Errorf("error %q does not say how to fix it", err)
	}
	if target.spec.Command != "" {
		t.Error("a target was invoked despite the unusable binary")
	}
}

func TestMCPSetupRefusesAnUnrunnableBinary(t *testing.T) {
	target := &fakeTarget{name: "fake"}
	_, run := newSetupTest(t, target)
	swap(t, &verifyLumiBinary, func(context.Context, string) error {
		return errors.New("no such file or directory")
	})

	if _, _, err := run(); err == nil {
		t.Fatal("setup accepted a binary that does not run")
	}
	if target.spec.Command != "" {
		t.Error("a target was invoked despite the failed verification")
	}
}

// A reminder printed on a run that changed nothing trains people to ignore it.
func TestMCPSetupRestartReminderOnlyWhenDesktopChanged(t *testing.T) {
	const reminder = "Quit and reopen Claude Desktop"

	t.Run("changed", func(t *testing.T) {
		target := &fakeTarget{name: "claude-desktop", result: mcpsetup.Result{
			Status: mcpsetup.StatusAdded, Detail: "…", Changed: true}}
		_, run := newSetupTest(t, target)
		_, stderr, err := run()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		if !strings.Contains(stderr, reminder) {
			t.Errorf("stderr lacks the restart reminder:\n%s", stderr)
		}
	})

	t.Run("unchanged", func(t *testing.T) {
		target := &fakeTarget{name: "claude-desktop", result: mcpsetup.Result{
			Status: mcpsetup.StatusUnchanged, Detail: "already configured"}}
		_, run := newSetupTest(t, target)
		_, stderr, err := run()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		if strings.Contains(stderr, reminder) {
			t.Errorf("no-op run printed the restart reminder:\n%s", stderr)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		target := &fakeTarget{name: "claude-desktop", result: mcpsetup.Result{
			Status: mcpsetup.StatusAdded, Detail: "…"}}
		_, run := newSetupTest(t, target)
		_, stderr, err := run("--dry-run")
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		if strings.Contains(stderr, reminder) {
			t.Errorf("--dry-run printed the restart reminder:\n%s", stderr)
		}
	})
}

// One client failing must never stop another from being configured.
func TestMCPSetupPartialFailureStillConfiguresTheOtherClient(t *testing.T) {
	failing := &fakeTarget{
		name:   "claude-code",
		result: mcpsetup.Result{Status: mcpsetup.StatusConflict, Detail: "differs", Current: "lumi mcp"},
		err:    errors.New("refusing to overwrite"),
	}
	succeeding := &fakeTarget{
		name:   "claude-desktop",
		result: mcpsetup.Result{Status: mcpsetup.StatusAdded, Detail: "/usr/local/bin/lumi mcp", Changed: true},
	}
	_, run := newSetupTest(t, failing, succeeding)

	stdout, _, err := run()
	if err == nil {
		t.Fatal("setup returned nil despite a conflict")
	}
	if succeeding.spec.Command == "" {
		t.Error("the second client was never attempted")
	}
	if !strings.Contains(stdout, "claude-desktop") || !strings.Contains(stdout, "added") {
		t.Errorf("stdout omits the client that succeeded:\n%s", stdout)
	}
}

func TestMCPSetupResultsGoToStdoutAndDiagnosticsToStderr(t *testing.T) {
	conflicted := &fakeTarget{
		name: "claude-code",
		result: mcpsetup.Result{
			Status:  mcpsetup.StatusConflict,
			Detail:  "an entry with different settings already exists",
			Current: "lumi mcp",
			Manual:  `  "lumi": {}`,
		},
		err: errors.New("refusing to overwrite"),
	}
	skipped := &fakeTarget{
		name: "claude-desktop",
		result: mcpsetup.Result{
			Status:     mcpsetup.StatusSkipped,
			Detail:     "Claude Desktop is not installed",
			Manual:     `  "lumi": {}`,
			ManualHint: `add this under "mcpServers"`,
		},
	}
	_, run := newSetupTest(t, conflicted, skipped)

	stdout, stderr, _ := run()

	// stdout carries results only: one line per client, no prose.
	for _, want := range []string{"claude-code", "conflict", "claude-desktop", "skipped"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	// The one-line reason is part of the result; the explanatory prose is not.
	for _, unwanted := range []string{"Re-run with --force", "To configure it by hand", "current:"} {
		if strings.Contains(stdout, unwanted) {
			t.Errorf("stdout carries the diagnostic %q:\n%s", unwanted, stdout)
		}
	}

	// stderr carries the explanations and both escape hatches.
	for _, want := range []string{"current:", "desired:", "--force", "--name", "mcpServers"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// A dry run must read as a preview, not as a claim that something happened.
// --dry-run is documented as a health check, so it must not create the data
// directory it would otherwise bake into the entry.
func TestMCPSetupDryRunCreatesNoDirectories(t *testing.T) {
	target := &fakeTarget{name: "claude-code", result: mcpsetup.Result{
		Status: mcpsetup.StatusAdded, Detail: "…"}}
	swap(t, &resolveLumiBinary, func() (string, error) { return "/usr/local/bin/lumi", nil })
	swap(t, &verifyLumiBinary, func(context.Context, string) error { return nil })
	swap(t, &newSetupTargets, func(clientSelection) []mcpsetup.Target { return []mcpsetup.Target{target} })

	root := filepath.Join(t.TempDir(), "absent")
	a := &app{dataDir: root}
	run := func(args ...string) error {
		cmd := a.mcpSetupCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return cmd.ExecuteContext(context.Background())
	}

	if err := run("--dry-run"); err != nil {
		t.Fatalf("setup --dry-run: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("--dry-run created %s", root)
	}
	// The entry it previews still has to name the real root.
	if !slices.Contains(target.spec.Args, root) {
		t.Errorf("args = %v, want --data-dir %s", target.spec.Args, root)
	}

	// A real run does create it, so the path is there when an agent launches.
	if err := run(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Errorf("a real run did not create %s: %v", root, err)
	}
}

func TestMCPSetupDryRunUsesConditionalWording(t *testing.T) {
	target := &fakeTarget{name: "fake", result: mcpsetup.Result{
		Status: mcpsetup.StatusAdded, Detail: "/usr/local/bin/lumi mcp"}}
	_, run := newSetupTest(t, target)

	stdout, _, err := run("--dry-run")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(stdout, "would add") {
		t.Errorf("stdout = %q, want \"would add\"", stdout)
	}
}

func TestParseClientSelection(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		value   string
		want    clientSelection
		wantErr bool
	}{
		"all":            {value: "all", want: clientSelection{code: true, desktop: true, codex: true}},
		"code":           {value: "code", want: clientSelection{code: true, explicit: true}},
		"desktop":        {value: "desktop", want: clientSelection{desktop: true, explicit: true}},
		"codex":          {value: "codex", want: clientSelection{codex: true, explicit: true}},
		"case insensive": {value: "Codex", want: clientSelection{codex: true, explicit: true}},
		"padded":         {value: " all ", want: clientSelection{code: true, desktop: true, codex: true}},
		"unknown":        {value: "cursor", wantErr: true},
		"target name":    {value: "claude-desktop", want: clientSelection{desktop: true, explicit: true}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := parseClientSelection(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseClientSelection(%q) succeeded, want an error", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseClientSelection(%q): %v", tc.value, err)
			}
			if got != tc.want {
				t.Errorf("parseClientSelection(%q) = %+v, want %+v", tc.value, got, tc.want)
			}
		})
	}
}

// Every `Target` name is a --client value selecting exactly that client. The
// names are the only client vocabulary a caller reading the JSON has: Lumi.app's
// MCP tab replaces one conflicting entry by handing the `target` it was given
// straight back to --client, so a target whose own name this flag rejects would
// make that button fail for that client alone. Derived from
// defaultSetupTargets rather than listed, so a fourth client is covered here the
// moment it is added — and fails this test until parseClientSelection knows its
// name.
func TestEveryTargetNameIsAClientValue(t *testing.T) {
	t.Parallel()
	all, err := parseClientSelection("all")
	if err != nil {
		t.Fatalf("parseClientSelection: %v", err)
	}
	for _, target := range defaultSetupTargets(all) {
		name := target.Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sel, err := parseClientSelection(name)
			if err != nil {
				t.Fatalf("parseClientSelection(%q): %v", name, err)
			}
			if !sel.explicit {
				t.Errorf("--client %q did not name a specific client: %+v", name, sel)
			}
			var selected []string
			for _, got := range defaultSetupTargets(sel) {
				selected = append(selected, got.Name())
			}
			if !slices.Equal(selected, []string{name}) {
				t.Errorf("--client %q selected %v, want only %q", name, selected, name)
			}
		})
	}
}

// "all" is the default, so a client Lumi has learned to configure must actually
// be reached by it — a target added to the package but not to the default
// selection would silently never run.
func TestDefaultSetupTargetsCoversEveryClient(t *testing.T) {
	t.Parallel()
	sel, err := parseClientSelection("all")
	if err != nil {
		t.Fatalf("parseClientSelection: %v", err)
	}
	var names []string
	for _, target := range defaultSetupTargets(sel) {
		names = append(names, target.Name())
	}
	want := []string{"claude-code", "claude-desktop", "codex"}
	if !slices.Equal(names, want) {
		t.Errorf("--client all selected %v, want %v", names, want)
	}
}

// Reached through "all", an uninstalled client is a visible skip; named
// explicitly, it is an error. Both halves have to hold for every target.
func TestDefaultSetupTargetsMarksExplicitClientsRequired(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"code", "desktop", "codex"} {
		sel, err := parseClientSelection(value)
		if err != nil {
			t.Fatalf("parseClientSelection(%q): %v", value, err)
		}
		targets := defaultSetupTargets(sel)
		if len(targets) != 1 {
			t.Fatalf("--client %s selected %d targets, want 1", value, len(targets))
		}
		if !requiredOf(t, targets[0]) {
			t.Errorf("--client %s did not mark %s required", value, targets[0].Name())
		}
	}

	sel, err := parseClientSelection("all")
	if err != nil {
		t.Fatalf("parseClientSelection: %v", err)
	}
	for _, target := range defaultSetupTargets(sel) {
		if requiredOf(t, target) {
			t.Errorf("--client all marked %s required", target.Name())
		}
	}
}

// requiredOf reads the Required field off any concrete target.
func requiredOf(t *testing.T, target mcpsetup.Target) bool {
	t.Helper()
	field := reflect.ValueOf(target).Elem().FieldByName("Required")
	if !field.IsValid() {
		t.Fatalf("%T has no Required field", target)
	}
	return field.Bool()
}

func TestIsTemporaryBinary(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		path string
		want bool
	}{
		"go run build cache": {filepath.Join(os.TempDir(), "go-build42", "b001", "exe", "lumi"), true},
		"go-build anywhere":  {"/var/somewhere/go-build999/lumi", true},
		"installed":          {"/usr/local/bin/lumi", false},
		"homebrew":           {"/opt/homebrew/bin/lumi", false},
		"repo checkout":      {"/Users/someone/Projects/lumi/lumi", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isTemporaryBinary(tc.path); got != tc.want {
				t.Errorf("isTemporaryBinary(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// --json is what the macOS app reads, paired with --dry-run as the read-only
// status query this package otherwise has no path for. So the payload has to
// carry everything the app shows — the resolved binary, the argv, and a
// paste-able snippet per client — and stdout has to stay pure JSON.
func TestMCPSetupJSONCarriesTheWholeReport(t *testing.T) {
	registered := &fakeTarget{
		name: "claude-code",
		result: mcpsetup.Result{
			Status:     mcpsetup.StatusUnchanged,
			Detail:     "already configured",
			Manual:     `  "lumi": {}`,
			ManualHint: `add this under "mcpServers"`,
		},
	}
	skipped := &fakeTarget{
		name: "codex",
		result: mcpsetup.Result{
			Status:     mcpsetup.StatusSkipped,
			Detail:     "the codex CLI was not found on PATH",
			Manual:     "[mcp_servers.lumi]",
			ManualHint: "add this to ~/.codex/config.toml",
		},
	}
	dataDir, run := newSetupTest(t, registered, skipped)

	stdout, stderr, err := run("--dry-run", "--json")
	if err != nil {
		t.Fatalf("setup --dry-run --json: %v", err)
	}
	if stderr != "" {
		t.Errorf("--json wrote to stderr:\n%s", stderr)
	}

	var report mcpSetupReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not JSON (%v):\n%s", err, stdout)
	}
	if report.Name != "lumi" {
		t.Errorf("name = %q, want lumi", report.Name)
	}
	if report.Command != "/usr/local/bin/lumi" {
		t.Errorf("command = %q, want the resolved binary", report.Command)
	}
	if want := []string{"mcp", "--data-dir", dataDir}; !slices.Equal(report.Args, want) {
		t.Errorf("args = %v, want %v", report.Args, want)
	}
	if !strings.Contains(report.CommandLine, dataDir) {
		t.Errorf("command_line = %q, want the absolute data dir", report.CommandLine)
	}
	if !report.DryRun {
		t.Error("dry_run = false under --dry-run")
	}
	if len(report.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(report.Results))
	}
	if report.Results[0].Target != "claude-code" || report.Results[0].Status != mcpsetup.StatusUnchanged {
		t.Errorf("first result = %+v", report.Results[0])
	}
	// The snippet travels with every result, including the one that needs no
	// action: the app's "Copy client config" button is unconditional, and Swift
	// must never build the JSON or TOML itself.
	for _, r := range report.Results {
		if r.Manual == "" || r.ManualHint == "" {
			t.Errorf("%s carries no manual snippet: %+v", r.Target, r)
		}
	}
}

// A conflict still has to produce a complete document, because the app decodes
// the payload first and only consults the exit status when there was nothing to
// read.
func TestMCPSetupJSONPrintsThePayloadEvenWhenItFails(t *testing.T) {
	conflicted := &fakeTarget{
		name: "claude-code",
		result: mcpsetup.Result{
			Status:  mcpsetup.StatusConflict,
			Detail:  "an entry with different settings already exists",
			Current: "/opt/homebrew/bin/lumi mcp",
		},
		err: errors.New("refusing to overwrite"),
	}
	_, run := newSetupTest(t, conflicted)

	stdout, _, err := run("--json")
	if err == nil {
		t.Fatal("setup returned nil despite a conflict")
	}
	var report mcpSetupReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not JSON (%v):\n%s", err, stdout)
	}
	if len(report.Results) != 1 || report.Results[0].Status != mcpsetup.StatusConflict {
		t.Fatalf("report does not describe the conflict: %+v", report)
	}
	if report.Results[0].Current != "/opt/homebrew/bin/lumi mcp" {
		t.Errorf("current = %q, want the existing entry", report.Results[0].Current)
	}
	if report.DryRun {
		t.Error("dry_run = true without --dry-run")
	}
}

// The human output is the shipped contract and --json must not have moved it.
func TestMCPSetupWithoutJSONIsUnchanged(t *testing.T) {
	target := &fakeTarget{
		name:   "claude-code",
		result: mcpsetup.Result{Status: mcpsetup.StatusAdded, Detail: "/usr/local/bin/lumi mcp", Changed: true},
	}
	_, run := newSetupTest(t, target)

	stdout, _, err := run()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("the default run emitted JSON:\n%s", stdout)
	}
	if !strings.Contains(stdout, "claude-code") || !strings.Contains(stdout, "added") {
		t.Errorf("the default run lost its results line:\n%s", stdout)
	}
}

// A target sets StatusAdded before it attempts the write and returns that same
// result when the write fails. Without the error travelling in the payload, a
// reader of stdout alone renders "registered" for a run that registered
// nothing.
func TestMCPSetupJSONCarriesTheFailureWithTheResult(t *testing.T) {
	failed := &fakeTarget{
		name: "claude-code",
		result: mcpsetup.Result{
			Status:  mcpsetup.StatusAdded,
			Detail:  "/usr/local/bin/lumi mcp",
			Changed: false,
		},
		err: errors.New("claude mcp add failed: exit status 1"),
	}
	succeeded := &fakeTarget{
		name: "codex",
		result: mcpsetup.Result{
			Status:  mcpsetup.StatusAdded,
			Detail:  "/usr/local/bin/lumi mcp",
			Changed: true,
		},
	}
	_, run := newSetupTest(t, failed, succeeded)

	stdout, _, err := run("--json")
	if err == nil {
		t.Fatal("setup returned nil despite a failed write")
	}
	var report mcpSetupReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not JSON (%v):\n%s", err, stdout)
	}
	if len(report.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(report.Results))
	}
	// The error is on the client that produced it, and only on that one.
	if !strings.Contains(report.Results[0].Error, "claude mcp add failed") {
		t.Errorf("the failing client carries no error: %+v", report.Results[0])
	}
	if report.Results[1].Error != "" {
		t.Errorf("the succeeding client carries an error: %+v", report.Results[1])
	}
	// The status is still what the target reported. Correcting it here would
	// hide which step failed; the error is what says the write did not land.
	if report.Results[0].Status != mcpsetup.StatusAdded {
		t.Errorf("status = %q, want the target's own status", report.Results[0].Status)
	}
}
