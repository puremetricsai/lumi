//go:build unix

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Releasing the lock must not unlink it, because the lock is the inode and not
// the name. Unlinking frees the two separately, which lets a contender holding
// an already-opened descriptor take the old inode while a newcomer creates a
// fresh one at the same path — two processes each believing they hold the
// compress lock, which is the state internal/compress's crash-safety claim
// depends on being impossible. This walks that exact interleaving.
func TestCompressLockCannotBeHeldByTwoProcessesAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), compressLockName)

	release, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The window: a contender opens the path while the lock is still held, and
	// is descheduled before it can flock the descriptor.
	contender, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()

	release()

	// The contender now takes the lock on the inode it opened. This succeeds
	// whether or not the file was unlinked; it is the next step that differs.
	if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the contender could not take the released lock: %v", err)
	}

	// With the file left in place there is one inode, so a newcomer is excluded.
	// With an unlinking release it would create a second inode at the same path
	// and lock that, and both runs would compress the same files.
	newcomer, err := lockFile(path)
	if err == nil {
		newcomer()
		t.Fatal("two processes hold the compress lock at once")
	}
	if !errors.Is(err, errHeld) {
		t.Fatalf("the newcomer failed for the wrong reason: %v", err)
	}
}
