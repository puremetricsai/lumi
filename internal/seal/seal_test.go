package seal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func testKey(t *testing.T, seed byte) Key {
	t.Helper()
	master := make([]byte, 32)
	for i := range master {
		master[i] = seed + byte(i)
	}
	key, err := DeriveMedia(master)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func write(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSealHidesTheContentAndReadsItBack(t *testing.T) {
	key := testKey(t, 1)
	original := []byte("\xff\xd8\xff a screenshot of something private")
	path := write(t, t.TempDir(), "shot.jpg", original)

	if err := key.SealFile(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(Magic)) {
		t.Error("a sealed file does not start with the magic header")
	}
	if bytes.Contains(raw, []byte("private")) {
		t.Error("plaintext survives in a sealed file")
	}
	got, err := key.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Error("the unsealed content does not match what was sealed")
	}
}

// The name must not change: internal/compress classifies by extension and
// ReadAudioEnvelope dispatches on `.wav`.
func TestSealKeepsTheFileName(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "chunk.wav", []byte("RIFF....WAVE"))
	if err := testKey(t, 2).SealFile(path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "chunk.wav" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("sealing left %v, want only chunk.wav", names)
	}
}

// Re-sealing is what makes `lumi encrypt on` resumable after a crash: the
// header is the record of what is already done.
func TestSealIsIdempotent(t *testing.T) {
	key := testKey(t, 3)
	path := write(t, t.TempDir(), "shot.jpg", []byte("original bytes"))
	if err := key.SealFile(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.SealFile(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("sealing an already sealed file changed it")
	}
	got, err := key.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original bytes" {
		t.Errorf("double seal produced %q", got)
	}
}

func TestRoundTripThroughUnseal(t *testing.T) {
	key := testKey(t, 4)
	path := write(t, t.TempDir(), "chunk.wav", []byte("RIFF sample data"))
	if err := key.SealFile(path); err != nil {
		t.Fatal(err)
	}
	if err := key.UnsealFile(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "RIFF sample data" {
		t.Errorf("unsealed to %q", raw)
	}
	// And unsealing again is a no-op rather than an error, so `encrypt off`
	// re-runs cleanly.
	if err := key.UnsealFile(path); err != nil {
		t.Fatal(err)
	}
}

// A nil key is the plaintext build: every caller runs the same code and nothing
// is touched.
func TestZeroKeyIsAPassThrough(t *testing.T) {
	var key Key
	dir := t.TempDir()
	path := write(t, dir, "shot.jpg", []byte("plain"))

	if err := key.SealFile(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "plain" {
		t.Error("a nil key modified the file")
	}
	got, err := key.ReadFile(path)
	if err != nil || string(got) != "plain" {
		t.Errorf("ReadFile with a nil key = %q, %v", got, err)
	}
	temp, cleanup, err := key.TempCopy(path)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if temp != path {
		t.Errorf("TempCopy with a nil key returned %q, want the original path", temp)
	}
}

// A plaintext file stays readable with a key set. Both kinds are on disk at once
// during a conversion, and for the seconds between a capture landing and being
// sealed.
func TestKeyedReadStillReadsPlaintext(t *testing.T) {
	key := testKey(t, 5)
	path := write(t, t.TempDir(), "shot.jpg", []byte("not yet sealed"))
	got, err := key.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "not yet sealed" {
		t.Errorf("read %q from an unsealed file", got)
	}
}

func TestWrongKeyIsAnErrorNotEmptyContent(t *testing.T) {
	path := write(t, t.TempDir(), "shot.jpg", []byte("secret"))
	if err := testKey(t, 6).SealFile(path); err != nil {
		t.Fatal(err)
	}
	if got, err := testKey(t, 99).ReadFile(path); err == nil {
		t.Errorf("the wrong key returned %q instead of an error", got)
	}
	var none Key
	if got, err := none.ReadFile(path); err == nil {
		t.Errorf("no key returned %q from a sealed file instead of an error", got)
	}
}

func TestTempCopyIsPlaintextOutsideTheMediaDirectory(t *testing.T) {
	key := testKey(t, 7)
	dir := t.TempDir()
	path := write(t, dir, "chunk.wav", []byte("RIFF audio"))
	if err := key.SealFile(path); err != nil {
		t.Fatal(err)
	}

	temp, cleanup, err := key.TempCopy(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(temp)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "RIFF audio" {
		t.Errorf("the temporary copy holds %q", raw)
	}
	if filepath.Dir(temp) == dir {
		t.Error("the temporary copy was written beside the media, where prune --all would eat it")
	}
	if filepath.Base(temp) != "chunk.wav" {
		t.Errorf("the temporary copy is named %q; callers dispatch on the extension", filepath.Base(temp))
	}
	cleanup()
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Error("cleanup left the plaintext copy on disk")
	}
}

func TestSealIntoWritesASealedCopyAndRefusesToOverwrite(t *testing.T) {
	key := testKey(t, 8)
	dir := t.TempDir()
	source := write(t, dir, "encoded.heic", []byte("compressed bytes"))
	destination := filepath.Join(dir, "shot.heic")

	if err := key.SealInto(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := key.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "compressed bytes" {
		t.Errorf("SealInto produced %q", got)
	}
	if err := key.SealInto(source, destination); err == nil {
		t.Error("SealInto overwrote an existing destination")
	}
}

// The two derived keys must differ, or the split buys nothing.
func TestDerivedKeysDiffer(t *testing.T) {
	master := bytes.Repeat([]byte{0x5a}, 32)
	db, err := DeriveDB(master)
	if err != nil {
		t.Fatal(err)
	}
	media, err := DeriveMedia(master)
	if err != nil {
		t.Fatal(err)
	}
	if len(db) != KeyLen || len(media) != KeyLen {
		t.Fatalf("derived %d and %d bytes, want %d each", len(db), len(media), KeyLen)
	}
	if bytes.Equal(db, media) {
		t.Error("the database and media keys are the same value")
	}
	if bytes.Equal(db, master) {
		t.Error("the database key is the master key")
	}
	// Derivation is deterministic, or an upgrade cannot read yesterday's data.
	again, err := DeriveDB(master)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(db, again) {
		t.Error("DeriveDB is not deterministic")
	}
}

func TestNoMasterDerivesNoKeys(t *testing.T) {
	db, err := DeriveDB(nil)
	if err != nil || db != nil {
		t.Errorf("DeriveDB(nil) = %v, %v; want nil, nil", db, err)
	}
	media, err := DeriveMedia(nil)
	if err != nil || media != nil {
		t.Errorf("DeriveMedia(nil) = %v, %v; want nil, nil", media, err)
	}
}
