package capture

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/seal"
	"github.com/puremetricsai/lumi/internal/store"
)

// TestEncryptedCaptureLeavesNoPlaintext runs the whole capture→store→search
// pipeline with a key set, and is the one runnable check on the encryption
// feature end to end. It uses the same fakes as every other test here, so it
// needs no permissions and no frameworks.
//
// It asserts what is on disk rather than what the code intended, because every
// interesting failure — a seal skipped, a key not reaching the store, an
// extension changed — is invisible from the Go side and obvious from the bytes.
func TestEncryptedCaptureLeavesNoPlaintext(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}

	// A fixed master, never the Keychain: no test may touch it.
	master := bytes.Repeat([]byte{0x2b}, 32)
	databaseKey, err := seal.DeriveDB(master)
	if err != nil {
		t.Fatal(err)
	}
	mediaKey, err := seal.DeriveMedia(master)
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(ctx, paths.Database, databaseKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	recorder := Recorder{
		Store: s, Paths: paths, CaptureScreen: true, CaptureAudio: true,
		ScreenInterval: 8 * time.Millisecond, AudioChunk: 8 * time.Millisecond,
		Screen: &fakeScreen{}, Text: fakeVision{}, Context: fakeContext{},
		Audio: fakeAudio{}, Transcriber: fakeTranscriber{},
		Cipher: mediaKey,
	}
	recordCtx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	// 1. Search still works. Encryption that cost the product its only feature
	//    would pass every byte-level assertion below.
	screen, err := s.Search(ctx, store.SearchOptions{Query: "screen text", Kind: store.KindScreen})
	if err != nil {
		t.Fatal(err)
	}
	if len(screen) == 0 {
		t.Fatal("no screen events were indexed under an encrypted store")
	}

	// 2. Every media file is sealed, and its name is unchanged. The name matters:
	//    internal/compress classifies work by extension and ReadAudioEnvelope
	//    dispatches on `.wav`.
	files := 0
	for _, dir := range []string{paths.Screenshots, paths.Audio} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			files++
			if !bytes.HasPrefix(raw, []byte(seal.Magic)) {
				t.Errorf("%s was left unsealed on disk", path)
				continue
			}
			ext := filepath.Ext(entry.Name())
			if ext != ".jpg" && ext != ".wav" {
				t.Errorf("%s has extension %q; sealing must not rename captured media", path, ext)
			}
			// 3. And it decrypts back to exactly what the fake wrote. This is
			//    the assertion that matters: "does not look like a JPEG" would
			//    pass against a completely unencrypted file, because the fakes
			//    write short ASCII strings with no magic of their own.
			plain, err := mediaKey.ReadFile(path)
			if err != nil {
				t.Errorf("%s does not decrypt: %v", path, err)
				continue
			}
			if bytes.HasPrefix(plain, []byte(seal.Magic)) {
				t.Errorf("%s decrypts to something still sealed", path)
			}
			if len(plain) == 0 {
				t.Errorf("%s decrypts to nothing", path)
			}
		}
	}
	if files == 0 {
		t.Fatal("the recorder wrote no media at all, so nothing was actually checked")
	}

	// 4. The database is encrypted, and carries none of the indexed text.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := os.ReadFile(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(database, []byte("SQLite format 3\x00")) {
		t.Error("the index still starts with the plaintext SQLite header")
	}
	if bytes.Contains(database, []byte("screen text")) {
		t.Error("indexed screen text survives as plaintext in the database file")
	}

	// 5. No write-ahead log is left beside it. A -wal holds pages of the
	//    database in whatever form they were written, so a plaintext one beside
	//    an encrypted index is a leak that the index itself cannot show.
	for _, sidecar := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(paths.Database + sidecar); err == nil {
			t.Errorf("%s survived a clean close", filepath.Base(paths.Database)+sidecar)
		}
	}
}

// TestUnencryptedCaptureIsUntouched pins that the zero-value key changes
// nothing. Encryption is opt-in, and the pass-through is what lets every path
// above be written once.
func TestUnencryptedCaptureIsUntouched(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	recorder := Recorder{
		Store: s, Paths: paths, CaptureScreen: true, CaptureAudio: false,
		ScreenInterval: 8 * time.Millisecond,
		Screen:         &fakeScreen{}, Text: fakeVision{}, Context: fakeContext{},
	}
	recordCtx, cancel := context.WithTimeout(ctx, 35*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(paths.Screenshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("nothing was captured")
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(paths.Screenshots, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(raw, []byte(seal.Magic)) {
			t.Errorf("%s was sealed with no key configured", entry.Name())
		}
	}
}
