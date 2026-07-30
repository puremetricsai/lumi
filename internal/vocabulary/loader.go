package vocabulary

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
)

// Snapshot is one observation of the vocabulary file.
//
// Terms is always usable and Err is advisory: a failed read yields an empty
// list *and* an error rather than a choice between them, so no caller is
// forced to treat a degraded read as fatal. This mirrors how ScreenContext
// reports a failed Accessibility read as a populated context.
//
// Exists means "confirmed present and statable", so callers must check Err
// before Exists. Absence is reported here rather than as an error because
// whether absence is acceptable is a caller's policy: routine for a recorder,
// fatal for an explicit `lumi transcribe --vocabulary` path.
type Snapshot struct {
	Terms   []string
	Exists  bool
	Dropped int
	Err     error
	Changed bool
}

// Loader reads the vocabulary file, caching successful reads. A *Loader is
// safe for concurrent use and must be shared rather than copied, since the
// cache and the Changed comparison both live in it.
type Loader struct {
	Path string

	mu sync.Mutex
	// cached is the last successful snapshot, valid while the file still
	// matches cachedInfo. haveCached distinguishes "no successful read yet"
	// from a zero snapshot.
	haveCached bool
	cachedInfo os.FileInfo
	cached     Snapshot
	// previous is the last snapshot returned, used only to compute Changed.
	havePrevious bool
	previous     Snapshot
}

// Load observes the vocabulary file now.
//
// Two cache rules, deliberately independent:
//
//  1. A successful read is cached against the file's identity, mtime, size,
//     and mode, so the steady-state cost is one stat per call.
//  2. A failed read is never cached, so the next call retries unconditionally.
//
// Rule 2 exists because a stat-keyed cache cannot observe recovery: chmod
// leaves size and mtime untouched, so caching the failure would leave a
// long-running recorder transcribing without vocabulary indefinitely after the
// user fixed the permissions — while `doctor`, running in a fresh process with
// a cold cache, read the same file successfully and called it healthy.
//
// Changed compares the resulting snapshot against the previously returned one,
// not the stat key. That separation is what lets rule 2 retry every call
// without the caller logging every call.
func (l *Loader) Load() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()

	snapshot := l.observe()
	// Hand out a private copy. observe() may return the cached snapshot, whose
	// Terms slice shares a backing array with l.cached; a caller that sorted or
	// wrote into it in place would silently corrupt every later Load(). Cloning
	// is free at MaxTerms and makes the read-only guarantee structural rather
	// than a doc comment nobody reads.
	snapshot.Terms = slices.Clone(snapshot.Terms)
	snapshot.Changed = !l.havePrevious || !equivalent(l.previous, snapshot)
	l.previous = snapshot
	// l.previous must not alias the slice just handed to the caller either:
	// assigning the struct above copies the slice header, not the backing
	// array, so without this second clone a caller mutating its returned
	// Terms would also mutate what the next Load() compares against in
	// equivalent(), reporting a spurious Changed.
	l.previous.Terms = slices.Clone(snapshot.Terms)
	l.havePrevious = true
	return snapshot
}

func (l *Loader) observe() Snapshot {
	info, err := os.Stat(l.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Absence is the normal state for a user who has not opted in. It is
		// reported through Exists, not Err, because only the caller knows
		// whether it is acceptable.
		l.invalidate()
		return Snapshot{}
	case err != nil:
		// Exists stays false: the file could not be confirmed present, which
		// is why callers must check Err first.
		l.invalidate()
		return Snapshot{Err: fmt.Errorf("stat vocabulary file %s: %w", l.Path, err)}
	}

	if l.haveCached && unchanged(l.cachedInfo, info) {
		return l.cached
	}

	data, err := os.ReadFile(l.Path)
	if err != nil {
		// The file is present but unusable — the mode-000 case. Exists is
		// true, and the failure is deliberately not cached.
		l.invalidate()
		return Snapshot{Exists: true, Err: fmt.Errorf("read vocabulary file %s: %w", l.Path, err)}
	}

	terms, dropped := Parse(data)
	snapshot := Snapshot{Terms: terms, Exists: true, Dropped: dropped}
	l.haveCached = true
	l.cachedInfo = info
	l.cached = snapshot
	return snapshot
}

func (l *Loader) invalidate() {
	l.haveCached = false
	l.cachedInfo = nil
	l.cached = Snapshot{}
}

// unchanged reports whether the file is the same file with the same contents,
// as far as a stat can tell. Identity is compared because editors commonly
// save via temp-file-plus-rename, which can yield a new inode with an
// identical size and a preserved mtime. Mode is compared too: chmod changes
// neither mtime nor size, so a cache keyed only on those would keep serving a
// stale successful read after the file's permissions changed underneath it —
// the same blind spot rule 2 exists to close, but in the opposite direction
// (success masking a later failure, rather than a failure masking a later
// success). Comparing Mode() here is what makes a chmod invalidate the cache
// in both directions.
func unchanged(cached, current os.FileInfo) bool {
	return os.SameFile(cached, current) &&
		cached.ModTime().Equal(current.ModTime()) &&
		cached.Size() == current.Size() &&
		cached.Mode() == current.Mode()
}

// equivalent reports whether two snapshots say the same thing, which is what
// makes a persistently broken file warn once instead of once per call.
func equivalent(a, b Snapshot) bool {
	return a.Exists == b.Exists &&
		a.Dropped == b.Dropped &&
		errorText(a.Err) == errorText(b.Err) &&
		slices.Equal(a.Terms, b.Terms)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
