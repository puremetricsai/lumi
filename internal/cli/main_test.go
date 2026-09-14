package cli

import (
	"os"
	"testing"

	"github.com/puremetricsai/lumi/internal/macosnative"
)

// TestMain replaces the Keychain for the whole package.
//
// No test may reach the real one. It would prompt, it would leave an item behind
// on the developer's machine, and — the reason this file exists — the suite's
// behaviour would otherwise depend on whether the developer happens to have
// encryption turned on. It did: with a real key present, every test that builds
// a plaintext store and runs a command failed with "file is not a database",
// and the failure looked like a bug in the code under test rather than in the
// harness.
//
// Individual tests that need to exercise a particular state call fakeKeyring to
// take their own copy; this is only the floor.
func TestMain(m *testing.M) {
	keyring = keychain{
		has:    func() (bool, error) { return false, nil },
		load:   func() ([]byte, error) { return nil, macosnative.ErrNoEncryptionKey },
		store:  func([]byte) error { return nil },
		delete: func() error { return nil },
	}
	os.Exit(m.Run())
}
