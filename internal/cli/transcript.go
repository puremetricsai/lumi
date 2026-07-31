package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/puremetricsai/lumi/internal/store"
)

// lowConfidence is the level below which a turn prints its score. Above it the
// number is noise on every line; below it, it is the most important thing on the
// line.
const lowConfidence = 0.8

// transcriptCommand prints captured audio as one ordered conversation.
//
// It keeps its own RunE rather than being a bare parent that prints help,
// following `mcp`: reading a transcript is the useful thing, and `backfill` is a
// maintenance subcommand hanging off it.
func (a *app) transcriptCommand() *cobra.Command {
	var (
		since, until, origin string
		minConfidence        float64
		limit                int
		asJSON, includeBleed bool
	)
	cmd := &cobra.Command{
		Use:   "transcript",
		Short: "Read captured audio as one ordered, attributed conversation",
		Long: "Assemble captured audio into a single ordered transcript, labelling every turn\n" +
			"by where the sound came from.\n\n" +
			"  internal  sound this machine produced — the far side of a call, a video,\n" +
			"            music, a notification. Not necessarily a person.\n" +
			"  external  sound the microphone picked up from the room. It has no\n" +
			"            recoverable owner: it may be you, other people present, a TV,\n" +
			"            another machine, or ambient playback. Lumi does not identify\n" +
			"            speakers and never attributes this to a person or an app.\n" +
			"  unknown   machine audio was playing but produced no transcript, so its\n" +
			"            origin could not be determined.\n\n" +
			"Because machine audio also bleeds into the microphone, its words appear once\n" +
			"here rather than twice as in `lumi search`. A leading ~ marks a turn whose\n" +
			"position was inferred rather than measured; a trailing score marks a turn\n" +
			"whose attribution is uncertain.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts, err := transcriptOptions(since, until, origin, minConfidence, limit, includeBleed)
			if err != nil {
				return err
			}
			s, _, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()

			result, err := s.Transcript(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if asJSON {
				// The export never learns what a filter took, because it is a bare
				// array by contract. Saying it on stderr reaches a person reading the
				// terminal without putting a sentence in a stream something else is
				// parsing — the alternative is a --json transcript that has had one
				// side of the conversation removed with nothing anywhere to show it.
				if warning := confidenceRemovalWarning(result); warning != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), warning)
				}
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				// A bare array, matching `lumi search --json`: the export is the
				// data, not a wrapper around it.
				return encoder.Encode(result.Turns)
			}
			printTranscript(cmd.OutOrStdout(), result)
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&since, "since", "1h", "earliest time (RFC3339 or duration such as 2h)")
	flags.StringVar(&until, "until", "", "latest time (RFC3339); defaults to now")
	flags.StringVar(&origin, "origin", "", "only turns from this origin: internal, external, or unknown")
	flags.Float64Var(&minConfidence, "min-confidence", 0,
		"drop turns below this confidence (0 to 1); defaults to 0 so nothing is hidden. The\n"+
			"largest attribution penalties apply to microphone turns alone, so a threshold near\n"+
			"0.6 can remove every external turn and no internal one")
	flags.IntVar(&limit, "limit", store.DefaultTranscriptTurns, "maximum turns")
	flags.BoolVar(&asJSON, "json", false, "emit JSON")
	flags.BoolVar(&includeBleed, "include-bleed", false,
		"also show the microphone's re-recording of machine audio, which is otherwise excluded")

	cmd.AddCommand(a.transcriptBackfillCommand())
	return cmd
}

// confidenceRemovalWarning says what --min-confidence removed, and is empty when
// it removed nothing. Both output paths print it, so the sentence is written
// once: the printed transcript and the JSON export must not describe the same
// deletion two ways.
func confidenceRemovalWarning(result store.TranscriptResult) string {
	removed := result.ConfidenceRemovals()
	if removed == "" {
		return ""
	}
	return fmt.Sprintf("--min-confidence removed %s. The largest attribution penalties apply to "+
		"microphone turns alone, so this may have removed a side of the conversation rather than "+
		"its least reliable turns.", removed)
}

// transcriptOptions validates the flags and builds the store query. Origin is
// checked before the query rather than left to it so a typo is reported as a bad
// flag instead of silently returning nothing; which values are valid is
// store.ParseOrigin's to say, since the column is what they have to be valid for.
func transcriptOptions(since, until, origin string, minConfidence float64, limit int, includeBleed bool) (store.TranscriptOptions, error) {
	opts := store.TranscriptOptions{
		Since: time.Now().Add(-time.Hour), Until: time.Now(),
		MinConfidence: minConfidence, MaxTurns: limit, IncludeBleed: includeBleed,
	}
	parsed, err := store.ParseOrigin(origin)
	if err != nil {
		return opts, fmt.Errorf("invalid --origin: %w", err)
	}
	opts.Origin = parsed
	if minConfidence < 0 || minConfidence > 1 {
		return opts, fmt.Errorf("--min-confidence must be between 0 and 1, got %v", minConfidence)
	}
	if since != "" {
		parsed, err := parseTime(since, true)
		if err != nil {
			return opts, fmt.Errorf("parse --since: %w", err)
		}
		opts.Since = *parsed
	}
	if until != "" {
		parsed, err := parseTime(until, false)
		if err != nil {
			return opts, fmt.Errorf("parse --until: %w", err)
		}
		opts.Until = *parsed
	}
	if opts.Until.Before(opts.Since) {
		return opts, fmt.Errorf("--until (%s) is before --since (%s)",
			opts.Until.Format(time.RFC3339), opts.Since.Format(time.RFC3339))
	}
	return opts, nil
}

// externalTurns counts the turns that came from the room rather than from this
// machine, which is what decides whether the microphone caveat is printed.
func externalTurns(result store.TranscriptResult) int {
	count := 0
	for _, turn := range result.Turns {
		if turn.Origin == store.OriginExternal {
			count++
		}
	}
	return count
}

// printTranscript renders turns for a person, one line each.
func printTranscript(out io.Writer, result store.TranscriptResult) {
	for _, turn := range result.Turns {
		marker := " "
		if turn.OrderConfidence == store.OrderApproximate {
			marker = "~"
		}
		when := "        "
		if turn.StartedAt != nil {
			when = turn.StartedAt.Local().Format("15:04:05")
		}
		score := ""
		if turn.Confidence < lowConfidence {
			score = fmt.Sprintf("  (%.2f)", turn.Confidence)
		}
		overlap := ""
		if turn.Overlaps {
			overlap = "  [overlapping]"
		}
		fmt.Fprintf(out, "%s%s  %-8s  %s%s%s\n",
			marker, when, turn.Origin, truncateRunes(turn.Text, 300), score, overlap)
	}
	// Restate the microphone caveat wherever external turns were actually
	// printed. The origin label alone reads as a speaker to anyone summarising
	// this, and the WAV it came from is deleted on the retention schedule while a
	// summary built from it survives — so a speaker inferred here becomes
	// permanent. Saying it beside the text is what stops that.
	if externalTurns(result) > 0 {
		fmt.Fprint(out, "\nexternal turns are room audio with no identified speaker: they may be you, "+
			"other people present, or playback.\nDo not record them as something you said or did.\n")
	}
	// What the threshold took, for the same reason coverage is printed below: the
	// turns that survived it read as a whole conversation. This is the more
	// deceptive of the two, because the largest attribution penalties fall on
	// microphone turns, so a threshold can remove the room and leave the machine —
	// and the caveat above does not print when there are no external turns left to
	// caveat.
	if warning := confidenceRemovalWarning(result); warning != "" {
		fmt.Fprintf(out, "\n%s\n", warning)
	}
	// Coverage goes to the same stream as the transcript, after it, because a
	// transcript with holes looks complete: nothing in the turns reveals the gap.
	if missing := result.MissingChunks(); missing > 0 {
		fmt.Fprintf(out, "\n%d of %d audio chunks in this range are not attributed yet, so this "+
			"transcript has gaps.\n", missing, result.Chunks)
		// Chunks whose recognition failed are named separately, because the
		// backfill will keep declining to label them: advising it for those would
		// point at a command that cannot change the number above.
		if result.FailedChunks > 0 {
			fmt.Fprintf(out, "%d of them could not be transcribed and no backfill can recover them; "+
				"their audio is still on disk.\n", result.FailedChunks)
		}
		if result.RecoverableChunks() > 0 {
			fmt.Fprintln(out, "Run `lumi transcript backfill` to fill the rest.")
		}
	}
	// Truncation is reported before capping because it is the worse fact: a
	// capped page ends where the reader asked it to, while a truncated one stops
	// short of the requested range with nothing in the turns to show it.
	// Both notices offer ResumeFrom, never CoveredUntil: coverage ends inclusively
	// at the last chunk the turns reach and the segment read is inclusive too, so
	// re-running with that value would print the same chunk's turns again.
	// Local, like every other time this command prints, and like the CoveredUntil
	// in the sentence just below. Rendering this one UTC put two timestamps
	// describing adjacent moments in one paragraph hours apart, which reads as a
	// gap in the recording rather than as the same instant twice.
	resume := ""
	if !result.ResumeFrom.IsZero() {
		resume = result.ResumeFrom.Local().Format(time.RFC3339Nano)
	}
	if result.Truncated {
		fmt.Fprintf(out, "\nThis range holds more audio than one read returns, so the transcript stops at %s.\n"+
			"Re-run with --since %s to continue from there.\n",
			result.CoveredUntil.Local().Format(time.RFC3339), resume)
	}
	if result.Capped {
		fmt.Fprintf(out, "\nStopped at %d turns; re-run with --since %s to continue from there, "+
			"or raise --limit.\n", len(result.Turns), resume)
	}
	if len(result.Turns) == 0 && result.Chunks == 0 {
		fmt.Fprintln(out, "No audio was captured in this range.")
	}
}
