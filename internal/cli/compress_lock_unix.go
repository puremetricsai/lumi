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
	// anything — the lock itself is the state.
	file.Truncate(0)
	fmt.Fprintf(file, "%d\n", os.Getpid())
	return func() {
		// Unlink before closing: the reverse order leaves a window where the lock
		// is free but the file is still there, and removes a file a new holder
		// may by then have opened.
		os.Remove(path)
		file.Close()
	}, nil
}
