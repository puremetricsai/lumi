package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/puremetricsai/lumi/internal/store"
)

// capturePreview replaces the QuickLook call and records what it was handed, so
// these tests assert on the decrypted file itself rather than on a window
// appearing.
func capturePreview(t *testing.T) *struct {
	path    string
	content []byte
	mode    os.FileMode
	called  bool
} {
	t.Helper()
	restore := previewMedia
	t.Cleanup(func() { previewMedia = restore })

	seen := &struct {
		path    string
		content []byte
		mode    os.FileMode
		called  bool
	}{}
	previewMedia = func(path string) error {
		seen.called = true
		seen.path = path
		seen.content, _ = os.ReadFile(path)
		if info, err := os.Stat(path); err == nil {
			seen.mode = info.Mode().Perm()
		}
		return nil
	}
	return seen
}

// TestRevealDecryptsOneEventsMedia is the check on the only sanctioned way a
// user reads back a sealed file themselves.
func TestRevealDecryptsOneEventsMedia(t *testing.T) {
	fakeKeyring(t)
	paths, media := seedDataDir(t)
	if _, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "on"); err != nil {
		t.Fatal(err)
	}
	seen := capturePreview(t)

	// The first seeded row is the first screenshot.
	s, _, _, err := (&app{dataDir: paths.Root}).openStoreWithKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Search(context.Background(), store.SearchOptions{Query: "confidentialphrase"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if len(events) == 0 {
		t.Fatal("nothing to reveal")
	}
	var target store.Event
	for _, e := range events {
		if e.MediaPath == media[0] {
			target = e
		}
	}
	if target.ID == 0 {
		t.Fatalf("could not find the event for %s", media[0])
	}

	// The file on disk is sealed, so this is a real decrypt.
	if raw, err := os.ReadFile(target.MediaPath); err != nil {
		t.Fatal(err)
	} else if bytes.Contains(raw, []byte("private")) {
		t.Fatal("the media was not sealed, so this test proves nothing")
	}

	out, err := runLumi(t, "--data-dir", paths.Root, "reveal",
		strconv.FormatInt(target.ID, 10))
	if err != nil {
		t.Fatalf("reveal: %v\n%s", err, out)
	}
	if !seen.called {
		t.Fatal("reveal opened nothing")
	}
	if string(seen.content) != "a private screenshot" {
		t.Errorf("reveal handed over %q", seen.content)
	}
	if seen.mode != 0o600 {
		t.Errorf("the decrypted copy is mode %04o, want 0600", seen.mode)
	}
	if filepath.Base(seen.path) != filepath.Base(target.MediaPath) {
		t.Errorf("the copy is named %q; QuickLook picks a renderer from the extension",
			filepath.Base(seen.path))
	}
	// It must not be written beside the media, where prune --all would find it.
	if filepath.Dir(seen.path) == paths.Screenshots {
		t.Error("the decrypted copy was written into the media directory")
	}

	// And it is gone once the preview closes. That bound is the whole reason
	// reveal is an acceptable second content exit.
	if _, err := os.Stat(seen.path); !os.IsNotExist(err) {
		t.Error("the decrypted copy outlived the preview window")
	}
}

// With no key there is nothing to decrypt, and the file is already openable —
// so reveal says so rather than making a pointless copy.
func TestRevealOnAnUnencryptedStoreNamesTheFile(t *testing.T) {
	fakeKeyring(t)
	paths, media := seedDataDir(t)
	seen := capturePreview(t)

	s, err := store.Open(context.Background(), paths.Database, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Search(context.Background(), store.SearchOptions{Query: "confidentialphrase"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	out, err := runLumi(t, "--data-dir", paths.Root, "reveal", strconv.FormatInt(events[0].ID, 10))
	if err != nil {
		t.Fatalf("reveal: %v\n%s", err, out)
	}
	if seen.called {
		t.Error("reveal opened a preview for a file the user can already open")
	}
	if !strings.Contains(out, media[0]) && !strings.Contains(out, events[0].MediaPath) {
		t.Errorf("reveal did not name the file:\n%s", out)
	}
}

func TestRevealRejectsAnUnknownEvent(t *testing.T) {
	fakeKeyring(t)
	paths, _ := seedDataDir(t)
	capturePreview(t)

	if _, err := runLumi(t, "--data-dir", paths.Root, "reveal", "99999"); err == nil {
		t.Error("reveal accepted an id that does not exist")
	}
	if _, err := runLumi(t, "--data-dir", paths.Root, "reveal", "not-a-number"); err == nil {
		t.Error("reveal accepted a non-numeric id")
	}
}
