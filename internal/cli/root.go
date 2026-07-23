package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/puremetricsai/lumi/internal/capture"
	"github.com/puremetricsai/lumi/internal/cerebras"
	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/platform"
	"github.com/puremetricsai/lumi/internal/retention"
	"github.com/puremetricsai/lumi/internal/store"
	"github.com/spf13/cobra"
)

const version = "0.1.0-dev"

// answerer is the inference backend used by `ask`. It exists so tests can
// exercise the command without a network call; production always uses
// cerebras.Client.
type answerer interface {
	Answer(ctx context.Context, question, activityContext string) (string, error)
}

type app struct {
	dataDir string
	// newAnswerer is overridden in tests. Nil means the real Cerebras client.
	newAnswerer func(model string) answerer
}

func (a *app) answerer(model string) answerer {
	if a.newAnswerer != nil {
		return a.newAnswerer(model)
	}
	return cerebras.Client{APIKey: os.Getenv("CEREBRAS_API_KEY"), Model: model}
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
	cmd.AddCommand(a.recordCommand(), a.searchCommand(), a.askCommand(), a.pruneCommand(),
		a.doctorCommand(), a.permissionsCommand(), a.nativeSmokeCommand())
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

func (a *app) recordCommand() *cobra.Command {
	var interval, audioChunk, duration time.Duration
	var noScreen, noAudio bool
	var speechLocale string
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Continuously capture, process, and index screen and audio activity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if noScreen && noAudio {
				return errors.New("--no-screen and --no-audio cannot be used together")
			}
			if err := requireRecordingPermissions(cmd.Context(), !noScreen, !noAudio, !noAudio); err != nil {
				return err
			}
			s, paths, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			ctx := cmd.Context()
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			if !noAudio {
				logger.Info("ensuring speech recognition assets", "locale", speechLocale)
				if err := macosnative.EnsureSpeechAssets(ctx, speechLocale); err != nil {
					return fmt.Errorf("ensure speech recognition assets for %s: %w", speechLocale, err)
				}
			}
			if duration > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, duration)
				defer cancel()
			}
			recorder := capture.Recorder{
				Store: s, Paths: paths, ScreenInterval: interval, AudioChunk: audioChunk,
				CaptureScreen: !noScreen, CaptureAudio: !noAudio, Logger: logger,
				Screen:      capture.NativeScreens{},
				Text:        capture.VisionText{},
				Context:     capture.AccessibilityContext{},
				Audio:       capture.NativeAudio{},
				Transcriber: capture.NativeSpeech{Locale: speechLocale},
			}
			logger.Info("recording started", "database", paths.Database, "screen", !noScreen, "audio", !noAudio)
			return recorder.Run(ctx)
		},
	}
	flags := cmd.Flags()
	flags.DurationVar(&interval, "interval", 2*time.Second, "screen capture interval")
	flags.DurationVar(&audioChunk, "audio-chunk", 30*time.Second, "audio chunk duration")
	flags.DurationVar(&duration, "duration", 0, "stop after this duration (zero runs until interrupted)")
	flags.BoolVar(&noScreen, "no-screen", false, "disable screen capture and text extraction")
	flags.BoolVar(&noAudio, "no-audio", false, "disable audio capture and transcription")
	flags.StringVar(&speechLocale, "speech-locale", "en-US", "SpeechAnalyzer recognition locale")
	return cmd
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
	flags.IntVar(&limit, "limit", 20, "maximum results")
	flags.BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func (a *app) askCommand() *cobra.Command {
	var since, model, app, window string
	var limit, maxContextChars int
	cmd := &cobra.Command{
		Use:   "ask <question>",
		Short: "Answer a question from local activity using Cerebras",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			opts, err := searchOptions("", "all", since, "", app, window, limit)
			if err != nil {
				return err
			}
			events, stage, err := retrieveContext(cmd.Context(), s, args[0], opts)
			if err != nil {
				return err
			}
			if len(events) == 0 {
				return errors.New("no local activity has been indexed yet")
			}
			// Never answer from a degraded retrieval silently.
			switch stage {
			case stageAnyTerm:
				fmt.Fprintln(cmd.ErrOrStderr(), "note: no events matched every term; ranked by best partial match")
			case stageRecent:
				fmt.Fprintf(cmd.ErrOrStderr(), "note: the question had no searchable terms; using the %d most recent events\n", len(events))
			}
			answer, err := a.answerer(model).Answer(cmd.Context(), args[0], contextFor(events, maxContextChars))
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), answer)
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "24h", "activity window (RFC3339 or duration)")
	cmd.Flags().StringVar(&app, "app", "", "restrict activity to this application")
	cmd.Flags().StringVar(&window, "window", "", "restrict activity to windows whose title contains this text")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum activity records sent to Cerebras")
	cmd.Flags().StringVar(&model, "model", defaultCerebrasModel(), "Cerebras model; set $LUMI_CEREBRAS_MODEL to change the default")
	cmd.Flags().IntVar(&maxContextChars, "max-context-chars", defaultContextChars, "character budget for the activity context sent to Cerebras")
	return cmd
}

func (a *app) pruneCommand() *cobra.Command {
	var olderThan string
	var maxBytes int64
	var dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete old events and their media files",
		Long: "Delete indexed events and the screenshots or audio they point at.\n" +
			"--older-than takes a Go duration (720h) or an RFC3339 timestamp; Go durations have no 'd' unit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := retention.Options{MaxBytes: maxBytes, DryRun: dryRun}
			if olderThan != "" {
				before, err := parseTime(olderThan, true)
				if err != nil {
					return fmt.Errorf("parse --older-than: %w", err)
				}
				opts.Before = before
			}
			s, _, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
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
	flags.BoolVar(&dryRun, "dry-run", false, "report what would be deleted without deleting")
	flags.BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func (a *app) doctorCommand() *cobra.Command {
	var speechLocale string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check platform, capture tools, models, and API configuration",
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
			if os.Getenv("CEREBRAS_API_KEY") == "" {
				fmt.Fprintln(os.Stdout, "Cerebras API key\toptional\tset CEREBRAS_API_KEY to use lumi ask")
			} else {
				fmt.Fprintln(os.Stdout, "Cerebras API key\tok\tconfigured")
			}
			fmt.Fprintf(os.Stdout, "Cerebras model\tok\t%s\n", defaultCerebrasModel())
			fmt.Fprintf(os.Stdout, "data directory\tok\t%s\n", paths.Root)
			if missing {
				return errors.New("one or more recording requirements are missing; run `./lumi permissions --request` for macOS capture permissions")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&speechLocale, "speech-locale", "en-US", "SpeechAnalyzer recognition locale")
	return cmd
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
			if snapshot.App == "" {
				return errors.New("Accessibility smoke test returned no frontmost application")
			}
			audio, err := macosnative.RecordAudio(cmd.Context(), directory, "audio", 0.5)
			if err != nil {
				return err
			}
			sources := make(map[string]bool)
			for _, frame := range audio {
				contents, err := os.ReadFile(frame.Path)
				if err != nil {
					return fmt.Errorf("read %s audio smoke output: %w", frame.Source, err)
				}
				if len(contents) < 12 || string(contents[:4]) != "RIFF" || string(contents[8:12]) != "WAVE" {
					return fmt.Errorf("%s audio smoke output is not a WAV file", frame.Source)
				}
				sources[frame.Source] = true
			}
			if !sources["system"] || !sources["microphone"] {
				return fmt.Errorf("native audio smoke test requires system and microphone outputs, got %v", sources)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"native capture ok: %d displays, Accessibility app %q, Vision ok, system audio ok, microphone ok\n",
				len(frames), snapshot.App)
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

// defaultCerebrasModel resolves the model: an explicit flag beats the
// environment, which beats the built-in default.
func defaultCerebrasModel() string {
	if model := strings.TrimSpace(os.Getenv("LUMI_CEREBRAS_MODEL")); model != "" {
		return model
	}
	return config.DefaultCerebrasModel
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

func printEvents(events []store.Event) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	for _, event := range events {
		text := truncateRunes(strings.Join(strings.Fields(event.Text), " "), 180)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", event.CapturedAt.Local().Format("2006-01-02 15:04:05"), event.Kind, event.App, text)
		fmt.Fprintf(w, "\tmedia\t%s\t\n", event.MediaPath)
	}
}
