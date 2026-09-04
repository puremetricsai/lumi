package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestEveryCommandDeclaresItsContent is the enforcement, not code review.
//
// The guard refuses a command that does not declare itself, so a new
// content-emitting command fails closed rather than leaking. This test is what
// turns that from a runtime surprise on a user's encrypted machine into a
// failure on the author's own.
func TestEveryCommandDeclaresItsContent(t *testing.T) {
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, child := range cmd.Commands() {
			walk(child)
		}
		if !cmd.Runnable() {
			// A parent with no RunE only prints help.
			return
		}
		switch cmd.Annotations[contentAnnotation] {
		case contentEmits, contentNone:
		default:
			t.Errorf("%s does not declare whether it emits captured content; "+
				"wrap it in emitsContent or emitsNoContent", cmd.CommandPath())
		}
	}
	walk(newRootCommand())
}

// TestSearchAndTranscriptAreTheEmittingCommands pins the classification itself,
// so moving `search` to "none" is a test failure rather than a silent hole.
func TestSearchAndTranscriptAreTheEmittingCommands(t *testing.T) {
	emitting := map[string]bool{}
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, child := range cmd.Commands() {
			walk(child)
		}
		if cmd.Runnable() && cmd.Annotations[contentAnnotation] == contentEmits {
			emitting[cmd.CommandPath()] = true
		}
	}
	walk(newRootCommand())

	want := []string{"lumi search", "lumi transcript", "lumi transcribe"}
	for _, path := range want {
		if !emitting[path] {
			t.Errorf("%s should be marked as emitting captured content", path)
		}
		delete(emitting, path)
	}
	for path := range emitting {
		t.Errorf("%s is marked as emitting captured content; if that is right, add it to this test",
			path)
	}
}

// TestContentCommandsRefuseWhileEncrypted is the check that makes the guard
// real. Without it the whole feature is a directory of ciphertext that any
// process can read by running the binary that owns the key.
func TestContentCommandsRefuseWhileEncrypted(t *testing.T) {
	restore := keyring
	t.Cleanup(func() { keyring = restore })
	keyring = keychain{has: func() (bool, error) { return true, nil }}

	for _, args := range [][]string{
		{"search", "anything"},
		{"transcript"},
	} {
		root := newRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"--data-dir", t.TempDir()}, args...))

		err := root.Execute()
		if err == nil {
			t.Errorf("lumi %s printed content while encryption was on:\n%s",
				strings.Join(args, " "), out.String())
			continue
		}
		if !errors.Is(err, errEncryptedContent) {
			t.Errorf("lumi %s failed with %v, want the encrypted-content refusal",
				strings.Join(args, " "), err)
		}
	}
}

// The refusal must not fire when encryption is off, or the guard would be a
// regression for every user who never turns it on.
func TestContentCommandsRunWhileNotEncrypted(t *testing.T) {
	restore := keyring
	t.Cleanup(func() { keyring = restore })
	keyring = keychain{
		has:  func() (bool, error) { return false, nil },
		load: func() ([]byte, error) { return nil, nil },
	}

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--data-dir", t.TempDir(), "search", "anything"})
	if err := root.Execute(); err != nil {
		t.Fatalf("lumi search failed with encryption off: %v", err)
	}
}

// A Keychain that cannot be read is not evidence that encryption is off.
// Guessing "off" would print the whole index on exactly the machine where
// something is already wrong.
func TestAnUnreadableKeychainRefusesRatherThanGuesses(t *testing.T) {
	restore := keyring
	t.Cleanup(func() { keyring = restore })
	keyring = keychain{has: func() (bool, error) { return false, errors.New("keychain is unavailable") }}

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--data-dir", t.TempDir(), "search", "anything"})
	if err := root.Execute(); err == nil {
		t.Fatalf("lumi search ran despite an unreadable Keychain:\n%s", out.String())
	}
}

// An undeclared command is refused whether or not encryption is on. This is the
// property that makes the default safe.
func TestAnUndeclaredCommandIsRefused(t *testing.T) {
	restore := keyring
	t.Cleanup(func() { keyring = restore })
	keyring = keychain{has: func() (bool, error) { return false, nil }}

	a := &app{}
	undeclared := &cobra.Command{Use: "undeclared", RunE: func(*cobra.Command, []string) error { return nil }}
	if err := a.guardContent(undeclared); err == nil {
		t.Fatal("a command with no content annotation was allowed to run")
	}
}
