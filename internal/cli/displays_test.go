package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puremetricsai/lumi/internal/macosnative"
)

// fakeDisplayCapture stands in for ScreenCaptureKit, which needs a Screen
// Recording grant this test must not require.
func fakeDisplayCapture(t *testing.T, frames []macosnative.ScreenFrame, contents map[string][]byte) {
	t.Helper()
	previous := captureDisplayThumbnails
	t.Cleanup(func() { captureDisplayThumbnails = previous })
	captureDisplayThumbnails = func(_ context.Context, directory string) ([]macosnative.ScreenFrame, error) {
		written := make([]macosnative.ScreenFrame, 0, len(frames))
		for _, frame := range frames {
			frame.Path = filepath.Join(directory, filepath.Base(frame.Path))
			if content, ok := contents[filepath.Base(frame.Path)]; ok {
				if err := os.WriteFile(frame.Path, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			written = append(written, frame)
		}
		return written, nil
	}
}

func TestConnectedDisplaysCarriesThumbnails(t *testing.T) {
	fakeDisplayCapture(t,
		[]macosnative.ScreenFrame{
			{Path: "b.jpg", DisplayID: 9, Width: 480, Height: 300},
			{Path: "a.jpg", DisplayID: 2, Width: 480, Height: 270},
		},
		map[string][]byte{"a.jpg": []byte("first"), "b.jpg": []byte("second")})

	displays, err := connectedDisplays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Ordered by ID, so a picker built from this does not reshuffle between
	// two runs on an unchanged setup.
	if len(displays) != 2 || displays[0].DisplayID != 2 || displays[1].DisplayID != 9 {
		t.Fatalf("displays = %+v, want display 2 then 9", displays)
	}
	decoded, err := base64.StdEncoding.DecodeString(displays[0].ThumbnailBase64)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "first" {
		t.Errorf("thumbnail = %q, want the captured image", decoded)
	}
}

// A frame whose file was written and then could not be read back still yields a
// row, explained by its error. That is the only failure the Go side can recover
// from: a display the native capture never got an image for produces no frame,
// and so no row at all — see connectedDisplays.
func TestConnectedDisplaysKeepsDisplaysWithoutThumbnails(t *testing.T) {
	fakeDisplayCapture(t,
		[]macosnative.ScreenFrame{{Path: "gone.jpg", DisplayID: 4, Width: 480, Height: 270}},
		nil)

	displays, err := connectedDisplays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(displays) != 1 || displays[0].DisplayID != 4 {
		t.Fatalf("displays = %+v, want display 4", displays)
	}
	if displays[0].ThumbnailBase64 != "" {
		t.Error("a display with no readable image should carry no thumbnail")
	}
	if displays[0].CaptureError == "" {
		t.Error("a missing thumbnail should be explained")
	}
}

// The temporary directory the thumbnails are captured into is the command's
// own, and must not outlive it.
func TestConnectedDisplaysRemovesItsThumbnailDirectory(t *testing.T) {
	var directory string
	previous := captureDisplayThumbnails
	t.Cleanup(func() { captureDisplayThumbnails = previous })
	captureDisplayThumbnails = func(_ context.Context, dir string) ([]macosnative.ScreenFrame, error) {
		directory = dir
		return []macosnative.ScreenFrame{{Path: filepath.Join(dir, "x.jpg"), DisplayID: 1}}, nil
	}
	if _, err := connectedDisplays(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Errorf("thumbnail directory %s survived the command", directory)
	}
}

func TestDisplaysCommandJSON(t *testing.T) {
	fakeDisplayCapture(t,
		[]macosnative.ScreenFrame{{Path: "a.jpg", DisplayID: 3, Width: 480, Height: 270}},
		map[string][]byte{"a.jpg": []byte("image")})

	var out strings.Builder
	cmd := (&app{}).displaysCommand()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var displays []Display
	if err := json.Unmarshal([]byte(out.String()), &displays); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if len(displays) != 1 || displays[0].DisplayID != 3 || displays[0].ThumbnailBase64 == "" {
		t.Errorf("displays = %+v, want one display carrying a thumbnail", displays)
	}
}
