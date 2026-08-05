// Package selfexec watches the executable a long-lived process was started
// from, and replaces the process with the new one when that file changes.
//
// It exists for `lumi mcp`. An agent launches that server as a subprocess and
// keeps it for the lifetime of the session, so a `brew upgrade lumi` that
// replaces the file on disk changes nothing about the already-running process:
// on Unix the old image stays mapped until it exits. The user upgrades, the
// agent keeps talking to the old build, and nothing anywhere reports it. No MCP
// transport or protocol revision addresses this — it is process lifecycle, and
// on stdio the client owns it — so the server has to notice on its own.
//
// This package only decides *whether* the binary moved and performs the
// replacement. When it is safe to do so is the caller's question, and on a
// JSON-RPC stream it is a sharp one: see internal/mcp/reexec.go.
package selfexec

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Stamp identifies one build of a binary file well enough to notice it was
// replaced.
//
// It is deliberately not a content hash. The file is tens of megabytes, this is
// checked between requests on a live server, and the question is only "is this
// the same file I started from" — a question path, size and mtime answer, since
// every way a binary is installed writes a new file rather than editing one in
// place.
type Stamp struct {
	// Path is the *resolved* target, which is the field that carries a
	// Homebrew-style upgrade. A packaged install is reached through a stable
	// symlink whose target moves every version bump
	// (internal/mcpsetup/CLAUDE.md), so /opt/homebrew/bin/lumi keeps its own
	// identity across an upgrade while the Cellar path it points at changes.
	// Stamping the resolved target is what makes that upgrade visible; stamping
	// the symlink would report no change at all.
	Path    string
	Size    int64
	ModTime time.Time
}

// Watcher compares a binary against the stamp it had when the process started.
//
// The zero value is not usable; build one with NewWatcher.
type Watcher struct {
	// execPath is the path to exec, kept unresolved on purpose: it is the stable
	// name the client was configured with, and the one that will point at the
	// *next* build after the one this process is replacing itself with.
	execPath string
	original Stamp
}

// NewWatcher stamps the binary the current process was started from.
//
// The watched path is os.Args[0] when that is absolute, and os.Executable()
// otherwise. The distinction matters for exactly the Homebrew case above:
// `lumi mcp setup` bakes an absolute argv[0] into every client config
// (internal/cli/CLAUDE.md), and that path is the symlink, while os.Executable()
// reports the resolved Cellar target that an upgrade abandons rather than
// rewrites.
func NewWatcher() (*Watcher, error) {
	path, err := watchPath()
	if err != nil {
		return nil, err
	}
	stamp, err := stat(path)
	if err != nil {
		return nil, err
	}
	return &Watcher{execPath: path, original: stamp}, nil
}

// Path reports the binary being watched.
func (w *Watcher) Path() string { return w.execPath }

// Changed reports whether the watched binary differs from the one this process
// started as.
//
// A failure to stat is deliberately *not* a change. The file can be missing for
// a moment mid-install, and treating that as "upgraded" would exec a path that
// does not exist yet, killing a working server to chase a build that is not
// there. Reporting false leaves the process serving the build it already has,
// which is the conservative direction: the next check picks the upgrade up.
func (w *Watcher) Changed() bool {
	current, err := stat(w.execPath)
	if err != nil {
		return false
	}
	return current != w.original
}

// stat resolves symlinks and stamps the target.
func stat(path string) (Stamp, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Stamp{}, fmt.Errorf("resolve %s: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Stamp{}, fmt.Errorf("stat %s: %w", resolved, err)
	}
	return Stamp{Path: resolved, Size: info.Size(), ModTime: info.ModTime()}, nil
}

// watchPath picks the path whose replacement this process should notice.
func watchPath() (string, error) {
	if len(os.Args) > 0 && filepath.IsAbs(os.Args[0]) {
		return os.Args[0], nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the running binary: %w", err)
	}
	return exe, nil
}
