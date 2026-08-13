//go:build !unix

package cli

import "errors"

// lockFile has no portable equivalent off Unix. The CLI refuses to run on
// anything but darwin/arm64 anyway (internal/platform), so this exists to keep
// the package building under cross-compilation and vet, not as a path anyone
// reaches. It fails closed: without a lock, compress must not run.
func lockFile(string) (func(), error) {
	return nil, errors.New("compression needs an advisory file lock, which is unavailable on this platform")
}
