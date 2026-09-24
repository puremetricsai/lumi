// Package mcpsetup configures installed agents to use Lumi's MCP tools.
//
// It owns what Lumi knows about foreign configuration formats and nothing
// else. A Spec carries a name, a binary path, and an argv, so this package has
// no opinion about --data-dir, os.Executable, or config.Paths; the caller
// supplies all three. It depends on nothing else of Lumi's.
//
// The targets are deliberately asymmetric, because what each client is
// willing to tell us differs.
//
// Claude Code's ~/.claude.json is live application state that Claude Code
// itself rewrites, so it is read for detection and written only by shelling out
// to the `claude` CLI — and it must be read as a file, because `claude mcp get`
// prints for humans and cannot answer "does the existing entry differ".
//
// Claude Desktop has no CLI, so its config is read-modify-written in place.
//
// Codex CLI is mediated by its own CLI in both directions. `codex mcp get
// --json` prints a structured, comparable entry, which removes the reason to
// read the file; and `codex mcp add`/`remove` edit ~/.codex/config.toml through
// toml_edit, preserving comments and sibling keys, which is more than a
// round-trip through a Go TOML encoder could promise. So Lumi never touches
// that file itself, and this package stays free of a TOML dependency.
package mcpsetup

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultName is the server name setup uses unless told otherwise, and the only
// one Pi's extension supports.
const DefaultName = "lumi"

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
	// DryRun reports what would happen without writing anything. Statuses are
	// identical to a real run, so a conflict still comes back as a conflict.
	// Read-only detection still happens — for Codex that means a `codex mcp
	// get`, since its CLI is the only thing that will describe an entry — but
	// nothing that could modify a client's config is run.
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
	// StatusFailed means the client is installed but could not be inspected, so
	// Lumi does not know what it holds and wrote nothing. Always accompanied by
	// an error. It is deliberately not StatusConflict: nothing is in the way,
	// and offering --force for it would be advice that cannot work.
	StatusFailed Status = "failed"
)

// Result describes what one Target did.
//
// The json tags are the machine-readable contract `lumi mcp setup --json`
// prints; the field names are this package's and the wire names are fixed here
// so a rename cannot silently break a reader.
type Result struct {
	// Target is the client's display name, e.g. "claude-code".
	Target string `json:"target"`
	// Status is the outcome.
	Status Status `json:"status"`
	// Detail is a short phrase for the results line: the rendered command line
	// on a write, a reason on a skip.
	Detail string `json:"detail"`
	// Current is the existing entry rendered as a command line. Set only on
	// StatusConflict, so the caller can show current against desired.
	Current string `json:"current"`
	// Manual is a paste-able config snippet, set on every result so a caller
	// can offer it unprompted and a user who cannot or will not let Lumi write
	// the file still has the answer. Its format follows the client — JSON for
	// the Claude targets, TOML for Codex, TypeScript for Pi — which is why it is built here and
	// never by a caller.
	Manual string `json:"manual"`
	// ManualHint is the sentence fragment introducing Manual, e.g. `add this
	// under "mcpServers"`. It travels with Manual because the two formats need
	// different instructions, and a caller printing one hardcoded sentence for
	// all clients would tell a Codex user to paste TOML into a JSON object.
	ManualHint string `json:"manual_hint"`
	// Changed reports whether this run actually modified the client's config.
	// It is false under DryRun even when Status is Added or Replaced, which is
	// what keeps "restart the app" reminders honest.
	Changed bool `json:"changed"`
	// AfterChange is what the user must do for the client to load the change,
	// e.g. relaunch it. Set only alongside Changed, and only by a client that
	// needs it, so a caller prints whatever arrives and never keeps its own list
	// of which clients need a reload.
	AfterChange string `json:"after_change"`
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
	// The user's own PATH, not launchd's. A client CLI installed by npm is a
	// `#!/usr/bin/env node` script, so resolving it is not enough — without an
	// interpreter on PATH it exits 127 and the target reports the client as
	// unreadable rather than unrunnable. os/exec keeps the last duplicate key.
	cmd.Env = append(os.Environ(), "PATH="+userPATH())
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

// The two manual-configuration instructions, one per config format.
const (
	jsonManualHint = `add this under "mcpServers"`
	tomlManualHint = "add this to ~/.codex/config.toml"
)

// manualJSON fills in the paste-by-hand answer for a client configured in JSON.
// Hint and snippet are set together, in one place per format, so the
// instruction can never drift from the syntax it is introducing.
func (r *Result) manualJSON(spec Spec) {
	r.Manual = ManualSnippet(spec)
	r.ManualHint = jsonManualHint
}

// manualTOML is manualJSON for Codex CLI's config.toml.
func (r *Result) manualTOML(spec Spec) {
	r.Manual = TOMLSnippet(spec)
	r.ManualHint = tomlManualHint
}

// ManualSnippet renders the mcpServers fragment a user can paste by hand. It is
// shown whenever Lumi declines to write a file itself — a client that is not
// installed, or a conflict it will not overwrite — and it is what makes this
// command useful for JSON-configured clients Lumi does not know about, such as
// Cursor.
func ManualSnippet(spec Spec) string {
	return entrySnippet(spec.Name, newEntry(spec))
}

// TOMLSnippet renders the [mcp_servers.<name>] table a Codex CLI user can paste
// into ~/.codex/config.toml by hand.
func TOMLSnippet(spec Spec) string {
	return entryTOMLSnippet(spec.Name, newEntry(spec))
}

// bareTOMLKey matches the keys TOML lets us write unquoted. Anything else — a
// name with a space or a dot in it — has to be quoted, or the table header
// would silently parse as a differently-nested table.
var bareTOMLKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// tomlKey renders a key, quoting it only when it has to be.
func tomlKey(key string) string {
	if bareTOMLKey.MatchString(key) {
		return key
	}
	return tomlString(key)
}

// tomlString renders a TOML basic string.
//
// json.Marshal does the quoting: JSON's string escapes (\", \\, \n, \uXXXX) are
// a subset of TOML's, so its output is always a valid TOML basic string, and it
// keeps paths containing quotes or backslashes correct where a hand-built
// string would not. strconv.Quote would be wrong here — it can emit \xNN, which
// TOML does not accept.
func tomlString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Unreachable for a Go string; returning something visibly wrong beats
		// panicking in a helper whose whole job is to be printed.
		return fmt.Sprintf("%q", value)
	}
	return string(encoded)
}

// entryTOMLSnippet renders any entry as a paste-able Codex table. It backs
// TOMLSnippet and the rollback error that has to hand back an entry Lumi
// removed but could not restore.
func entryTOMLSnippet(entryName string, e entry) string {
	var b strings.Builder
	// A whole table, not a fragment: unlike the JSON snippet this is pasted at
	// the top level of config.toml rather than inside an existing object.
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", tomlKey(entryName))
	fmt.Fprintf(&b, "command = %s\n", tomlString(e.Command))
	if len(e.Args) > 0 {
		quoted := make([]string, 0, len(e.Args))
		for _, arg := range e.Args {
			quoted = append(quoted, tomlString(arg))
		}
		fmt.Fprintf(&b, "args = [%s]\n", strings.Join(quoted, ", "))
	}
	if len(e.Env) > 0 {
		// An inline table rather than a [mcp_servers.<name>.env] header, so the
		// snippet stays one self-contained block wherever it is pasted. Sorted
		// so the rendering is deterministic.
		pairs := make([]string, 0, len(e.Env))
		for _, key := range slices.Sorted(maps.Keys(e.Env)) {
			pairs = append(pairs, fmt.Sprintf("%s = %s", tomlKey(key), tomlString(e.Env[key])))
		}
		fmt.Fprintf(&b, "env = { %s }\n", strings.Join(pairs, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
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

// --- Finding a client's CLI when there is no useful PATH ---------------------
//
// launchd gives Lumi.app PATH=/usr/bin:/bin:/usr/sbin:/sbin, which contains no
// `claude` or `codex` installed by npm, nvm, or any version manager. Asking the
// user's own shell is the only way to find one, and the reasoning for every
// choice below — why interactive, why two call sites — is in this package's
// CLAUDE.md.

// shellVars is what the user's own interactive login shell exports that launchd
// hides from Lumi.app.
type shellVars struct {
	path       string
	piAgentDir string
}

// userShell probes every variable in one shell run. Each run is a whole rc
// chain, and most users leave PI_CODING_AGENT_DIR unset, so probing it on its
// own would cost a second shell to learn nothing.
var userShell = sync.OnceValue(probeUserShell)

// userPATH is the PATH the user's shell would have, falling back to this
// process's own on any trouble.
//
// userPATH and userPiAgentDir are vars rather than funcs so tests can stub
// them: a unit test must never spawn a real shell, and the developer's own
// installs must not answer for the machine under test.
var userPATH = func() string {
	if path := userShell().path; path != "" {
		return path
	}
	return os.Getenv("PATH")
}

// userPiAgentDir is the PI_CODING_AGENT_DIR the user's shell exports, or "".
// launchd hides an export in ~/.zshrc exactly as it hides the PATH one, and Pi
// only ever runs from that shell.
var userPiAgentDir = func() string { return userShell().piAgentDir }

// pathProbeTimeout bounds one shell probe. An rc chain that hangs must not hang
// a settings tab; falling back to the inherited PATH is always safe.
const pathProbeTimeout = 5 * time.Second

// pathMarker and piAgentDirMarker prefix the probe's answers so they can be
// picked out of whatever else the user's startup files decide to print. Without
// them the answer has to be guessed at positionally, and a ~/.zlogout — which
// runs *after* the -c command, with no newline between them — silently appends
// its message to the last line.
const (
	pathMarker       = "LUMIPATH:"
	piAgentDirMarker = "LUMIPIDIR:"
)

// probeCommand is the -c argument: one marked line per variable.
const probeCommand = `printf '\n` + pathMarker + `%s\n` + piAgentDirMarker + `%s\n' "$PATH" "$` + piAgentDirVar + `"`

// pathProbeFlags are the shell invocations tried in order.
//
// The first is what nvm and its equivalents need: they initialise from ~/.zshrc,
// which zsh sources only for an *interactive* shell, and a login-only probe was
// measured returning a PATH without the nvm bin directory it was run to find.
// The second exists because csh and tcsh reject `-l` outright ("Unknown option")
// and would otherwise get no probe at all; they read ~/.cshrc when interactive,
// which is where a csh user's PATH is set. Retrying beats dispatching on the
// shell's name — it needs no table, and it self-heals for any shell that
// dislikes a flag. Only a failed attempt costs a second one.
var pathProbeFlags = [][]string{{"-l", "-i", "-c"}, {"-i", "-c"}}

// probeUserShell asks the user's shell for its variables, returning zero values
// on any trouble. An attempt counts as failed only when it yields no PATH; an
// unset PI_CODING_AGENT_DIR is an answer, not a reason to retry.
func probeUserShell() shellVars {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	for _, flags := range pathProbeFlags {
		if vars := runShellProbe(shell, flags); vars.path != "" {
			return vars
		}
	}
	return shellVars{}
}

// runShellProbe runs one invocation and returns what it reported.
func runShellProbe(shell string, flags []string) shellVars {
	ctx, cancel := context.WithTimeout(context.Background(), pathProbeTimeout)
	defer cancel()

	args := append(append([]string{}, flags...), probeCommand)
	cmd := exec.CommandContext(ctx, shell, args...)
	// TERM=dumb keeps an rc file from drawing a prompt or probing a terminal
	// that is not there. Stdin and Stderr are left nil, which is /dev/null: no
	// rc file waits for input, and nothing here reads its diagnostics.
	cmd.Env = append(os.Environ(), "TERM=dumb")
	// The context alone does not bound this. CommandContext kills the shell but
	// not its descendants, so an rc file that backgrounds anything inheriting
	// stdout leaves Output() waiting on a pipe that never closes — measured at
	// 60s against a 5s timeout. WaitDelay is what actually caps it.
	cmd.WaitDelay = time.Second

	// Parsed before the error is consulted, deliberately: when WaitDelay fires
	// the command reports failure while stdout already holds a complete answer,
	// and throwing that away would reintroduce the bug it exists to bound.
	out, _ := cmd.Output()
	return parseShellProbe(string(out))
}

// parseShellProbe pulls the marked values out of the probe's stdout, leaving a
// field empty if its marker is not there. Split out so it can be tested without
// a shell.
//
// The last marked line wins. A startup banner prints before the markers and a
// logout message after them, so neither can be mistaken for the answer.
func parseShellProbe(out string) shellVars {
	var vars shellVars
	for _, line := range strings.Split(out, "\n") {
		if _, value, found := strings.Cut(line, pathMarker); found {
			vars.path = strings.TrimSpace(value)
		}
		if _, value, found := strings.Cut(line, piAgentDirMarker); found {
			vars.piAgentDir = strings.TrimSpace(value)
		}
	}
	return vars
}

// lookCLI is the default LookPath both targets use: this process's PATH first,
// then the user's shell's.
//
// It is reached only through each target's injectable LookPath field, so a test
// that stubs that field is unaffected by whatever the developer has installed.
func lookCLI(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, dir := range filepath.SplitList(userPATH()) {
		// exec.LookPath on a path containing a separator checks that one file
		// and nothing else: not a directory, and executable *by this user*,
		// which a mode-bit test gets wrong for a file owned by somebody else.
		// A candidate that fails is skipped rather than ending the search.
		if path, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}
