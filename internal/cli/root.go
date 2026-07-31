package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/puremetricsai/lumi/internal/capture"
	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/platform"
	"github.com/puremetricsai/lumi/internal/retention"
	"github.com/puremetricsai/lumi/internal/store"
	"github.com/puremetricsai/lumi/internal/vocabulary"
	"github.com/spf13/cobra"
)

const version = "0.1.0-dev"

type app struct {
	dataDir string
}

func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return newRootCommand().ExecuteContext(ctx)
}

func newRootCommand() *cobra.Command {
	a := &app{}
	cmd := &cobra.Command{
		Use:           "lumi",
		Short:         "Searchable, local-first work activity for Apple Silicon Macs",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return platform.Validate()
		},
	}
	cmd.PersistentFlags().StringVar(&a.dataDir, "data-dir", "", "data directory (default: $LUMI_HOME or ~/Library/Application Support/Lumi)")
	cmd.AddCommand(a.recordCommand(), a.searchCommand(), a.transcriptCommand(), a.pruneCommand(),
		a.doctorCommand(), a.permissionsCommand(), a.nativeSmokeCommand(), a.mcpCommand(),
		a.transcribeCommand())
	cmd.AddCommand(&cobra.Command{Use: "version", Short: "Print the Lumi version", Run: func(*cobra.Command, []string) {
		fmt.Fprintln(os.Stdout, version)
	}})
	return cmd
}

func (a *app) paths() (config.Paths, error) {
	if a.dataDir != "" {
		return config.FromRoot(a.dataDir)
	}
	return config.DefaultPaths()
}

// smokeAccessibilityNote reports a degraded Accessibility read without failing
// the smoke test: attribution succeeded through the fallback, which is the
// property being asserted, but the operator should still see the cause.
func smokeAccessibilityNote(accessibilityError string) string {
	if accessibilityError == "" {
		return ""
	}
	return ", degraded: " + accessibilityError
}

// smokeAudioOutputNote renders the processes holding an active audio output
// stream. "none" and a failed read are kept distinct: the first says no process
// held a stream, the second says Lumi cannot tell — and only the second is a
// reason to distrust the audio attribution the recorder will write.
func smokeAudioOutputNote(processes []macosnative.AudioProcess, err error) string {
	if err != nil {
		return "unavailable: " + err.Error()
	}
	if len(processes) == 0 {
		return "none"
	}
	named := make([]string, 0, len(processes))
	for _, process := range processes {
		name := process.Name
		if name == "" {
			name = process.BundleID
		}
		if name == "" {
			name = "unnamed"
		}
		named = append(named, fmt.Sprintf("%s (pid=%d)", name, process.PID))
	}
	return strings.Join(named, ", ")
}

func (a *app) openStore(ctx context.Context) (*store.Store, config.Paths, error) {
	paths, err := a.paths()
	if err != nil {
		return nil, paths, err
	}
	if err := paths.Ensure(); err != nil {
		return nil, paths, err
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		return nil, paths, err
	}
	return s, paths, nil
}

// recordCommand is a parent that only holds the start/status/stop
// subcommands; with no RunE, cobra prints its help when invoked bare.
func (a *app) recordCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Control the background recorder (start, status, stop)",
		Long: "Manage the background recorder that captures, processes, and indexes screen\n" +
			"and audio activity. `record start` launches it in the background; `record\n" +
			"status` reports its state; `record stop` shuts it down gracefully.",
	}
	cmd.AddCommand(a.recordStartCommand(), a.recordStatusCommand(), a.recordStopCommand())
	return cmd
}

// recordFlags holds the capture flags shared by `record start`. In background
// mode they are forwarded verbatim to the detached `--foreground` child.
type recordFlags struct {
	interval, audioChunk, duration time.Duration
	noScreen, noAudio              bool
	speechLocale                   string
}

func (f *recordFlags) bind(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.DurationVar(&f.interval, "interval", 2*time.Second, "screen capture interval")
	flags.DurationVar(&f.audioChunk, "audio-chunk", 30*time.Second, "audio chunk duration")
	flags.DurationVar(&f.duration, "duration", 0, "stop after this duration (zero runs until interrupted)")
	flags.BoolVar(&f.noScreen, "no-screen", false, "disable screen capture and text extraction")
	flags.BoolVar(&f.noAudio, "no-audio", false, "disable audio capture and transcription")
	flags.StringVar(&f.speechLocale, "speech-locale", "en-US", "SpeechAnalyzer recognition locale")
}

// runForeground runs the capture pipeline in the current process until the
// context is cancelled (signal or --duration). This is the worker that the
// detached child executes.
func (a *app) runForeground(cmd *cobra.Command, f recordFlags) error {
	if f.noScreen && f.noAudio {
		return errors.New("--no-screen and --no-audio cannot be used together")
	}
	if err := requireRecordingPermissions(cmd.Context(), !f.noScreen, !f.noAudio, !f.noAudio); err != nil {
		return err
	}
	s, paths, err := a.openStore(cmd.Context())
	if err != nil {
		return err
	}
	defer s.Close()
	ctx := cmd.Context()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if !f.noAudio {
		logger.Info("ensuring speech recognition assets", "locale", f.speechLocale)
		if err := macosnative.EnsureSpeechAssets(ctx, f.speechLocale); err != nil {
			return fmt.Errorf("ensure speech recognition assets for %s: %w", f.speechLocale, err)
		}
	}
	if f.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.duration)
		defer cancel()
	}
	recorder := capture.Recorder{
		Store: s, Paths: paths, ScreenInterval: f.interval, AudioChunk: f.audioChunk,
		CaptureScreen: !f.noScreen, CaptureAudio: !f.noAudio, Logger: logger,
		Screen:  capture.NativeScreens{},
		Text:    capture.VisionText{},
		Context: capture.AccessibilityContext{},
		Audio:   capture.NativeAudio{},
		// Both audio-source seams are always wired. Leaving either nil changes what
		// an absent source list *means* in every row written — "no source was
		// found" rather than "no source was looked for" — and nothing downstream
		// can tell those apart afterwards.
		AudioOutputs: capture.NativeAudioOutputs{},
		AudioMarkers: capture.NativeAudioMarkers{},
		Transcriber: capture.NativeSpeech{
			Locale:     f.speechLocale,
			Vocabulary: &vocabulary.Loader{Path: paths.Vocabulary},
			Logger:     logger,
		},
	}
	logger.Info("recording started", "database", paths.Database, "screen", !f.noScreen, "audio", !f.noAudio)
	return recorder.Run(ctx)
}

func (a *app) searchCommand() *cobra.Command {
	var kind, since, until, app, window string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "search [text]",
		Short: "Full-text search screen text and audio transcripts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			opts, err := searchOptions(query, kind, since, until, app, window, limit)
			if err != nil {
				return err
			}
			s, _, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			events, err := s.Search(cmd.Context(), opts)
			if err != nil {
				return err
			}
			// Both tracks of an audio chunk are separate rows and stay that way:
			// `lumi search --json` is a faithful export of what was stored, and
			// nothing here may decide that two rows sharing a captured_at
			// recorded the same sound. See internal/mcp's audioProvenanceContract.
			if asJSON {
				encoder := json.NewEncoder(os.Stdout)
				encoder.SetIndent("", "  ")
				return encoder.Encode(events)
			}
			printEvents(events)
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&kind, "type", "all", "content type: all, screen, or audio")
	flags.StringVar(&since, "since", "", "earliest time (RFC3339 or duration such as 8h)")
	flags.StringVar(&until, "until", "", "latest time (RFC3339)")
	flags.StringVar(&app, "app", "", "only events captured from this application (exact, case-insensitive)")
	flags.StringVar(&window, "window", "", "only events whose window title contains this text")
	flags.IntVar(&limit, "limit", store.DefaultSearchLimit, "maximum results")
	flags.BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func (a *app) pruneCommand() *cobra.Command {
	var olderThan string
	var maxBytes int64
	var dryRun, asJSON, all, yes bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete old events and their media files",
		Long: "Delete indexed events and the screenshots or audio they point at.\n" +
			"--older-than takes a Go duration (720h) or an RFC3339 timestamp; Go durations have no 'd' unit.\n" +
			"--all wipes every indexed event and all media; it prompts for confirmation unless --yes or --dry-run is set.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := retention.Options{MaxBytes: maxBytes, DryRun: dryRun, All: all}
			if olderThan != "" {
				before, err := parseTime(olderThan, true)
				if err != nil {
					return fmt.Errorf("parse --older-than: %w", err)
				}
				opts.Before = before
			}
			// --all deletes everything irreversibly, so require an explicit
			// confirmation. A dry run deletes nothing, and --yes is the escape
			// hatch for scripts.
			if all && !dryRun && !yes {
				// Confirmation banner, prompt, and abort notice are interactive
				// text for a human, not command results, so they go to stderr;
				// stdout stays reserved for the JSON/summary result (which would
				// otherwise be corrupted under --json).
				confirmed, err := confirmPruneAll(cmd.InOrStdin(), cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(cmd.ErrOrStderr(), "aborted; nothing was deleted")
					return nil
				}
			}
			s, paths, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			// --all sweeps orphaned media (files no row references) from the
			// media directories; age/size policies ignore MediaDirs.
			opts.MediaDirs = []string{paths.Screenshots, paths.Audio}
			result, err := retention.Prune(cmd.Context(), s, opts)
			if err != nil {
				return err
			}
			if asJSON {
				encoder := json.NewEncoder(os.Stdout)
				encoder.SetIndent("", "  ")
				return encoder.Encode(result)
			}
			verb := "deleted"
			if dryRun {
				verb = "would delete"
			}
			fmt.Fprintf(os.Stdout, "%s %d events, %.1f MiB of media\n",
				verb, result.Events, float64(result.Bytes)/(1024*1024))
			if result.MissingFiles > 0 {
				fmt.Fprintf(os.Stdout, "%d events referenced media that was already gone\n", result.MissingFiles)
			}
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&olderThan, "older-than", "", "delete events older than this duration (e.g. 720h) or RFC3339 time")
	flags.Int64Var(&maxBytes, "max-bytes", 0, "cap total media size in bytes, deleting oldest first (zero disables)")
	flags.BoolVar(&all, "all", false, "delete every indexed event and all media files (prompts to confirm)")
	flags.BoolVarP(&yes, "yes", "y", false, "skip the --all confirmation prompt (for scripts)")
	flags.BoolVar(&dryRun, "dry-run", false, "report what would be deleted without deleting")
	flags.BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

// confirmPruneAll asks the operator to type "yes" before an irreversible
// `prune --all`. It reads a single line from in and returns whether the answer
// was an unambiguous yes.
func confirmPruneAll(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprintln(out, "This will permanently delete ALL indexed events and every screenshot and")
	fmt.Fprintln(out, "audio file Lumi has captured. This cannot be undone.")
	fmt.Fprint(out, "Type 'yes' to continue: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes"), nil
}

func (a *app) doctorCommand() *cobra.Command {
	var speechLocale string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check platform, capture permissions, speech assets, and the data directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "platform\tok\tApple Silicon macOS\n")
			missing := false
			permissions, permissionErr := macosnative.PermissionStatus(cmd.Context())
			if permissionErr != nil {
				fmt.Fprintf(os.Stdout, "native capture\tmissing\t%v\n", permissionErr)
				missing = true
			} else {
				for _, permission := range []struct{ name, status, settings string }{
					{"Screen Recording", permissions.ScreenRecording, "Privacy & Security > Screen & System Audio Recording"},
					{"Accessibility", permissions.Accessibility, "Privacy & Security > Accessibility"},
					{"Input Monitoring", permissions.InputMonitoring, "optional; only needed by future event-tap capture"},
					{"Microphone", permissions.Microphone, "Privacy & Security > Microphone"},
					{"Speech Recognition", permissions.SpeechRecognition, "Privacy & Security > Speech Recognition"},
				} {
					state := "ok"
					if permission.status != "granted" {
						state = "missing"
						if permission.name == "Input Monitoring" {
							state = "optional"
						} else {
							missing = true
						}
					}
					fmt.Fprintf(os.Stdout, "%s permission\t%s\t%s (%s)\n", permission.name, state, permission.status, permission.settings)
				}
			}
			assetsInstalled, speechErr := macosnative.SpeechAssetsInstalled(cmd.Context(), speechLocale)
			if speechErr != nil {
				fmt.Fprintf(os.Stdout, "speech assets (%s)\tmissing\t%v\n", speechLocale, speechErr)
				missing = true
			} else if assetsInstalled {
				fmt.Fprintf(os.Stdout, "speech assets (%s)\tok\tinstalled\n", speechLocale)
			} else {
				fmt.Fprintf(os.Stdout, "speech assets (%s)\tmissing\tlocale assets not installed; `./lumi record` downloads them at startup\n", speechLocale)
				missing = true
			}
			fmt.Fprintf(os.Stdout, "data directory\tok\t%s\n", paths.Root)
			reportVocabulary(os.Stdout, paths)
			if err := reportAttributionHealth(cmd.Context(), os.Stdout, paths); err != nil {
				return err
			}
			if missing {
				return errors.New("one or more recording requirements are missing; run `./lumi permissions --request` for macOS capture permissions")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&speechLocale, "speech-locale", "en-US", "SpeechAnalyzer recognition locale")
	return cmd
}

// attributionWindow is how far back doctor measures observed attribution. It is
// short on purpose: the question is "is capture healthy right now", not "has it
// ever been".
const attributionWindow = time.Hour

// reportAttributionHealth reports what the index observed, which is the check a
// TCC status cannot make: permission is read from this fresh process, while the
// recorder that has been failing all day is a different one.
//
// It deliberately does not use openStore. That would call paths.Ensure and
// store.Open, so a mistyped --data-dir would be created empty and then reported
// as healthy. A missing database is a finding, not something to fix by making one.
//
// The result is never `missing`: doctor exits non-zero on missing, and a
// degraded history is a diagnosis, not an unmet requirement.
func reportAttributionHealth(ctx context.Context, out io.Writer, paths config.Paths) error {
	if _, err := os.Stat(paths.Database); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "attribution\tok\tno screen history at %s\n", paths.Database)
			return nil
		}
		return fmt.Errorf("stat index: %w", err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		return err
	}
	defer s.Close()
	report, err := s.AttributionHealth(ctx, time.Now().UTC().Add(-attributionWindow))
	if err != nil {
		return err
	}
	if report.Total == 0 {
		fmt.Fprintf(out, "attribution\tok\tno screen events in the last hour\n")
		return nil
	}
	if report.Unattributed == 0 {
		fmt.Fprintf(out, "attribution\tok\tall %d screen events in the last hour have an app\n", report.Total)
		return nil
	}
	last := "never"
	if report.HasLastAttributed {
		last = report.LastAttributed.Format(time.RFC3339)
	}
	// Counts lead, and the percentage never rounds a real gap down to zero: at
	// the default two-second interval an hour holds thousands of events, so
	// integer division would report "warn 0% have no app" for a genuine outage.
	fmt.Fprintf(out, "attribution\twarn\t%d of %d screen events in the last hour have no app (%s; last attributed %s)\n",
		report.Unattributed, report.Total, formatPercent(report.Unattributed, report.Total), last)
	return nil
}

// formatPercent renders a non-zero share as at least "<1%", never as "0%".
func formatPercent(part, total int64) string {
	if total <= 0 {
		return "0%"
	}
	percent := float64(part) * 100 / float64(total)
	if percent < 1 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", percent)
}

// reportVocabulary reports the vocabulary file's state. The three cases are
// kept distinct because they need different remedies: "no file" and "file I
// cannot read" have opposite fixes, and collapsing them would send a user
// looking for a missing file that is actually sitting there unreadable.
//
// This never affects doctor's exit status: vocabulary is optional.
//
// Like the rest of doctor, it describes what it can observe from this process.
// A running recorder holds its own cache, so this is the file now, not the
// daemon's current terms.
func reportVocabulary(out io.Writer, paths config.Paths) {
	snapshot := (&vocabulary.Loader{Path: paths.Vocabulary}).Load()
	switch {
	case snapshot.Err != nil:
		fmt.Fprintf(out, "vocabulary\twarn\t%v\n", snapshot.Err)
	case !snapshot.Exists:
		fmt.Fprintf(out, "vocabulary\tnone\tno vocabulary file at %s\n", paths.Vocabulary)
	case snapshot.Dropped > 0:
		fmt.Fprintf(out, "vocabulary\tok\t%d terms (%d dropped past the %d-term cap)\n",
			len(snapshot.Terms), snapshot.Dropped, vocabulary.MaxTerms)
	default:
		fmt.Fprintf(out, "vocabulary\tok\t%d terms\n", len(snapshot.Terms))
	}
}

func (a *app) permissionsCommand() *cobra.Command {
	var request, inputMonitoring bool
	cmd := &cobra.Command{
		Use:   "permissions",
		Short: "Show or request native macOS capture permissions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var permissions macosnative.Permissions
			var err error
			if request {
				permissions, err = macosnative.RequestPermissions(cmd.Context(), inputMonitoring)
			} else {
				permissions, err = macosnative.PermissionStatus(cmd.Context())
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Screen Recording\t%s\n", permissions.ScreenRecording)
			fmt.Fprintf(cmd.OutOrStdout(), "Accessibility\t%s\n", permissions.Accessibility)
			fmt.Fprintf(cmd.OutOrStdout(), "Input Monitoring\t%s\n", permissions.InputMonitoring)
			fmt.Fprintf(cmd.OutOrStdout(), "Microphone\t%s\n", permissions.Microphone)
			fmt.Fprintf(cmd.OutOrStdout(), "Speech Recognition\t%s\n", permissions.SpeechRecognition)
			if request && (permissions.ScreenRecording != "granted" ||
				permissions.Accessibility != "granted" || permissions.Microphone != "granted" ||
				permissions.SpeechRecognition != "granted") {
				return errors.New("finish approving Lumi in System Settings, then restart the command")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&request, "request", false, "open the native macOS permission request flows")
	cmd.Flags().BoolVar(&inputMonitoring, "input-monitoring", false,
		"also request optional Input Monitoring permission")
	return cmd
}

func (a *app) nativeSmokeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "native-smoke",
		Short: "Run a bounded native capture test without indexing media",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRecordingPermissions(cmd.Context(), true, true, false); err != nil {
				return err
			}
			directory, err := os.MkdirTemp("", "lumi-native-smoke-")
			if err != nil {
				return fmt.Errorf("create native smoke directory: %w", err)
			}
			defer os.RemoveAll(directory)

			frames, err := macosnative.CaptureScreens(cmd.Context(), directory, "screen")
			if err != nil {
				return err
			}
			if _, err := macosnative.RecognizeText(cmd.Context(), frames[0].Path); err != nil {
				return fmt.Errorf("run Vision smoke test: %w", err)
			}
			snapshot, err := macosnative.Accessibility(cmd.Context())
			if err != nil {
				return fmt.Errorf("run Accessibility smoke test: %w", err)
			}
			// snapshot.App alone only proves the NSWorkspace path, which needs
			// no grant at all. The attribution guarantee is that a title source
			// is always resolved, degraded or not.
			if snapshot.App == "" {
				return errors.New("Accessibility smoke test returned no frontmost application")
			}
			if snapshot.TitleSource == "" {
				return errors.New("Accessibility smoke test resolved no title source")
			}
			if snapshot.TitleSource != "none" && snapshot.Window == "" {
				return fmt.Errorf("Accessibility smoke test reported title source %q with an empty window",
					snapshot.TitleSource)
			}
			if snapshot.AppSource == "" {
				return errors.New("Accessibility smoke test resolved no app source")
			}
			frontmost, err := macosnative.FrontmostDiagnostic(cmd.Context())
			if err != nil {
				return fmt.Errorf("run frontmost diagnostic: %w", err)
			}
			// The diagnostic is reported, never asserted against the snapshot
			// above. They are separate native calls, so a focus change between
			// them makes them differ for an entirely legitimate reason — which
			// would fail this smoke test intermittently while proving nothing.
			// That they share a resolver is guaranteed by construction, and is
			// pinned by the unit tests over lumi_resolve_frontmost_json.
			audio, err := macosnative.RecordAudio(cmd.Context(), directory, "audio", 0.5)
			if err != nil {
				return err
			}
			sources := make(map[string]bool)
			anchors := make([]string, 0, len(audio))
			for _, frame := range audio {
				contents, err := os.ReadFile(frame.Path)
				if err != nil {
					return fmt.Errorf("read %s audio smoke output: %w", frame.Source, err)
				}
				if len(contents) < 12 || string(contents[:4]) != "RIFF" || string(contents[8:12]) != "WAVE" {
					return fmt.Errorf("%s audio smoke output is not a WAV file", frame.Source)
				}
				sources[frame.Source] = true
				// A track that wrote a file but reported no start anchor leaves
				// its timings unplaceable against the other track's, so this is
				// a failure rather than a note. It is also the only place the
				// ObjC writer change is exercisable by hand.
				if frame.StartedAtUnixNS == 0 {
					return fmt.Errorf("%s audio smoke output carries no start anchor", frame.Source)
				}
				anchors = append(anchors, fmt.Sprintf("%s started=%s pts=%dns measured=%dms",
					frame.Source,
					time.Unix(0, frame.StartedAtUnixNS).UTC().Format(time.RFC3339Nano),
					frame.SessionStartPTSNS, frame.MeasuredDurationMS))
			}
			if !sources["system"] || !sources["microphone"] {
				return fmt.Errorf("native audio smoke test requires system and microphone outputs, got %v", sources)
			}
			trusted := "unknown"
			if snapshot.Trusted != nil {
				trusted = strconv.FormatBool(*snapshot.Trusted)
			}
			agreement := "disagree"
			if frontmost.Agree {
				agreement = "agree"
			}
			// The output-stream list is reported and never asserted. An empty set
			// is the correct answer when nothing holds a stream, so failing on it
			// would make the smoke test depend on whether the operator happened to
			// have audio running — the same reasoning that keeps the frontmost
			// diagnostic above reported rather than compared.
			audioOutputs, audioOutputsErr := macosnative.AudioProcesses(cmd.Context())
			fmt.Fprintf(cmd.OutOrStdout(),
				"native capture ok: %d displays, app %q via %s, window %q via %s (AX trusted=%s%s), "+
					"Vision ok, system audio ok, microphone ok\n"+
					"frontmost: Accessibility pid=%d %q | NSWorkspace pid=%d %q | "+
					"window list pid=%d %q | resolved %q via %s (%s)\n"+
					"audio timing: %s\n"+
					"active audio output: %s\n",
				len(frames), snapshot.App, snapshot.AppSource, snapshot.Window, snapshot.TitleSource,
				trusted, smokeAccessibilityNote(snapshot.Error),
				frontmost.Accessibility.PID, frontmost.Accessibility.App,
				frontmost.Workspace.PID, frontmost.Workspace.App,
				frontmost.WindowList.PID, frontmost.WindowList.App,
				frontmost.Resolved.App, frontmost.Resolved.AppSource, agreement,
				strings.Join(anchors, " | "),
				smokeAudioOutputNote(audioOutputs, audioOutputsErr))
			return nil
		},
	}
}

func requireRecordingPermissions(ctx context.Context, screen, audio, speech bool) error {
	permissions, err := macosnative.PermissionStatus(ctx)
	if err != nil {
		return fmt.Errorf("check native macOS permissions: %w", err)
	}
	var missing []string
	if (screen || audio) && permissions.ScreenRecording != "granted" {
		missing = append(missing, "Screen Recording (System Settings > Privacy & Security > Screen & System Audio Recording)")
	}
	if screen && permissions.Accessibility != "granted" {
		missing = append(missing, "Accessibility (System Settings > Privacy & Security > Accessibility)")
	}
	if audio && permissions.Microphone != "granted" {
		missing = append(missing, "Microphone (System Settings > Privacy & Security > Microphone)")
	}
	if speech && permissions.SpeechRecognition != "granted" {
		missing = append(missing, "Speech Recognition (System Settings > Privacy & Security > Speech Recognition)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("grant required macOS permissions to this terminal or the Lumi binary: %s; run `./lumi permissions --request`", strings.Join(missing, "; "))
	}
	return nil
}

func searchOptions(query, kind, since, until, app, window string, limit int) (store.SearchOptions, error) {
	opts := store.SearchOptions{Query: query, App: app, Window: window, Limit: limit}
	switch kind {
	case "", "all":
	case "screen":
		opts.Kind = store.KindScreen
	case "audio":
		opts.Kind = store.KindAudio
	default:
		return opts, fmt.Errorf("invalid content type %q (want all, screen, or audio)", kind)
	}
	var err error
	if since != "" {
		opts.Since, err = parseTime(since, true)
		if err != nil {
			return opts, fmt.Errorf("parse --since: %w", err)
		}
	}
	if until != "" {
		opts.Until, err = parseTime(until, false)
		if err != nil {
			return opts, fmt.Errorf("parse --until: %w", err)
		}
	}
	return opts, nil
}

func parseTime(value string, allowDuration bool) (*time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return &parsed, nil
	}
	if allowDuration {
		if duration, err := time.ParseDuration(value); err == nil {
			parsed := time.Now().Add(-duration)
			return &parsed, nil
		}
	}
	return nil, fmt.Errorf("%q is not RFC3339%s", value, map[bool]string{true: " or a duration", false: ""}[allowDuration])
}

const ellipsis = "…"

// truncateRunes limits s to max bytes without ever splitting a rune,
// appending an ellipsis when it had to cut.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	limit := max - len(ellipsis)
	if limit <= 0 {
		// No room for the marker; return whole runes only.
		return trimToRunes(s, max)
	}
	return trimToRunes(s, limit) + ellipsis
}

func trimToRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func printEvents(events []store.Event) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	for _, event := range events {
		text := truncateRunes(strings.Join(strings.Fields(event.Text), " "), 180)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", event.CapturedAt.Local().Format("2006-01-02 15:04:05"), event.Kind, event.App, text)
		fmt.Fprintf(w, "\tmedia\t%s\t\n", event.MediaPath)
	}
}
