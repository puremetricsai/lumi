package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/store"
)

type Recorder struct {
	Store          *store.Store
	Paths          config.Paths
	Screen         ScreenSource
	Text           TextExtractor
	Context        ContextExtractor
	Audio          AudioSource
	Transcriber    SpeechTranscriber
	Comparer       *FrameComparer
	ScreenInterval time.Duration
	AudioChunk     time.Duration
	CaptureScreen  bool
	CaptureAudio   bool
	Logger         *slog.Logger
}

type SpeechTranscriber interface {
	Transcribe(context.Context, string) (string, error)
}

func (r *Recorder) Run(ctx context.Context) error {
	if r.Store == nil {
		return errors.New("recorder store is required")
	}
	if !r.CaptureScreen && !r.CaptureAudio {
		return errors.New("at least one of screen or audio capture must be enabled")
	}
	if r.CaptureScreen && (r.Screen == nil || r.Text == nil) {
		return errors.New("screen source and fallback text extractor are required")
	}
	if r.CaptureAudio && (r.Audio == nil || r.Transcriber == nil) {
		return errors.New("audio source and transcriber are required")
	}
	if r.ScreenInterval <= 0 {
		r.ScreenInterval = 2 * time.Second
	}
	if r.AudioChunk <= 0 {
		r.AudioChunk = 30 * time.Second
	}
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.Comparer == nil {
		r.Comparer = &FrameComparer{}
	}
	if err := r.Paths.Ensure(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	if r.CaptureScreen {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.screenLoop(ctx)
		}()
	}
	if r.CaptureAudio {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.audioLoop(ctx)
		}()
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (r *Recorder) screenLoop(ctx context.Context) {
	r.captureScreen(ctx)
	ticker := time.NewTicker(r.ScreenInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.captureScreen(ctx)
		}
	}
}

func (r *Recorder) captureScreen(ctx context.Context) {
	now := time.Now().UTC()
	frames, err := r.Screen.Capture(ctx, r.Paths.Screenshots, fileStamp(now))
	if err != nil {
		if ctx.Err() == nil {
			r.Logger.Error("screen capture failed", "error", err)
		}
		return
	}
	var screenContext ScreenContext
	var contextErr error
	if r.Context != nil {
		processingCtx, cancel := preservationContext(ctx)
		screenContext, contextErr = r.Context.Snapshot(processingCtx)
		cancel()
		if contextErr != nil {
			r.Logger.Warn("Accessibility extraction failed; using Vision fallback", "error", contextErr)
		}
	}
	for _, frame := range frames {
		duplicate, similarity, compareErr := r.Comparer.Duplicate(
			frame.Path, frame.DisplayID, screenContext.InputActive, now)
		if compareErr != nil {
			r.Logger.Warn("frame comparison failed; preserving frame", "path", frame.Path, "error", compareErr)
		}
		if duplicate {
			if err := os.Remove(frame.Path); err != nil {
				r.Logger.Warn("remove duplicate screenshot", "path", frame.Path, "error", err)
			}
			continue
		}
		text := ""
		textSource := "accessibility"
		var processErr error
		if contextErr == nil && screenContext.Text != "" &&
			(screenContext.DisplayID == 0 || screenContext.DisplayID == frame.DisplayID) {
			text = screenContext.Text
		} else {
			textSource = "vision"
			processingCtx, cancel := preservationContext(ctx)
			text, processErr = r.Text.Extract(processingCtx, frame.Path)
			cancel()
		}
		metadata := screenMetadata(frame, textSource, similarity, processErr, contextErr, compareErr)
		event := &store.Event{Kind: store.KindScreen, CapturedAt: now, Text: text,
			App: screenContext.App, Window: screenContext.Window, MediaPath: frame.Path,
			TextSource: textSource, DisplayID: frame.DisplayID, Metadata: metadata}
		storeCtx, cancel := preservationContext(ctx)
		err := r.Store.Insert(storeCtx, event)
		cancel()
		if err != nil {
			r.Logger.Error("store screen event", "path", frame.Path, "error", err)
			continue
		}
		r.Logger.Info("captured screen", "id", event.ID, "display", frame.DisplayID,
			"app", screenContext.App, "text_source", textSource, "characters", len(text))
		if processErr != nil {
			r.Logger.Warn("Vision failed; screenshot was still indexed", "error", processErr)
		}
	}
}

func screenMetadata(frame ScreenFrame, textSource string, similarity float64, processErr, contextErr, compareErr error) json.RawMessage {
	metadata := map[string]any{
		"display_id":       frame.DisplayID,
		"width":            frame.Width,
		"height":           frame.Height,
		"text_source":      textSource,
		"frame_similarity": similarity,
	}
	if processErr != nil {
		metadata["processor_error"] = processErr.Error()
	}
	if contextErr != nil {
		metadata["accessibility_error"] = contextErr.Error()
	}
	if compareErr != nil {
		metadata["comparison_error"] = compareErr.Error()
	}
	if frame.CaptureError != "" {
		metadata["capture_error"] = frame.CaptureError
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return processorMetadata(err)
	}
	return data
}

func (r *Recorder) audioLoop(ctx context.Context) {
	for ctx.Err() == nil {
		now := time.Now().UTC()
		frames, err := r.Audio.Record(ctx, r.Paths.Audio, fileStamp(now), r.AudioChunk)
		if err != nil {
			if ctx.Err() == nil {
				r.Logger.Error("audio capture failed", "error", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
			continue
		}
		for _, frame := range frames {
			text := ""
			var processErr error
			if ctx.Err() != nil {
				processErr = fmt.Errorf("transcription skipped after capture stopped: %w", ctx.Err())
			} else {
				text, processErr = r.Transcriber.Transcribe(ctx, frame.Path)
			}
			event := &store.Event{Kind: store.KindAudio, CapturedAt: now, Text: text, MediaPath: frame.Path,
				DurationMS: frame.DurationMS, AudioSource: frame.Source,
				Metadata: audioMetadata(frame.Source, frame.CaptureError, processErr)}
			storeCtx, cancel := preservationContext(ctx)
			err := r.Store.Insert(storeCtx, event)
			cancel()
			if err != nil {
				r.Logger.Error("store audio event", "path", frame.Path, "error", err)
				continue
			}
			r.Logger.Info("captured audio", "id", event.ID, "source", frame.Source, "characters", len(text))
			if processErr != nil {
				r.Logger.Warn("transcription failed; audio was still indexed", "source", frame.Source, "error", processErr)
			}
		}
	}
}

// preservationContext gives already-written media a short, cancellation-free
// window to finish processor diagnostics and its database insert. Native
// ScreenCaptureKit calls are synchronous at this boundary and can return media
// just after the recording context is canceled; abandoning that media would
// create an unindexed orphan.
func preservationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func audioMetadata(source, captureError string, processErr error) json.RawMessage {
	metadata := map[string]string{"audio_source": source}
	if captureError != "" {
		metadata["capture_error"] = captureError
	}
	if processErr != nil {
		metadata["processor_error"] = processErr.Error()
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return processorMetadata(err)
	}
	return data
}

func processorMetadata(err error) json.RawMessage {
	if err == nil {
		return json.RawMessage(`{}`)
	}
	data, marshalErr := json.Marshal(map[string]string{"processor_error": err.Error()})
	if marshalErr != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

func fileStamp(t time.Time) string {
	return fmt.Sprintf("%s-%06d", t.Format("20060102T150405Z"), t.Nanosecond()/1000)
}
