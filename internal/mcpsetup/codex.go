package mcpsetup

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"time"
)

// codexName is the display name used in results and error messages.
const codexName = "codex"

// codexCLITimeout bounds each `codex` invocation, for the same reason
// claudeCLITimeout does: the CLI is doing a local file edit, and hanging a
// setup command forever is worse than reporting the failure.
const codexCLITimeout = 10 * time.Second

// Codex configures Codex CLI's MCP servers in ~/.codex/config.toml.
//
// Unlike the two Claude targets, this one goes through the client's CLI in both
// directions. `codex mcp get --json` prints a structured entry, so there is no
// need to parse the file to answer "does the existing entry differ from this
// one" — the question `claude mcp get` could not answer. And `codex mcp add`
// edits config.toml through toml_edit, preserving comments and the ~35 sibling
// tables a real file carries, which is more than a round-trip through a Go TOML
// encoder could promise. Lumi therefore never touches that file itself.
//
// There is no scope flag to pass: `codex mcp add` always writes the user-level
// $CODEX_HOME/config.toml.
//
// The zero value works in production. Every field exists so tests can run
// without a `codex` binary on the machine.
type Codex struct {
	// Runner runs the codex CLI. nil uses os/exec.
	Runner Runner
	// CLIPath is the codex binary. Empty resolves it via LookPath.
	CLIPath string
	// LookPath finds a binary on PATH. nil uses exec.LookPath.
	LookPath func(string) (string, error)
	// Required makes an unconfigurable client an error rather than a skip. The
	// caller sets it when the user named this client explicitly.
	Required bool
}

func (c *Codex) Name() string { return codexName }

// resolveCLI finds the codex binary, or returns "" if there is none.
//
// PATH only — deliberately no candidate install locations, and deliberately no
// probe of ~/.codex. That directory is created and populated by the ChatGPT
// desktop app too, so its presence is no evidence the CLI is installed, and its
// absence is no evidence it is not. The binary is the only honest signal.
func (c *Codex) resolveCLI() string {
	if c.CLIPath != "" {
		return c.CLIPath
	}
	lookPath := c.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if path, err := lookPath("codex"); err == nil {
		return path
	}
	return ""
}

// codexEntry is what `codex mcp get --json` says about one name.
type codexEntry struct {
	// entry is the registration, set only when readable.
	entry entry
	// raw is the output it was decoded from. It is the only record of an entry
	// Lumi could not otherwise render.
	raw string
	// enabled mirrors codex's own `enabled` field. A disabled entry is still an
	// entry, but codex will not launch it.
	enabled bool
	// found reports whether codex knows the name at all.
	found bool
	// readable reports whether the entry is a plain stdio command Lumi can
	// compare against and re-add.
	readable bool
}

// matches reports whether this is already the registration spec asks for.
//
// A disabled entry never matches, however identical its command and args:
// reporting "unchanged" for one would tell the user setup succeeded while codex
// silently refuses to launch the server — the quiet-failure mode this whole
// subsystem exists to avoid. It is a difference rather than something
// unreadable, so --force fixes it.
func (c codexEntry) matches(spec Spec) bool {
	return c.readable && c.enabled && c.entry.matches(spec)
}

// currentLine renders the existing entry for the conflict diff. Disabled is
// called out, because otherwise the current and desired lines print identically
// and the conflict reads as a bug.
func (c codexEntry) currentLine() string {
	if !c.readable {
		return unreadableEntry
	}
	if !c.enabled {
		return c.entry.commandLine() + " (disabled)"
	}
	return c.entry.commandLine()
}

// restorable reports whether this entry could be put back through `codex mcp
// add` exactly as it was. An unreadable one cannot be rendered at all, and a
// disabled one cannot be re-disabled — `codex mcp add` has no flag for it — so
// a rollback would quietly enable a server the user had switched off.
func (c codexEntry) restorable() bool { return c.readable && c.enabled }

// existingEntry reports what codex currently holds under name.
//
// `codex mcp get` exits non-zero both for a name it has never heard of and for
// a config it cannot load, and the exit code alone does not separate them. So a
// failure is followed by `codex mcp list --json`, which is a pure health probe:
// it succeeds (printing `[]` on an empty config) whenever the CLI and the file
// are fine, and fails with the same diagnostic whenever they are not.
//
// The distinction has to be made here rather than left to the write that
// follows, because under --dry-run there is no write. Treating an unreadable
// config as "not configured" would make the documented health check print
// "would add" and exit 0 on a machine where a real run cannot get that far.
//
// An entry that is present but not a plain stdio command is reported as
// found-but-unreadable rather than absent. It is the user's registration, and a
// --force-less replacement would destroy it silently. Lumi cannot render an
// HTTP entry back through `codex mcp add`'s stdio form, so it declines to
// pretend it understands one.
func (c *Codex) existingEntry(ctx context.Context, runner Runner, cli, name string) (codexEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, codexCLITimeout)
	defer cancel()

	out, err := runner.Run(ctx, cli, "mcp", "get", name, "--json")
	if err != nil {
		if _, probeErr := runner.Run(ctx, cli, "mcp", "list", "--json"); probeErr != nil {
			return codexEntry{}, fmt.Errorf(
				"%s: codex cannot read its own configuration, so Lumi cannot tell "+
					"whether %q is already registered: %w%s",
				codexName, name, err, indentOutput(out))
		}
		// codex is healthy and simply does not know the name.
		return codexEntry{}, nil
	}

	var got struct {
		// A pointer: absent and false are the same to codex — it writes no
		// `enabled` key for a server that is on — but conflating them in the
		// decoder would read every entry as disabled.
		Enabled   *bool `json:"enabled"`
		Transport struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"transport"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		return codexEntry{raw: out, found: true}, nil
	}
	if got.Transport.Type != "stdio" || got.Transport.Command == "" {
		return codexEntry{raw: out, found: true}, nil
	}
	return codexEntry{
		entry: entry{
			Command: got.Transport.Command,
			Args:    got.Transport.Args,
			Env:     got.Transport.Env,
		},
		raw:      out,
		enabled:  got.Enabled == nil || *got.Enabled,
		found:    true,
		readable: true,
	}, nil
}

func (c *Codex) Apply(ctx context.Context, spec Spec, opts Options) (Result, error) {
	result := Result{Target: codexName}
	// The paste-able snippet is filled in before anything else can go
	// wrong, so every result carries it. It costs a string on the happy
	// path and it is what lets a caller offer "copy client config"
	// unconditionally instead of only after Lumi has declined to write.
	result.manualTOML(spec)

	cli := c.resolveCLI()
	if cli == "" {
		result.Status = StatusSkipped
		result.Detail = "the codex CLI was not found on PATH"
		if c.Required {
			return result, notInstalledErr(codexName, result.Detail)
		}
		return result, nil
	}

	runner := c.Runner
	if runner == nil {
		runner = execRunner{}
	}

	existing, err := c.existingEntry(ctx, runner, cli, spec.Name)
	if err != nil {
		// A read Lumi cannot trust is fatal in both modes, so --dry-run keeps
		// telling the truth about what a real run would face.
		result.Status = StatusFailed
		result.Detail = "codex cannot read its own configuration"
		return result, err
	}

	switch {
	case existing.found && !existing.readable && !opts.Force:
		result.Status = StatusConflict
		result.Detail = "an entry already exists that Lumi cannot read"
		result.Current = existing.currentLine()
		return result, conflictErr(codexName, spec.Name)
	case existing.matches(spec):
		result.Status = StatusUnchanged
		result.Detail = "already configured"
		return result, nil
	case existing.found && !existing.enabled && !opts.Force:
		result.Status = StatusConflict
		result.Detail = "the entry already exists but codex has it disabled"
		result.Current = existing.currentLine()
		return result, conflictErr(codexName, spec.Name)
	case existing.found && !opts.Force:
		result.Status = StatusConflict
		result.Detail = "an entry with different settings already exists"
		result.Current = existing.currentLine()
		return result, conflictErr(codexName, spec.Name)
	}

	result.Status = StatusAdded
	if existing.found {
		result.Status = StatusReplaced
	}
	result.Detail = spec.CommandLine()

	if opts.DryRun {
		return result, nil
	}

	ctx, cancel := context.WithTimeout(ctx, codexCLITimeout)
	defer cancel()

	if existing.found {
		// Remove first rather than relying on `add` to overwrite: the same
		// reasoning as Claude Code. A version that rejects a duplicate instead
		// would leave --force silently doing nothing.
		if out, err := runner.Run(ctx, cli, "mcp", "remove", spec.Name); err != nil {
			return result, fmt.Errorf("%s: codex mcp remove failed: %w%s",
				codexName, err, indentOutput(out))
		}
	}

	// The -- separator is load-bearing here exactly as it is for claude: without
	// it codex's own flag parser consumes --data-dir and the server ends up
	// pointed at the default index.
	if out, err := runner.Run(ctx, cli, codexAddArgs(spec.Name, newEntry(spec))...); err != nil {
		addErr := fmt.Errorf("%s: codex mcp add failed: %w%s",
			codexName, err, indentOutput(out))
		if !existing.found {
			return result, addErr
		}
		// The remove above already succeeded, so the user is currently left
		// with no entry at all. Put theirs back.
		return result, c.restore(ctx, runner, cli, spec.Name, existing, addErr)
	}
	result.Changed = true
	return result, nil
}

// restore re-adds the entry --force removed, after the replacing add failed.
//
// Without it a failed add is silently destructive: remove succeeds, add does
// not, and a registration the user had before running setup is simply gone.
// Both outcomes are folded into the returned error, because either way the run
// failed; what differs is whether the user still has to do something.
//
// Some entries cannot be put back faithfully. An unreadable one cannot be
// rendered at all, and a disabled one cannot be re-disabled, because `codex mcp
// add` has no flag for it — re-adding would hand the user back a server they
// had deliberately switched off, and call it a restore. Both get their raw
// `codex mcp get --json` output instead: not paste-able into config.toml, but
// the only complete record of what was removed, and printing it beats saying
// something is gone without saying what.
func (c *Codex) restore(ctx context.Context, runner Runner, cli, name string, old codexEntry, cause error) error {
	if !old.restorable() {
		why := "because Lumi could not read it"
		if old.readable {
			why = "because codex has no way to re-add it disabled"
		}
		return fmt.Errorf("%w\n%s: the previous %q entry was removed and cannot be "+
			"restored automatically, %s.\n"+
			"It was:\n%s\nRe-add it with `codex mcp add`.",
			cause, codexName, name, why, indentOutput(old.raw))
	}

	// A fresh deadline, detached from cancellation. The add may well have
	// failed by exhausting the shared timeout or because the user interrupted
	// the command, and an already-done context would skip the rollback at
	// exactly the moment it is needed.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), codexCLITimeout)
	defer cancel()

	if out, err := runner.Run(ctx, cli, codexAddArgs(name, old.entry)...); err != nil {
		return fmt.Errorf("%w\n%s: the previous %q entry was removed and could not be "+
			"restored (%v%s).\nRe-add it by hand in ~/.codex/config.toml:\n%s",
			cause, codexName, name, err, indentOutput(out), entryTOMLSnippet(name, old.entry))
	}
	return fmt.Errorf("%w\n%s: the previous %q entry was restored; nothing was changed",
		cause, codexName, name)
}

// codexAddArgs renders `codex mcp add` for an entry. It serves both the new
// entry and the rollback, so a restored entry keeps whatever env the original
// carried.
//
// No --scope: codex has none, and always writes $CODEX_HOME/config.toml. No
// --transport either: codex spells a non-stdio server with --url instead, and
// such an entry never reaches here, because existingEntry classifies it as
// unreadable.
func codexAddArgs(name string, e entry) []string {
	args := []string{"mcp", "add"}
	// Sorted so the invocation is deterministic and testable.
	for _, key := range slices.Sorted(maps.Keys(e.Env)) {
		args = append(args, "--env", key+"="+e.Env[key])
	}
	args = append(args, name, "--", e.Command)
	return append(args, e.Args...)
}

var _ Target = (*Codex)(nil)
