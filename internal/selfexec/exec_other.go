//go:build !unix

package selfexec

import "fmt"

// Exec is unavailable off Unix: replacing a process image in place has no
// portable equivalent, and a fork-and-exit would not preserve the client's
// pipes, which is the only reason this mechanism works.
//
// The CLI refuses to run on anything but darwin/arm64 anyway
// (internal/platform), so this exists to keep the package building under
// cross-compilation and vet, not as a path anyone reaches.
func Exec(path string) error {
	return fmt.Errorf("re-exec is not supported on this platform")
}
