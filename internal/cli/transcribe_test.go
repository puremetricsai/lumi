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

	_, err := a.resolveTranscribeVocabulary(io.Discard, missing, false, true)
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

	if _, err := a.resolveTranscribeVocabulary(io.Discard, path, false, true); err == nil {
		t.Fatal("resolveTranscribeVocabulary returned nil error for an explicit unreadable path")
	}
}

// TestDefaultUnreadableVocabularyWarnsAndContinues is the counterpart to
// TestExplicitUnreadableVocabularyPathIsAnError: the same Err-not-nil branch
// takes the opposite action when the file was never explicitly named. It must
// warn to the injected writer and let the run continue baseline rather than
// fail it, since running with no vocabulary is a legitimate outcome for the
// default path. The warning itself is asserted, not just the non-error return,
// so a regression that silently swallowed the read error would be caught here.
func TestDefaultUnreadableVocabularyWarnsAndContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files regardless")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "vocabulary.txt"), []byte("Acme Corp\n"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &app{dataDir: root}
	var out bytes.Buffer

	terms, err := a.resolveTranscribeVocabulary(&out, "", false, false)
	if err != nil {
		t.Fatalf("resolveTranscribeVocabulary with an unreadable default file: %v", err)
	}
	if len(terms) != 0 {
		t.Fatalf("terms = %q, want empty", terms)
	}
	if !strings.Contains(out.String(), "warning:") {
		t.Fatalf("output = %q, want a warning about the unreadable file", out.String())
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

	_, err := a.resolveTranscribeVocabulary(io.Discard, "", false, true)
	if err == nil {
		t.Fatal("resolveTranscribeVocabulary returned nil error for an explicit empty path")
	}
}

// TestResolveTranscribeVocabularyCapsAtMaxTerms pins that a file exceeding
// MaxTerms still returns exactly MaxTerms terms through the resolver, and that
// the dropped-terms warning names both the drop count and the cap. Asserting
// only len(terms) == MaxTerms would pass even if the warning line were deleted
// entirely, since vocabulary.Parse enforces the cap on its own; the warning
// content is what this test exists to pin.
func TestResolveTranscribeVocabularyCapsAtMaxTerms(t *testing.T) {
	const extra = 5
	var lines []string
	for i := 0; i < vocabulary.MaxTerms+extra; i++ {
		lines = append(lines, fmt.Sprintf("term-%03d", i))
	}
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &app{dataDir: t.TempDir()}
	var out bytes.Buffer

	terms, err := a.resolveTranscribeVocabulary(&out, path, false, true)
	if err != nil {
		t.Fatalf("resolveTranscribeVocabulary: %v", err)
	}
	if len(terms) != vocabulary.MaxTerms {
		t.Fatalf("len(terms) = %d, want %d (MaxTerms)", len(terms), vocabulary.MaxTerms)
	}
	warning := out.String()
	if !strings.Contains(warning, fmt.Sprintf("%d", extra)) {
		t.Fatalf("warning = %q, want it to report %d dropped terms", warning, extra)
	}
	if !strings.Contains(warning, fmt.Sprintf("%d", vocabulary.MaxTerms)) {
		t.Fatalf("warning = %q, want it to mention the %d-term cap", warning, vocabulary.MaxTerms)
	}
}

// TestResolveTranscribeVocabularyWarnsOnlyWhenTermsAreDropped is the negative
// counterpart: a file at or under MaxTerms must produce no diagnostic output
// at all, so the dropped-terms warning cannot fire spuriously on an ordinary
// vocabulary file.
func TestResolveTranscribeVocabularyWarnsOnlyWhenTermsAreDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	if err := os.WriteFile(path, []byte("Acme Corp\nMostafa\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &app{dataDir: t.TempDir()}
	var out bytes.Buffer

	terms, err := a.resolveTranscribeVocabulary(&out, path, false, true)
	if err != nil {
		t.Fatalf("resolveTranscribeVocabulary: %v", err)
	}
	if len(terms) != 2 {
		t.Fatalf("len(terms) = %d, want 2", len(terms))
	}
	if out.String() != "" {
		t.Fatalf("output = %q, want empty: nothing was dropped", out.String())
	}
}

func TestMissingDefaultVocabularyIsNotAnError(t *testing.T) {
	a := &app{dataDir: t.TempDir()}

	terms, err := a.resolveTranscribeVocabulary(io.Discard, "", false, false)
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

	terms, err := a.resolveTranscribeVocabulary(io.Discard, path, false, true)
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
	terms, err := a.resolveTranscribeVocabulary(io.Discard, missing, true, true)
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

	terms, err := a.resolveTranscribeVocabulary(io.Discard, "", false, false)
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
// real RunE, so it also proves no native call happens on the failing path.
//
// The audio path handed in also does not exist. That is deliberate (a real
// WAV would invoke the native SpeechAnalyzer path, making the test slow and
// permission-dependent), but it means a bare "execErr != nil" assertion does
// not actually pin the ordering: if resolve-then-transcribe were inverted to
// transcribe-then-resolve, macosnative.TranscribeAudio would fail on the
// missing audio file first, producing a non-nil error and empty stdout just
// the same, and this test would stay green through the regression it exists
// to catch. What discriminates the two orderings is the error's *identity* —
// which path it names — so the assertion below checks that the error mentions
// the bogus vocabulary path and does not mention the audio path. If ordering
// were inverted, the error would name the audio path instead and this
// assertion would fail.
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
	audioPath := filepath.Join(t.TempDir(), "nonexistent.wav")
	vocabPath := filepath.Join(t.TempDir(), "typo.txt")
	cmd.SetArgs([]string{
		audioPath,
		"--vocabulary", vocabPath,
	})

	execErr := cmd.ExecuteContext(context.Background())

	os.Stdout = realStdout
	w.Close()
	out := <-captured

	if execErr == nil {
		t.Fatal("transcribe with a bogus --vocabulary path returned a nil error")
	}
	if !strings.Contains(execErr.Error(), vocabPath) {
		t.Fatalf("execErr = %q, want it to name the bogus vocabulary path %q — "+
			"a native transcription error about the missing audio file would also be "+
			"non-nil, so identity, not mere presence, is what pins resolve-before-transcribe",
			execErr, vocabPath)
	}
	if strings.Contains(execErr.Error(), audioPath) {
		t.Fatalf("execErr = %q, must not name the audio path %q: that would mean "+
			"TranscribeAudio ran before vocabulary resolution failed", execErr, audioPath)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty: vocabulary resolution must fail before transcription runs", out)
	}
}
