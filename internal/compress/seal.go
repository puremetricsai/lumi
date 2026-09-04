package compress

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/puremetricsai/lumi/internal/seal"
)

// stagedDestination decides where a pass writes its encode.
//
// With no key it is the destination itself, which is what this package always
// did: one write, no copy. With a key it is a plaintext file in the system
// temporary directory, because the encoders read and write with framework calls
// that know nothing about sealing — and because a pass verifies by reopening
// what it wrote, which has to be the plaintext.
//
// The staging file is never a sibling of the media. internal/compress's own
// reconcile walk reads an unrecognised sibling as an orphaned encode, and
// internal/retention's --all sweep deletes any unreferenced file it finds.
func stagedDestination(cipher seal.Key, destination string) (string, func(), error) {
	if !cipher.Enabled() {
		return destination, func() {}, nil
	}
	dir, err := os.MkdirTemp("", seal.TempPrefix)
	if err != nil {
		return "", func() {}, fmt.Errorf("staging directory: %w", err)
	}
	// The base name carries the destination's extension, which every pass
	// depends on to pick a codec.
	return filepath.Join(dir, filepath.Base(destination)), func() { os.RemoveAll(dir) }, nil
}

// placeEncoded seals a verified encode into its final name and reads it back.
//
// The read-back is the point. Everything above it verified the *plaintext* the
// pass produced; this is the only thing that verifies what is actually on disk
// under the key that will have to open it later. It runs before the original is
// unlinked, so a failure costs nothing.
func placeEncoded(cipher seal.Key, encoded, destination string) error {
	if !cipher.Enabled() {
		// The pass wrote straight to destination; there is nothing to place and
		// nothing sealed to verify.
		return nil
	}
	want, err := os.ReadFile(encoded)
	if err != nil {
		return fmt.Errorf("read the staged encode: %w", err)
	}
	if err := cipher.SealInto(encoded, destination); err != nil {
		return fmt.Errorf("encrypt the compressed file: %w", err)
	}
	got, err := cipher.ReadFile(destination)
	if err != nil {
		return fmt.Errorf("read back the encrypted compressed file: %w", err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("the encrypted compressed file does not decrypt to what was encoded")
	}
	return nil
}
