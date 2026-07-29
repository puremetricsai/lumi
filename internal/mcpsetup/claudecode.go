package mcpsetup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// claudeCodeName is the display name used in results and error messages.
const claudeCodeName = "claude-code"

// claudeCLITimeout bounds each `claude` invocation. The CLI is doing a local
// file edit; if it has not finished in ten seconds it is wedged, and hanging a
// setup command forever is worse than reporting the failure.
const claudeCLITimeout = 10 * time.Second

// ClaudeCode configures Claude Code's user-scope MCP servers.
//
// Reading and writing are deliberately asymmetric. ~/.claude.json is not a
// config file so much as live application state — caches, session data, machine
// identity — that a running Claude Code rewrites underneath us, so a
// read-modify-write against it can silently drop whatever the app wrote in the
// interim. Reading cannot corrupt anything, and it is the only way to get an
// exact structured comparison; `claude mcp get` prints for humans, and cannot
// answer "does the existing entry differ from this one". So: read the file,
// write only through the CLI.
//
// The zero value works in production. Every field exists so tests can run
// without a `claude` binary or a real home directory.
type ClaudeCode struct {
	// Runner runs the claude CLI. nil uses os/exec.
	Runner Runner
	// CLIPath is the claude binary. Empty resolves it via LookPath and the
	// known install locations.
	CLIPath string
	// LookPath finds a binary on PATH. nil uses exec.LookPath.
	LookPath func(string) (string, error)
	// StatePath is ~/.claude.json when empty. It is only ever read.
	StatePath string
	// Required makes an unconfigurable client an error rather than a skip. The
	// caller sets it when the user named this client explicitly.
	Required bool
}

func (c *ClaudeCode) Name() string { return claudeCodeName }

// claudeCLICandidates are the install locations checked when `claude` is not on
// PATH. A non-login shell frequently lacks ~/.local/bin, so a bare LookPath
// misses an install that plainly exists.
func claudeCLICandidates(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
	}
}

// resolveCLI finds the claude binary, or returns "" if there is none.
func (c *ClaudeCode) resolveCLI() string {
	if c.CLIPath != "" {
		return c.CLIPath
	}
	lookPath := c.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if path, err := lookPath("claude"); err == nil {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, candidate := range claudeCLICandidates(home) {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// statePath resolves the file read for detection.
func (c *ClaudeCode) statePath() (string, error) {
	if c.StatePath != "" {
		return c.StatePath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// existingEntry reports the entry currently registered under name.
//
// Every failure here — missing file, unreadable file, invalid JSON, absent or
// null mcpServers — means "not configured", never an error. The file belongs to
// another application; Lumi is in no position to be strict about its contents,
// and refusing to proceed because Claude Code's state file was mid-write would
// make this command flaky for no gain.
func (c *ClaudeCode) existingEntry(name string) (entry, bool) {
	path, err := c.statePath()
	if err != nil {
		return entry{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return entry{}, false
	}
	// Only mcpServers is decoded. The rest of the file is ~150KB of state we
	// have no business materialising.
	var state struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return entry{}, false
	}
	raw, ok := state.MCPServers[name]
	if !ok {
		return entry{}, false
	}
	var existing entry
	if err := json.Unmarshal(raw, &existing); err != nil {
		return entry{}, false
	}
	return existing, true
}

func (c *ClaudeCode) Apply(ctx context.Context, spec Spec, opts Options) (Result, error) {
	result := Result{Target: claudeCodeName}

	cli := c.resolveCLI()
	if cli == "" {
		result.Status = StatusSkipped
		result.Detail = "the claude CLI was not found on PATH"
		result.Manual = ManualSnippet(spec)
		if c.Required {
			return result, notInstalledErr(claudeCodeName, result.Detail)
		}
		return result, nil
	}

	existing, found := c.existingEntry(spec.Name)
	switch {
	case found && existing.matches(spec):
		result.Status = StatusUnchanged
		result.Detail = "already configured"
		return result, nil
	case found && !opts.Force:
		result.Status = StatusConflict
		result.Detail = "an entry with different settings already exists"
		result.Current = existing.commandLine()
		result.Manual = ManualSnippet(spec)
		return result, conflictErr(claudeCodeName, spec.Name)
	}

	result.Status = StatusAdded
	if found {
		result.Status = StatusReplaced
	}
	result.Detail = spec.CommandLine()

	if opts.DryRun {
		return result, nil
	}

	runner := c.Runner
	if runner == nil {
		runner = execRunner{}
	}
	ctx, cancel := context.WithTimeout(ctx, claudeCLITimeout)
	defer cancel()

	if found {
		// Remove first rather than relying on `add` to overwrite: that
		// behaviour is undocumented, and a version that rejects a duplicate
		// instead would leave --force silently doing nothing.
		if out, err := runner.Run(ctx, cli, "mcp", "remove", spec.Name, "--scope", "user"); err != nil {
			return result, fmt.Errorf("%s: claude mcp remove failed: %w%s",
				claudeCodeName, err, indentOutput(out))
		}
	}

	// The -- separator is load-bearing. Without it claude's own flag parser
	// consumes --data-dir and the server ends up pointed at the default index.
	args := append([]string{"mcp", "add", "--scope", "user", spec.Name, "--", spec.Command}, spec.Args...)
	if out, err := runner.Run(ctx, cli, args...); err != nil {
		return result, fmt.Errorf("%s: claude mcp add failed: %w%s",
			claudeCodeName, err, indentOutput(out))
	}
	result.Changed = true
	return result, nil
}

// indentOutput appends a subprocess's combined output to an error message, or
// nothing when it was silent. Without it a broken or outdated `claude` reports
// only "exit status 1".
func indentOutput(out string) string {
	out = strings.TrimRight(out, "\r\n")
	if out == "" {
		return ""
	}
	return "\n" + out
}

var _ Target = (*ClaudeCode)(nil)
