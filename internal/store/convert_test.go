package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testKey is a fixed key. No test may reach the Keychain: the key is a value
// this package takes, and where it comes from is internal/cli's business.
func testKey(seed byte) []byte {
	key := make([]byte, KeyLen)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

func seed(t *testing.T, ctx context.Context, s *Store, text string) {
	t.Helper()
	if err := s.Insert(ctx, &Event{
		Kind:       KindScreen,
		CapturedAt: time.Now().UTC(),
		Text:       text,
		MediaPath:  "/media/" + text + ".jpg",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestEncryptedDatabaseHoldsNoPlaintext is the assertion the whole feature
// rests on. It reads the file as bytes rather than trusting the header, because
// a header can be right while the pages behind it are not.
func TestEncryptedDatabaseHoldsNoPlaintext(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lumi.db")
	key := testKey(1)

	s, err := Open(ctx, path, key)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, ctx, s, "distinctiveneedle")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(raw, []byte(sqliteHeader)) {
		t.Error("an encrypted database still starts with the plaintext SQLite header")
	}
	for _, term := range []string{"distinctiveneedle", "/media/", "events_fts"} {
		if bytes.Contains(raw, []byte(term)) {
			t.Errorf("plaintext %q survives in the encrypted database file", term)
		}
	}
}

// TestOpenRefusesTheWrongKey pins that a bad key is an error rather than an
// empty result. Returning no rows would read as "nothing was captured", which
// is the one wrong answer a user cannot tell from the truth.
func TestOpenRefusesTheWrongKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lumi.db")

	s, err := Open(ctx, path, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	seed(t, ctx, s, "hello")
	s.Close()

	for _, tc := range []struct {
		name string
		key  []byte
	}{
		{"a different key", testKey(9)},
		{"no key at all", nil},
	} {
		if opened, err := Open(ctx, path, tc.key); err == nil {
			opened.Close()
			t.Errorf("opening an encrypted database with %s succeeded", tc.name)
		}
	}
}

// TestOpenRejectsAMisSizedKey: a key of the wrong length is a caller bug, and
// padding or truncating it would write a database nothing can open again.
func TestOpenRejectsAMisSizedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lumi.db")
	if s, err := Open(context.Background(), path, []byte("short")); err == nil {
		s.Close()
		t.Fatal("Open accepted a 5-byte key")
	}
}

// TestConvertRoundTrips is the gate on `lumi encrypt`. Both directions, with
// the rows readable at each end.
func TestConvertRoundTrips(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	plain := filepath.Join(dir, "lumi.db")
	key := testKey(7)

	s, err := Open(ctx, plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"alpha", "bravo", "charlie"} {
		seed(t, ctx, s, word)
	}
	s.Close()

	encrypted := filepath.Join(dir, "encrypted.db")
	if err := ConvertTo(ctx, plain, nil, encrypted, key); err != nil {
		t.Fatal(err)
	}
	enc, err := Open(ctx, encrypted, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := enc.Search(ctx, SearchOptions{Query: "bravo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("search of the encrypted conversion returned %d rows, want 1", len(got))
	}
	enc.Close()

	// And back. This is the `encrypt off` path, and it is the one that must
	// not lose anything: the encrypted original is deleted after it.
	back := filepath.Join(dir, "back.db")
	if err := ConvertTo(ctx, encrypted, key, back, nil); err != nil {
		t.Fatal(err)
	}
	plainAgain, err := Open(ctx, back, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plainAgain.Close()
	all, err := plainAgain.Search(ctx, SearchOptions{Query: "alpha OR bravo OR charlie", Match: MatchAny})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("round trip returned %d of 3 rows", len(all))
	}
}

// TestConvertRefusesAnExistingDestination: VACUUM INTO will not overwrite, and
// the caller's scratch file is what makes a killed conversion re-runnable — so
// this refusal has to be the store's, not a surprise from SQLite.
func TestConvertRefusesAnExistingDestination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "lumi.db")
	s, err := Open(ctx, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	occupied := filepath.Join(dir, "taken.db")
	if err := os.WriteFile(occupied, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ConvertTo(ctx, source, nil, occupied, testKey(3)); err == nil {
		t.Fatal("ConvertTo overwrote an existing destination")
	}
}

// TestStaleReportsAReplacedFile pins what `lumi mcp` relies on: a handle held
// across `lumi encrypt` is reading a file that no longer has that name.
func TestStaleReportsAReplacedFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "lumi.db")

	s, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Stale() {
		t.Fatal("a freshly opened store reports itself stale")
	}

	replacement := filepath.Join(dir, "replacement.db")
	other, err := Open(ctx, replacement, nil)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if !s.Stale() {
		t.Error("a store whose file was replaced by rename does not report itself stale")
	}
}
