package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/store"
)

type fakeScreen struct{ count atomic.Int64 }

func (f *fakeScreen) Capture(_ context.Context, directory, prefix string) ([]ScreenFrame, error) {
	value := f.count.Add(1)
	path := filepath.Join(directory, fmt.Sprintf("%s-display-1.jpg", prefix))
	err := os.WriteFile(path, []byte(fmt.Sprintf("fake-jpeg-%d", value)), 0o600)
	return []ScreenFrame{{Path: path, DisplayID: 1, Width: 100, Height: 100}}, err
}

type repeatedScreen struct{ contents []byte }

func (s repeatedScreen) Capture(_ context.Context, directory, prefix string) ([]ScreenFrame, error) {
	path := filepath.Join(directory, prefix+"-display-9.jpg")
	err := os.WriteFile(path, s.contents, 0o600)
	return []ScreenFrame{{Path: path, DisplayID: 9, Width: 64, Height: 36}}, err
}

type flakyScreen struct{ attempts atomic.Int64 }

func (s *flakyScreen) Capture(ctx context.Context, directory, prefix string) ([]ScreenFrame, error) {
	if s.attempts.Add(1) == 1 {
		return nil, errors.New("transient ScreenCaptureKit failure")
	}
	return (&fakeScreen{}).Capture(ctx, directory, prefix)
}

type hotplugScreen struct {
	calls    atomic.Int64
	contents []byte
}

func (s *hotplugScreen) Capture(_ context.Context, directory, prefix string) ([]ScreenFrame, error) {
	var displayIDs []uint32
	switch s.calls.Add(1) {
	case 1:
		displayIDs = []uint32{1}
	case 2:
		displayIDs = []uint32{1, 2}
	default:
		displayIDs = []uint32{2}
	}
	frames := make([]ScreenFrame, 0, len(displayIDs))
	for _, displayID := range displayIDs {
		path := filepath.Join(directory, fmt.Sprintf("%s-display-%d.jpg", prefix, displayID))
		if err := os.WriteFile(path, s.contents, 0o600); err != nil {
			return nil, err
		}
		frames = append(frames, ScreenFrame{Path: path, DisplayID: displayID, Width: 64, Height: 36})
	}
	return frames, nil
}

type fakeVision struct{}

func (fakeVision) Extract(context.Context, string) (string, error) {
	return "Lumi end to end screen text", nil
}

type countingText struct{ calls atomic.Int64 }

func (c *countingText) Extract(context.Context, string) (string, error) {
	c.calls.Add(1)
	return "Vision fallback", nil
}

type failingText struct{}

func (failingText) Extract(context.Context, string) (string, error) {
	return "", errors.New("Vision request failed")
}

type accessibilityText struct{}

func (accessibilityText) Snapshot(context.Context) (ScreenContext, error) {
	return ScreenContext{
		App: "Notes", Window: "Plan", Text: "Accessibility primary text", DisplayID: 1,
	}, nil
}

type fakeContext struct{}

// Snapshot honours cancellation because the native read does:
// macosnative.Accessibility returns ctx.Err() before touching AX. A fake that
// ignored it would pass whether or not the caller preserved the context, which
// is precisely the bug that left every session's last chunk unattributed.
func (fakeContext) Snapshot(ctx context.Context) (ScreenContext, error) {
	if err := ctx.Err(); err != nil {
		return ScreenContext{}, err
	}
	return ScreenContext{App: "Test App", Window: "Test Window", AppSource: "workspace"}, nil
}

// titleOnlyContext mimics a GPUI/Metal app (e.g. Zed) whose Accessibility tree
// exposes only the window title, so Text equals Window.
type titleOnlyContext struct{}

func (titleOnlyContext) Snapshot(context.Context) (ScreenContext, error) {
	return ScreenContext{App: "Zed", Window: "lumi — .env", Text: "lumi — .env", DisplayID: 1}, nil
}

// degradedContext mimics the production failure this fallback exists for: the
// Accessibility read failed, but NSWorkspace still named the frontmost app and
// the window list still supplied a title.
type degradedContext struct{}

func (degradedContext) Snapshot(context.Context) (ScreenContext, error) {
	trusted := true
	return ScreenContext{
		App: "Comet", Window: "Booking — Shashi Hotel", DisplayID: 1,
		Trusted:            &trusted,
		AppSource:          "window_list",
		TitleSource:        "window_list",
		AccessibilityError: "read macOS Accessibility tree: read focused Accessibility window (AX error -25204)",
	}, nil
}

// degradedWithTextContext is a degraded snapshot that nonetheless carries
// substantive Accessibility text, pinning that the removed contextErr gate did
// not take the text path with it.
type degradedWithTextContext struct{}

func (degradedWithTextContext) Snapshot(context.Context) (ScreenContext, error) {
	trusted := false
	return ScreenContext{
		App: "Notes", Window: "Plan", Text: "Accessibility primary text", DisplayID: 1,
		Trusted:            &trusted,
		AppSource:          "window_list",
		TitleSource:        "window_list",
		AccessibilityError: "read macOS Accessibility tree: degraded",
	}, nil
}

type failingContext struct{}

func (failingContext) Snapshot(context.Context) (ScreenContext, error) {
	return ScreenContext{}, errors.New("read macOS Accessibility tree: snapshot unavailable")
}

// trackStream stands in for the native capture session: it is opened once and
// keeps producing chunks however long the consumer spends on each one, and every
// chunk carries the instant its audio began rather than the instant the consumer
// got round to it.
type trackStream struct {
	sources   []string
	directory string
	chunk     time.Duration
	origin    time.Time
	index     int
}

func newTrackStream(directory string, chunk time.Duration, sources ...string) *trackStream {
	return &trackStream{sources: sources, directory: directory, chunk: chunk, origin: time.Now().UTC()}
}

func (s *trackStream) Next(ctx context.Context) (AudioChunk, error) {
	select {
	case <-ctx.Done():
		return AudioChunk{}, ErrAudioStreamClosed
	case <-time.After(s.chunk):
	}
	startedAt := s.origin.Add(time.Duration(s.index) * s.chunk)
	s.index++
	frames := make([]AudioFrame, 0, len(s.sources))
	for _, source := range s.sources {
		path := filepath.Join(s.directory, fileStamp(startedAt)+"-"+source+".wav")
		if err := os.WriteFile(path, []byte("fake-wave"), 0o600); err != nil {
			return AudioChunk{}, err
		}
		frames = append(frames, AudioFrame{Path: path, Source: source, DurationMS: s.chunk.Milliseconds()})
	}
	return AudioChunk{StartedAt: startedAt, Frames: frames}, nil
}

func (s *trackStream) Close() error { return nil }

type fakeAudio struct{}

func (fakeAudio) Open(ctx context.Context, directory string, chunk time.Duration) (AudioStream, error) {
	return newTrackStream(directory, chunk, "microphone"), nil
}

type dualAudio struct{}

func (dualAudio) Open(ctx context.Context, directory string, chunk time.Duration) (AudioStream, error) {
	return newTrackStream(directory, chunk, "system", "microphone"), nil
}

// countingAudio reports how many times a stream was opened, which is how the
// recorder's promise to keep capture running between chunks is checked.
type countingAudio struct {
	opens atomic.Int32
}

func (a *countingAudio) Open(ctx context.Context, directory string, chunk time.Duration) (AudioStream, error) {
	a.opens.Add(1)
	return newTrackStream(directory, chunk, "system", "microphone"), nil
}

// completionAfterCancelAudio finishes nothing until the recording is cancelled,
// then delivers the chunk the session finalised on the way down — the shape
// native shutdown has, and audio that must still be indexed.
type completionAfterCancelAudio struct{}

func (completionAfterCancelAudio) Open(ctx context.Context, directory string, chunk time.Duration) (AudioStream, error) {
	return &completionAfterCancelStream{directory: directory, chunk: chunk}, nil
}

type completionAfterCancelStream struct {
	directory string
	chunk     time.Duration
	delivered bool
}

func (s *completionAfterCancelStream) Next(ctx context.Context) (AudioChunk, error) {
	<-ctx.Done()
	if s.delivered {
		return AudioChunk{}, ErrAudioStreamClosed
	}
	s.delivered = true
	startedAt := time.Now().UTC()
	path := filepath.Join(s.directory, fileStamp(startedAt)+"-system.wav")
	if err := os.WriteFile(path, []byte("late-native-wave"), 0o600); err != nil {
		return AudioChunk{}, err
	}
	return AudioChunk{StartedAt: startedAt, Frames: []AudioFrame{
		{Path: path, Source: "system", DurationMS: s.chunk.Milliseconds()},
	}}, nil
}

func (s *completionAfterCancelStream) Close() error { return nil }

type fakeTranscriber struct{}

func (fakeTranscriber) Transcribe(context.Context, string) (Transcription, error) {
	return Transcription{Text: "Lumi end to end audio transcript"}, nil
}

type failingTranscriber struct{}

func (failingTranscriber) Transcribe(context.Context, string) (Transcription, error) {
	return Transcription{}, errors.New("transcription failed")
}

// slowTranscriber takes longer than a chunk lasts, which is the condition that
// used to push every following chunk's timestamp out.
type slowTranscriber struct{ delay time.Duration }

func (t slowTranscriber) Transcribe(ctx context.Context, _ string) (Transcription, error) {
	select {
	case <-ctx.Done():
	case <-time.After(t.delay):
	}
	return Transcription{Text: "Lumi end to end audio transcript"}, nil
}

type countingTranscriber struct{ calls atomic.Int64 }

func (t *countingTranscriber) Transcribe(context.Context, string) (Transcription, error) {
	t.calls.Add(1)
	return Transcription{Text: "unexpected"}, nil
}

// segmentTranscriber returns timed segments whose text concatenates, with no
// separator, to exactly the flat transcript — the same relationship the native
// bridge guarantees, so tests that depend on it are not depending on a fiction.
type segmentTranscriber struct{}

func (segmentTranscriber) Transcribe(context.Context, string) (Transcription, error) {
	segments := []TimedSegment{
		{StartMS: 0, EndMS: 1200, Text: "First phrase.", Confidence: 0.9,
			Runs: []TimedRun{{StartMS: 0, EndMS: 600, Text: "First", Confidence: 0.9},
				{StartMS: 600, EndMS: 1200, Text: " phrase.", Confidence: 0.9}}},
		{StartMS: 1200, EndMS: 2400, Text: " Second phrase.", Confidence: 0.8,
			Runs: []TimedRun{{StartMS: 1200, EndMS: 1800, Text: " Second", Confidence: 0.8},
				{StartMS: 1800, EndMS: 2400, Text: " phrase.", Confidence: 0.8}}},
	}
	text := ""
	for _, segment := range segments {
		text += segment.Text
	}
	return Transcription{Text: text, Segments: segments}, nil
}

func TestRecorderCaptureProcessStoreSearch(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	recorder := Recorder{
		Store: s, Paths: paths, CaptureScreen: true, CaptureAudio: true,
		ScreenInterval: 8 * time.Millisecond, AudioChunk: 8 * time.Millisecond,
		Screen: &fakeScreen{}, Text: fakeVision{}, Context: fakeContext{},
		Audio: fakeAudio{}, Transcriber: fakeTranscriber{},
	}
	recordCtx, cancel := context.WithTimeout(ctx, 35*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	screen, err := s.Search(ctx, store.SearchOptions{Query: "screen text", Kind: store.KindScreen})
	if err != nil {
		t.Fatal(err)
	}
	if len(screen) == 0 || screen[0].App != "Test App" {
		t.Fatalf("screen pipeline did not produce a searchable event: %#v", screen)
	}
	if screen[0].TextSource != "vision" || screen[0].DisplayID != 1 {
		t.Fatalf("screen provenance was not stored: %#v", screen[0])
	}
	if _, err := os.Stat(screen[0].MediaPath); err != nil {
		t.Fatalf("screen media was not preserved: %v", err)
	}

	audio, err := s.Search(ctx, store.SearchOptions{Query: "audio transcript", Kind: store.KindAudio})
	if err != nil {
		t.Fatal(err)
	}
	if len(audio) == 0 || audio[0].DurationMS != 8 {
		t.Fatalf("audio pipeline did not produce a searchable event: %#v", audio)
	}
	if audio[0].AudioSource != "microphone" {
		t.Fatalf("audio provenance was not stored: %#v", audio[0])
	}
	if filepath.Ext(audio[0].MediaPath) != ".wav" {
		t.Fatalf("unexpected audio media path: %s", audio[0].MediaPath)
	}
}

func TestRecorderUsesFullScreenVisionAndPreservesAccessibility(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	vision := &countingText{}
	recorder := Recorder{
		Store: s, Paths: paths, CaptureScreen: true, ScreenInterval: time.Hour,
		Screen: &fakeScreen{}, Text: vision, Context: accessibilityText{},
	}
	recordCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}
	// Full-screen OCR is the primary text source even when Accessibility has
	// substantive text, so the whole display is captured rather than only the
	// focused window.
	if got := vision.calls.Load(); got == 0 {
		t.Fatal("Vision OCR was not used as the primary screen-text source")
	}
	events, err := s.Search(ctx, store.SearchOptions{Query: "Vision fallback"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TextSource != "vision" {
		t.Fatalf("full-screen Vision text was not primary: %#v", events)
	}
	// App/Window attribution still comes from Accessibility, and the focused
	// window's Accessibility text is preserved in metadata.
	if events[0].App != "Notes" || events[0].Window != "Plan" {
		t.Fatalf("Accessibility attribution was not retained: %#v", events[0])
	}
	if !strings.Contains(string(events[0].Metadata), "Accessibility primary text") {
		t.Fatalf("substantive Accessibility text was not preserved in metadata: %s", events[0].Metadata)
	}
}

func TestRecorderIndexesAccessibilityTextWhenVisionFails(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	recorder := Recorder{
		Store: s, Paths: paths, Screen: &fakeScreen{}, Text: failingText{},
		Context: accessibilityText{}, Comparer: &FrameComparer{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recorder.captureScreen(ctx)

	// Vision produced no usable text, so the substantive Accessibility text must
	// stay searchable via Event.Text (events_fts and search never read metadata).
	events, err := s.Search(ctx, store.SearchOptions{Query: "Accessibility primary"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "Accessibility primary text" {
		t.Fatalf("Accessibility text was not indexed when Vision failed: %#v", events)
	}
	if events[0].TextSource != "accessibility" {
		t.Fatalf("fallback provenance was not recorded: %#v", events[0])
	}
	// The failed Vision attempt is still recorded as diagnostic provenance.
	if !strings.Contains(string(events[0].Metadata), "processor_error") {
		t.Fatalf("Vision failure was not recorded in metadata: %s", events[0].Metadata)
	}
}

func TestRecorderFallsBackToVisionWhenAccessibilityIsTitleOnly(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	recorder := Recorder{
		Store: s, Paths: paths, Screen: &fakeScreen{}, Text: fakeVision{},
		Context: titleOnlyContext{}, Comparer: &FrameComparer{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recorder.captureScreen(ctx)

	// A GPUI/Metal app (e.g. Zed) exposes only its window title through
	// Accessibility; the recorder must OCR the full screen instead of indexing
	// the title as the body.
	events, err := s.Search(ctx, store.SearchOptions{Query: "screen text"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TextSource != "vision" {
		t.Fatalf("title-only Accessibility did not fall back to Vision: %#v", events)
	}
	if events[0].Text == events[0].Window {
		t.Fatalf("indexed text is still the window title only: %#v", events[0])
	}
	if strings.Contains(string(events[0].Metadata), "accessibility_text") {
		t.Fatalf("title-only Accessibility text should not be preserved as substantive: %s", events[0].Metadata)
	}
}

func TestRecorderDeletesPerceptualDuplicatesFromDiskAndIndex(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sourcePath := filepath.Join(t.TempDir(), "source.jpg")
	writeSolidJPEG(t, sourcePath, color.RGBA{R: 40, G: 80, B: 120, A: 255})
	contents, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	recorder := Recorder{
		Store: s, Paths: paths, Screen: repeatedScreen{contents: contents}, Text: fakeVision{},
		Context: accessibilityText{}, Comparer: &FrameComparer{MaxSilence: time.Hour},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recorder.captureScreen(ctx)
	recorder.captureScreen(ctx)

	events, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("duplicate frame created %d indexed events, want 1", len(events))
	}
	files, err := os.ReadDir(paths.Screenshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("duplicate frame left %d screenshot files, want 1", len(files))
	}
}

func TestRecorderRecoversAfterTransientScreenFailure(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	screen := &flakyScreen{}
	recorder := Recorder{
		Store: s, Paths: paths, CaptureScreen: true, ScreenInterval: 2 * time.Millisecond,
		Screen: screen, Text: fakeVision{}, Context: fakeContext{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recordCtx, cancel := context.WithTimeout(ctx, 12*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}
	if screen.attempts.Load() < 2 {
		t.Fatal("screen capture was not retried")
	}
	events, err := s.Search(ctx, store.SearchOptions{Query: "screen text"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("recorder did not recover after transient screen failure")
	}
}

func TestRecorderHandlesDisplayHotplugBetweenCaptures(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sourcePath := filepath.Join(t.TempDir(), "display.jpg")
	writeSolidJPEG(t, sourcePath, color.RGBA{R: 60, G: 90, B: 120, A: 255})
	contents, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	recorder := Recorder{
		Store: s, Paths: paths, Screen: &hotplugScreen{contents: contents},
		Text: fakeVision{}, Context: accessibilityText{}, Comparer: &FrameComparer{MaxSilence: time.Hour},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recorder.captureScreen(ctx) // display 1
	recorder.captureScreen(ctx) // displays 1 and 2
	recorder.captureScreen(ctx) // display 2

	events, err := s.Search(ctx, store.SearchOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	displays := map[uint32]int{}
	for _, event := range events {
		displays[event.DisplayID]++
	}
	if displays[1] != 1 || displays[2] != 1 || len(events) != 2 {
		t.Fatalf("hotplug capture produced unexpected display events: %#v", events)
	}
}

func TestRecorderPreservesMediaAfterProcessorFailures(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sourcePath := filepath.Join(t.TempDir(), "source.jpg")
	writeSolidJPEG(t, sourcePath, color.White)
	contents, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	screenRecorder := Recorder{
		Store: s, Paths: paths, Screen: repeatedScreen{contents: contents}, Text: failingText{},
		Comparer: &FrameComparer{}, Logger: logger,
	}
	screenRecorder.captureScreen(ctx)

	audioRecorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 2 * time.Millisecond,
		Audio: fakeAudio{}, Transcriber: failingTranscriber{}, Logger: logger,
	}
	recordCtx, cancel := context.WithTimeout(ctx, 7*time.Millisecond)
	defer cancel()
	if err := audioRecorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	events, err := s.Search(ctx, store.SearchOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("processor failures lost events: %#v", events)
	}
	seen := map[store.Kind]bool{}
	for _, event := range events {
		if _, err := os.Stat(event.MediaPath); err != nil {
			t.Errorf("processor failure lost %s media: %v", event.Kind, err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(event.Metadata, &metadata); err != nil {
			t.Fatal(err)
		}
		if _, ok := metadata["processor_error"]; !ok {
			t.Errorf("%s event omitted processor error: %s", event.Kind, event.Metadata)
		}
		seen[event.Kind] = true
	}
	if !seen[store.KindScreen] || !seen[store.KindAudio] {
		t.Fatalf("expected preserved screen and audio events, got %#v", seen)
	}
}

// A failed Accessibility read used to erase App and Window even though the
// frontmost application name never depended on Accessibility at all. Attribution
// must survive the failure, and the failure must still be recorded.
func TestRecorderAttributesEventsWhenAccessibilityFails(t *testing.T) {
	ctx := context.Background()
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()

	recorder := Recorder{
		Store: s, Paths: paths, Screen: &fakeScreen{}, Text: fakeVision{},
		Context: degradedContext{}, Comparer: &FrameComparer{}, Logger: logger,
	}
	recorder.captureScreen(ctx)

	events, err := s.Search(ctx, store.SearchOptions{App: "Comet", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("degraded snapshot lost app attribution: %#v", events)
	}
	if events[0].Window != "Booking — Shashi Hotel" {
		t.Errorf("window title = %q, want the window-list title", events[0].Window)
	}
	metadata := unmarshalMetadata(t, events[0].Metadata)
	if metadata["attribution_source"] != "window_list" {
		t.Errorf("attribution_source = %v, want window_list", metadata["attribution_source"])
	}
	// app_source is recorded alongside attribution_source, not merged into it:
	// the source that named the app and the source that supplied the title are
	// separately diagnosable.
	if metadata["app_source"] != "window_list" {
		t.Errorf("app_source = %v, want window_list", metadata["app_source"])
	}
	if metadata["accessibility_trusted"] != true {
		t.Errorf("accessibility_trusted = %v, want true", metadata["accessibility_trusted"])
	}
	if _, ok := metadata["accessibility_error"]; !ok {
		t.Errorf("degraded snapshot dropped accessibility_error: %s", events[0].Metadata)
	}
}

// A degraded snapshot is not a failed one: Accessibility text that did arrive
// must still be preserved in metadata.
func TestRecorderPreservesAccessibilityTextFromDegradedSnapshot(t *testing.T) {
	ctx := context.Background()
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()

	recorder := Recorder{
		Store: s, Paths: paths, Screen: &fakeScreen{}, Text: fakeVision{},
		Context: degradedWithTextContext{}, Comparer: &FrameComparer{}, Logger: logger,
	}
	recorder.captureScreen(ctx)

	events, err := s.Search(ctx, store.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one event, got %#v", events)
	}
	metadata := unmarshalMetadata(t, events[0].Metadata)
	if metadata["accessibility_text"] != "Accessibility primary text" {
		t.Errorf("degraded snapshot dropped Accessibility text: %s", events[0].Metadata)
	}
	if metadata["accessibility_trusted"] != false {
		t.Errorf("accessibility_trusted = %v, want false", metadata["accessibility_trusted"])
	}
}

// Losing the snapshot entirely must still never lose the frame.
func TestRecorderPreservesMediaWhenSnapshotFailsOutright(t *testing.T) {
	ctx := context.Background()
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()

	recorder := Recorder{
		Store: s, Paths: paths, Screen: &fakeScreen{}, Text: fakeVision{},
		Context: failingContext{}, Comparer: &FrameComparer{}, Logger: logger,
	}
	recorder.captureScreen(ctx)

	events, err := s.Search(ctx, store.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("snapshot failure lost the event: %#v", events)
	}
	if _, err := os.Stat(events[0].MediaPath); err != nil {
		t.Errorf("snapshot failure lost media: %v", err)
	}
	if events[0].App != "" {
		t.Errorf("app = %q, want empty when nothing was resolved", events[0].App)
	}
	metadata := unmarshalMetadata(t, events[0].Metadata)
	if _, ok := metadata["accessibility_error"]; !ok {
		t.Errorf("snapshot failure omitted accessibility_error: %s", events[0].Metadata)
	}
	if _, ok := metadata["accessibility_trusted"]; ok {
		t.Errorf("unknown trust must not be reported: %s", events[0].Metadata)
	}
}

// The escalation is time-based and rate-limited: one loud line per outage, not
// one per frame, and never a claim about trust the recorder cannot support.
func TestRecorderEscalatesSustainedAttributionLossOnce(t *testing.T) {
	start := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	trusted := true
	degraded := ScreenContext{App: "", AccessibilityError: "AX error -25204", Trusted: &trusted}

	var buffer strings.Builder
	recorder := Recorder{Logger: slog.New(slog.NewTextHandler(&buffer, nil))}
	for i := range 40 {
		recorder.noteAttribution(start.Add(time.Duration(i)*time.Second), degraded, nil)
	}
	if got := strings.Count(buffer.String(), "level=ERROR"); got != 1 {
		t.Errorf("escalations = %d, want exactly 1 across a 40s outage:\n%s", got, buffer.String())
	}
	if !strings.Contains(buffer.String(), "window-list fallback") {
		t.Errorf("trusted-but-failing outage reported the wrong remedy:\n%s", buffer.String())
	}

	// A recovery re-arms the escalation so a second outage is loud again.
	recorder.noteAttribution(start.Add(41*time.Second), ScreenContext{App: "Ghostty"}, nil)
	for i := 42; i < 120; i++ {
		recorder.noteAttribution(start.Add(time.Duration(i)*time.Second), degraded, nil)
	}
	if got := strings.Count(buffer.String(), "level=ERROR"); got != 2 {
		t.Errorf("escalations after recovery = %d, want 2:\n%s", got, buffer.String())
	}
}

// The window-list fallback exists so a failed Accessibility read still yields an
// app. Escalating that as "indexed without an app" would fire on every routine
// fallback and contradict the events actually being written.
func TestRecorderDoesNotReportMissingAppWhenTheFallbackSuppliedOne(t *testing.T) {
	start := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	trusted := true
	attributed := ScreenContext{
		App: "Comet", Window: "Home / X", TitleSource: "window_list",
		Trusted: &trusted, AccessibilityError: "AX error -25204",
	}

	var buffer strings.Builder
	recorder := Recorder{Logger: slog.New(slog.NewTextHandler(&buffer, nil))}
	for i := range 40 {
		recorder.noteAttribution(start.Add(time.Duration(i)*time.Second), attributed, nil)
	}
	if strings.Contains(buffer.String(), "indexed without an app") {
		t.Errorf("claimed no app while App=%q:\n%s", attributed.App, buffer.String())
	}
	if strings.Contains(buffer.String(), "level=ERROR") {
		t.Errorf("a covered fallback must not escalate to error:\n%s", buffer.String())
	}
	// It is still reported: the Accessibility text is genuinely lost.
	if !strings.Contains(buffer.String(), "window-list fallback") {
		t.Errorf("sustained Accessibility failure went unreported:\n%s", buffer.String())
	}
}

func TestRecorderDoesNotClaimRevokedTrustWhenTrustIsUnknown(t *testing.T) {
	start := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	var buffer strings.Builder
	recorder := Recorder{Logger: slog.New(slog.NewTextHandler(&buffer, nil))}
	for i := range 40 {
		recorder.noteAttribution(start.Add(time.Duration(i)*time.Second),
			ScreenContext{}, errors.New("snapshot unavailable"))
	}
	if strings.Contains(buffer.String(), "re-grant") {
		t.Errorf("unknown trust was reported as revoked:\n%s", buffer.String())
	}
	if !strings.Contains(buffer.String(), "lumi doctor") {
		t.Errorf("total snapshot failure gave no actionable remedy:\n%s", buffer.String())
	}
}

func newRecorderFixture(t *testing.T) (*store.Store, config.Paths, *slog.Logger) {
	t.Helper()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	return s, paths, slog.New(slog.NewTextHandler(io.Discard, nil))
}

func unmarshalMetadata(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func TestRecorderIndexesSystemAndMicrophoneAudioSeparately(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: fakeTranscriber{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recordCtx, cancel := context.WithTimeout(ctx, 15*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}
	events, err := s.Search(ctx, store.SearchOptions{Query: "audio transcript"})
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]bool{}
	for _, event := range events {
		sources[event.AudioSource] = true
	}
	if !sources["system"] || !sources["microphone"] {
		t.Fatalf("expected separately indexed system and microphone audio, got %#v", events)
	}

	// The two frames of a chunk are stamped with one shared `now`, so their
	// stored captured_at is byte-identical. store.CollapseAudioTracks groups on
	// exactly that string, so pin the guarantee here: for at least one chunk a
	// system and a microphone row must share the RFC3339Nano key. This is
	// currently only implicit in recorder.go's audioLoop.
	bySource := map[string]map[string]bool{"system": {}, "microphone": {}}
	for _, event := range events {
		if set, ok := bySource[event.AudioSource]; ok {
			set[event.CapturedAt.UTC().Format(time.RFC3339Nano)] = true
		}
	}
	var shared bool
	for key := range bySource["system"] {
		if bySource["microphone"][key] {
			shared = true
			break
		}
	}
	if !shared {
		t.Fatalf("system and microphone rows of a chunk must share a captured_at, got %#v", events)
	}
}

// TestRecorderKeepsOneAudioStreamOpenAcrossChunks pins the shape of the fix for
// the audio lost between chunks. Capture used to be a blocking call per chunk,
// so the tap was closed for the whole of transcription, indexing, attribution,
// and the next stream's setup — about two seconds in thirty, landing
// mid-sentence. Reopening per chunk would reintroduce exactly that gap.
func TestRecorderKeepsOneAudioStreamOpenAcrossChunks(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	audio := &countingAudio{}
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: audio, Transcriber: fakeTranscriber{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recordCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}
	events, err := s.Search(ctx, store.SearchOptions{Limit: store.MaxSearchLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 4 {
		t.Fatalf("expected several audio chunks to be indexed, got %d", len(events))
	}
	if opens := audio.opens.Load(); opens != 1 {
		t.Fatalf("capture must stay open across chunks, but the stream was opened %d times", opens)
	}
}

// TestRecorderStampsAudioChunksFromTheirOwnStart pins captured_at to the instant
// a chunk's audio began. Reading the clock between chunks instead made every
// timestamp absorb however long the previous chunk took to process, which is
// what let indexed chunks sit 32s apart while each held 30s of sound.
func TestRecorderStampsAudioChunksFromTheirOwnStart(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const chunk = 5 * time.Millisecond
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: chunk,
		Audio: dualAudio{}, Transcriber: slowTranscriber{delay: 15 * time.Millisecond},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recordCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}
	events, err := s.Search(ctx, store.SearchOptions{Limit: store.MaxSearchLimit})
	if err != nil {
		t.Fatal(err)
	}
	stamps := make([]time.Time, 0, len(events))
	for _, event := range events {
		if len(stamps) == 0 || !event.CapturedAt.Equal(stamps[len(stamps)-1]) {
			stamps = append(stamps, event.CapturedAt)
		}
	}
	if len(stamps) < 3 {
		t.Fatalf("expected at least three distinct chunk timestamps, got %d", len(stamps))
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap != chunk {
			t.Fatalf("chunk %d is %s after its predecessor, but each chunk covers %s; "+
				"the difference is audio that was never captured", i, gap, chunk)
		}
	}
}

func TestRecorderIndexesNativeAudioCompletedAfterCancellation(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transcriber := &countingTranscriber{}
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 30 * time.Second,
		Audio: completionAfterCancelAudio{}, Transcriber: transcriber,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recordCtx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}
	if transcriber.calls.Load() != 0 {
		t.Fatal("transcriber should be skipped after recording cancellation")
	}
	events, err := s.Search(ctx, store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].AudioSource != "system" {
		t.Fatalf("late native audio was not indexed: %#v", events)
	}
	if _, err := os.Stat(events[0].MediaPath); err != nil {
		t.Fatalf("late native audio was not preserved: %v", err)
	}
	if !strings.Contains(string(events[0].Metadata), "transcription skipped after capture stopped") {
		t.Fatalf("late native audio omitted cancellation provenance: %s", events[0].Metadata)
	}
}

// bleedAudio produces a chunk where the microphone re-recorded the machine and
// the room also spoke, which is the shape a third of real bleed chunks have.
type bleedTranscriber struct{}

func (bleedTranscriber) Transcribe(_ context.Context, path string) (Transcription, error) {
	const machine = "The deployment finished with no warnings."
	if strings.Contains(path, "system") {
		return Transcription{
			Text: machine,
			Segments: []TimedSegment{{StartMS: 0, EndMS: 2000, Text: machine, Confidence: 0.9,
				Runs: []TimedRun{{StartMS: 0, EndMS: 2000, Text: machine, Confidence: 0.9}}}},
		}, nil
	}
	const heard = machine + " Good, then let us ship it."
	return Transcription{
		Text: heard,
		Segments: []TimedSegment{{StartMS: 30, EndMS: 4000, Text: heard, Confidence: 0.9,
			Runs: []TimedRun{
				{StartMS: 30, EndMS: 2030, Text: machine, Confidence: 0.9},
				{StartMS: 2030, EndMS: 4000, Text: " Good, then let us ship it.", Confidence: 0.9},
			}}},
	}, nil
}

// failingSegments rejects every attribution write.
type failingSegments struct{ calls atomic.Int64 }

func (f *failingSegments) ReplaceChunkSegments(context.Context, string, []store.Segment) error {
	f.calls.Add(1)
	return errors.New("segment write failed")
}

func recorderPaths(t *testing.T) (config.Paths, *store.Store) {
	t.Helper()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return paths, s
}

// TestRecorderStoresAttributionAlongsideTheAudio covers the whole chunk path:
// both tracks indexed as before, plus segments saying which words the machine
// produced and which the room did.
func TestRecorderStoresAttributionAlongsideTheAudio(t *testing.T) {
	ctx := context.Background()
	paths, s := recorderPaths(t)

	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 8 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: bleedTranscriber{},
	}
	recordCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	events, err := s.Search(ctx, store.SearchOptions{Kind: store.KindAudio, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("got %d audio events, want both tracks", len(events))
	}
	// Read every chunk rather than the newest one. Shutdown cancels the final
	// chunk before transcription, so it is indexed with empty text and produces
	// only a silence marker — asserting against it would be testing the wrong
	// thing.
	segments, err := s.SegmentsBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) == 0 {
		t.Fatal("chunks were indexed but never attributed")
	}

	var sawInternal, sawExternal, sawBleed bool
	for _, segment := range segments {
		if segment.EventID == 0 {
			t.Errorf("segment %d is not tied to any event", segment.Seq)
		}
		switch {
		case segment.IsBleed:
			sawBleed = true
			if segment.SourceTrack != "microphone" {
				t.Errorf("bleed came from the %s track", segment.SourceTrack)
			}
			if segment.Origin != store.OriginInternal {
				t.Errorf("bleed labelled %s, want internal", segment.Origin)
			}
		case segment.Origin == store.OriginInternal:
			sawInternal = true
		case segment.Origin == store.OriginExternal:
			sawExternal = true
			if !strings.Contains(segment.Text, "ship it") {
				t.Errorf("external segment is %q, want the room's own words", segment.Text)
			}
		}
	}
	if !sawInternal {
		t.Error("the machine's own track produced no internal segment")
	}
	if !sawBleed {
		t.Error("the microphone's re-recording was not marked as bleed")
	}
	if !sawExternal {
		t.Error("the room's speech was not preserved as external")
	}
}

// TestSegmentWriteFailureStillIndexesTheAudio is the invariant test. Captured
// media and its transcript must survive any downstream failure — attribution is a
// second opinion about audio that is already safely indexed, and a chunk left
// unattributed is picked up by the backfill's derived work queue.
func TestSegmentWriteFailureStillIndexesTheAudio(t *testing.T) {
	ctx := context.Background()
	paths, s := recorderPaths(t)
	segments := &failingSegments{}

	recorder := Recorder{
		Store: s, Segments: segments, Paths: paths, CaptureAudio: true,
		AudioChunk: 8 * time.Millisecond,
		Audio:      dualAudio{}, Transcriber: bleedTranscriber{},
	}
	recordCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatalf("a failing segment write brought down the recorder: %v", err)
	}
	if segments.calls.Load() == 0 {
		t.Fatal("the attribution write was never attempted")
	}

	events, err := s.Search(ctx, store.SearchOptions{Query: "deployment", Kind: store.KindAudio})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("audio was lost when attribution failed")
	}
	for _, event := range events {
		if event.Text == "" {
			t.Error("an indexed event lost its transcript")
		}
		if _, err := os.Stat(event.MediaPath); err != nil {
			t.Errorf("captured media went missing: %v", err)
		}
	}

	// The chunk must remain in the backfill's queue, which is what makes the
	// failure recoverable without any retry loop in the recorder.
	missing, err := s.ChunksMissingSegments(ctx, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) == 0 {
		t.Error("the unattributed chunk is not queued for backfill")
	}
}

// TestMicrophoneOnlyChunkIsAllExternal covers the stated requirement that a
// microphone chunk with no system counterpart is entirely the room's.
func TestMicrophoneOnlyChunkIsAllExternal(t *testing.T) {
	ctx := context.Background()
	paths, s := recorderPaths(t)

	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 8 * time.Millisecond,
		Audio: fakeAudio{}, Transcriber: fakeTranscriber{},
	}
	recordCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	events, err := s.Search(ctx, store.SearchOptions{Kind: store.KindAudio, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no audio was indexed")
	}
	segments, err := s.SegmentsBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) == 0 {
		t.Fatal("microphone-only chunks produced no segments")
	}
	for _, segment := range segments {
		if segment.Origin != store.OriginExternal {
			t.Errorf("segment %q labelled %s, want external", segment.Text, segment.Origin)
		}
		if segment.IsBleed {
			t.Error("a chunk with no machine audio produced bleed")
		}
	}
}

// silentTranscriber is a chunk in which nobody spoke and nothing played: the
// recognizer returns no words and no error, which is the overwhelmingly common
// case in a real index.
type silentTranscriber struct{}

func (silentTranscriber) Transcribe(context.Context, string) (Transcription, error) {
	return Transcription{}, nil
}

// TestSilentChunkIsRecordedAsAttributed is the recorder half of the drainable
// work queue.
//
// A chunk holding no words produces no speech to attribute, and used to produce
// no row either — leaving it indistinguishable from a chunk the recorder never
// got to. Since silence is the common case, the backfill queue would fill with
// chunks it could never finish, and coverage would report them as permanent
// holes in every transcript.
func TestSilentChunkIsRecordedAsAttributed(t *testing.T) {
	ctx := context.Background()
	paths, s := recorderPaths(t)

	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 8 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: silentTranscriber{},
	}
	recordCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	missing, err := s.ChunksMissingSegments(ctx, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("%d silent chunks stayed on the backfill queue: %v", len(missing), missing)
	}
	segments, err := s.SegmentsBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) == 0 {
		t.Fatal("a silent chunk recorded nothing at all")
	}
	for _, segment := range segments {
		if segment.Origin != store.OriginSilent {
			t.Errorf("silent chunk produced origin %q", segment.Origin)
		}
		if segment.Text != "" {
			t.Errorf("silence marker carries text %q", segment.Text)
		}
		if segment.EventID == 0 {
			t.Error("silence marker is not tied to any event")
		}
	}
}

// TestFailedTranscriptionIsNeverRecordedAsSilence is the other half of the
// silence marker: a recognizer that failed and one that heard nothing return the
// same empty transcript, and the two need opposite conclusions.
//
// Writing the marker for a failure asserts the room was quiet on the strength of
// evidence that was never gathered, and — because the marker drains the chunk
// from the derived work queue — makes that assertion permanent. Under streaming
// capture this is not a rare case: every chunk still queued when the recorder
// stops takes the "transcription skipped after capture stopped" path.
func TestFailedTranscriptionIsNeverRecordedAsSilence(t *testing.T) {
	ctx := context.Background()
	paths, s := recorderPaths(t)

	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 8 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: failingTranscriber{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recordCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	segments, err := s.SegmentsBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range segments {
		if segment.Origin == store.OriginSilent {
			t.Errorf("a chunk whose transcription failed was labelled %q", segment.Origin)
		}
	}
	// The audio is still indexed, so the chunk must stay on the queue rather than
	// leaving it with a verdict nothing ever reached.
	events, err := s.Search(ctx, store.SearchOptions{Kind: store.KindAudio, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no audio was indexed, so the test proves nothing")
	}
	missing, err := s.ChunksMissingSegments(ctx, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) == 0 {
		t.Error("a chunk whose transcription failed drained from the backfill queue")
	}
}

// microphoneFailureTranscriber transcribes the machine's audio and fails on the
// room's, which is the partial failure the silence gate must not swallow.
type microphoneFailureTranscriber struct{}

func (microphoneFailureTranscriber) Transcribe(_ context.Context, path string) (Transcription, error) {
	if strings.Contains(path, "system") {
		const machine = "The deployment finished with no warnings."
		return Transcription{
			Text: machine,
			Segments: []TimedSegment{{StartMS: 0, EndMS: 2000, Text: machine, Confidence: 0.9,
				Runs: []TimedRun{{StartMS: 0, EndMS: 2000, Text: machine, Confidence: 0.9}}}},
		}, nil
	}
	return Transcription{}, errors.New("transcribe microphone: recognizer unavailable")
}

// TestOneFailedTrackStillAttributesWhatTranscribed keeps the silence gate narrow.
//
// Only the silent verdict reads an empty transcript as evidence about the world;
// every other path labels what it actually has. A system track that transcribed
// is machine audio whether or not the microphone's recognizer worked, so
// refusing to attribute the whole chunk on any failure would throw away a sound
// verdict and leave the chunk queued for a backfill that would reach the same
// conclusion.
func TestOneFailedTrackStillAttributesWhatTranscribed(t *testing.T) {
	ctx := context.Background()
	paths, s := recorderPaths(t)

	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 8 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: microphoneFailureTranscriber{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	recordCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	segments, err := s.SegmentsBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) == 0 {
		t.Fatal("a chunk whose system track transcribed produced no attribution")
	}
	for _, segment := range segments {
		if segment.Origin != store.OriginInternal {
			t.Errorf("segment %q labelled %s, want internal", segment.Text, segment.Origin)
		}
	}
}

// fakeAudioOutputs reports two processes holding an output stream: one macOS
// could name and one it could not.
type fakeAudioOutputs struct{}

// Active honours cancellation for the same reason fakeContext.Snapshot does:
// macosnative.AudioProcesses checks ctx.Err() first.
func (fakeAudioOutputs) Active(ctx context.Context) ([]AudioProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []AudioProcess{
		{PID: 812, BundleID: "com.perplexity.comet", Name: "Comet"},
		{PID: 991},
	}, nil
}

// silentAudioOutputs reports that no process held an output stream, which is a
// finding rather than a failure and must not be recorded as one.
type silentAudioOutputs struct{}

func (silentAudioOutputs) Active(context.Context) ([]AudioProcess, error) {
	return nil, nil
}

type failingAudioOutputs struct{}

func (failingAudioOutputs) Active(context.Context) ([]AudioProcess, error) {
	return nil, errors.New("list processes with active audio output: CoreAudio unavailable")
}

// audioEventsFrom returns every indexed audio row. Tests scan all of them
// because the final chunk of a bounded run is always cancelled mid-flight.
func audioEventsFrom(t *testing.T, s *store.Store) []store.Event {
	t.Helper()
	events, err := s.Search(context.Background(), store.SearchOptions{Kind: store.KindAudio, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// TestAudioEventsCarryFocusedAppAttribution pins the headline behaviour: an
// audio row now answers "what was the user working in", the same question
// events.app answers for every screen row.
func TestAudioEventsCarryFocusedAppAttribution(t *testing.T) {
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: fakeTranscriber{}, Context: fakeContext{},
		AudioOutputs: silentAudioOutputs{}, Logger: logger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := recorder.Run(ctx); err != nil {
		t.Fatal(err)
	}
	events := audioEventsFrom(t, s)
	if len(events) == 0 {
		t.Fatal("expected indexed audio events")
	}
	attributed := 0
	for _, event := range events {
		if event.App == "" {
			continue
		}
		attributed++
		if event.App != "Test App" || event.Window != "Test Window" {
			t.Errorf("audio row %d attributed to %q/%q, want Test App/Test Window",
				event.ID, event.App, event.Window)
		}
		metadata := unmarshalMetadata(t, event.Metadata)
		if metadata["app_source"] != "workspace" {
			t.Errorf("audio row %d app_source = %v, want workspace", event.ID, metadata["app_source"])
		}
	}
	if attributed == 0 {
		t.Fatal("no audio event carried an app, want the focused application stamped")
	}
}

// TestBothAudioTracksShareOneAttribution is the property that keeps collapse
// stable. CollapseAudioTracks picks a survivor by (hasText, isSystem, runeLen,
// -id), so if the two tracks of a chunk could carry different apps, which one a
// search reports would depend on which track happened to transcribe.
func TestBothAudioTracksShareOneAttribution(t *testing.T) {
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: bleedTranscriber{}, Context: fakeContext{},
		AudioOutputs: fakeAudioOutputs{}, Logger: logger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := recorder.Run(ctx); err != nil {
		t.Fatal(err)
	}
	events := audioEventsFrom(t, s)
	byChunk := map[string][]store.Event{}
	for _, event := range events {
		key := event.CapturedAt.UTC().Format(time.RFC3339Nano)
		byChunk[key] = append(byChunk[key], event)
	}
	pairs := 0
	for key, chunk := range byChunk {
		if len(chunk) < 2 {
			continue
		}
		pairs++
		for _, event := range chunk[1:] {
			if event.App != chunk[0].App || event.Window != chunk[0].Window {
				t.Errorf("chunk %s: track %q attributed to %q/%q but track %q to %q/%q",
					key, chunk[0].AudioSource, chunk[0].App, chunk[0].Window,
					event.AudioSource, event.App, event.Window)
			}
		}
	}
	if pairs == 0 {
		t.Fatal("expected at least one chunk with both tracks indexed")
	}
	// The survivor of a collapse must carry the same attribution as the pair it
	// stood for, which is what makes the stamp reportable at all.
	survivors, _ := store.CollapseAudioTracks(events)
	for _, survivor := range survivors {
		if survivor.App != "Test App" {
			t.Errorf("collapsed chunk survivor attributed to %q, want Test App", survivor.App)
		}
	}
}

// TestAudioMetadataRecordsActiveOutputProcesses covers the fact no other source
// can supply: which application held the audio output, as against which one the
// user was looking at.
func TestAudioMetadataRecordsActiveOutputProcesses(t *testing.T) {
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: fakeTranscriber{}, Context: fakeContext{},
		AudioOutputs: fakeAudioOutputs{}, Logger: logger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := recorder.Run(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range audioEventsFrom(t, s) {
		metadata := unmarshalMetadata(t, event.Metadata)
		raw, ok := metadata["active_audio_output_processes"]
		if !ok {
			continue
		}
		found = true
		processes, ok := raw.([]any)
		if !ok || len(processes) != 2 {
			t.Fatalf("active_audio_output_processes = %#v, want two entries", raw)
		}
		first, ok := processes[0].(map[string]any)
		if !ok {
			t.Fatalf("active output process[0] = %#v, want an object", processes[0])
		}
		if first["name"] != "Comet" || first["bundle_id"] != "com.perplexity.comet" {
			t.Errorf("active output process[0] = %#v, want Comet", first)
		}
		// A process macOS could not name keeps its pid and omits the rest,
		// rather than being dropped: it genuinely held a stream.
		second, ok := processes[1].(map[string]any)
		if !ok {
			t.Fatalf("active output process[1] = %#v, want an object", processes[1])
		}
		if _, present := second["name"]; present {
			t.Errorf("unnamed process carried a name: %#v", second)
		}
		if second["pid"] != float64(991) {
			t.Errorf("active output process[1] pid = %v, want 991", second["pid"])
		}
	}
	if !found {
		t.Fatal("no audio event recorded active_audio_output_processes")
	}
}

// TestSilentMachineOmitsActiveOutputProcesses keeps "no process held a stream"
// distinct from "Lumi could not tell". An empty list written as [] would read as
// the latter, and an agent cannot tell them apart after the fact.
func TestSilentMachineOmitsActiveOutputProcesses(t *testing.T) {
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: fakeTranscriber{}, Context: fakeContext{},
		AudioOutputs: silentAudioOutputs{}, Logger: logger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := recorder.Run(ctx); err != nil {
		t.Fatal(err)
	}
	events := audioEventsFrom(t, s)
	if len(events) == 0 {
		t.Fatal("expected indexed audio events")
	}
	for _, event := range events {
		metadata := unmarshalMetadata(t, event.Metadata)
		if _, present := metadata["active_audio_output_processes"]; present {
			t.Errorf("silent machine recorded active output processes: %v",
				metadata["active_audio_output_processes"])
		}
		if _, present := metadata["active_audio_output_processes_error"]; present {
			t.Errorf("silent machine recorded an error: %v", metadata["active_audio_output_processes_error"])
		}
	}
}

// TestAudioSurvivesFailedAttribution is the never-lose-media invariant on this
// path: attribution is a nicety, the recording is not.
func TestAudioSurvivesFailedAttribution(t *testing.T) {
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: fakeTranscriber{}, Context: failingContext{},
		AudioOutputs: failingAudioOutputs{}, Logger: logger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := recorder.Run(ctx); err != nil {
		t.Fatal(err)
	}
	events := audioEventsFrom(t, s)
	if len(events) == 0 {
		t.Fatal("a failed attribution read cost the chunk its audio events")
	}
	for _, event := range events {
		if event.MediaPath == "" {
			t.Errorf("audio row %d lost its media path", event.ID)
		}
		if event.App != "" {
			t.Errorf("audio row %d claimed app %q from a failed snapshot", event.ID, event.App)
		}
		metadata := unmarshalMetadata(t, event.Metadata)
		if metadata["accessibility_error"] == nil {
			t.Errorf("audio row %d recorded no accessibility_error", event.ID)
		}
		if metadata["active_audio_output_processes_error"] == nil {
			t.Errorf("audio row %d recorded no active_audio_output_processes_error", event.ID)
		}
	}
}

// TestAudioAttributionIsOptional pins that a recorder wired with neither seam
// still captures. Both are nil in the zero value and under callers that supply
// only what they need.
func TestAudioAttributionIsOptional(t *testing.T) {
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: fakeTranscriber{}, Logger: logger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := recorder.Run(ctx); err != nil {
		t.Fatal(err)
	}
	events := audioEventsFrom(t, s)
	if len(events) == 0 {
		t.Fatal("expected indexed audio events with no attribution seams wired")
	}
	for _, event := range events {
		if event.App != "" || event.Window != "" {
			t.Errorf("audio row %d invented attribution %q/%q", event.ID, event.App, event.Window)
		}
	}
}

// TestAudioIsReachableByAppFilter states the behaviour change deliberately:
// audio rows now enter the app index, so an app filter returns them.
func TestAudioIsReachableByAppFilter(t *testing.T) {
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: dualAudio{}, Transcriber: fakeTranscriber{}, Context: fakeContext{},
		Logger: logger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := recorder.Run(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := s.Search(context.Background(), store.SearchOptions{
		Kind: store.KindAudio, App: "Test App", Limit: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("an app filter returned no audio, want the chunks captured while that app was focused")
	}
}

// TestShutdownChunkKeepsAttribution covers the chunk a cancelled recording
// finalises on the way down. Its audio is deliberately preserved and indexed, so
// the attribution describing it must survive the same cancellation — reading it
// under the cancelled context left the last chunk of every session unattributed.
func TestShutdownChunkKeepsAttribution(t *testing.T) {
	s, paths, logger := newRecorderFixture(t)
	defer s.Close()
	recorder := Recorder{
		Store: s, Paths: paths, CaptureAudio: true, AudioChunk: 5 * time.Millisecond,
		Audio: completionAfterCancelAudio{}, Transcriber: fakeTranscriber{},
		Context: fakeContext{}, AudioOutputs: fakeAudioOutputs{}, Logger: logger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := recorder.Run(ctx); err != nil {
		t.Fatal(err)
	}
	events := audioEventsFrom(t, s)
	if len(events) == 0 {
		t.Fatal("the shutdown chunk was not indexed at all")
	}
	for _, event := range events {
		if event.App != "Test App" {
			t.Errorf("shutdown chunk row %d attributed to %q, want Test App", event.ID, event.App)
		}
		metadata := unmarshalMetadata(t, event.Metadata)
		if _, present := metadata["accessibility_error"]; present {
			t.Errorf("shutdown chunk row %d recorded accessibility_error %v",
				event.ID, metadata["accessibility_error"])
		}
		if _, present := metadata["active_audio_output_processes"]; !present {
			t.Errorf("shutdown chunk row %d lost its active output processes", event.ID)
		}
	}
}
