// Package mcpsetup registers a stdio MCP server with the MCP clients installed
// on this machine.
//
// It owns what Lumi knows about two foreign configuration formats and nothing
// else. A Spec carries a name, a binary path, and an argv, so this package has
// no opinion about --data-dir, os.Executable, or config.Paths; the caller
// supplies all three. It depends on nothing else of Lumi's.
//
// The two targets are deliberately asymmetric. Claude Code's ~/.claude.json is
// live application state that Claude Code itself rewrites, so it is read for
// detection and written only by shelling out to the `claude` CLI. Claude
// Desktop has no CLI, so its config is read-modify-written in place.
package mcpsetup

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// Spec is the MCP server entry a client should end up holding. It is a pure
// value: two Specs built from the same binary and data directory are equal, and
// that is what makes the "is this already configured" comparison exact rather
// than a heuristic.
type Spec struct {
	// Name is the key the entry lives under in the client's mcpServers map.
	Name string
	// Command is the absolute path to the lumi binary.
	Command string
	// Args is the argv passed to Command, excluding the binary name itself.
	Args []string
}

// CommandLine renders the spec the way a shell would show it, for diffs and
// result lines. It is display-only; nothing parses it back.
func (s Spec) CommandLine() string {
	return strings.Join(append([]string{s.Command}, s.Args...), " ")
}

// Options tunes how a Target applies a Spec.
type Options struct {
	// Force replaces an existing entry that differs from the Spec. Without it,
	// a differing entry is a conflict and nothing is written.
	Force bool
	// DryRun reports what would happen without writing anything or running any
	// subprocess. Statuses are identical to a real run, so a conflict still
	// comes back as a conflict.
	DryRun bool
}

// Status is the outcome of applying a Spec to one client.
type Status string

const (
	// StatusAdded means no entry existed and one was written.
	StatusAdded Status = "added"
	// StatusUnchanged means an identical entry already existed.
	StatusUnchanged Status = "unchanged"
	// StatusReplaced means a differing entry was overwritten under Force.
	StatusReplaced Status = "replaced"
	// StatusConflict means a differing entry was left alone. Always accompanied
	// by an error.
	StatusConflict Status = "conflict"
	// StatusSkipped means the client is not installed on this machine.
	StatusSkipped Status = "skipped"
)

// Result describes what one Target did.
type Result struct {
	// Target is the client's display name, e.g. "claude-code".
	Target string
	// Status is the outcome.
	Status Status
	// Detail is a short phrase for the results line: the rendered command line
	// on a write, a reason on a skip.
	Detail string
	// Current is the existing entry rendered as a command line. Set only on
	// StatusConflict, so the caller can show current against desired.
	Current string
	// Manual is a paste-able JSON snippet, set on skip and conflict so a user
	// who cannot or will not let Lumi write the file still has the answer.
	Manual string
	// Changed reports whether this run actually modified the client's config.
	// It is false under DryRun even when Status is Added or Replaced, which is
	// what keeps "restart the app" reminders honest.
	Changed bool
}

// Target is one MCP client Lumi knows how to configure.
type Target interface {
	// Name is the client's display name.
	Name() string
	// Apply brings the client's config in line with spec. A conflict returns
	// both a Result and an error; the caller prints the Result either way.
	Apply(ctx context.Context, spec Spec, opts Options) (Result, error)
}

// Runner runs an external command to completion and returns its combined
// output. It exists so tests can drive the Claude Code target without a `claude`
// binary on the machine.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// execRunner is the production Runner.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// A configuration helper must never block waiting for input.
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// entry is an mcpServers value, in the shape both clients use.
type entry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// matches reports whether an existing entry already says what spec says.
//
// Only command and args are compared. An absent type and an explicit "stdio"
// mean the same thing, so neither counts as a difference. A non-empty env does
// count: nobody sets one by accident, and silently dropping it would be exactly
// the kind of invisible damage Force exists to gate.
func (e entry) matches(spec Spec) bool {
	if e.Type != "" && e.Type != "stdio" {
		return false
	}
	if len(e.Env) > 0 {
		return false
	}
	return e.Command == spec.Command && slices.Equal(e.Args, spec.Args)
}

// unreadableEntry stands in for Result.Current when a client holds an entry
// under Lumi's name that does not decode into entry. There is no command line
// to render, but the conflict still has to say what is in the way.
const unreadableEntry = "(an entry Lumi cannot read)"

// commandLine renders an existing entry for the conflict diff.
func (e entry) commandLine() string {
	return strings.Join(append([]string{e.Command}, e.Args...), " ")
}

func newEntry(spec Spec) entry {
	return entry{Command: spec.Command, Args: spec.Args}
}

// ManualSnippet renders the mcpServers fragment a user can paste by hand. It is
// shown whenever Lumi declines to write a file itself — a client that is not
// installed, or a conflict it will not overwrite — and it is what makes this
// command useful for clients Lumi does not know about, such as Cursor or Codex.
func ManualSnippet(spec Spec) string {
	return entrySnippet(spec.Name, newEntry(spec))
}

// entrySnippet renders any entry as a paste-able mcpServers fragment. It backs
// ManualSnippet and the rollback error that has to hand back an entry Lumi
// removed but could not restore.
func entrySnippet(entryName string, e entry) string {
	// A key-and-value fragment, not a whole object: it is shown under a "add
	// this under mcpServers" line, so braces of its own would be wrong.
	//
	// Marshalling rather than formatting keeps names and paths containing
	// quotes or backslashes correct; a hand-built string would emit invalid
	// JSON for them.
	name, err := json.Marshal(entryName)
	if err != nil {
		return fmt.Sprintf("  (could not render snippet: %v)", err)
	}
	value, err := json.MarshalIndent(e, "  ", "  ")
	if err != nil {
		// entry holds only strings and maps of strings, so this is unreachable;
		// returning the error text beats panicking in a helper whose whole job
		// is to be printed.
		return fmt.Sprintf("  (could not render snippet: %v)", err)
	}
	return fmt.Sprintf("  %s: %s", name, value)
}

// conflictErr is the error returned alongside StatusConflict. The message names
// both escape hatches, because a user who hits this has to choose between them.
func conflictErr(target, name string) error {
	return fmt.Errorf("%s: refusing to overwrite the existing %q entry; "+
		"re-run with --force to replace it, or --name to add a second entry", target, name)
}

// notInstalledErr is returned when a client named explicitly via --client turns
// out to be unconfigurable. Reached implicitly, the same condition is a skip.
func notInstalledErr(target, reason string) error {
	return fmt.Errorf("%s: %s", target, reason)
}
