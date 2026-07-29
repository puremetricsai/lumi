package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
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

	// attribution is owned by the screen goroutine alone; captureScreen is the
	// only reader and writer, so it needs no lock.
	attribution attributionHealth
}

// attributionEscalationDelay is how long attribution must stay degraded before
// the recorder escalates, and attributionReminderInterval how often it repeats
// while the outage persists. Both are durations rather than tick counts because
// ScreenInterval is a flag: counting ticks would escalate after 1.5s at one
// setting and 150s at another.
const (
	attributionEscalationDelay  = 30 * time.Second
	attributionReminderInterval = 5 * time.Minute
)

type attributionHealth struct {
	degradedSince  time.Time
	lastEscalation time.Time
}

// noteAttribution escalates a sustained loss of app attribution. The previous
// behaviour — a Warn on every frame — produced tens of lines per minute, which
// is precisely how a day of unattributed capture went unnoticed.
func (r *Recorder) noteAttribution(now time.Time, screenContext ScreenContext, contextErr error) {
	if contextErr == nil && !screenContext.Degraded() {
		if !r.attribution.degradedSince.IsZero() {
			r.Logger.Info("app attribution recovered", "app", screenContext.App)
		}
		r.attribution = attributionHealth{}
		return
	}
	first := r.attribution.degradedSince.IsZero()
	if first {
		r.attribution.degradedSince = now
		r.Logger.Warn("app attribution degraded; falling back to full-screen Vision text",
			"error", attributionCause(screenContext, contextErr))
		return
	}
	if now.Sub(r.attribution.degradedSince) < attributionEscalationDelay {
		return
	}
	if !r.attribution.lastEscalation.IsZero() &&
		now.Sub(r.attribution.lastEscalation) < attributionReminderInterval {
		return
	}
	r.attribution.lastEscalation = now
	r.Logger.Error("app attribution has been unavailable; screen events are being indexed without an app",
		"since", r.attribution.degradedSince.Format(time.RFC3339),
		"remedy", attributionRemedy(screenContext, contextErr),
		"error", attributionCause(screenContext, contextErr))
}

// attributionRemedy branches on all three trust states. Reporting "trust was
// revoked" whenever a bool reads false would be wrong for the total-failure
// case, where trust was never sampled at all.
func attributionRemedy(screenContext ScreenContext, contextErr error) string {
	switch {
	case contextErr != nil, screenContext.Trusted == nil:
		return "the Accessibility snapshot itself is failing; run 'lumi doctor'"
	case !*screenContext.Trusted:
		return "this process no longer holds Accessibility; re-grant it in System Settings " +
			"> Privacy & Security > Accessibility, then restart 'lumi record'"
	default:
		return "Accessibility is granted but focused-window reads keep failing; " +
			"attribution is using the window-list fallback"
	}
}

func attributionCause(screenContext ScreenContext, contextErr error) string {
	switch {
	case contextErr != nil:
		return contextErr.Error()
	case screenContext.AccessibilityError != "":
		return screenContext.AccessibilityError
	default:
		return "no frontmost application name was resolved"
	}
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
	// One focused-window snapshot is taken per tick and its App/Window are
	// stamped onto every display's frame. On a multi-display setup an app filter
	// therefore returns frames from displays where that app was not visible:
	// the attribution answers "what was the user working in when this was
	// captured", not "what is shown in this image". Per-display attribution
	// would need a snapshot per display and is deliberately not done here.
	var screenContext ScreenContext
	var contextErr error
	if r.Context != nil {
		processingCtx, cancel := preservationContext(ctx)
		screenContext, contextErr = r.Context.Snapshot(processingCtx)
		cancel()
	}
	r.noteAttribution(now, screenContext, contextErr)
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
		// Full-display Vision OCR is the primary screen-text source: the
		// screenshot already contains the entire display, so OCR captures every
		// visible window rather than only the focused window's Accessibility text.
		// The Accessibility tree still supplies App/Window/InputActive attribution,
		// and its focused-window text is preserved in metadata when substantive so
		// no information is lost.
		textSource := "vision"
		processingCtx, cancel := preservationContext(ctx)
		text, processErr := r.Text.Extract(processingCtx, frame.Path)
		cancel()

		// No contextErr guard: a degraded snapshot may still carry substantive
		// Accessibility text, and substantiveAXText already rejects the empty
		// text a failed read leaves behind.
		axText := ""
		if substantiveAXText(screenContext) &&
			(screenContext.DisplayID == 0 || screenContext.DisplayID == frame.DisplayID) {
			axText = screenContext.Text
		}
		// If Vision produced no usable text, fall back to indexing the substantive
		// Accessibility text so the event stays searchable: events_fts and search
		// read Event.Text, not metadata. Otherwise keep the AX text as supplementary
		// provenance in metadata alongside the full-screen OCR body.
		axMetadata := axText
		if strings.TrimSpace(text) == "" && axText != "" {
			text = axText
			textSource = "accessibility"
			axMetadata = ""
		}
		metadata := screenMetadata(frame, textSource, axMetadata, screenContext,
			similarity, processErr, contextErr, compareErr)
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

// substantiveAXText reports whether the Accessibility snapshot carries more than
// the window title. It mirrors the render-time heuristic in
// internal/cli/context.go (screenEvidence) so "useful screen text" means the same
// thing at capture and at query time.
func substantiveAXText(c ScreenContext) bool {
	text := strings.TrimSpace(c.Text)
	return text != "" && text != strings.TrimSpace(c.Window)
}

func screenMetadata(frame ScreenFrame, textSource, axText string, screenContext ScreenContext,
	similarity float64, processErr, contextErr, compareErr error) json.RawMessage {
	metadata := map[string]any{
		"display_id":       frame.DisplayID,
		"width":            frame.Width,
		"height":           frame.Height,
		"text_source":      textSource,
		"frame_similarity": similarity,
	}
	if axText != "" {
		metadata["accessibility_text"] = axText
	}
	if screenContext.TitleSource != "" {
		metadata["attribution_source"] = screenContext.TitleSource
	}
	if screenContext.Trusted != nil {
		metadata["accessibility_trusted"] = *screenContext.Trusted
	}
	if processErr != nil {
		metadata["processor_error"] = processErr.Error()
	}
	// accessibility_error keeps exactly the meaning it has for events already in
	// the index — "the Accessibility read did not succeed" — whether the read
	// degraded the snapshot or destroyed it.
	switch {
	case contextErr != nil:
		metadata["accessibility_error"] = contextErr.Error()
	case screenContext.AccessibilityError != "":
		metadata["accessibility_error"] = screenContext.AccessibilityError
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
