package capture

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/puremetricsai/lumi/internal/transcript"
	"github.com/puremetricsai/lumi/internal/wav"
)

// AudioLevel is how loud one track has been over the interval just past.
//
// It exists so a supervising app can draw a level meter without inventing a
// second definition of "level". The figures come from the same windowed envelope
// that decides whether a chunk was silent, at the same resolution
// (transcript.EnvelopeWindowMS) and through the same formula and silence floor
// (internal/wav), so a meter and a silence verdict can never disagree about what
// was heard.
//
// The measurement is live. Sound is summed inside the capture callback as it
// arrives, and drained on a ticker, so a meter fed from this moves with the room
// rather than once per finished chunk. It is measured on the stream before the
// writer downmixes it to Lumi's mono 16kHz WAV, so it will differ slightly from
// the stored file's envelope: the signal is genuinely different, the measurement
// is not.
type AudioLevel struct {
	Source     string    `json:"source"`
	CapturedAt time.Time `json:"captured_at"`
	// PeakDBFS is the loudest window in the interval, and MedianDBFS the typical
	// one. Both are needed: an interval of near-silence with one door slam has a
	// high peak and a low median, and a meter showing only the peak would call
	// that a conversation.
	PeakDBFS   float64 `json:"peak_dbfs"`
	MedianDBFS float64 `json:"median_dbfs"`
	// WindowMS is the resolution the two figures were measured at, so a reader
	// never has to assume it.
	WindowMS int `json:"window_ms"`
	// DurationMS is how much sound the figures summarise, which is how a reader
	// knows how stale the measurement is allowed to get before the meter decays.
	DurationMS int64 `json:"duration_ms"`
	// Silent says the interval reached wav.SilenceFloorDBFS, which is the floor
	// a signal is clamped to rather than a level anything reaches: below it is
	// less sound than a live input's own noise. It is answered here so no reader
	// has to compare against the floor itself — that comparison is the rule, and
	// a second copy of it in another language drifts invisibly.
	//
	// It is a fact about the sound, not a verdict about the source. Nothing
	// playing is what a system track sounds like most of the time; a microphone
	// that reports this is not being listened to. That reading belongs to
	// whoever is drawing the meter.
	Silent bool `json:"silent"`
}

// LevelSink receives one measurement per track per interval. It is called from
// the level goroutine and must not block.
type LevelSink func(AudioLevel)

// ScreenCapture is what one screen tick captured, reported so a supervising app
// can say how many displays are being recorded rather than how many are
// connected. Those are different numbers as soon as a display selection is in
// play, and only the recorder knows the first one.
type ScreenCapture struct {
	CapturedAt time.Time `json:"captured_at"`
	// DisplayIDs is derived from the frames the tick actually produced, so it
	// cannot drift from what was recorded.
	DisplayIDs []uint32 `json:"display_ids"`
	// IntervalMS is how often the next one is due, which is how a reader knows
	// how stale this may get before it stops meaning "recording now". It is
	// carried rather than assumed: the interval is a flag, and a recorder
	// started from a terminal need not match the app's own preference.
	IntervalMS int64 `json:"interval_ms"`
	// SelectionFallback says the configured display selection named nothing that
	// was connected, so every display is being recorded instead.
	SelectionFallback bool `json:"selection_fallback"`
}

// ScreenSink receives one report per screen tick. It is called from the screen
// goroutine and must not block. Nil means no report is made.
type ScreenSink func(ScreenCapture)

// LevelSampler is an AudioStream that can report the sound it has received since
// the last call, window by window, as mean squares of normalised samples.
//
// It is a separate optional interface rather than a method on AudioStream
// because only the native stream can answer it: the samples exist inside the
// capture callback, and nowhere else until a chunk closes. A stream that does
// not implement it simply has no meter.
type LevelSampler interface {
	SampleLevels() (system, microphone []float64, err error)
}

// LevelInterval is how often the live meters are drained.
//
// Deliberately twice transcript.EnvelopeWindowMS rather than equal to it: a
// drain that ran at exactly the window length would return nothing on half its
// calls, since a window has to *finish* to be reported. Two windows per drain
// makes every tick carry a measurement, which is what makes a meter move
// smoothly instead of stuttering between empty and full ticks.
const LevelInterval = 2 * transcript.EnvelopeWindowMS * time.Millisecond

// sampleLevels drains the live meters and reports each track that had sound to
// summarise, until the recording stops.
//
// It is its own goroutine, and that is the invariant rather than a tidiness
// choice. The sink is a writer the recorder does not control — in the app's case
// a pipe whose far end is a GUI — and a reader that stopped draining that pipe
// would block the sink. On the capture path that would stop media being
// recorded because a level meter was not being read. Here the worst case is that
// meters stop moving, which is the correct thing to sacrifice.
//
// Every failure is a debug line and nothing louder. A level is a readout of
// sound that has already been handed to the writer; nothing downstream needs it.
func (r *Recorder) sampleLevels(ctx context.Context, sampler LevelSampler) {
	if r.Levels == nil {
		return
	}
	ticker := time.NewTicker(LevelInterval)
	defer ticker.Stop()
	defer func() {
		if recovered := recover(); recovered != nil {
			r.Logger.Error("audio level sink panicked; capture is unaffected", "panic", recovered)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		system, microphone, err := sampler.SampleLevels()
		if err != nil {
			r.Logger.Debug("could not sample audio levels", "error", err)
			continue
		}
		measuredAt := time.Now().UTC()
		for _, track := range []struct {
			source string
			energy []float64
		}{
			{transcript.TrackSystem, system},
			{transcript.TrackMicrophone, microphone},
		} {
			level, ok := levelFrom(track.source, track.energy, measuredAt)
			if !ok {
				continue
			}
			r.Levels(level)
		}
	}
}

// levelFrom summarises one track's drained energy. It reports nothing when no
// window finished in the interval, which is not the same as silence: silence
// completes windows too, at wav.SilenceFloorDBFS.
func levelFrom(source string, energy []float64, measuredAt time.Time) (AudioLevel, bool) {
	if len(energy) == 0 {
		return AudioLevel{}, false
	}
	envelope := make([]float64, len(energy))
	for i, meanSquare := range energy {
		envelope[i] = wav.DBFSFromMeanSquare(meanSquare)
	}
	peak, median, err := summarizeEnvelope(envelope)
	if err != nil {
		return AudioLevel{}, false
	}
	span := int64(len(envelope)) * int64(transcript.EnvelopeWindowMS)
	return AudioLevel{
		Source:     source,
		CapturedAt: measuredAt.Add(-time.Duration(span) * time.Millisecond),
		PeakDBFS:   peak,
		MedianDBFS: median,
		WindowMS:   transcript.EnvelopeWindowMS,
		DurationMS: span,
		Silent:     peak <= wav.SilenceFloorDBFS,
	}, true
}

// summarizeEnvelope reduces a windowed dBFS envelope to its loudest and typical
// window. An empty envelope has no level rather than a level of zero — zero
// dBFS is full scale, which would peg a meter at maximum for an interval that
// was never measured.
func summarizeEnvelope(envelope []float64) (peak, median float64, err error) {
	if len(envelope) == 0 {
		return 0, 0, errNoEnvelope
	}
	sorted := make([]float64, len(envelope))
	copy(sorted, envelope)
	sort.Float64s(sorted)
	peak = sorted[len(sorted)-1]
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		median = sorted[middle]
	} else {
		median = (sorted[middle-1] + sorted[middle]) / 2
	}
	if math.IsNaN(peak) || math.IsNaN(median) {
		return 0, 0, errNoEnvelope
	}
	return peak, median, nil
}

// errNoEnvelope reports an interval that produced no measurable windows.
var errNoEnvelope = errors.New("audio produced no energy windows")
