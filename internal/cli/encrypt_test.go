package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/seal"
	"github.com/puremetricsai/lumi/internal/store"
)

// fakeKeyring replaces the Keychain for tests. No test may touch the real one:
// it would prompt, and it would leave an item behind on the developer's machine.
func fakeKeyring(t *testing.T) *[]byte {
	t.Helper()
	restore := keyring
	t.Cleanup(func() { keyring = restore })

	var stored []byte
	keyring = keychain{
		has: func() (bool, error) { return stored != nil, nil },
		load: func() ([]byte, error) {
			if stored == nil {
				return nil, macosnative.ErrNoEncryptionKey
			}
			return stored, nil
		},
		store:  func(key []byte) error { stored = append([]byte(nil), key...); return nil },
		delete: func() error { stored = nil; return nil },
	}
	return &stored
}

func seedDataDir(t *testing.T) (config.Paths, []string) {
	t.Helper()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), paths.Database, nil)
	if err != nil {
		t.Fatal(err)
	}
	var media []string
	for i, spec := range []struct{ dir, name, body string }{
		{paths.Screenshots, "20260903-120000-display-1.jpg", "a private screenshot"},
		{paths.Screenshots, "20260903-120010-display-1.jpg", "another private screenshot"},
		{paths.Audio, "20260903-120000-system.wav", "RIFF private audio"},
	} {
		path := filepath.Join(spec.dir, spec.name)
		if err := os.WriteFile(path, []byte(spec.body), 0o600); err != nil {
			t.Fatal(err)
		}
		media = append(media, path)
		kind := store.KindScreen
		if spec.dir == paths.Audio {
			kind = store.KindAudio
		}
		if err := s.Insert(context.Background(), &store.Event{
			Kind: kind, CapturedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
			Text: "confidentialphrase", MediaPath: path,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return paths, media
}

func runLumi(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// TestEncryptRoundTrip is the gate on the whole command: on, then off, with the
// data intact and readable at both ends.
func TestEncryptRoundTrip(t *testing.T) {
	fakeKeyring(t)
	paths, media := seedDataDir(t)

	if out, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "on"); err != nil {
		t.Fatalf("encrypt on: %v\n%s", err, out)
	}

	// Nothing on disk holds the captured words any more.
	for _, path := range media {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(raw, []byte(seal.Magic)) {
			t.Errorf("%s was not sealed", path)
		}
		if bytes.Contains(raw, []byte("private")) {
			t.Errorf("%s still holds its plaintext", path)
		}
	}
	database, err := os.ReadFile(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(database, []byte("confidentialphrase")) {
		t.Error("the indexed text survives as plaintext in the converted database")
	}
	for _, sidecar := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(paths.Database + sidecar); err == nil {
			t.Errorf("a %s sidecar was left beside the encrypted database", sidecar)
		}
	}

	// The rows are still there and still searchable, read through the key.
	k, err := resolveKeys()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), paths.Database, k.database)
	if err != nil {
		t.Fatal(err)
	}
	found, err := s.Search(context.Background(), store.SearchOptions{Query: "confidentialphrase"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Errorf("found %d of 3 rows after encrypting", len(found))
	}
	s.Close()

	// And back.
	if out, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "off"); err != nil {
		t.Fatalf("encrypt off: %v\n%s", err, out)
	}
	for _, path := range media {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte("private")) {
			t.Errorf("%s did not come back to its plaintext", path)
		}
	}
	plain, err := store.Open(context.Background(), paths.Database, nil)
	if err != nil {
		t.Fatalf("the decrypted database will not open: %v", err)
	}
	defer plain.Close()
	found, err = plain.Search(context.Background(), store.SearchOptions{Query: "confidentialphrase"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Errorf("found %d of 3 rows after decrypting", len(found))
	}
	if has, err := keyring.has(); err != nil || has {
		t.Errorf("the key survived `encrypt off`: %v, %v", has, err)
	}
}

// TestEncryptResumesAfterAnInterruptedRun is the property the magic header buys.
// A directory half converted is a correct state, and re-running finishes it —
// with no journal, no progress file, and no chance of double-sealing.
func TestEncryptResumesAfterAnInterruptedRun(t *testing.T) {
	stored := fakeKeyring(t)
	paths, media := seedDataDir(t)

	// Simulate a run killed after the key was stored and one file sealed.
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	if err := keyring.store(master); err != nil {
		t.Fatal(err)
	}
	k, err := resolveKeys()
	if err != nil {
		t.Fatal(err)
	}
	if err := k.media.SealFile(media[0]); err != nil {
		t.Fatal(err)
	}

	out, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "on")
	if err != nil {
		t.Fatalf("encrypt on after an interrupted run: %v\n%s", err, out)
	}
	if !bytes.Equal(*stored, master) {
		t.Error("the resumed run minted a new key, which would strand the files the first run sealed")
	}
	for _, path := range media {
		sealed, err := seal.IsSealed(path)
		if err != nil {
			t.Fatal(err)
		}
		if !sealed {
			t.Errorf("%s was left unsealed by the resumed run", path)
		}
		// A double seal would decrypt to something that is itself sealed.
		plain, err := k.media.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(plain, []byte(seal.Magic)) {
			t.Errorf("%s was sealed twice", path)
		}
	}
}

// A fresh install can be encrypted before anything is recorded. This is the case
// that broke when the file header was the source of truth: there is no database
// yet, so a header check reads "plaintext" and the next keyed writer fails.
func TestEncryptOnBeforeAnythingIsRecorded(t *testing.T) {
	fakeKeyring(t)
	root := t.TempDir()

	if out, err := runLumi(t, "--data-dir", root, "encrypt", "on"); err != nil {
		t.Fatalf("encrypt on with an empty data directory: %v\n%s", err, out)
	}
	// The store opened next must be created encrypted, and must be readable.
	a := &app{dataDir: root}
	s, paths, k, err := a.openStoreWithKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !k.enabled() {
		t.Fatal("no key was resolved after encrypting an empty data directory")
	}
	if err := s.Insert(context.Background(), &store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC(),
		Text: "afterwards", MediaPath: "/tmp/x.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	encrypted, err := store.FileIsEncrypted(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if !encrypted {
		t.Error("a database created after `encrypt on` was written in plaintext")
	}
}

// `encrypt status` is what the app's toggle reads, so its shape is a contract.
func TestEncryptStatusReportsBothHalves(t *testing.T) {
	fakeKeyring(t)
	paths, _ := seedDataDir(t)

	out, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"enabled":false`, `"database_encrypted":false`, `"database":`} {
		if !bytes.Contains([]byte(out), []byte(key)) {
			t.Errorf("status JSON is missing %s:\n%s", key, out)
		}
	}

	if _, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "on"); err != nil {
		t.Fatal(err)
	}
	out, err = runLumi(t, "--data-dir", paths.Root, "encrypt", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(out), []byte(`"enabled":true`)) ||
		!bytes.Contains([]byte(out), []byte(`"database_encrypted":true`)) {
		t.Errorf("status did not report an encrypted store:\n%s", out)
	}
}

// A conversion that cannot be undone must say so rather than fail obscurely
// halfway through.
func TestEncryptOffRefusesWhenTheKeyIsGone(t *testing.T) {
	fakeKeyring(t)
	paths, _ := seedDataDir(t)
	if _, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "on"); err != nil {
		t.Fatal(err)
	}
	if err := keyring.delete(); err != nil {
		t.Fatal(err)
	}

	out, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "off")
	if err == nil {
		t.Fatalf("encrypt off succeeded with no key:\n%s", out)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("cannot be decrypted")) {
		t.Errorf("the refusal does not explain that the data is unrecoverable: %v", err)
	}
}

// TestEncryptOnSweepsMediaLeftUnsealed is the regression for a promise that was
// false.
//
// internal/capture logs a warning and leaves a file readable when a seal fails,
// deliberately — the never-lose-media rule — on the stated promise that the next
// `lumi encrypt on` picks it up. But `on` used to refuse whenever the key and
// the database agreed, and that check never looked at the media, so a file that
// missed its seal at capture time stayed plaintext forever with nothing saying
// so. Re-running is idempotent by construction; it must be allowed to run.
func TestEncryptOnSweepsMediaLeftUnsealed(t *testing.T) {
	fakeKeyring(t)
	paths, _ := seedDataDir(t)
	if out, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "on"); err != nil {
		t.Fatalf("encrypt on: %v\n%s", err, out)
	}

	// A capture whose seal failed: indexed, readable, no magic header.
	missed := filepath.Join(paths.Screenshots, "20260903-130000-display-1.jpg")
	if err := os.WriteFile(missed, []byte("a screenshot that missed its seal"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runLumi(t, "--data-dir", paths.Root, "encrypt", "on")
	if err != nil {
		t.Fatalf("encrypt on refused to sweep unsealed media: %v\n%s", err, out)
	}
	sealed, err := seal.IsSealed(missed)
	if err != nil {
		t.Fatal(err)
	}
	if !sealed {
		t.Error("a file left unsealed by a failed capture-time seal was never picked up")
	}
	// And the files already done were skipped rather than sealed twice.
	if !bytes.Contains([]byte(out), []byte("already done")) {
		t.Errorf("the resumed run did not report skipping the files already sealed:\n%s", out)
	}
}
