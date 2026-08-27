package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/puremetricsai/lumi/internal/mcpsetup"
	"github.com/spf13/cobra"
)

// verifyTimeout bounds the `lumi version` probe run before anything is written.
const verifyTimeout = 5 * time.Second

// These three vars are the test seams for a command whose whole job is to
// touch files outside the repository. Without them a test run would rewrite the
// developer's own Claude configuration.
var (
	// resolveLumiBinary reports the path to the running lumi binary. Under
	// `go test` os.Executable() is the test binary, so tests substitute this.
	resolveLumiBinary = os.Executable
	// verifyLumiBinary checks the resolved binary actually runs.
	verifyLumiBinary = verifyBinary
	// newSetupTargets builds the clients to configure.
	newSetupTargets = defaultSetupTargets
)

// clientSelection is the set of clients one --client value asks for.
//
// A struct rather than positional bools: at two clients the argument list was
// still readable, at three a caller passing them in the wrong order would
// compile and silently configure the wrong client.
type clientSelection struct {
	code    bool
	desktop bool
	codex   bool
	// explicit reports whether the user named a specific client. A client asked
	// for by name that turns out to be unconfigurable is an error, while the
	// same client reached through "all" is a visible skip.
	explicit bool
}

// defaultSetupTargets builds the real clients. explicit marks the target as
// Required, which turns "not installed" from a skip into an error.
func defaultSetupTargets(sel clientSelection) []mcpsetup.Target {
	var targets []mcpsetup.Target
	if sel.code {
		targets = append(targets, &mcpsetup.ClaudeCode{Required: sel.explicit})
	}
	if sel.desktop {
		targets = append(targets, &mcpsetup.ClaudeDesktop{Required: sel.explicit})
	}
	if sel.codex {
		targets = append(targets, &mcpsetup.Codex{Required: sel.explicit})
	}
	return targets
}

type mcpSetupFlags struct {
	client string
	name   string
	dryRun bool
	force  bool
	asJSON bool
}

// mcpSetupReport is `lumi mcp setup --json`.
//
// It carries the resolved binary and argv as well as the per-client results,
// because a reader showing "this is what gets registered" would otherwise have
// to rebuild them — and `lumiBinaryPath` plus the absolute --data-dir are
// exactly the parts it cannot rebuild correctly.
//
// `--json` is orthogonal to `--dry-run`: paired with it this is the read-only
// status query the package otherwise has no path for, and on its own it is the
// machine-readable outcome of a real run.
type mcpSetupReport struct {
	Name        string               `json:"name"`
	Command     string               `json:"command"`
	Args        []string             `json:"args"`
	CommandLine string               `json:"command_line"`
	DryRun      bool                 `json:"dry_run"`
	Results     []mcpSetupResultJSON `json:"results"`
}

// mcpSetupResultJSON is one client's result plus the error that client
// produced, if any.
//
// The error has to travel *with* the result, because the status alone is not
// enough to tell a machine reader what happened. A target sets StatusAdded
// before it attempts the write and returns that same result when the write
// fails, so a reader that trusts the status renders "registered" for a run that
// registered nothing. A human never sees that: the error is on stderr and the
// exit code is non-zero. A reader decoding stdout would, which is why this
// exists.
type mcpSetupResultJSON struct {
	mcpsetup.Result
	// Error is the failure this client reported, empty when it succeeded.
	Error string `json:"error,omitempty"`
}

// mcpSetupCommand registers `lumi mcp` with the MCP clients on this machine.
//
// Everything the server needs is baked into the generated argv — an absolute
// binary path and an absolute --data-dir, always, even at the default root.
// That is the same bare-environment constraint `lumi mcp` itself documents: a
// client spawns the server with no environment, so LUMI_HOME never reaches it.
// It also makes the desired entry a pure function of (binary, root), which is
// what lets the "already configured?" check be an exact comparison.
func (a *app) mcpSetupCommand() *cobra.Command {
	var f mcpSetupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Register lumi as an MCP server with Claude Code, Claude Desktop, and Codex CLI",
		Long: "Write the lumi MCP server entry into the configuration of every MCP client\n" +
			"installed on this machine — Claude Code, Claude Desktop, and Codex CLI.\n" +
			"Clients launch `lumi mcp` themselves over stdio, so nothing runs in the\n" +
			"background and no port is opened.\n\n" +
			"Setup is idempotent: a second run reports 'unchanged' and writes nothing. An\n" +
			"entry that already exists with different settings is never overwritten without\n" +
			"--force.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runMCPSetup(cmd, f)
		},
	}
	cmd.Flags().StringVar(&f.client, "client", "all", "which clients to configure (code, desktop, codex, all)")
	cmd.Flags().StringVar(&f.name, "name", "lumi", "name to register the server under")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "report what would change without writing anything")
	cmd.Flags().BoolVar(&f.force, "force", false, "replace an existing entry that differs")
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "emit JSON")
	return cmd
}

func (a *app) runMCPSetup(cmd *cobra.Command, f mcpSetupFlags) error {
	selection, err := parseClientSelection(f.client)
	if err != nil {
		return err
	}
	if strings.TrimSpace(f.name) == "" {
		return errors.New("--name must not be empty")
	}

	paths, err := a.paths()
	if err != nil {
		return err
	}
	// Create the data directory now, so the path baked into the entry exists by
	// the time an agent first launches the server. Not under --dry-run: that
	// flag promises to write nothing, and creating three directories under a
	// mistyped --data-dir is exactly the kind of trace a health check must not
	// leave. The previewed entry still names the root either way.
	if !f.dryRun {
		if err := paths.Ensure(); err != nil {
			return err
		}
	}

	exe, err := lumiBinaryPath()
	if err != nil {
		return err
	}
	if err := verifyLumiBinary(cmd.Context(), exe); err != nil {
		return err
	}

	spec := mcpsetup.Spec{
		Name:    f.name,
		Command: exe,
		Args:    []string{"mcp", "--data-dir", paths.Root},
	}
	opts := mcpsetup.Options{Force: f.force, DryRun: f.dryRun}

	targets := newSetupTargets(selection)
	results := make([]mcpsetup.Result, 0, len(targets))
	// Never nil: a JSON null here would make every reader special-case "no
	// clients" separately from "no results".
	rows := make([]mcpSetupResultJSON, 0, len(targets))
	var errs []error
	for _, target := range targets {
		// One client's failure must never stop the others from being
		// configured; the errors are joined and returned once at the end.
		result, err := target.Apply(cmd.Context(), spec, opts)
		results = append(results, result)
		row := mcpSetupResultJSON{Result: result}
		if err != nil {
			errs = append(errs, err)
			// Carried with the result rather than only joined, so --json can
			// say which client failed.
			row.Error = err.Error()
		}
		rows = append(rows, row)
	}

	if f.asJSON {
		// The payload is written before the joined error is returned, so a run
		// that ends in a conflict still prints a complete document and then
		// exits non-zero — the same shape `permissions --json` has, and what
		// lets a reader decode the payload first and consult the status only
		// when there was nothing to read.
		if err := writeSetupJSON(cmd.OutOrStdout(), spec, rows, f.dryRun); err != nil {
			return err
		}
		return errors.Join(errs...)
	}

	printSetupResults(cmd.OutOrStdout(), results, f.dryRun)
	printSetupDiagnostics(cmd.ErrOrStderr(), results, spec, f.dryRun)
	return errors.Join(errs...)
}

// parseClientSelection maps --client onto the target set.
//
// A `Target`'s own name is accepted alongside the short one, because it is the
// only client name a caller reading the JSON has: Lumi.app hands the `target` it
// was given straight back rather than keeping a second copy of this vocabulary in
// Swift. A fourth client needs both of its names here.
func parseClientSelection(value string) (clientSelection, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "all":
		return clientSelection{code: true, desktop: true, codex: true}, nil
	case "code", "claude-code":
		return clientSelection{code: true, explicit: true}, nil
	case "desktop", "claude-desktop":
		return clientSelection{desktop: true, explicit: true}, nil
	case "codex":
		return clientSelection{codex: true, explicit: true}, nil
	default:
		return clientSelection{}, fmt.Errorf(
			"unknown --client %q: expected code, desktop, codex, or all", value)
	}
}

// lumiBinaryPath resolves the binary to configure.
//
// Deliberately no filepath.EvalSymlinks: the durable name is whatever path this
// process was reached through, and resolving one can only make it less stable. A
// packaged install is /Applications/Lumi.app/Contents/MacOS/lumi, a real file
// that install.sh replaces along with the bundle around it — same path, new
// file — so there is nothing to resolve today. Reached through a symlink
// instead — a developer's own, whose target moves every version bump — the
// link is the name that survives.
func lumiBinaryPath() (string, error) {
	exe, err := resolveLumiBinary()
	if err != nil {
		return "", fmt.Errorf("locate the lumi binary: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("locate the lumi binary: %w", err)
	}
	if isTemporaryBinary(exe) {
		return "", fmt.Errorf(
			"refusing to configure %s: that binary lives in a temporary build directory "+
				"and will be deleted when this process exits.\n"+
				"Run `task build`, then `./lumi mcp setup`.", exe)
	}
	return exe, nil
}

// isTemporaryBinary reports whether a path is a throwaway build artifact. Under
// `go run` os.Executable() points into the build cache, and that path is
// unlinked the moment the process exits — configuring it would produce an entry
// that is already broken by the time the success line prints.
func isTemporaryBinary(exe string) bool {
	if strings.HasPrefix(exe, filepath.Clean(os.TempDir())+string(os.PathSeparator)) {
		return true
	}
	for _, part := range strings.Split(exe, string(os.PathSeparator)) {
		if strings.HasPrefix(part, "go-build") {
			return true
		}
	}
	return false
}

// verifyBinary runs `lumi version` before anything is written. It costs
// milliseconds and catches the two failures that actually happen — a path that
// no longer resolves, and a build that cannot link liblumispeech.a — at the one
// moment the user can still act on them, rather than as a silent disappearance
// on the agent's side days later.
func verifyBinary(ctx context.Context, exe string) error {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "version")
	cmd.Stdin = nil
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimRight(string(out), "\r\n")
		if msg != "" {
			msg = "\n" + msg
		}
		return fmt.Errorf("%s is not runnable (%w)%s", exe, err, msg)
	}
	return nil
}

// writeSetupJSON writes the machine-readable report.
//
// Nothing advisory is emitted alongside it: every reason a human run prints to
// stderr — a skip's manual snippet, a conflict's current entry, whether Claude
// Desktop actually changed — is a field of the payload, and a second rendering
// of it on stderr would be a copy that can drift.
func writeSetupJSON(w io.Writer, spec mcpsetup.Spec, rows []mcpSetupResultJSON, dryRun bool) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(mcpSetupReport{
		Name:        spec.Name,
		Command:     spec.Command,
		Args:        spec.Args,
		CommandLine: spec.CommandLine(),
		DryRun:      dryRun,
		Results:     rows,
	})
}

// printSetupResults writes one aligned line per client to stdout. These are the
// command's results; everything advisory goes to stderr.
func printSetupResults(w io.Writer, results []mcpsetup.Result, dryRun bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Target, statusVerb(r.Status, dryRun), r.Detail)
	}
	tw.Flush()
}

// statusVerb renders a status, in the conditional under --dry-run. Statuses
// themselves are identical either way — only the wording changes — so a
// dry run stays a faithful preview.
func statusVerb(status mcpsetup.Status, dryRun bool) string {
	if !dryRun {
		return string(status)
	}
	switch status {
	case mcpsetup.StatusAdded:
		return "would add"
	case mcpsetup.StatusReplaced:
		return "would replace"
	default:
		return string(status)
	}
}

// printSetupDiagnostics writes everything that is not a result: why a client
// was skipped, what a conflict looks like, and what the user has to do next.
func printSetupDiagnostics(w io.Writer, results []mcpsetup.Result, spec mcpsetup.Spec, dryRun bool) {
	desired := spec.CommandLine()
	desktopChanged := false
	for _, r := range results {
		switch r.Status {
		case mcpsetup.StatusSkipped, mcpsetup.StatusFailed:
			// Both mean "nothing was written and you are on your own", so both
			// hand back the snippet. The instruction comes from the target, not
			// from here: the two Claude clients take JSON under "mcpServers" and
			// Codex takes a TOML table, and one hardcoded sentence would be
			// wrong for one of them.
			fmt.Fprintf(w, "\n%s: %s — %s.\n"+
				"  To configure it by hand, %s:\n\n%s\n",
				r.Target, r.Status, r.Detail, r.ManualHint, r.Manual)
		case mcpsetup.StatusConflict:
			fmt.Fprintf(w, "\n%s: %s\n    current:  %s\n    desired:  %s\n"+
				"  Re-run with --force to replace it, or --name to add a second entry.\n",
				r.Target, r.Detail, r.Current, desired)
		}
		if r.Target == "claude-desktop" && r.Changed {
			desktopChanged = true
		}
	}

	// A binary inside the working tree is configured as-is — silently
	// preferring a `lumi` on PATH would configure a different binary than the
	// one the user ran — but the entry breaks if the repository moves, so say so.
	if wd, err := os.Getwd(); err == nil &&
		strings.HasPrefix(spec.Command, wd+string(os.PathSeparator)) {
		fmt.Fprintf(w, "\nNote: this points clients at the development build at %s,\n"+
			"  which stops working if the repository moves. Re-run setup after\n"+
			"  installing lumi somewhere permanent.\n", spec.Command)
	}

	// The reminder is emitted only when Claude Desktop's config actually
	// changed. Printing it on a no-op run trains people to ignore it.
	if desktopChanged && !dryRun {
		fmt.Fprintf(w, "\nQuit and reopen Claude Desktop to load the change.\n")
	}
}
