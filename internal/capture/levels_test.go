package capture

import (
	"io"
	"log/slog"
	"math"
	"testing"
	"time"
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

// The sink is optional, and a nil sink must mean the file is never opened at
// all — the measurement exists for a meter, not for capture.
func TestEmitLevelDoesNothingWithoutASink(t *testing.T) {
	r := &Recorder{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// A path that would fail loudly if it were ever read.
	r.emitLevel(t.Context(), "system", "/nonexistent/does-not-exist.wav", time.Now().UTC(), 30000)
	// Reaching here without a panic or a log is the assertion; with a sink set
	// the same call must also survive an unreadable file.
	var got []AudioLevel
	r.Levels = func(level AudioLevel) { got = append(got, level) }
	r.emitLevel(t.Context(), "system", "/nonexistent/does-not-exist.wav", time.Now().UTC(), 30000)
	if len(got) != 0 {
		t.Errorf("an unreadable file produced %d measurements; want none", len(got))
	}
}
