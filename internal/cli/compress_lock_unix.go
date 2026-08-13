//go:build unix

package cli

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on path, returning a release
// function, or reports that another process already holds it.
//
// flock rather than a pid file, and the difference is correctness rather than
// taste. A pid file has to be created and then written, and a contender reading
// it in between sees an empty file it cannot attribute to anybody — so it
// concludes the lock is stale, removes it, and takes a second one. Making that
// window smaller does not close it, and the stale-takeover path has the same
// check-then-remove race against a lock somebody else just created.
//
// The kernel releases a flock when the descriptor closes, including when the
// process dies however abruptly, so there is no stale state to reason about at
// all: a lock that exists is held by a process that exists.
func lockFile(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the lock file: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errHeld
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Written for a human reading the data directory, never read back to decide
	// anything — the lock itself is the state. Because the file outlives the run
	// (see below), a pid found here after a clean exit is expected to be stale;
	// it names whoever held the lock last, not whoever holds it now.
	file.Truncate(0)
	fmt.Fprintf(file, "%d\n", os.Getpid())
	return func() {
		// Close only — the file is deliberately never unlinked.
		//
		// Unlinking it reintroduces the double-holder state flock exists to
		// prevent, because the lock is the *inode*, not the name. A contender
		// that has opened the path but not yet flocked it holds a live fd on
		// inode I; unlinking frees the name while closing frees I's lock, and
		// from that moment they are two different locks. The contender then
		// acquires I while a third process creates a fresh inode at the same
		// path and acquires that — two holders, which is exactly the state
		// internal/compress's crash-safety claim depends on being impossible.
		//
		// A leftover file is harmless for the same reason: it carries no lock,
		// so it excludes nobody. TestCompressIgnoresALockFileNobodyHolds pins
		// that, and it is why no stale-takeover logic is needed here.
		file.Close()
	}, nil
}
