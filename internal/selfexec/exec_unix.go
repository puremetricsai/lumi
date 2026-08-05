//go:build unix

package selfexec

import (
	"fmt"
	"os"
	"syscall"
)

// Exec replaces the current process image with a fresh one from the watched
// path, preserving argv and the environment.
//
// This is execve, not fork: on success it does not return, the pid does not
// change, and — the property the whole design rests on — file descriptors 0, 1
// and 2 are carried across untouched. The MCP client is holding the other end of
// those pipes, so from its side nothing happened at all. It is still talking to
// the same process on the same stdin and stdout; only the code behind them is
// newer. A restart the client had to perform would instead mean tearing down and
// relaunching the server, which is the thing it does not do mid-session.
//
// It returns only on failure. There is no partial state to unwind — a failed
// execve leaves the current image running and intact — so the caller's correct
// response is to log it and carry on serving as the build it already is.
func Exec(path string) error {
	// syscall.Exec needs argv[0] itself, and the caller's argv is what makes the
	// new process the same server: `lumi mcp --data-dir …` has to survive, or the
	// replacement would come up as a bare `lumi` and print help onto the JSON-RPC
	// stream.
	argv := os.Args
	if len(argv) == 0 {
		return fmt.Errorf("cannot re-exec %s: no argv", path)
	}
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		return fmt.Errorf("re-exec %s: %w", path, err)
	}
	// Unreachable: a successful Exec never returns.
	return nil
}
