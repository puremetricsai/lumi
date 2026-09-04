package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// The content guard: while encryption is on, `lumi mcp` is the only thing that
// puts captured screen text or transcripts where another process can read them.
//
// Encrypting the files is only half of what the user asked for. The Keychain ACL
// means this binary can decrypt without a prompt — so an agent that finds the
// binary could simply run `lumi search "password"` and read the whole index out
// of a terminal, and the encryption would have bought nothing against the threat
// it was chosen for. The guard is what closes that.
//
// It is a cobra annotation rather than a list inside a function, and the default
// is to **refuse**. A new command that emits nothing still has to say so, and an
// author who forgets gets a loud failure on their own machine rather than a
// silent leak on a user's. TestEveryCommandDeclaresItsContent, not code review,
// is what keeps that true.
//
// This is a speed bump, not a boundary, and nothing here should be described as
// more. Any process running as this user can spawn `lumi mcp` and drive JSON-RPC
// by hand. What it buys is that ambient access — a grep, a backup, a stray
// `cat`, another account, a stolen disk — reaches ciphertext, and that reading
// the history takes going through the MCP surface the user granted on purpose.

// contentAnnotation is the annotation key, and the two values it takes.
const (
	contentAnnotation = "lumi.content"
	// contentEmits marks a command that prints captured screen text or
	// transcripts to stdout.
	contentEmits = "emits"
	// contentNone marks a command that prints counts, paths, diagnostics or
	// nothing — anything that is not the captured content itself.
	contentNone = "none"
)

func emitsContent(cmd *cobra.Command) *cobra.Command {
	return annotateContent(cmd, contentEmits)
}

func emitsNoContent(cmd *cobra.Command) *cobra.Command {
	return annotateContent(cmd, contentNone)
}

func annotateContent(cmd *cobra.Command, value string) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[contentAnnotation] = value
	return cmd
}

// guardContent refuses a content-emitting command while encryption is on.
//
// It runs from the root's PersistentPreRunE, after cobra has parsed flags, so
// --data-dir is already resolved. A command with no annotation is refused for
// the reason above: failing closed is the only default that cannot be forgotten
// into a leak.
func (a *app) guardContent(cmd *cobra.Command) error {
	// A parent with no RunE only prints help, and cobra runs PersistentPreRunE
	// for it too. Help is not content.
	if cmd.Runnable() == false {
		return nil
	}
	switch cmd.Annotations[contentAnnotation] {
	case contentNone:
		return nil
	case contentEmits:
	default:
		return fmt.Errorf("%s does not declare whether it emits captured content; "+
			"annotate it with emitsContent or emitsNoContent", cmd.CommandPath())
	}
	enabled, err := encryptionEnabled()
	if err != nil {
		// Refuse rather than guess. A Keychain that cannot be reached is not
		// evidence that encryption is off, and treating it as such would print
		// the whole index on exactly the machine where something is wrong.
		return fmt.Errorf("could not tell whether Lumi's history is encrypted, so %s will not "+
			"print captured content: %w", cmd.CommandPath(), err)
	}
	if !enabled {
		return nil
	}
	return wrapEncryptedContent(cmd.CommandPath())
}
