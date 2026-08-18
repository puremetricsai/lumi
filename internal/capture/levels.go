package capture

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/puremetricsai/lumi/internal/transcript"
)

// AudioLevel is how loud one captured track was, measured after its file was
// written and indexed.
//
// It exists so a supervising app can draw a level meter without inventing a
// second definition of "level". Both numbers come from the same windowed
// envelope that decides whether a chunk was silent, at the same resolution
// (transcript.EnvelopeWindowMS), so a meter and a silence verdict can never
// disagree about what was heard.
//
// The measurement is per finished chunk, which is the resolution the capture
// pipeline actually produces: audio reaches Go only when a chunk closes. A meter
// fed from this refreshes once per --audio-chunk, and that is a property of
// where the samples are, not a shortcut.
type AudioLevel struct {
	Source     string    `json:"source"`
	CapturedAt time.Time `json:"captured_at"`
	// PeakDBFS is the loudest window in the chunk, and MedianDBFS the typical
	// one. Both are needed: a chunk of near-silence with one door slam has a
	// high peak and a low median, and a meter showing only the peak would call
	// that a conversation.
	PeakDBFS   float64 `json:"peak_dbfs"`
	MedianDBFS float64 `json:"median_dbfs"`
	// WindowMS is the resolution the two figures were measured at, so a reader
	// never has to assume it.
	WindowMS int `json:"window_ms"`
	// DurationMS is the chunk's own length, which is how a reader knows how
	// stale the measurement is allowed to get before the meter should decay.
	DurationMS int64 `json:"duration_ms"`
}

// LevelSink receives one measurement per captured track. It is called from the
// audio goroutine and must not block.
type LevelSink func(AudioLevel)

// measureLevel reduces one captured file to a peak and a median.
//
// It reads through ReadAudioEnvelope rather than internal/wav directly, for the
// reason that function exists: the recorder and the backfill must not disagree
// about how a captured file is opened, and a chunk `lumi compress` has since
// rewritten as FLAC is not a RIFF file.
func measureLevel(ctx context.Context, path string) (peak, median float64, err error) {
	envelope, _, err := ReadAudioEnvelope(ctx, path, transcript.EnvelopeWindowMS)
	if err != nil {
		return 0, 0, err
	}
	return summarizeEnvelope(envelope)
}

// summarizeEnvelope reduces a windowed dBFS envelope to its loudest and typical
// window. An empty envelope has no level rather than a level of zero — zero
// dBFS is full scale, which would peg a meter at maximum for a file that was
// never measured.
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

// errNoEnvelope reports a file that produced no measurable windows.
var errNoEnvelope = errors.New("audio file produced no energy windows")

// emitLevel measures one stored track and hands the result to the sink, off the
// capture path entirely.
//
// The goroutine is not a micro-optimisation, it is the invariant. This is called
// from the audio goroutine between inserting a row and attributing the chunk, and
// the sink is a writer the recorder does not control — in the app's case a pipe
// whose far end is a GUI. A reader that stops draining that pipe would block the
// sink, and a synchronous call would then block capture itself: media would stop
// being recorded because a level meter was not being read. Reading the file is
// bounded work, but it is on the same path and belongs on the same side of the
// boundary.
//
// Three properties follow, and all three are deliberate:
//
//   - It never blocks the caller. A busy emitter drops the measurement rather
//     than queueing it; a level meter's whole value is that it is current, so a
//     backlog of stale readings is worth nothing and costs memory.
//   - It never panics the recorder. The sink is caller-supplied code, and a
//     panic in it would otherwise take down capture.
//   - It never cancels with capture. The context is detached, so a measurement
//     already under way at shutdown finishes instead of vanishing — the same
//     courtesy preservationContext extends to media.
//
// Every failure is a debug line and nothing louder. A level is a readout of media
// that is already safe on disk and already indexed; nothing downstream needs it.
func (r *Recorder) emitLevel(ctx context.Context, source, path string, capturedAt time.Time, durationMS int64) {
	if r.Levels == nil || path == "" {
		return
	}
	// A measurement already in flight means this one is skipped, which is the
	// correct answer: the next chunk brings a fresher reading anyway.
	if !r.levelBusy.CompareAndSwap(false, true) {
		r.Logger.Debug("skipped an audio level measurement; the previous one is still running",
			"source", source)
		return
	}
	sink, logger := r.Levels, r.Logger
	go func() {
		defer func() {
			r.levelBusy.Store(false)
			if recovered := recover(); recovered != nil {
				logger.Error("audio level sink panicked; capture is unaffected",
					"source", source, "panic", recovered)
			}
		}()
		peak, median, err := measureLevel(context.WithoutCancel(ctx), path)
		if err != nil {
			logger.Debug("could not measure audio level", "source", source, "path", path, "error", err)
			return
		}
		sink(AudioLevel{
			Source:     source,
			CapturedAt: capturedAt,
			PeakDBFS:   peak,
			MedianDBFS: median,
			WindowMS:   transcript.EnvelopeWindowMS,
			DurationMS: durationMS,
		})
	}()
}
