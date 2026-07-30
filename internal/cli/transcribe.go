package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/vocabulary"
	"github.com/spf13/cobra"
)

// transcribeCommand replays one WAV through the same native path the recorder
// uses. It exists so a vocabulary change can be attributed: comparing two live
// recordings confounds the term list with how the words were spoken, while
// replaying fixed audio does not.
func (a *app) transcribeCommand() *cobra.Command {
	var speechLocale, vocabularyPath string
	var noVocabulary bool
	cmd := &cobra.Command{
		Use:   "transcribe <file.wav>",
		Short: "Transcribe one WAV file with the on-device SpeechAnalyzer",
		Long: "Transcribe one WAV file and print the text. Reads the vocabulary file from the data\n" +
			"directory unless --vocabulary or --no-vocabulary says otherwise, so the same audio can\n" +
			"be replayed with and without contextual terms.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve the vocabulary first: an explicit path that cannot be
			// used must fail before any transcription happens, so the command
			// never prints a baseline transcript that looks like a
			// vocabulary-assisted one.
			terms, err := a.resolveTranscribeVocabulary(
				vocabularyPath, noVocabulary, cmd.Flags().Changed("vocabulary"))
			if err != nil {
				return err
			}
			text, err := macosnative.TranscribeAudio(cmd.Context(), args[0], speechLocale, terms)
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, text)
			return nil
		},
	}
	cmd.Flags().StringVar(&speechLocale, "speech-locale", "en-US", "SpeechAnalyzer recognition locale")
	cmd.Flags().StringVar(&vocabularyPath, "vocabulary", "", "vocabulary file to bias recognition (default: <data-dir>/vocabulary.txt)")
	cmd.Flags().BoolVar(&noVocabulary, "no-vocabulary", false, "transcribe without any vocabulary, for a baseline comparison")
	cmd.MarkFlagsMutuallyExclusive("vocabulary", "no-vocabulary")
	return cmd
}

// resolveTranscribeVocabulary reads the term list for one transcribe run.
//
// An explicitly supplied path must exist and be readable. Checking only for a
// read error would let the likeliest mistake through: a typo'd or deleted path
// is absent, not unreadable, so it carries no error, and the run would produce
// a perfectly ordinary baseline transcript that the user would then compare
// against another baseline and conclude the vocabulary did nothing.
//
// The default path is different: running with no vocabulary is a legitimate
// baseline, so its absence stays silent. Only the explicit flag carries an
// assertion that the file is there.
func (a *app) resolveTranscribeVocabulary(path string, disabled, explicit bool) ([]string, error) {
	if disabled {
		return nil, nil
	}
	if explicit && path == "" {
		return nil, errors.New("--vocabulary requires a path; use --no-vocabulary for a baseline")
	}
	if path == "" {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		path = paths.Vocabulary
	}
	snapshot := (&vocabulary.Loader{Path: path}).Load()
	if snapshot.Err != nil {
		if explicit {
			return nil, snapshot.Err
		}
		fmt.Fprintf(os.Stderr, "warning: %v\n", snapshot.Err)
		return nil, nil
	}
	if explicit && !snapshot.Exists {
		return nil, fmt.Errorf("vocabulary file %s does not exist", path)
	}
	if snapshot.Dropped > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d terms dropped past the %d-term cap\n",
			snapshot.Dropped, vocabulary.MaxTerms)
	}
	return snapshot.Terms, nil
}
