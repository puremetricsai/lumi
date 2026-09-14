package cli

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/spf13/cobra"
)

// thumbnailWidth is how wide a display preview is captured. Wide enough to
// recognise a desktop at a glance, small enough that a handful of them fit
// comfortably in one JSON document.
const thumbnailWidth = 480

// captureDisplayThumbnails is a package var purely as a test seam: without it a
// test would need a live Screen Recording grant to assert this command's output
// shape, and would be skipped everywhere it matters.
var captureDisplayThumbnails = func(ctx context.Context, directory string) ([]macosnative.ScreenFrame, error) {
	return macosnative.CaptureScreens(ctx, directory, "thumbnail", nil, thumbnailWidth)
}

// Display is one connected display and a preview of what is on it.
//
// The thumbnail travels as base64 in the document rather than as a file on
// disk: the one caller is reading this over a pipe already, and a file would
// add a lifetime nobody owns — the reader cannot know when the writer is
// finished with it, and the writer cannot know when the reader is.
type Display struct {
	DisplayID uint32 `json:"display_id"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	// ThumbnailBase64 is a JPEG. Empty when the capture failed, which
	// CaptureError then explains.
	ThumbnailBase64 string `json:"thumbnail_base64,omitempty"`
	CaptureError    string `json:"capture_error,omitempty"`
}

// displaysCommand lists the connected displays, which is where the IDs
// `record start --displays` takes come from.
//
// Like `doctor`, it never opens the store: it has no use for one, and opening it
// would create a mistyped --data-dir and then report happily on the empty
// result. The thumbnails it captures are previews, written to a temporary
// directory and deleted before the command returns — they are never indexed and
// never enter the store, so the rule against losing captured media does not
// reach them.
func (a *app) displaysCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "displays",
		Short: "List the connected displays and preview what is on each",
		RunE: func(cmd *cobra.Command, _ []string) error {
			displays, err := connectedDisplays(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(displays)
			}
			for _, display := range displays {
				// A row's CaptureError may belong to a display that has no row
				// of its own, so it is only this display's answer when there is
				// no image to go with it.
				status := cmp.Or(display.CaptureError, "no image")
				if display.ThumbnailBase64 != "" {
					status = "captured"
					if display.CaptureError != "" {
						status += " (another display failed: " + display.CaptureError + ")"
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%d\t%d x %d\t%s\n",
					display.DisplayID, display.Width, display.Height, status)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func connectedDisplays(ctx context.Context) ([]Display, error) {
	directory, err := os.MkdirTemp("", "lumi-display-thumbnails")
	if err != nil {
		return nil, fmt.Errorf("create thumbnail directory: %w", err)
	}
	defer os.RemoveAll(directory)

	frames, err := captureDisplayThumbnails(ctx, directory)
	if err != nil {
		return nil, err
	}
	displays := make([]Display, 0, len(frames))
	for _, frame := range frames {
		display := Display{
			DisplayID: frame.DisplayID, Width: frame.Width, Height: frame.Height,
			CaptureError: frame.CaptureError,
		}
		// Native emits a frame only for a display it captured *and* wrote, so a
		// display whose capture failed has no row here at all, and its error is
		// joined onto every frame that did succeed. A row carrying both a
		// thumbnail and a CaptureError is therefore reporting some other
		// display's failure — the field is kept because it is the only channel
		// by which that failure reaches anyone.
		//
		// This guard covers the narrower case native leaves open: a file that
		// was written and then could not be read back.
		if image, readErr := os.ReadFile(frame.Path); readErr != nil {
			display.CaptureError = cmp.Or(display.CaptureError, readErr.Error())
		} else {
			display.ThumbnailBase64 = base64.StdEncoding.EncodeToString(image)
		}
		displays = append(displays, display)
	}
	// Ordered by ID so two runs on an unchanged setup produce the same list and
	// a picker built from it does not reshuffle under the user.
	sort.Slice(displays, func(i, j int) bool { return displays[i].DisplayID < displays[j].DisplayID })
	return displays, nil
}
