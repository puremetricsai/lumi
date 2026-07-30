package vocabulary

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func writeVocabulary(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadMissingFileIsNotAFailure(t *testing.T) {
	loader := &Loader{Path: filepath.Join(t.TempDir(), "vocabulary.txt")}

	snapshot := loader.Load()

	if snapshot.Err != nil {
		t.Fatalf("Err = %v, want nil", snapshot.Err)
	}
	if snapshot.Exists {
		t.Fatal("Exists = true, want false")
	}
	if len(snapshot.Terms) != 0 {
		t.Fatalf("Terms = %q, want empty", snapshot.Terms)
	}
	if !snapshot.Changed {
		t.Fatal("Changed = false on the first Load, want true")
	}
}

func TestLoadReturnsTermsAndCachesUnchangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	writeVocabulary(t, path, "Acme Corp\nMostafa\n")
	loader := &Loader{Path: path}

	first := loader.Load()
	if !slices.Equal(first.Terms, []string{"Acme Corp", "Mostafa"}) {
		t.Fatalf("Terms = %q", first.Terms)
	}
	if !first.Exists || first.Err != nil {
		t.Fatalf("Exists = %v, Err = %v", first.Exists, first.Err)
	}
	if !first.Changed {
		t.Fatal("Changed = false on the first Load, want true")
	}

	second := loader.Load()
	if !slices.Equal(second.Terms, first.Terms) {
		t.Fatalf("cached Terms = %q, want %q", second.Terms, first.Terms)
	}
	if second.Changed {
		t.Fatal("Changed = true on an unchanged repeat Load, want false")
	}
}

func TestLoadPicksUpAnEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	writeVocabulary(t, path, "Alpha\n")
	loader := &Loader{Path: path}
	loader.Load()

	writeVocabulary(t, path, "Alpha\nBravo\n")
	// Force a distinct mtime so the test does not depend on filesystem
	// timestamp granularity.
	touch(t, path)

	snapshot := loader.Load()
	if !slices.Equal(snapshot.Terms, []string{"Alpha", "Bravo"}) {
		t.Fatalf("Terms = %q, want [Alpha Bravo]", snapshot.Terms)
	}
	if !snapshot.Changed {
		t.Fatal("Changed = false after an edit, want true")
	}
}

// TestLoadRecoversAfterAnUnreadableFileBecomesReadable is the regression test
// for the cache bug an adversarial review found: chmod changes neither size
// nor mtime, so a stat-keyed cache that stored the failure could never observe
// the user fixing permissions, and a long-running recorder would transcribe
// without vocabulary forever.
func TestLoadRecoversAfterAnUnreadableFileBecomesReadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files regardless, so this test would pass vacuously")
	}
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	writeVocabulary(t, path, "Acme Corp\n")
	loader := &Loader{Path: path}
	if snapshot := loader.Load(); len(snapshot.Terms) != 1 {
		t.Fatalf("baseline Terms = %q, want one term", snapshot.Terms)
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before chmod: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}

	broken := loader.Load()
	if broken.Err == nil {
		t.Fatal("Err = nil for an unreadable file, want an error")
	}
	if !broken.Exists {
		t.Fatal("Exists = false for a present but unreadable file, want true")
	}
	if len(broken.Terms) != 0 {
		t.Fatalf("Terms = %q for an unreadable file, want empty", broken.Terms)
	}
	if !broken.Changed {
		t.Fatal("Changed = false when the file became unreadable, want true")
	}

	// A persistent failure must not report Changed on every call, or the
	// unconditional retry would cost one log line per audio chunk.
	if loader.Load().Changed {
		t.Fatal("Changed = true on a repeated identical failure, want false")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod 600: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after chmod: %v", err)
	}
	// The premise of the bug: only permissions changed.
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("chmod changed size or mtime (%d/%v -> %d/%v); test premise is invalid",
			before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}

	recovered := loader.Load()
	if recovered.Err != nil {
		t.Fatalf("Err = %v after permissions were restored, want nil", recovered.Err)
	}
	if !slices.Equal(recovered.Terms, []string{"Acme Corp"}) {
		t.Fatalf("Terms = %q after recovery, want [Acme Corp]", recovered.Terms)
	}
	if !recovered.Changed {
		t.Fatal("Changed = false on recovery, want true so the operator sees it")
	}
}

// TestLoadDetectsReplacementWithIdenticalSizeAndMtime pins device+inode in the
// cache key: editors commonly save via temp-file-plus-rename.
func TestLoadDetectsReplacementWithIdenticalSizeAndMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vocabulary.txt")
	writeVocabulary(t, path, "Alpha\n")
	loader := &Loader{Path: path}
	loader.Load()

	original, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat original: %v", err)
	}

	replacement := filepath.Join(dir, "replacement.txt")
	writeVocabulary(t, replacement, "Bravo\n") // same byte length as "Alpha\n"
	if err := os.Chtimes(replacement, original.ModTime(), original.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("rename: %v", err)
	}

	current, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat replaced: %v", err)
	}
	if current.Size() != original.Size() || !current.ModTime().Equal(original.ModTime()) {
		t.Skip("filesystem did not preserve size and mtime across rename; nothing to assert")
	}

	snapshot := loader.Load()
	if !slices.Equal(snapshot.Terms, []string{"Bravo"}) {
		t.Fatalf("Terms = %q after replacement, want [Bravo]", snapshot.Terms)
	}
}

func TestLoadReportsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	content := ""
	for i := 0; i < MaxTerms+3; i++ {
		content += "term-" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "\n"
	}
	writeVocabulary(t, path, content)

	snapshot := (&Loader{Path: path}).Load()
	if snapshot.Dropped != 3 {
		t.Fatalf("Dropped = %d, want 3", snapshot.Dropped)
	}
}

func TestLoadReturnsTermsTheCallerCannotCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	writeVocabulary(t, path, "Alpha\nBravo\n")
	loader := &Loader{Path: path}

	first := loader.Load()
	if len(first.Terms) != 2 {
		t.Fatalf("Terms = %q, want two terms", first.Terms)
	}

	// A caller mutating what it was handed must not reach the cache.
	first.Terms[0] = "CORRUPTED"

	second := loader.Load()
	if !slices.Equal(second.Terms, []string{"Alpha", "Bravo"}) {
		t.Fatalf("Terms = %q after a caller mutated an earlier result, want [Alpha Bravo]", second.Terms)
	}
	if second.Changed {
		t.Fatal("Changed = true, want false: the file did not change, only a caller's private copy did")
	}
}

// touch advances the file's mtime so a reload is detectable regardless of
// filesystem timestamp granularity.
func touch(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	next := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}
