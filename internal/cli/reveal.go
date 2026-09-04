package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/puremetricsai/lumi/internal/seal"
	"github.com/puremetricsai/lumi/internal/store"
)

// `lumi reveal` opens one event's screenshot or audio while encryption is on.
//
// It is the second thing that can put captured content in front of somebody, and
// pretending otherwise would be dishonest: `lumi reveal 1 && cat` is an
// id-enumeration attack on exactly the media the content guard protects. So the
// plaintext's lifetime is bounded rather than left on disk — the copy exists
// only while a QuickLook panel is open in front of the user, and is unlinked the
// moment that window closes.
//
// `qlmanage -p` rather than `open`: `open -W` waits for the *application* to
// quit, and Preview is usually already running, so it returns immediately and
// the file would be deleted before it was drawn. qlmanage blocks for as long as
// its own panel is up, which is the property this needs.
var previewMedia = func(path string) error {
	preview := exec.Command("/usr/bin/qlmanage", "-p", path)
	// QuickLook is chatty on stderr and says nothing a user needs.
	preview.Stdout, preview.Stderr = nil, nil
	return preview.Run()
}

func (a *app) revealCommand() *cobra.Command {
	return emitsNoContent(&cobra.Command{
		Use:   "reveal <event-id>",
		Short: "Decrypt one event's screenshot or audio and open it",
		Long: "Decrypt the media belonging to one event and open it in QuickLook.\n\n" +
			"The decrypted copy exists only while the preview window is open and is deleted when\n" +
			"it closes. Event ids come from Lumi's MCP tools, which return them with every result.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("%q is not an event id", args[0])
			}
			s, _, k, err := a.openStoreWithKeys(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()

			event, err := s.EventByID(cmd.Context(), id)
			if errors.Is(err, store.ErrEventNotFound) {
				return fmt.Errorf("no event with id %d", id)
			}
			if err != nil {
				return err
			}
			if event.MediaPath == "" {
				return fmt.Errorf("event %d has no media", id)
			}
			if _, err := os.Stat(event.MediaPath); err != nil {
				return fmt.Errorf("event %d names media that is not there: %s", id, event.MediaPath)
			}

			// With no key the file is already openable and this command has
			// nothing to add — say so rather than shelling out to a preview the
			// user could have opened from Finder.
			if !k.enabled() {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Lumi's history is not encrypted; this file opens directly:\n%s\n", event.MediaPath)
				return nil
			}

			dir, err := os.MkdirTemp("", seal.TempPrefix)
			if err != nil {
				return fmt.Errorf("temporary directory: %w", err)
			}
			defer os.RemoveAll(dir)
			plain, err := k.media.ReadFile(event.MediaPath)
			if err != nil {
				return err
			}
			// The base name is kept so QuickLook picks a renderer from the
			// extension, and 0600 so nothing but this user can read it for the
			// seconds it exists.
			temp := filepath.Join(dir, filepath.Base(event.MediaPath))
			if err := os.WriteFile(temp, plain, 0o600); err != nil {
				return fmt.Errorf("write the decrypted copy: %w", err)
			}
			return previewMedia(temp)
		},
	})
}
