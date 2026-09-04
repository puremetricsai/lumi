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
	"github.com/puremetricsai/lumi/internal/transcript"
)

// SegmentWriter stores one chunk's attributed segments. *store.Store satisfies
// it; the interface exists as a test seam, so a test can prove that a failed
// attribution write still leaves the audio indexed.
type SegmentWriter interface {
	ReplaceChunkSegments(ctx context.Context, capturedAt string, segments []store.Segment) error
}

type Recorder struct {
	Store *store.Store
	// Segments defaults to Store when nil.
	Segments SegmentWriter
	Paths    config.Paths
	Screen   ScreenSource
	Text     TextExtractor
	Context  ContextExtractor
	Audio    AudioSource
	// AudioOutputs is optional, but leaving it nil makes an audio row's absent
	// active_audio_output_processes mean "never sampled" rather than "no process
	// held a stream", and nothing downstream can tell those apart. Lumi's own
	// constructor always wires it, so that ambiguity is confined to tests; a
	// marker distinguishing them would be runtime state for a state production
	// cannot reach.
	AudioOutputs AudioOutputs
	// AudioMarkers is optional and carries the same caveat as AudioOutputs: it is
	// the weaker fallback signal, consulted only when CoreAudio names nobody.
	AudioMarkers   AudioMarkers
	Transcriber    SpeechTranscriber
	Comparer       *FrameComparer
	ScreenInterval time.Duration
	AudioChunk     time.Duration
	// EmitterInterval is how often the emitter state is sampled while a chunk
	// records. It defaults from AudioChunk; see defaultEmitterInterval.
	EmitterInterval time.Duration
	CaptureScreen   bool
	CaptureAudio    bool
	Logger          *slog.Logger
	// Levels is optional. When set, and when the audio stream can report its own
	// sound (LevelSampler), each track's level is drained live and reported here
	// so a supervising app can draw a meter that moves with the room. Nil means
	// no measurement is taken at all — nothing in the capture pipeline needs it.
	Levels LevelSink
	// Screens is optional and is the screen tick's counterpart to Levels: one
	// report per tick naming the displays that tick captured. Nil means no
	// report is made.
	Screens ScreenSink
	// attribution is owned by the screen goroutine alone; captureScreen is the
	// only reader and writer, so it needs no lock.
	attribution attributionHealth
	// timeline is written by the screen and emitter loops and read by the audio
	// loop at chunk close, so it carries its own lock.
	timeline *emitterTimeline
	// selectionFallback is the previous tick's answer, owned by the screen
	// goroutine alone. It exists so the warning is logged when the state
	// changes rather than on every tick: at a two-second interval, a standing
	// condition would otherwise fill the log with one line per tick and bury
	// the moment it began.
	selectionFallback bool
}

// defaultEmitterInterval samples roughly a dozen times per chunk, bounded at
// both ends because AudioChunk is a flag: a one-second chunk must not sample
// every 80ms, and a five-minute chunk must not sample only twice.
func defaultEmitterInterval(chunk time.Duration) time.Duration {
	interval := chunk / 12
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	if interval > 2*time.Second {
		interval = 2 * time.Second
	}
	return interval
}

// ringCapacity holds a few chunks' worth of observations, so a chunk whose
// indexing runs long still finds its own window intact.
func ringCapacity(chunk, interval time.Duration) int {
	if interval <= 0 {
		return 64
	}
	capacity := 3 * int(chunk/interval)
	if capacity < 64 {
		capacity = 64
	}
	return capacity
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

// noteAttribution escalates a sustained degradation. The previous behaviour — a
// Warn on every frame — produced tens of lines per minute, which is precisely
// how a day of unattributed capture went unnoticed.
//
// Two outcomes are reported differently, because they cost different things. An
// Accessibility failure that the window-list fallback covers loses only the
// supplementary AX text and warns; one that leaves no application name at all
// makes the event unfindable by app and is an error. Escalating both as "indexed
// without an app" would fire on every routine fallback and train the reader to
// ignore the line — the exact failure this monitor exists to prevent.
func (r *Recorder) noteAttribution(now time.Time, screenContext ScreenContext, contextErr error) {
	if contextErr == nil && !screenContext.Degraded() {
		if !r.attribution.degradedSince.IsZero() {
			r.Logger.Info("app attribution recovered", "app", screenContext.App)
		}
		r.attribution = attributionHealth{}
		return
	}
	unattributed := contextErr != nil || screenContext.Unattributed()
	if r.attribution.degradedSince.IsZero() {
		r.attribution.degradedSince = now
		r.Logger.Warn("Accessibility read failed", "attributed", !unattributed,
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
	arguments := []any{
		"since", r.attribution.degradedSince.Format(time.RFC3339),
		"remedy", attributionRemedy(screenContext, contextErr),
		"error", attributionCause(screenContext, contextErr),
	}
	if unattributed {
		r.Logger.Error("app attribution has been unavailable; screen events are being indexed without an app",
			arguments...)
		return
	}
	r.Logger.Warn("Accessibility has been unavailable; app attribution is using the window-list fallback "+
		"and Accessibility text is not being captured",
		append(arguments, "app", screenContext.App)...)
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

// SpeechTranscriber returns a chunk's transcript together with its timed
// segments. Segments may be empty — a silent file produces none — but Text is
// always the authority for what was said, and is what gets indexed.
//
// The segments are deliberately part of this interface rather than an optional
// capability a caller type-asserts for: everything downstream of the recorder
// depends on whether they arrived, and an assertion would hide that difference
// behind a silent fallback.
type SpeechTranscriber interface {
	Transcribe(context.Context, string) (Transcription, error)
}

func (r *Recorder) Run(ctx context.Context) error {
	if r.Store == nil {
		return errors.New("recorder store is required")
	}
	if r.Segments == nil {
		r.Segments = r.Store
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
	if r.EmitterInterval <= 0 {
		r.EmitterInterval = defaultEmitterInterval(r.AudioChunk)
	}
	r.timeline = newEmitterTimeline(
		ringCapacity(r.AudioChunk, r.EmitterInterval),
		ringCapacity(r.AudioChunk, r.ScreenInterval))
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.emitterLoop(ctx)
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

// emitterLoop samples what is producing sound while chunks record.
//
// It reads CoreAudio process objects and the window list, neither of which needs
// the Accessibility grant, and it deliberately does *not* take a focus snapshot
// while the screen loop is running: AccessibilityContext.Snapshot serializes on a
// package-level mutex shared with the screen tick, and a second caller at this
// cadence would contend with capture for no gain. When screen capture is on, the
// screen tick already produces exactly the focus samples this timeline needs, so
// they are reused. When it is off there is no tick and no contention, and this
// loop takes them itself.
func (r *Recorder) emitterLoop(ctx context.Context) {
	sampleForeground := !r.CaptureScreen && r.Context != nil
	r.sampleEmitters(ctx, sampleForeground)
	ticker := time.NewTicker(r.EmitterInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sampleEmitters(ctx, sampleForeground)
		}
	}
}

// sampleEmitters reads the emitter state once, records it on the timeline, and
// returns it so a caller that needs the sample immediately does not have to read
// it back out of a window query.
func (r *Recorder) sampleEmitters(ctx context.Context, withForeground bool) EmitterObservation {
	now := time.Now().UTC()
	observation := EmitterObservation{At: now}
	if r.AudioOutputs != nil {
		processes, err := r.AudioOutputs.Active(ctx)
		if err != nil {
			observation.ProcessErr = err.Error()
		} else {
			observation.Processes = processes
		}
	} else {
		observation.ProcessErr = "audio output processes were never sampled"
	}
	if r.AudioMarkers != nil {
		windows, err := r.AudioMarkers.Playing(ctx)
		if err != nil {
			observation.WindowErr = err.Error()
		} else {
			observation.Windows = windows
		}
	} else {
		observation.WindowErr = "audio marker windows were never sampled"
	}
	r.timeline.observeEmitters(observation)
	if !withForeground {
		return observation
	}
	screenContext, err := r.Context.Snapshot(ctx)
	r.timeline.observeForeground(ForegroundObservation{At: now, Context: screenContext, Err: err})
	return observation
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
	fallback := len(frames) > 0 && frames[0].SelectionFallback
	if fallback != r.selectionFallback {
		if fallback {
			r.Logger.Warn("none of the selected displays are connected; recording every display")
		} else {
			r.Logger.Info("recording the selected displays again")
		}
		r.selectionFallback = fallback
	}
	if r.Screens != nil {
		displayIDs := make([]uint32, 0, len(frames))
		for _, frame := range frames {
			displayIDs = append(displayIDs, frame.DisplayID)
		}
		r.Screens(ScreenCapture{
			CapturedAt: now, DisplayIDs: displayIDs,
			IntervalMS:        r.ScreenInterval.Milliseconds(),
			SelectionFallback: fallback,
		})
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
	// Feed the same snapshot to the audio side rather than letting it take its
	// own. Accessibility reads serialize on a package-level mutex, so a second
	// sampler at the emitter cadence would contend with capture to learn what this
	// tick already knows. See emitterLoop.
	r.timeline.observeForeground(ForegroundObservation{At: now, Context: screenContext, Err: contextErr})
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
	// app_source is recorded separately from attribution_source: one says where
	// the window title came from, the other where the app name did. Merging them
	// would silently change what attribution_source means for indexed rows.
	if screenContext.AppSource != "" {
		metadata["app_source"] = screenContext.AppSource
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

// audioLoop keeps a capture stream open and consumes the chunks it produces.
// The stream is reopened only after a failure: transcription, indexing, and
// attribution all run while the next chunk is still being recorded, because
// doing them between two recordings is what used to cost the seconds of audio
// that fell into the gap.
func (r *Recorder) audioLoop(ctx context.Context) {
	for ctx.Err() == nil {
		stream, err := r.Audio.Open(ctx, r.Paths.Audio, r.AudioChunk)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.Logger.Error("audio capture failed", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		r.consumeAudio(ctx, stream)
	}
}

// consumeAudio drains one stream. It returns so the caller can reopen after a
// capture failure, and returns without reopening once the stream reports itself
// closed — which it does only after handing over everything it had recorded.
func (r *Recorder) consumeAudio(ctx context.Context, stream AudioStream) {
	defer func() {
		if err := stream.Close(); err != nil {
			r.Logger.Error("close audio capture stream", "error", err)
		}
	}()
	// The meters live exactly as long as this stream does, so a stream reopened
	// after a capture failure gets a fresh sampler and a stream that closed
	// stops reporting sound nobody is capturing.
	if sampler, ok := stream.(LevelSampler); ok && r.Levels != nil {
		levelCtx, stopLevels := context.WithCancel(ctx)
		defer stopLevels()
		go r.sampleLevels(levelCtx, sampler)
	}
	for {
		chunk, err := stream.Next(ctx)
		if err != nil {
			if !errors.Is(err, ErrAudioStreamClosed) && ctx.Err() == nil {
				r.Logger.Error("audio capture failed", "error", err)
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
			return
		}
		r.storeAudioChunk(ctx, chunk)
	}
}

// storeAudioChunk transcribes, indexes, and attributes one chunk. Both tracks
// are stamped with the chunk's own start rather than a fresh clock read, so the
// pair keeps the single captured_at that its segments are keyed by.
//
// The two attribution axes are resolved separately and mean different things.
// App/Window say what the user was working in — one sample, exactly as before —
// while audio_attribution and source_apps_json say what was producing the sound,
// folded from every sample taken across the chunk. Conflating them is the defect
// this shape exists to prevent: a video playing in a background browser is
// attributed to the browser, not to whatever the user had focused.
func (r *Recorder) storeAudioChunk(ctx context.Context, chunk AudioChunk) {
	capturedAt := chunk.StartedAt
	if capturedAt.IsZero() {
		capturedAt = time.Now().UTC()
	}
	capturedAt = capturedAt.UTC()
	attribution := r.audioAttribution(ctx, capturedAt, chunk)
	results := make([]audioChunkResult, 0, len(chunk.Frames))
	for _, frame := range chunk.Frames {
		var transcription Transcription
		var processErr error
		if ctx.Err() != nil {
			processErr = fmt.Errorf("transcription skipped after capture stopped: %w", ctx.Err())
		} else {
			transcription, processErr = r.Transcriber.Transcribe(ctx, frame.Path)
		}
		// The verdict is per track: the microphone is unattributed however loudly
		// the machine was playing, and only the system track may name a source.
		verdict := DecideAudioAttribution(attribution.inputFor(frame.Source))
		sourceApps, encodeErr := store.EncodeSourceApps(verdict.SourceApps)
		if encodeErr != nil {
			r.Logger.Warn("audio source list could not be encoded; the audio is still indexed",
				"source", frame.Source, "error", encodeErr)
			sourceApps = ""
		}
		event := &store.Event{Kind: store.KindAudio, CapturedAt: capturedAt, Text: transcription.Text, MediaPath: frame.Path,
			App: attribution.screen.App, Window: attribution.screen.Window,
			DurationMS: frame.DurationMS, AudioSource: frame.Source,
			TextSource:       transcription.Source,
			AudioAttribution: string(verdict.Attribution),
			SourceApps:       sourceApps,
			StreamOffsetMS:   chunk.StreamOffsetMS,
			Metadata: audioMetadata(frame.Source, frame.CaptureError, processErr, attribution,
				verdict, chunk)}
		storeCtx, cancel := preservationContext(ctx)
		err := r.Store.Insert(storeCtx, event)
		cancel()
		if err != nil {
			r.Logger.Error("store audio event", "path", frame.Path, "error", err)
			continue
		}
		r.Logger.Info("captured audio", "id", event.ID, "source", frame.Source,
			"characters", len(transcription.Text), "segments", len(transcription.Segments))
		if processErr != nil {
			r.Logger.Warn("transcription failed; audio was still indexed", "source", frame.Source, "error", processErr)
		}
		results = append(results, audioChunkResult{
			frame: frame, transcription: transcription, eventID: event.ID, failed: processErr != nil})
	}
	r.attributeChunk(ctx, capturedAt, results)
}

// audioChunkResult is one captured track: what was recorded, what it transcribed
// to, and which row it became.
type audioChunkResult struct {
	frame         AudioFrame
	transcription Transcription
	eventID       int64
	// failed records that this track has no transcript because recognition did
	// not happen, as opposed to happening and finding nothing. The recognizer
	// reports both as an empty string, and attribution reads that emptiness as
	// evidence — so which of the two it was has to be carried out of band.
	failed bool
}

// attributeChunk decides where a chunk's audio came from and stores the verdict.
//
// It runs only after every track of the chunk is already indexed, and a failure
// here is logged rather than returned. Captured media is never lost to a
// downstream failure: the WAVs are on disk and their transcripts are in the index
// before this is reached, so the worst case is a chunk with no attribution. That
// needs no retry loop either — `lumi transcript backfill` derives its work queue
// from exactly the chunks that have no segments, so this chunk is already in it.
//
// A chunk whose recognition failed is the one case that leaves the queue on
// purpose undrainable: the backfill applies the same rule and will not label it
// either, so it sits there until someone reads the WAV. That is the honest
// state, and `lumi transcript backfill` reports those chunks apart from the ones
// it can still do something about.
func (r *Recorder) attributeChunk(ctx context.Context, capturedAt time.Time, results []audioChunkResult) {
	if len(results) == 0 {
		return
	}
	chunk := transcript.Chunk{CapturedAt: capturedAt}
	eventIDs := make(map[string]int64, len(results))
	systemPath := ""
	for _, result := range results {
		eventIDs[result.frame.Source] = result.eventID
		switch result.frame.Source {
		case transcript.TrackSystem:
			chunk.System = buildTrack(result)
			systemPath = result.frame.Path
		case transcript.TrackMicrophone:
			chunk.Microphone = buildTrack(result)
		}
	}
	r.measureInternalEnergy(ctx, &chunk, systemPath)

	segments := transcript.Attribute(chunk, transcript.Options{})
	if len(segments) == 0 {
		return
	}
	if transcript.IsSilent(segments) && anyTranscriptionFailed(results) {
		// Silence is the one verdict read out of an *absence* of words, and a
		// failed recognizer produces exactly the same absence — so this is the one
		// verdict a failure can turn into a false claim about the world. Writing
		// the marker would also make it permanent, since the marker is what drains
		// the chunk from the work queue.
		//
		// The gate is deliberately the verdict rather than the failure alone. A
		// track that did transcribe is labelled on its own evidence: system audio
		// is internal unconditionally, and a system track that failed while the
		// microphone spoke is caught by the energy measurement, which reads the
		// WAV rather than the transcript.
		r.Logger.Warn("chunk left unattributed: nothing was transcribed and transcription failed",
			"captured_at", capturedAt)
		return
	}
	rows := store.SegmentRows(segments, eventIDs)
	if len(rows) == 0 {
		return
	}
	storeCtx, cancel := preservationContext(ctx)
	defer cancel()
	// The chunk key must be the byte-identical string Insert wrote into
	// events.captured_at: ChunksMissingSegments joins the two columns on equality,
	// so any divergence leaves every chunk this recorder writes permanently on the
	// backfill queue with SegmentCoverage reporting nothing attributed — and
	// reports no error while doing it. One shared renderer is what makes that
	// impossible rather than merely unlikely.
	if err := r.Segments.ReplaceChunkSegments(storeCtx, store.FormatCapturedAt(capturedAt), rows); err != nil {
		r.Logger.Warn("audio attribution was not stored; the audio is still indexed and `lumi transcript backfill` will retry",
			"error", err)
		return
	}
	if transcript.IsSilent(segments) {
		// A silent chunk still writes its marker, but saying so every thirty
		// seconds would bury the lines that carry information under the quietest
		// thing the recorder does.
		r.Logger.Debug("chunk held no speech", "captured_at", capturedAt)
		return
	}
	r.Logger.Info("attributed audio", "segments", len(rows), "method", string(segments[0].Method))
}

// anyTranscriptionFailed reports whether any of the chunk's tracks has no
// transcript because recognition did not happen.
func anyTranscriptionFailed(results []audioChunkResult) bool {
	for _, result := range results {
		if result.failed {
			return true
		}
	}
	return false
}

func buildTrack(result audioChunkResult) *transcript.Track {
	return &transcript.Track{
		Source:    result.frame.Source,
		StartedAt: result.frame.StartedAt,
		Text:      result.transcription.Text,
		Segments:  result.transcription.Segments,
	}
}

// measureInternalEnergy reads the system track when the verdict could turn on
// whether it was silent or merely untranscribable, which is what
// transcript.NeedsInternalEnergy decides — the rule lives there so this and the
// backfill skip exactly the same chunks. Which reader opens the file is
// ReadAudioEnvelope's, for the same reason.
func (r *Recorder) measureInternalEnergy(ctx context.Context, chunk *transcript.Chunk, systemPath string) {
	if systemPath == "" || !transcript.NeedsInternalEnergy(*chunk) {
		return
	}
	envelope, _, err := ReadAudioEnvelope(ctx, systemPath, transcript.EnvelopeWindowMS)
	if err != nil {
		// Without the measurement the microphone stays confidently external,
		// which is the pre-existing behaviour rather than a new risk.
		r.Logger.Debug("could not measure system audio energy", "error", err)
		return
	}
	chunk.System.Envelope = envelope
	chunk.System.EnvelopeWindowMS = transcript.EnvelopeWindowMS
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

// audioAttributionSample is what a chunk could learn about its own provenance:
// which application the user was working in, and what was producing sound. They
// answer different questions and routinely disagree — a call recorded while the
// user takes notes elsewhere is the ordinary case — so neither substitutes for
// the other.
type audioAttributionSample struct {
	screen    ScreenContext
	screenErr error
	// emitters is folded from every sample taken across the chunk, not from one
	// read at its close.
	emitters AttributionInput
	// foregroundSamples and foregroundApps describe how much focus moved while
	// the chunk recorded, which is what makes "App is one sample of a
	// thirty-second window" visible in the data rather than only in a comment.
	foregroundSamples int
	foregroundApps    int
}

// inputFor names the track a verdict is being decided for. Everything else about
// the chunk is shared; the track is what makes the microphone's answer different.
func (s audioAttributionSample) inputFor(track string) AttributionInput {
	input := s.emitters
	input.Track = track
	return input
}

// audioAttribution assembles what the chunk observed while it was recording.
//
// The emitter side is read from the timeline, which the emitter loop has been
// filling throughout the chunk — the point being that a 30-second chunk can span
// several application switches, and a single read at chunk close attributed all
// of it to whatever was true at the end.
//
// It falls back to sampling synchronously when the timeline holds nothing for
// this window. That covers the chunk in flight at shutdown and the very short
// chunks tests use, where the loop may not have ticked inside the window at all;
// the fallback is exactly the behaviour this function had before the timeline
// existed, so those paths did not change.
//
// Reads run under preservationContext, for the same reason the inserts do: the
// chunk's audio already exists and these merely describe it. Using the cancelled
// context cost the last chunk of every recording its attribution.
//
// No read here can fail the chunk. An error is carried into metadata and the
// corresponding field left empty, because captured media is never lost to a
// downstream failure.
func (r *Recorder) audioAttribution(ctx context.Context, capturedAt time.Time, chunk AudioChunk) audioAttributionSample {
	sampleCtx, cancel := preservationContext(ctx)
	defer cancel()
	var sample audioAttributionSample

	emitters, foreground := r.timeline.window(capturedAt, capturedAt.Add(chunkSpan(chunk, r.AudioChunk)))
	if len(emitters) == 0 {
		// Use the fresh sample itself rather than re-querying a widened window.
		// Widening to time.Now() would sweep in observations belonging to *later*
		// chunks, so a chunk whose indexing ran long could be attributed to an
		// application that only started playing after its audio had ended.
		//
		// This sample is taken after the chunk closed, which is imprecise but
		// bounded and is exactly what this function did before the timeline
		// existed. It is folded at the chunk's own start so its offsets stay
		// relative to the chunk.
		emitters = []EmitterObservation{r.sampleEmitters(sampleCtx, false)}
	}
	sample.emitters = foldEmitters(emitters, capturedAt)

	if observation, ok := lastForeground(foreground); ok {
		sample.screen, sample.screenErr = observation.Context, observation.Err
		sample.foregroundSamples = len(foreground)
		sample.foregroundApps = distinctForegroundApps(foreground)
	} else if r.Context != nil {
		sample.screen, sample.screenErr = r.Context.Snapshot(sampleCtx)
		sample.foregroundSamples = 1
		sample.foregroundApps = 1
	}
	return sample
}

// chunkSpan is how long the chunk actually covered, and bounds the window its
// emitters are read from.
//
// A measured duration wins outright over the requested one wherever any frame
// reports it. Taking the maximum of the two instead lets the requested duration
// override a *shorter* measurement, which is exactly backwards for the case that
// matters: a chunk closed early — at shutdown, or when a stream fault forces a
// reopen — really did hold less audio than was asked for, and stretching its
// window to the full interval attributes it to emitters that started after its
// sound had stopped. Measured 8s against a requested 30s widened the window by
// 22 seconds.
//
// The requested duration remains the fallback for chunks the native layer could
// not measure, which is every chunk recorded before it reported one.
func chunkSpan(chunk AudioChunk, configured time.Duration) time.Duration {
	var measured, requested int64
	for _, frame := range chunk.Frames {
		if frame.MeasuredDurationMS > measured {
			measured = frame.MeasuredDurationMS
		}
		if frame.DurationMS > requested {
			requested = frame.DurationMS
		}
	}
	if measured > 0 {
		return time.Duration(measured) * time.Millisecond
	}
	if requested > 0 {
		return time.Duration(requested) * time.Millisecond
	}
	return configured
}

func audioMetadata(source, captureError string, processErr error,
	attribution audioAttributionSample, verdict AttributionVerdict, chunk AudioChunk) json.RawMessage {
	metadata := map[string]any{"audio_source": source}
	if captureError != "" {
		metadata["capture_error"] = captureError
	}
	if processErr != nil {
		metadata["processor_error"] = processErr.Error()
	}
	// app_source and attribution_source keep exactly the meanings they carry on
	// screen rows: which source named the application, and which supplied the
	// window title. They differ routinely, and merging them would change what
	// attribution_source means for every row already indexed.
	if attribution.screen.AppSource != "" {
		metadata["app_source"] = attribution.screen.AppSource
	}
	if attribution.screen.TitleSource != "" {
		metadata["attribution_source"] = attribution.screen.TitleSource
	}
	switch {
	case attribution.screenErr != nil:
		metadata["accessibility_error"] = attribution.screenErr.Error()
	case attribution.screen.AccessibilityError != "":
		metadata["accessibility_error"] = attribution.screen.AccessibilityError
	}
	// An empty list is omitted rather than written as []. Absent means no
	// process held an output stream, which is a finding;
	// active_audio_output_processes_error means Lumi could not tell, which is
	// the opposite one. Absence carries that meaning only because the recorder
	// is always constructed with an AudioOutputs source; see Recorder.
	//
	// This key now holds the union across the chunk rather than one read at its
	// close — the same meaning, sampled better. It is still written because
	// get_event's metadata is the diagnostic channel, and because it and the
	// source_apps_json column are two renderings of one fold rather than two
	// rules that could disagree.
	if len(attribution.emitters.Processes) > 0 {
		metadata["active_audio_output_processes"] = attribution.emitters.Processes
	}
	if attribution.emitters.ProcessErr != "" {
		metadata["active_audio_output_processes_error"] = attribution.emitters.ProcessErr
	}
	if len(attribution.emitters.Markers) > 0 {
		metadata["audio_marker_windows"] = attribution.emitters.Markers
	}
	if attribution.emitters.MarkerErr != "" {
		metadata["audio_marker_windows_error"] = attribution.emitters.MarkerErr
	}
	if attribution.emitters.Attempts > 0 {
		metadata["audio_source_samples"] = attribution.emitters.Observations
		metadata["audio_source_sample_attempts"] = attribution.emitters.Attempts
	}
	// How much the focused application moved while this chunk recorded. App and
	// Window are a single sample of that window, and this is what says so.
	if attribution.foregroundSamples > 0 {
		metadata["foreground_samples"] = attribution.foregroundSamples
		metadata["foreground_apps_seen"] = attribution.foregroundApps
	}
	if verdict.Reason != "" {
		metadata["audio_attribution_reason"] = verdict.Reason
	}
	if chunk.ClockAnomaly {
		metadata["clock_anomaly"] = true
	}
	if !chunk.GridStartedAt.IsZero() {
		metadata["grid_started_at"] = store.FormatCapturedAt(chunk.GridStartedAt)
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
