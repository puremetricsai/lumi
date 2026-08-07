package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/puremetricsai/lumi/internal/capture"
	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/store"
	"github.com/puremetricsai/lumi/internal/transcript"
	"github.com/puremetricsai/lumi/internal/wav"
)

// retranscribeChunk is a package var purely as a test seam. Without it every
// backfill test would need Speech Recognition granted and real WAVs on disk,
// which is exactly the coupling that keeps a test suite from running in CI.
var retranscribeChunk = func(ctx context.Context, path, locale string) (macosnative.Transcription, error) {
	return macosnative.TranscribeAudioSegments(ctx, path, locale)
}

// minRetranscribeSimilarity is how closely a re-run must agree with the
// transcript already indexed before its timings may be used.
//
// Segments derive from events.text and never the reverse, so a recognition that
// no longer says what the event says may not become segment text at any
// confidence: the words would appear in a transcript while being absent from the
// event and from the FTS index behind `lumi search`. The threshold is not a
// quality bar on the new recognition — it is the test for "these are the same
// words, so its word timings describe this transcript". Below it the timings are
// dropped and the chunk falls back to the text path, which needs no audio at all.
const minRetranscribeSimilarity = 0.9

func (a *app) transcriptBackfillCommand() *cobra.Command {
	var (
		since, until           string
		limit                  int
		force, dryRun, explain bool
		retranscribe           bool
		whileRecording         bool
		pace                   time.Duration
		speechLocale           string
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Attribute captured audio that has no origin labels yet",
		Long: "Derive origin labels for audio chunks that have none, so `lumi transcript`\n" +
			"and the get_transcript MCP tool can read them.\n\n" +
			"By default this works from the transcripts already in the index and touches\n" +
			"no audio files, which is fast and works even after the WAVs have been pruned.\n" +
			"--retranscribe re-runs speech recognition to recover word-level timings,\n" +
			"which is far slower and needs the WAVs to still exist.\n\n" +
			"Work is picked up from whatever is unattributed, so an interrupted run simply\n" +
			"resumes: nothing is written twice and nothing is lost.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var sinceTime, untilTime *time.Time
			if since != "" {
				parsed, err := parseTime(since, true)
				if err != nil {
					return fmt.Errorf("parse --since: %w", err)
				}
				sinceTime = parsed
			}
			if until != "" {
				parsed, err := parseTime(until, false)
				if err != nil {
					return fmt.Errorf("parse --until: %w", err)
				}
				untilTime = parsed
			}
			s, paths, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()

			if !whileRecording {
				if err := refuseWhileRecording(paths, retranscribe); err != nil {
					return err
				}
			}
			return runBackfill(cmd.Context(), cmd.OutOrStdout(), s, backfillOptions{
				Since: sinceTime, Until: untilTime, Limit: limit, Force: force, DryRun: dryRun,
				Explain: explain, Retranscribe: retranscribe, Pace: pace,
				Locale: speechLocale,
			})
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&since, "since", "", "only attribute chunks captured at or after this time")
	flags.StringVar(&until, "until", "", "only attribute chunks captured at or before this time (RFC3339)")
	flags.IntVar(&limit, "limit", 0, "stop after this many chunks (0 means no limit)")
	flags.BoolVar(&force, "force", false, "re-attribute chunks that already have labels")
	flags.BoolVar(&dryRun, "dry-run", false, "report what would change without writing anything")
	flags.BoolVar(&explain, "explain", false, "print the evidence behind each chunk's verdict")
	flags.BoolVar(&retranscribe, "retranscribe", false,
		"re-run speech recognition to recover word timings; much slower and needs the WAV files")
	flags.BoolVar(&whileRecording, "while-recording", false,
		"run even though the recorder is active")
	flags.DurationVar(&pace, "pace", 0, "wait this long between chunks to leave headroom for the recorder")
	flags.StringVar(&speechLocale, "speech-locale", "en-US",
		"recognition locale used by --retranscribe; match what `lumi record start` used")
	return cmd
}

// refuseWhileRecording stops a re-transcribing backfill from competing with the
// live recorder.
//
// The database is safe either way — writes are short, single-connection, and only
// ever touch audio_segments, never the event rows the recorder owns. The hazard
// is compute: the recorder transcribes inline in its capture loop under a native
// timeout, so a second speech workload running beside it can make that timeout
// fire and cost a live chunk its transcript. The text-only path uses no
// recognizer at all, so it is allowed through.
func refuseWhileRecording(paths config.Paths, retranscribe bool) error {
	if !retranscribe {
		return nil
	}
	state, ok, err := readRecordState(paths)
	if err != nil || !ok || !processAlive(state.PID) {
		return nil
	}
	return fmt.Errorf("recording is in progress (pid %d) and --retranscribe competes with it for "+
		"speech recognition, which can cost live chunks their transcripts; "+
		"stop it with `lumi record stop`, drop --retranscribe to use the text-only path, "+
		"or pass --while-recording to override", state.PID)
}

type backfillOptions struct {
	Since, Until *time.Time
	Limit        int
	Force        bool
	DryRun       bool
	Explain      bool
	Retranscribe bool
	Pace         time.Duration
	Locale       string
}

func runBackfill(ctx context.Context, out io.Writer, s *store.Store, opts backfillOptions) error {
	var (
		chunks []string
		err    error
	)
	if opts.Force {
		chunks, err = s.AudioChunkTimes(ctx, opts.Since, opts.Until, opts.Limit)
	} else {
		chunks, err = s.ChunksMissingSegments(ctx, opts.Since, opts.Until, opts.Limit)
	}
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		fmt.Fprintln(out, "every audio chunk in range is already attributed")
		return nil
	}

	mode := "text-only"
	if opts.Retranscribe {
		mode = "re-transcribing"
	}
	fmt.Fprintf(out, "%d chunks to attribute (%s)\n", len(chunks), mode)

	var attributed, silentChunks, skipped, failed, untranscribed, segments int
	started := time.Now()
	for i, key := range chunks {
		if err := ctx.Err(); err != nil {
			// Ctrl-C loses at most the chunk in flight; everything already
			// committed stays, and the queue reflects that on the next run.
			fmt.Fprintf(out, "stopped after %d chunks\n", i)
			break
		}
		outcome, err := attributeStoredChunk(ctx, s, key, opts)
		if err != nil {
			failed++
			fmt.Fprintf(out, "  %s  failed: %v\n", key, err)
			continue
		}
		rows := outcome.rows
		if outcome.untranscribed {
			// Deliberately left on the queue: labelling it means calling it
			// silent, and nothing here can tell silence from a recognizer that
			// never ran. Re-running this command will reach the same conclusion,
			// so it is reported rather than counted as work still to do.
			untranscribed++
			if opts.Explain {
				fmt.Fprintf(out, "  %s  %s\n", key, outcome.explanation())
			}
			continue
		}
		// A chunk holding no words still writes its marker row, so it leaves the
		// queue; it is counted apart from real attribution because "700 chunks
		// attributed" would otherwise describe an afternoon of silence.
		silent := !anySpeech(rows)
		if len(rows) == 0 {
			skipped++
			if opts.Explain {
				fmt.Fprintf(out, "  %s  nothing to attribute (%s)\n", key, outcome.explanation())
			}
			continue
		}
		if opts.Explain {
			label := fmt.Sprintf("%d segments", len(rows))
			if silent {
				label = "silent"
			}
			fmt.Fprintf(out, "  %s  %s  %s\n", key, label, outcome.explanation())
		}
		if !opts.DryRun {
			if err := s.ReplaceChunkSegments(ctx, key, rows); err != nil {
				failed++
				fmt.Fprintf(out, "  %s  write failed: %v\n", key, err)
				continue
			}
		}
		if silent {
			silentChunks++
		} else {
			attributed++
			segments += len(rows)
		}

		// Report an estimate once the first chunk has been timed, because a
		// re-transcribing pass over a long history is a multi-hour operation and
		// that should be knowable before it is half done.
		if i == 0 && len(chunks) > 1 {
			each := time.Since(started)
			fmt.Fprintf(out, "  first chunk took %s; roughly %s for all %d\n",
				each.Round(time.Millisecond), (each * time.Duration(len(chunks))).Round(time.Second), len(chunks))
		}
		if opts.Pace > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(opts.Pace):
			}
		}
	}

	if opts.DryRun {
		fmt.Fprintf(out, "dry run: %d chunks would gain %d segments, %d hold no speech, "+
			"%d had nothing to attribute, %d failed\n",
			attributed, segments, silentChunks, skipped, failed)
		reportUntranscribed(out, untranscribed)
		return nil
	}
	fmt.Fprintf(out, "attributed %d chunks (%d segments), %d hold no speech, "+
		"%d had nothing to attribute, %d failed\n",
		attributed, segments, silentChunks, skipped, failed)
	reportUntranscribed(out, untranscribed)
	return nil
}

// reportUntranscribed names the chunks this command will keep declining to
// attribute, so their standing place in the queue reads as a finding rather than
// as work the next run will get to.
func reportUntranscribed(out io.Writer, count int) {
	if count == 0 {
		return
	}
	fmt.Fprintf(out, "%d could not be transcribed, so they stay unattributed; "+
		"their audio is still on disk and `lumi transcribe <file.wav>` can read it\n", count)
}

// anySpeech reports whether a chunk's derived rows carry any words, which is
// what separates a real attribution from the marker written for silence.
func anySpeech(rows []store.Segment) bool {
	for _, row := range rows {
		if store.HasSegmentText(row) {
			return true
		}
	}
	return false
}

// chunkOutcome is what re-deriving one chunk concluded.
type chunkOutcome struct {
	rows []store.Segment
	// explain composes the --explain line on demand rather than eagerly, because
	// the line re-aligns the two transcripts the attribution path just aligned and
	// --explain is off by default. Nil when there is nothing to say.
	explain func() string
	// untranscribed marks a chunk with no transcript to work from because
	// recognition failed rather than because there was nothing to hear. It is a
	// third outcome, not a variety of "nothing to attribute": the chunk stays on
	// the queue on purpose and no re-run will change that.
	untranscribed bool
}

func (o chunkOutcome) explanation() string {
	if o.explain == nil {
		return ""
	}
	return o.explain()
}

// attributeStoredChunk re-derives one chunk's segments from what is in the index,
// optionally re-reading its audio.
func attributeStoredChunk(ctx context.Context, s *store.Store, key string, opts backfillOptions) (chunkOutcome, error) {
	events, err := s.AudioEventsAt(ctx, key)
	if err != nil {
		return chunkOutcome{}, err
	}
	if len(events) == 0 {
		return chunkOutcome{explain: func() string { return "no rows" }}, nil
	}
	capturedAt := events[0].CapturedAt

	chunk := transcript.Chunk{CapturedAt: capturedAt}
	eventIDs := make(map[string]int64, len(events))
	paths := make(map[string]string, len(events))
	for _, event := range events {
		track := &transcript.Track{Source: event.AudioSource, Text: event.Text}
		eventIDs[event.AudioSource] = event.ID
		paths[event.AudioSource] = event.MediaPath
		switch event.AudioSource {
		case transcript.TrackSystem:
			chunk.System = track
		case transcript.TrackMicrophone:
			chunk.Microphone = track
		}
	}

	method := "text"
	if opts.Retranscribe {
		if err := loadTimings(ctx, &chunk, paths, opts); err != nil {
			// A missing or unreadable WAV is expected on an aged-out history, and
			// the text path still works, so this degrades rather than failing.
			method = fmt.Sprintf("text (no timings: %v)", err)
		} else {
			method = "timed"
		}
	}
	measureBackfillEnergy(&chunk, paths[transcript.TrackSystem])

	attributed := transcript.Attribute(chunk, transcript.Options{})
	// Deferred rather than computed: --explain is off by default, and composing
	// the line costs a second full alignment of the two transcripts the path just
	// aligned. Nothing below reads it more than once.
	verdict := func() string { return backfillVerdict(method, chunk, attributed) }
	if transcript.IsSilent(attributed) && store.AnyFailedTranscription(events) {
		// The same gate the recorder applies, for the same reason: silence is the
		// one verdict read out of an absence of words, so it is the one a failed
		// recognizer can turn into a false claim. Writing the marker here would
		// also make it permanent, since the marker is what drains the queue.
		return chunkOutcome{
			explain:       func() string { return "could not be transcribed; left unattributed (" + verdict() + ")" },
			untranscribed: true,
		}, nil
	}
	return chunkOutcome{rows: store.SegmentRows(attributed, eventIDs), explain: verdict}, nil
}

func loadTimings(ctx context.Context, chunk *transcript.Chunk, paths map[string]string, opts backfillOptions) error {
	// Fixed order, not a map range: this returns on the first failure, so with
	// map ordering which track's error surfaces — and which track kept the
	// timings loaded before it — would differ between two runs over identical
	// data.
	tracks := []struct {
		source string
		track  *transcript.Track
	}{{transcript.TrackSystem, chunk.System}, {transcript.TrackMicrophone, chunk.Microphone}}
	for _, entry := range tracks {
		source, track := entry.source, entry.track
		// A track with no stored transcript has nothing for timings to be timings
		// *of*: segments derive from events.text, so recovering words here would
		// mean writing text no event holds. It is also the common case by a wide
		// margin, and running the recognizer over an afternoon of silence costs
		// hours. A track whose recognition *failed* never reaches this function —
		// attributeStoredChunk declines the whole chunk first — so this skip means
		// "nothing was said", not "we never found out".
		if track == nil || track.Text == "" {
			continue
		}
		path := paths[source]
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s audio is gone", source)
		}
		result, err := retranscribeChunk(ctx, path, opts.Locale)
		if err != nil {
			return fmt.Errorf("re-transcribe %s: %w", source, err)
		}
		// A re-run sees a possibly newer model, and its words may simply differ
		// from the ones indexed. Its timings are usable only while it is saying
		// the same thing; past that the honest move is to keep the stored text and
		// lose the timings, which is exactly the text path.
		if similarity := transcript.Align(
			transcript.Tokenize(track.Text), transcript.Tokenize(result.Text),
		).Similarity(); similarity < minRetranscribeSimilarity {
			return fmt.Errorf("%s re-transcription differs from the indexed transcript (similarity %.2f)",
				source, similarity)
		}
		// The same conversion the recorder's own transcription goes through, so a
		// re-run lands on the values a live capture would have.
		track.Segments = capture.TimedSegmentsFrom(result.Segments)
		// StartedAt stays zero: chunks captured before the recorder reported a
		// first-sample-buffer wall clock have no anchor to recover. Both tracks
		// then share the chunk's own timestamp as a base, which keeps their
		// offsets comparable — the residual writer-start skew was measured at
		// 61 ms, well inside the alignment slack.
	}
	return nil
}

// measureBackfillEnergy takes the reading that separates a silent machine from
// one that played something untranscribable.
//
// Which chunks need it is transcript.NeedsInternalEnergy's rule, not this
// command's: the recorder asks the same question, and a backfill that measured
// where the recorder did not would reach a different verdict for the same audio —
// while reading a 960 KB WAV for every chunk of an afternoon's silence, the
// common case by a wide margin.
func measureBackfillEnergy(chunk *transcript.Chunk, systemPath string) {
	if systemPath == "" || !transcript.NeedsInternalEnergy(*chunk) {
		return
	}
	envelope, _, err := wav.ReadEnvelope(systemPath, transcript.EnvelopeWindowMS)
	if err != nil {
		return
	}
	chunk.System.Envelope = envelope
	chunk.System.EnvelopeWindowMS = transcript.EnvelopeWindowMS
}

// backfillVerdict summarizes why a chunk came out the way it did, for --explain.
func backfillVerdict(method string, chunk transcript.Chunk, segments []transcript.Segment) string {
	similarity := ""
	if chunk.System != nil && chunk.Microphone != nil &&
		chunk.System.Text != "" && chunk.Microphone.Text != "" {
		alignment := transcript.Align(
			transcript.Tokenize(chunk.System.Text), transcript.Tokenize(chunk.Microphone.Text))
		similarity = fmt.Sprintf(" similarity=%.2f", alignment.Similarity())
	}
	counts := map[transcript.Origin]int{}
	bleed := 0
	for _, segment := range segments {
		counts[segment.Origin]++
		if segment.IsBleed {
			bleed++
		}
	}
	return fmt.Sprintf("method=%s%s internal=%d external=%d unknown=%d bleed=%d",
		method, similarity,
		counts[transcript.OriginInternal], counts[transcript.OriginExternal],
		counts[transcript.OriginUnknown], bleed)
}
