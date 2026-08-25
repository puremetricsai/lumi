package capture

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/store"

	"github.com/puremetricsai/lumi/internal/transcript"
	"github.com/puremetricsai/lumi/internal/wav"
)

// A meter needs both figures. A chunk of near-silence containing one door slam
// has a high peak and a low median, and a reader given only the peak would draw
// it as a conversation.
func TestSummarizeEnvelopeSeparatesPeakFromTypical(t *testing.T) {
	quietWithOneSlam := []float64{-90, -88, -91, -12, -89, -90, -92}
	peak, median, err := summarizeEnvelope(quietWithOneSlam)
	if err != nil {
		t.Fatalf("summarizeEnvelope: %v", err)
	}
	if peak != -12 {
		t.Errorf("peak = %v, want the loudest window -12", peak)
	}
	if median != -90 {
		t.Errorf("median = %v, want a typical window near the quiet floor, got the slam", median)
	}
}

func TestSummarizeEnvelopeMedian(t *testing.T) {
	for _, c := range []struct {
		name           string
		envelope       []float64
		peak, median   float64
		wantMeasurable bool
	}{
		{"odd count takes the middle", []float64{-30, -10, -20}, -10, -20, true},
		{"even count averages the two middles", []float64{-40, -30, -20, -10}, -10, -25, true},
		{"a single window is both", []float64{-33}, -33, -33, true},
		{"digital silence is a real measurement", []float64{-120, -120}, -120, -120, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			peak, median, err := summarizeEnvelope(c.envelope)
			if err != nil {
				t.Fatalf("summarizeEnvelope: %v", err)
			}
			if peak != c.peak || median != c.median {
				t.Errorf("= (peak %v, median %v), want (%v, %v)", peak, median, c.peak, c.median)
			}
		})
	}
}

// An unmeasurable file must not report a level of zero. Zero dBFS is full
// scale, so a meter fed that would peg at maximum for a file nobody could read.
func TestSummarizeEnvelopeRefusesToInventALevel(t *testing.T) {
	if _, _, err := summarizeEnvelope(nil); err == nil {
		t.Error("an empty envelope produced a level; want an error")
	}
	if _, _, err := summarizeEnvelope([]float64{}); err == nil {
		t.Error("a zero-length envelope produced a level; want an error")
	}
	if _, _, err := summarizeEnvelope([]float64{math.NaN(), -40}); err == nil {
		t.Error("a NaN window produced a level; want an error")
	}
}

// An interval of digital silence must say so, because the alternative is every
// reader comparing against the floor itself. A track that is not being listened
// to reads exactly like a quiet room otherwise.
func TestLevelFromReportsSilence(t *testing.T) {
	measuredAt := time.Now().UTC()
	silent, ok := levelFrom("microphone", []float64{0, 0, 0}, measuredAt)
	if !ok {
		t.Fatal("silence produced no level; silence is a measurement")
	}
	if !silent.Silent {
		t.Errorf("peak %v was not reported silent; want silent at %v",
			silent.PeakDBFS, wav.SilenceFloorDBFS)
	}
	quiet, ok := levelFrom("microphone", []float64{1e-9, 1e-9}, measuredAt)
	if !ok {
		t.Fatal("energy produced no level")
	}
	if quiet.Silent {
		t.Errorf("a room at %v dBFS was reported silent; only the floor is",
			quiet.PeakDBFS)
	}
}

// A drained interval must reach the sink as decibels this repository already
// defines. Nothing else connects the live meter to internal/wav, so this is what
// stops the two drifting.
func TestLevelFromConvertsEnergyThroughWav(t *testing.T) {
	measuredAt := time.Now().UTC()
	quietWithOneSlam := []float64{1e-9, 1e-9, 0.25, 1e-9, 1e-9}
	level, ok := levelFrom("microphone", quietWithOneSlam, measuredAt)
	if !ok {
		t.Fatal("energy produced no level")
	}
	if level.Source != "microphone" {
		t.Errorf("source = %q, want microphone", level.Source)
	}
	if want := wav.DBFSFromMeanSquare(0.25); level.PeakDBFS != want {
		t.Errorf("peak = %v, want %v — the loudest window, through internal/wav", level.PeakDBFS, want)
	}
	if want := wav.DBFSFromMeanSquare(1e-9); level.MedianDBFS != want {
		t.Errorf("median = %v, want %v — a typical window, not the slam", level.MedianDBFS, want)
	}
	if level.WindowMS != transcript.EnvelopeWindowMS {
		t.Errorf("window = %dms, want transcript.EnvelopeWindowMS", level.WindowMS)
	}
	// Five 100ms windows is half a second of sound, and it ended now.
	if level.DurationMS != 500 {
		t.Errorf("duration = %dms, want the 500ms the windows actually cover", level.DurationMS)
	}
	if got := measuredAt.Sub(level.CapturedAt); got != 500*time.Millisecond {
		t.Errorf("captured %v before the drain, want the start of the sound it summarises", got)
	}
}

// Silence is a measurement and must move the meter to its floor. Nothing at all
// is the different case below, and conflating them would draw a dead microphone
// as a quiet room.
func TestLevelFromReportsSilenceButNotAbsence(t *testing.T) {
	level, ok := levelFrom("system", []float64{0, 0, 0}, time.Now().UTC())
	if !ok {
		t.Fatal("digital silence produced no level; it is a real measurement")
	}
	if level.PeakDBFS != wav.SilenceFloorDBFS {
		t.Errorf("peak = %v, want the silence floor %v", level.PeakDBFS, wav.SilenceFloorDBFS)
	}
	if _, ok := levelFrom("system", nil, time.Now().UTC()); ok {
		t.Error("an interval that completed no window produced a level; want none")
	}
}

// levelStream is a track stream that also reports sound, so the recorder's live
// metering is exercised without a Mac, a permission, or a capture framework.
type levelStream struct {
	*trackStream
	sampled atomic.Int32
}

func (s *levelStream) SampleLevels() (system, microphone []float64, err error) {
	s.sampled.Add(1)
	// One window of ordinary speech on each track, every drain.
	return []float64{0.01}, []float64{0.02}, nil
}

type levelAudio struct{ stream *levelStream }

func (a *levelAudio) Open(ctx context.Context, directory string, chunk time.Duration) (AudioStream, error) {
	a.stream = &levelStream{trackStream: newTrackStream(directory, chunk, "system", "microphone")}
	return a.stream, nil
}

// The meters must move while a chunk is still recording. This is the whole point
// of the live path: the previous design measured a finished file, so nothing
// reached a meter until a chunk closed — 30 seconds by default, which cannot
// tell a user whether their microphone is working.
func TestRecorderReportsLevelsWhileAChunkIsStillRecording(t *testing.T) {
	var mu sync.Mutex
	bySource := map[string]AudioLevel{}
	audio := &levelAudio{}

	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.Context(), paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A chunk far longer than this test runs, so a level that arrives provably
	// did not come from a finished chunk.
	r := Recorder{
		Store: s, Paths: paths, CaptureScreen: false, CaptureAudio: true,
		AudioChunk: time.Hour, Audio: audio, Transcriber: fakeTranscriber{},
		Levels: func(level AudioLevel) {
			mu.Lock()
			defer mu.Unlock()
			bySource[level.Source] = level
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		enough := len(bySource) == 2
		mu.Unlock()
		if enough {
			break
		}
		time.Sleep(LevelInterval / 2)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	for _, source := range []string{"system", "microphone"} {
		level, ok := bySource[source]
		if !ok {
			t.Errorf("no level ever arrived for %s; the meter would never move", source)
			continue
		}
		if level.PeakDBFS <= wav.SilenceFloorDBFS {
			t.Errorf("%s peak = %v, want real sound above the floor", source, level.PeakDBFS)
		}
	}
	if audio.stream != nil && audio.stream.sampled.Load() == 0 {
		t.Error("the recorder never drained the live meters")
	}
}

// failingSampler is a stream whose meters cannot be read, which is what a
// session closing under the level goroutine looks like.
type failingSampler struct {
	*trackStream
	calls atomic.Int32
}

func (s *failingSampler) SampleLevels() (system, microphone []float64, err error) {
	s.calls.Add(1)
	return nil, nil, errors.New("audio capture session is not open")
}

type failingLevelAudio struct{ stream *failingSampler }

func (a *failingLevelAudio) Open(ctx context.Context, directory string, chunk time.Duration) (AudioStream, error) {
	a.stream = &failingSampler{trackStream: newTrackStream(directory, chunk, "system", "microphone")}
	return a.stream, nil
}

// A sampler that fails must cost nothing but the meter. It must not reach the
// sink with an invented level, must not stop trying, and above all must not stop
// capture — the sink is caller-supplied code on the far side of a pipe, and this
// is the contract that keeps a level meter from being able to halt a recording.
func TestRecorderSurvivesAFailingLevelSampler(t *testing.T) {
	var mu sync.Mutex
	var levels []AudioLevel
	audio := &failingLevelAudio{}

	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.Context(), paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r := Recorder{
		Store: s, Paths: paths, CaptureScreen: false, CaptureAudio: true,
		AudioChunk: 25 * time.Millisecond, Audio: audio, Transcriber: fakeTranscriber{},
		Levels: func(level AudioLevel) {
			mu.Lock()
			defer mu.Unlock()
			levels = append(levels, level)
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*LevelInterval)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("a failing level sampler stopped the recording: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(levels) != 0 {
		t.Errorf("a failing sampler produced %d levels; a meter must never invent one", len(levels))
	}
	// Audio still reached the store, which is the property that matters.
	audioRows, err := s.Search(t.Context(), store.SearchOptions{Kind: store.KindAudio})
	if err != nil {
		t.Fatal(err)
	}
	if len(audioRows) == 0 {
		t.Error("no audio was indexed while the level sampler was failing")
	}
	if audio.stream != nil && audio.stream.calls.Load() < 2 {
		t.Errorf("the sampler was called %d times; one failure must not end the meter",
			audio.stream.calls.Load())
	}
}
