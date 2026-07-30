package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestExplicitMissingVocabularyPathIsAnError is the regression test for a
// defect an adversarial review found in the design: gating only on a read
// error let a typo'd --vocabulary path through, because a missing file is
// *absent*, not unreadable. The command would then print an ordinary baseline
// transcript that looks like a vocabulary-assisted one, corrupting exactly the
// comparison this command exists to make.
func TestExplicitMissingVocabularyPathIsAnError(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	missing := filepath.Join(t.TempDir(), "typo.txt")

	_, err := a.resolveTranscribeVocabulary(missing, false, true)
	if err == nil {
		t.Fatal("resolveTranscribeVocabulary returned nil error for an explicit missing path")
	}
}

func TestExplicitUnreadableVocabularyPathIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files regardless")
	}
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	if err := os.WriteFile(path, []byte("Acme Corp\n"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &app{dataDir: t.TempDir()}

	if _, err := a.resolveTranscribeVocabulary(path, false, true); err == nil {
		t.Fatal("resolveTranscribeVocabulary returned nil error for an explicit unreadable path")
	}
}

func TestMissingDefaultVocabularyIsNotAnError(t *testing.T) {
	a := &app{dataDir: t.TempDir()}

	terms, err := a.resolveTranscribeVocabulary("", false, false)
	if err != nil {
		t.Fatalf("resolveTranscribeVocabulary with an absent default file: %v", err)
	}
	if len(terms) != 0 {
		t.Fatalf("terms = %q, want empty", terms)
	}
}

func TestExplicitVocabularyPathIsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	if err := os.WriteFile(path, []byte("Acme Corp\nMostafa\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &app{dataDir: t.TempDir()}

	terms, err := a.resolveTranscribeVocabulary(path, false, true)
	if err != nil {
		t.Fatalf("resolveTranscribeVocabulary: %v", err)
	}
	if !slices.Equal(terms, []string{"Acme Corp", "Mostafa"}) {
		t.Fatalf("terms = %q", terms)
	}
}

func TestNoVocabularySkipsTheFileEntirely(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "typo.txt")
	a := &app{dataDir: t.TempDir()}

	// disabled must win over the explicit-path guard: --no-vocabulary is how a
	// baseline run is produced, and it must never fail on a stale path.
	terms, err := a.resolveTranscribeVocabulary(missing, true, true)
	if err != nil {
		t.Fatalf("resolveTranscribeVocabulary with --no-vocabulary: %v", err)
	}
	if terms != nil {
		t.Fatalf("terms = %q, want nil", terms)
	}
}

func TestDefaultVocabularyComesFromTheDataDir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "vocabulary.txt"), []byte("Alpha\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &app{dataDir: root}

	terms, err := a.resolveTranscribeVocabulary("", false, false)
	if err != nil {
		t.Fatalf("resolveTranscribeVocabulary: %v", err)
	}
	if !slices.Equal(terms, []string{"Alpha"}) {
		t.Fatalf("terms = %q, want [Alpha]", terms)
	}
}
