package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
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
			"  external  sound the microphone picked up from the room, usually you.\n" +
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
		"drop turns below this attribution confidence (0 to 1); defaults to 0 so nothing is hidden")
	flags.IntVar(&limit, "limit", store.DefaultTranscriptTurns, "maximum turns")
	flags.BoolVar(&asJSON, "json", false, "emit JSON")
	flags.BoolVar(&includeBleed, "include-bleed", false,
		"also show the microphone's re-recording of machine audio, which is otherwise excluded")

	cmd.AddCommand(a.transcriptBackfillCommand())
	return cmd
}

// transcriptOptions validates the flags and builds the store query. Origin is
// checked here rather than left to the store so a typo is reported as a bad flag
// instead of silently returning nothing.
func transcriptOptions(since, until, origin string, minConfidence float64, limit int, includeBleed bool) (store.TranscriptOptions, error) {
	opts := store.TranscriptOptions{
		Since: time.Now().Add(-time.Hour), Until: time.Now(),
		MinConfidence: minConfidence, MaxTurns: limit, IncludeBleed: includeBleed,
	}
	switch strings.TrimSpace(origin) {
	case "":
	case store.OriginInternal, store.OriginExternal, store.OriginUnknown:
		opts.Origin = strings.TrimSpace(origin)
	default:
		return opts, fmt.Errorf("invalid --origin %q (want internal, external, or unknown)", origin)
	}
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
	// Coverage goes to the same stream as the transcript, after it, because a
	// transcript with holes looks complete: nothing in the turns reveals the gap.
	if missing := result.Chunks - result.AttributedChunks; missing > 0 {
		fmt.Fprintf(out, "\n%d of %d audio chunks in this range are not attributed yet, so this "+
			"transcript has gaps.\nRun `lumi transcript backfill` to fill them.\n",
			missing, result.Chunks)
	}
	// Truncation is reported before capping because it is the worse fact: a
	// capped page ends where the reader asked it to, while a truncated one stops
	// short of the requested range with nothing in the turns to show it.
	if result.Truncated {
		fmt.Fprintf(out, "\nThis range holds more audio than one read returns, so the transcript stops at %s.\n"+
			"Re-run with --since %s to continue from there.\n",
			result.CoveredUntil.Local().Format(time.RFC3339),
			result.CoveredUntil.UTC().Format(time.RFC3339))
	}
	if result.Capped {
		fmt.Fprintf(out, "\nStopped at %d turns; narrow --since/--until or raise --limit for more.\n",
			len(result.Turns))
	}
	if len(result.Turns) == 0 && result.Chunks == 0 {
		fmt.Fprintln(out, "No audio was captured in this range.")
	}
}
