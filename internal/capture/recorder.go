package capture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/store"
)

type Recorder struct {
	Store          *store.Store
	Paths          config.Paths
	Screen         ScreenSource
	OCR            TextExtractor
	Audio          AudioSource
	Transcriber    SpeechTranscriber
	WindowContext  func(context.Context) (string, string)
	ScreenInterval time.Duration
	AudioChunk     time.Duration
	CaptureScreen  bool
	CaptureAudio   bool
	Logger         *slog.Logger

	lastScreenHash [sha256.Size]byte
	haveScreenHash bool
	mu             sync.Mutex
}

type ScreenSource interface {
	Capture(context.Context, string) error
}

type TextExtractor interface {
	Extract(context.Context, string) (string, error)
}

type AudioSource interface {
	Record(context.Context, string, time.Duration) error
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
	if r.CaptureScreen && (r.Screen == nil || r.OCR == nil) {
		return errors.New("screen source and OCR processor are required")
	}
	if r.CaptureAudio && (r.Audio == nil || r.Transcriber == nil) {
		return errors.New("audio source and transcriber are required")
	}
	if r.ScreenInterval <= 0 {
		r.ScreenInterval = 5 * time.Second
	}
	if r.AudioChunk <= 0 {
		r.AudioChunk = 30 * time.Second
	}
	if r.Logger == nil {
		r.Logger = slog.Default()
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
	path := filepath.Join(r.Paths.Screenshots, fileStamp(now)+".jpg")
	if err := r.Screen.Capture(ctx, path); err != nil {
		if ctx.Err() == nil {
			r.Logger.Error("screen capture failed", "error", err)
		}
		return
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		r.Logger.Error("read screenshot", "error", err)
		return
	}
	hash := sha256.Sum256(contents)
	r.mu.Lock()
	duplicate := r.haveScreenHash && hash == r.lastScreenHash
	if !duplicate {
		r.lastScreenHash = hash
		r.haveScreenHash = true
	}
	r.mu.Unlock()
	if duplicate {
		if err := os.Remove(path); err != nil {
			r.Logger.Warn("remove duplicate screenshot", "error", err)
		}
		return
	}
	windowContext := r.WindowContext
	if windowContext == nil {
		windowContext = FrontmostWindow
	}
	app, window := windowContext(ctx)
	text, processErr := r.OCR.Extract(ctx, path)
	metadata := processorMetadata(processErr)
	event := &store.Event{Kind: store.KindScreen, CapturedAt: now, Text: text, App: app,
		Window: window, MediaPath: path, Metadata: metadata}
	if err := r.Store.Insert(ctx, event); err != nil {
		r.Logger.Error("store screen event", "error", err)
		return
	}
	r.Logger.Info("captured screen", "id", event.ID, "app", app, "characters", len(text))
	if processErr != nil {
		r.Logger.Warn("OCR failed; screenshot was still indexed", "error", processErr)
	}
}

func (r *Recorder) audioLoop(ctx context.Context) {
	for ctx.Err() == nil {
		now := time.Now().UTC()
		path := filepath.Join(r.Paths.Audio, fileStamp(now)+".wav")
		if err := r.Audio.Record(ctx, path, r.AudioChunk); err != nil {
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
		text, processErr := r.Transcriber.Transcribe(ctx, path)
		if ctx.Err() != nil {
			return
		}
		event := &store.Event{Kind: store.KindAudio, CapturedAt: now, Text: text, MediaPath: path,
			DurationMS: r.AudioChunk.Milliseconds(), Metadata: processorMetadata(processErr)}
		if err := r.Store.Insert(ctx, event); err != nil {
			r.Logger.Error("store audio event", "error", err)
			continue
		}
		r.Logger.Info("captured audio", "id", event.ID, "characters", len(text))
		if processErr != nil {
			r.Logger.Warn("transcription failed; audio was still indexed", "error", processErr)
		}
	}
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
