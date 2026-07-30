package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/puremetricsai/lumi/internal/vocabulary"
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

// TestDefaultUnreadableVocabularyWarnsAndContinues is the counterpart to
// TestExplicitUnreadableVocabularyPathIsAnError: the same Err-not-nil branch
// takes the opposite action when the file was never explicitly named. It must
// warn to stderr and let the run continue baseline rather than fail it, since
// running with no vocabulary is a legitimate outcome for the default path.
func TestDefaultUnreadableVocabularyWarnsAndContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files regardless")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "vocabulary.txt"), []byte("Acme Corp\n"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &app{dataDir: root}

	terms, err := a.resolveTranscribeVocabulary("", false, false)
	if err != nil {
		t.Fatalf("resolveTranscribeVocabulary with an unreadable default file: %v", err)
	}
	if len(terms) != 0 {
		t.Fatalf("terms = %q, want empty", terms)
	}
}

// TestExplicitEmptyVocabularyPathIsAnError is the regression test for
// --vocabulary="" (realistic as --vocabulary="$VOCAB" with VOCAB unset): the
// flag was explicitly set but carries no path. Falling through to the default
// file would apply vocabulary-biased recognition while the user typed
// something that reads as "no path" — the same measurement corruption the
// missing-path guard exists to prevent, arriving by a different route.
func TestExplicitEmptyVocabularyPathIsAnError(t *testing.T) {
	a := &app{dataDir: t.TempDir()}

	_, err := a.resolveTranscribeVocabulary("", false, true)
	if err == nil {
		t.Fatal("resolveTranscribeVocabulary returned nil error for an explicit empty path")
	}
}

// TestResolveTranscribeVocabularyCapsAtMaxTerms pins that a file exceeding
// MaxTerms still returns exactly MaxTerms terms through the resolver. The
// stderr warning it also emits is not asserted here (awkward to capture
// through this seam); the durable, testable part is that the cap held.
func TestResolveTranscribeVocabularyCapsAtMaxTerms(t *testing.T) {
	var lines []string
	for i := 0; i < vocabulary.MaxTerms+50; i++ {
		lines = append(lines, fmt.Sprintf("term-%03d", i))
	}
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &app{dataDir: t.TempDir()}

	terms, err := a.resolveTranscribeVocabulary(path, false, true)
	if err != nil {
		t.Fatalf("resolveTranscribeVocabulary: %v", err)
	}
	if len(terms) != vocabulary.MaxTerms {
		t.Fatalf("len(terms) = %d, want %d (MaxTerms)", len(terms), vocabulary.MaxTerms)
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

func TestReportVocabularyStates(t *testing.T) {
	t.Run("absent file reports none", func(t *testing.T) {
		paths := testPaths(t)
		var out bytes.Buffer

		reportVocabulary(&out, paths)

		if !strings.Contains(out.String(), "vocabulary\tnone\t") {
			t.Fatalf("output = %q, want a none line", out.String())
		}
	})

	t.Run("parsed file reports the term count", func(t *testing.T) {
		paths := testPaths(t)
		if err := os.MkdirAll(paths.Root, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(paths.Vocabulary, []byte("Alpha\nBravo\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		var out bytes.Buffer

		reportVocabulary(&out, paths)

		if !strings.Contains(out.String(), "vocabulary\tok\t2 terms") {
			t.Fatalf("output = %q, want an ok line with 2 terms", out.String())
		}
	})

	t.Run("unreadable file reports warn", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads mode-000 files regardless")
		}
		paths := testPaths(t)
		if err := os.MkdirAll(paths.Root, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(paths.Vocabulary, []byte("Alpha\n"), 0o000); err != nil {
			t.Fatalf("write: %v", err)
		}
		var out bytes.Buffer

		reportVocabulary(&out, paths)

		if !strings.Contains(out.String(), "vocabulary\twarn\t") {
			t.Fatalf("output = %q, want a warn line", out.String())
		}
	})
}

// TestTranscribeCommandFailsBeforePrintingOnBadVocabularyPath is the
// command-level counterpart to TestExplicitMissingVocabularyPathIsAnError,
// which only calls the resolver directly and so cannot pin *when* resolution
// happens relative to transcription. A refactor that moved the resolve call
// after TranscribeAudio would keep every existing test green while letting a
// baseline transcript reach stdout ahead of the error — exactly the
// measurement corruption the ordering exists to prevent. This exercises the
// real RunE, so it also proves no native call happens on the failing path: the
// audio path handed in does not exist, and the command must still fail on the
// vocabulary path before ever touching it.
func TestTranscribeCommandFailsBeforePrintingOnBadVocabularyPath(t *testing.T) {
	realStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = realStdout })

	captured := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		captured <- string(data)
	}()

	a := &app{dataDir: t.TempDir()}
	cmd := a.transcribeCommand()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		filepath.Join(t.TempDir(), "nonexistent.wav"),
		"--vocabulary", filepath.Join(t.TempDir(), "typo.txt"),
	})

	execErr := cmd.ExecuteContext(context.Background())

	os.Stdout = realStdout
	w.Close()
	out := <-captured

	if execErr == nil {
		t.Fatal("transcribe with a bogus --vocabulary path returned a nil error")
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty: vocabulary resolution must fail before transcription runs", out)
	}
}
