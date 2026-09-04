package cli

import (
	"fmt"
	"os"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/spf13/cobra"
)

// transcribeCommand replays one audio file through the same native path the recorder
// uses. Replaying fixed audio is the only way to attribute a transcript change
// to a code or model change: comparing two live recordings confounds it with
// how the words were spoken.
func (a *app) transcribeCommand() *cobra.Command {
	var speechLocale string
	cmd := emitsContent(&cobra.Command{
		Use:   "transcribe <file>",
		Short: "Transcribe one audio file with the on-device SpeechAnalyzer",
		Long: "Transcribe one audio file and print the text, using the same recognizer the\n" +
			"recorder runs. Replaying a fixed file is how a transcript change is attributed\n" +
			"to a change in Lumi or in Apple's model rather than to the speech itself.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text, err := macosnative.TranscribeAudio(cmd.Context(), args[0], speechLocale)
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, text)
			return nil
		},
	})
	cmd.Flags().StringVar(&speechLocale, "speech-locale", "en-US", "SpeechAnalyzer recognition locale")
	return cmd
}
