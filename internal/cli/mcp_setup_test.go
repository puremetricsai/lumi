package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
	swap(t, &newSetupTargets, func(_, _, _ bool) []mcpsetup.Target { return targets })

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
			Status: mcpsetup.StatusSkipped,
			Detail: "Claude Desktop is not installed",
			Manual: `  "lumi": {}`,
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
		value                   string
		code, desktop, explicit bool
		wantErr                 bool
	}{
		"all":            {"all", true, true, false, false},
		"code":           {"code", true, false, true, false},
		"desktop":        {"desktop", false, true, true, false},
		"case insensive": {"Desktop", false, true, true, false},
		"padded":         {" all ", true, true, false, false},
		"unknown":        {"cursor", false, false, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, desktop, explicit, err := parseClientSelection(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseClientSelection(%q) succeeded, want an error", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseClientSelection(%q): %v", tc.value, err)
			}
			if code != tc.code || desktop != tc.desktop || explicit != tc.explicit {
				t.Errorf("got code=%v desktop=%v explicit=%v, want %v/%v/%v",
					code, desktop, explicit, tc.code, tc.desktop, tc.explicit)
			}
		})
	}
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
