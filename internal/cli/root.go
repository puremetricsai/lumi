package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/puremetricsai/lumi/internal/capture"
	"github.com/puremetricsai/lumi/internal/cerebras"
	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/platform"
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
	cmd.AddCommand(a.recordCommand(), a.searchCommand(), a.askCommand(), a.doctorCommand())
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
	var display int
	var audioDevice, ocrLanguage, whisperLanguage, whisperModel string
	var screencaptureBinary, tesseractBinary, ffmpegBinary, whisperBinary string
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Continuously capture, process, and index screen and audio activity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if noScreen && noAudio {
				return errors.New("--no-screen and --no-audio cannot be used together")
			}
			if !noAudio && whisperModel == "" {
				return errors.New("audio transcription requires --whisper-model or LUMI_WHISPER_MODEL (use --no-audio for screen-only capture)")
			}
			s, paths, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			ctx := cmd.Context()
			if duration > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, duration)
				defer cancel()
			}
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			recorder := capture.Recorder{
				Store: s, Paths: paths, ScreenInterval: interval, AudioChunk: audioChunk,
				CaptureScreen: !noScreen, CaptureAudio: !noAudio, Logger: logger,
				Screen:      capture.ScreenCapturer{Binary: screencaptureBinary, Display: display},
				OCR:         capture.OCR{Binary: tesseractBinary, Language: ocrLanguage},
				Audio:       capture.AudioRecorder{Binary: ffmpegBinary, Device: audioDevice},
				Transcriber: capture.Transcriber{Binary: whisperBinary, ModelPath: whisperModel, Language: whisperLanguage},
			}
			logger.Info("recording started", "database", paths.Database, "screen", !noScreen, "audio", !noAudio)
			return recorder.Run(ctx)
		},
	}
	flags := cmd.Flags()
	flags.DurationVar(&interval, "interval", 5*time.Second, "screen capture interval")
	flags.DurationVar(&audioChunk, "audio-chunk", 30*time.Second, "audio chunk duration")
	flags.DurationVar(&duration, "duration", 0, "stop after this duration (zero runs until interrupted)")
	flags.BoolVar(&noScreen, "no-screen", false, "disable screen capture and OCR")
	flags.BoolVar(&noAudio, "no-audio", false, "disable audio capture and transcription")
	flags.IntVar(&display, "display", 1, "macOS display number to capture")
	flags.StringVar(&audioDevice, "audio-device", "0", "FFmpeg AVFoundation audio device index")
	flags.StringVar(&ocrLanguage, "ocr-language", "eng", "Tesseract language")
	flags.StringVar(&whisperLanguage, "whisper-language", "en", "Whisper language")
	flags.StringVar(&whisperModel, "whisper-model", os.Getenv("LUMI_WHISPER_MODEL"), "path to a whisper.cpp model")
	flags.StringVar(&screencaptureBinary, "screencapture-binary", "/usr/sbin/screencapture", "screencapture executable")
	flags.StringVar(&tesseractBinary, "tesseract-binary", "tesseract", "Tesseract executable")
	flags.StringVar(&ffmpegBinary, "ffmpeg-binary", "ffmpeg", "FFmpeg executable")
	flags.StringVar(&whisperBinary, "whisper-binary", "whisper-cli", "whisper.cpp executable")
	return cmd
}

func (a *app) searchCommand() *cobra.Command {
	var kind, since, until, app, window string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "search [text]",
		Short: "Full-text search OCR and audio transcripts",
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

func (a *app) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check platform, capture tools, models, and API configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "platform\tok\tApple Silicon macOS\n")
			missing := false
			for _, binary := range []string{"/usr/sbin/screencapture", "tesseract", "ffmpeg", "whisper-cli"} {
				resolved, lookupErr := exec.LookPath(binary)
				if lookupErr != nil {
					fmt.Fprintf(os.Stdout, "%s\tmissing\t%s\n", filepath.Base(binary), lookupErr)
					missing = true
				} else {
					fmt.Fprintf(os.Stdout, "%s\tok\t%s\n", filepath.Base(binary), resolved)
				}
			}
			if model := os.Getenv("LUMI_WHISPER_MODEL"); model == "" {
				fmt.Fprintln(os.Stdout, "whisper model\tmissing\tset LUMI_WHISPER_MODEL or pass --whisper-model")
				missing = true
			} else if _, statErr := os.Stat(model); statErr != nil {
				fmt.Fprintf(os.Stdout, "whisper model\tmissing\t%v\n", statErr)
				missing = true
			} else {
				fmt.Fprintf(os.Stdout, "whisper model\tok\t%s\n", model)
			}
			if os.Getenv("CEREBRAS_API_KEY") == "" {
				fmt.Fprintln(os.Stdout, "Cerebras API key\toptional\tset CEREBRAS_API_KEY to use lumi ask")
			} else {
				fmt.Fprintln(os.Stdout, "Cerebras API key\tok\tconfigured")
			}
			fmt.Fprintf(os.Stdout, "Cerebras model\tok\t%s\n", defaultCerebrasModel())
			fmt.Fprintf(os.Stdout, "data directory\tok\t%s\n", paths.Root)
			if missing {
				return errors.New("one or more recording requirements are missing")
			}
			return nil
		},
	}
}

// defaultCerebrasModel resolves the model the same way --whisper-model
// resolves LUMI_WHISPER_MODEL: an explicit flag beats the environment, which
// beats the built-in default.
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
