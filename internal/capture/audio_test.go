package capture

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/puremetricsai/lumi/internal/vocabulary"
)

func TestVocabularyTermsWithoutALoader(t *testing.T) {
	if terms := (NativeSpeech{Locale: "en-US"}).vocabularyTerms(); terms != nil {
		t.Fatalf("vocabularyTerms() = %q, want nil when no loader is configured", terms)
	}
}

func TestVocabularyTermsReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	if err := os.WriteFile(path, []byte("Acme Corp\nMostafa\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var log bytes.Buffer
	speech := NativeSpeech{
		Locale:     "en-US",
		Vocabulary: &vocabulary.Loader{Path: path},
		Logger:     slog.New(slog.NewTextHandler(&log, nil)),
	}

	terms := speech.vocabularyTerms()

	if !slices.Equal(terms, []string{"Acme Corp", "Mostafa"}) {
		t.Fatalf("vocabularyTerms() = %q", terms)
	}
	if !strings.Contains(log.String(), "vocabulary loaded") {
		t.Fatalf("first load did not log; got %q", log.String())
	}
}

func TestVocabularyTermsLogsAFailureOnceAndKeepsGoing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode-000 files regardless")
	}
	path := filepath.Join(t.TempDir(), "vocabulary.txt")
	if err := os.WriteFile(path, []byte("Acme Corp\n"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	var log bytes.Buffer
	speech := NativeSpeech{
		Locale:     "en-US",
		Vocabulary: &vocabulary.Loader{Path: path},
		Logger:     slog.New(slog.NewTextHandler(&log, nil)),
	}

	if terms := speech.vocabularyTerms(); len(terms) != 0 {
		t.Fatalf("vocabularyTerms() = %q, want empty on an unreadable file", terms)
	}
	if count := strings.Count(log.String(), "vocabulary unavailable"); count != 1 {
		t.Fatalf("warned %d times on the first failure, want 1", count)
	}

	speech.vocabularyTerms()
	speech.vocabularyTerms()
	if count := strings.Count(log.String(), "vocabulary unavailable"); count != 1 {
		t.Fatalf("warned %d times across three calls, want 1", count)
	}
}

func TestVocabularyTermsStaysQuietWhenNoFileExists(t *testing.T) {
	var log bytes.Buffer
	speech := NativeSpeech{
		Locale:     "en-US",
		Vocabulary: &vocabulary.Loader{Path: filepath.Join(t.TempDir(), "vocabulary.txt")},
		Logger:     slog.New(slog.NewTextHandler(&log, nil)),
	}

	if terms := speech.vocabularyTerms(); len(terms) != 0 {
		t.Fatalf("vocabularyTerms() = %q, want empty", terms)
	}
	if log.Len() != 0 {
		t.Fatalf("logged %q for an absent file; absence is the normal state", log.String())
	}
}
