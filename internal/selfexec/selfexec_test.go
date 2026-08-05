package selfexec

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeBinary creates a stand-in for an installed binary.
func writeBinary(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// watcherFor builds a Watcher on an arbitrary path, bypassing NewWatcher's
// os.Args lookup so a test can control the file being watched.
func watcherFor(t *testing.T, path string) *Watcher {
	t.Helper()
	stamp, err := stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return &Watcher{execPath: path, original: stamp}
}

func TestUnchangedBinaryReportsNoChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lumi")
	writeBinary(t, path, "build one")

	if watcherFor(t, path).Changed() {
		t.Error("an untouched binary must not report a change")
	}
}

// This is the ordinary upgrade: the file at the same path is replaced.
func TestReplacedBinaryReportsAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lumi")
	writeBinary(t, path, "build one")
	w := watcherFor(t, path)

	// A same-size rewrite is the hard case, since only mtime distinguishes it.
	// Move the timestamp explicitly rather than sleeping for filesystem
	// granularity.
	writeBinary(t, path, "build two")
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	if !w.Changed() {
		t.Error("a replaced binary must report a change")
	}
}

// The Homebrew case, and the reason Stamp records the resolved target rather
// than the path it was reached through. `lumi mcp setup` bakes the stable
// symlink into every client config, and an upgrade repoints it at a new Cellar
// directory — so watching the link's own identity would report nothing at all.
func TestRepointedSymlinkReportsAChange(t *testing.T) {
	dir := t.TempDir()
	oldTarget := filepath.Join(dir, "lumi-1.0")
	newTarget := filepath.Join(dir, "lumi-1.1")
	link := filepath.Join(dir, "lumi")
	writeBinary(t, oldTarget, "build one")
	writeBinary(t, newTarget, "build two")
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Fatal(err)
	}

	w := watcherFor(t, link)
	if w.Changed() {
		t.Fatal("the symlink has not moved yet")
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newTarget, link); err != nil {
		t.Fatal(err)
	}
	if !w.Changed() {
		t.Error("a repointed symlink must report a change")
	}
	// The path to exec stays the stable link, which is what will point at the
	// build after this one.
	if w.Path() != link {
		t.Errorf("Path() should stay the configured path, got %q", w.Path())
	}
}

// A binary can be briefly absent mid-install. Reporting that as a change would
// exec a path that does not exist yet, killing a working server to chase a build
// that is not there — so an unreadable file is deliberately "no change".
func TestAMissingBinaryIsNotAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lumi")
	writeBinary(t, path, "build one")
	w := watcherFor(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if w.Changed() {
		t.Error("a missing binary must not be reported as a change")
	}
}

// A stat failure at construction has to be an error rather than a Watcher that
// silently never fires: the caller decides whether to run without the upgrade
// path, and cannot if it is not told.
func TestNewWatcherFailsOnAnUnreadablePath(t *testing.T) {
	if _, err := stat(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("expected stamping a missing file to fail")
	}
}

// NewWatcher prefers an absolute os.Args[0] because that is the path `lumi mcp
// setup` writes into client configs, and the one whose target an upgrade moves.
func TestNewWatcherPrefersAnAbsoluteArgv0(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lumi")
	writeBinary(t, path, "build one")

	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{path, "mcp"}

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if w.Path() != path {
		t.Errorf("expected the argv[0] path %q, got %q", path, w.Path())
	}
}

// A relative argv[0] names nothing stable — the server's working directory is
// whatever the client launched it in — so os.Executable is the only usable
// answer.
func TestNewWatcherFallsBackToOsExecutableForARelativeArgv0(t *testing.T) {
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{"lumi", "mcp"}

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if w.Path() != exe {
		t.Errorf("expected the executable path %q, got %q", exe, w.Path())
	}
}
