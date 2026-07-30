package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestFromRootDerivesEveryPath(t *testing.T) {
	root := t.TempDir()
	paths, err := FromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"Root", paths.Root, root},
		{"Database", paths.Database, filepath.Join(root, "lumi.db")},
		{"Screenshots", paths.Screenshots, filepath.Join(root, "screenshots")},
		{"Audio", paths.Audio, filepath.Join(root, "audio")},
		{"RecordState", paths.RecordState, filepath.Join(root, "record.json")},
		{"RecordLog", paths.RecordLog, filepath.Join(root, "record.log")},
		{name: "vocabulary", got: paths.Vocabulary, want: filepath.Join(root, "vocabulary.txt")},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestFromRootMakesRelativeRootsAbsolute(t *testing.T) {
	paths, err := FromRoot("relative/dir")
	if err != nil {
		t.Fatal(err)
	}
	// Absoluteness alone would be satisfied by any hardcoded path; the contract
	// is that the relative root resolves against the working directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wd, "relative", "dir")
	if paths.Root != want {
		t.Fatalf("Root = %q, want %q", paths.Root, want)
	}
	if paths.Database != filepath.Join(want, "lumi.db") {
		t.Fatalf("Database = %q, want it derived from %q", paths.Database, want)
	}
}

func TestDefaultPathsHonorsLumiHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LUMI_HOME", home)
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.Root != home {
		t.Fatalf("Root = %q, want %q", paths.Root, home)
	}
}

// Ensure creates the media directories 0700: the data directory holds
// screenshots of everything on screen, so it must never be group- or
// world-readable.
func TestEnsureCreatesDirectories0700(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "lumi")
	paths, err := FromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{paths.Root, paths.Screenshots, paths.Audio} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s permissions = %o, want 700 (holds captured media)", dir, perm)
		}
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	paths, err := FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("second Ensure must succeed on existing directories: %v", err)
	}
}

func TestVocabularyPathLivesUnderRoot(t *testing.T) {
	paths, err := FromRoot(t.TempDir())
	if err != nil {
		t.Fatalf("FromRoot: %v", err)
	}
	want := filepath.Join(paths.Root, "vocabulary.txt")
	if paths.Vocabulary != want {
		t.Fatalf("Vocabulary = %q, want %q", paths.Vocabulary, want)
	}
}

func TestEnsureDoesNotCreateVocabularyFile(t *testing.T) {
	paths, err := FromRoot(t.TempDir())
	if err != nil {
		t.Fatalf("FromRoot: %v", err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(paths.Vocabulary); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(Vocabulary) = %v, want fs.ErrNotExist", err)
	}
}
