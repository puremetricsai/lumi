package capture

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/vocabulary"
)

type AudioFrame struct {
	Path   string
	Source string
	// DurationMS is the duration that was requested, which is what every
	// indexed row means by it. MeasuredDurationMS is what the file holds.
	DurationMS   int64
	CaptureError string
	// StartedAt is the wall clock of this track's first sample buffer, and the
	// only sound anchor for its file-relative segment timings — the recorder's
	// captured_at is taken before ScreenCaptureKit is even asked for shareable
	// content. Zero when the native layer reported none, which is the case for
	// every chunk captured before this field existed.
	StartedAt time.Time
	// SessionStartPTSNS is the first sample buffer's presentation timestamp.
	// Both tracks are fed by one ScreenCaptureKit stream, so these share a host
	// timebase and their difference is the exact skew between the two files'
	// t=0. Zero when the native layer reported none.
	SessionStartPTSNS  int64
	MeasuredDurationMS int64
}

// TimedSegment is one transcribed phrase with a measured span, relative to the
// start of its own WAV. StartMS/EndMS come from the union of the phrase's word
// timings rather than the recognizer's result range, which overstates speech
// extent; see macosnative.SpeechSegment.
//
// A segment with empty Text and no Runs is a signal rather than noise: audio was
// present across this span but no words resolved.
type TimedSegment struct {
	StartMS    int64
	EndMS      int64
	Text       string
	Confidence float64
	Runs       []TimedRun
}

// TimedRun is one word-level span inside a segment. Runs whose timing would not
// resolve are dropped, so runs need not concatenate back to the segment text.
type TimedRun struct {
	StartMS    int64
	EndMS      int64
	Text       string
	Confidence float64
}

// Transcription is a chunk's transcript and its timed segments. Text is the
// segments' text concatenated with no separator, matching byte-for-byte what
// every already-indexed row holds.
type Transcription struct {
	Text     string
	Segments []TimedSegment
}

type AudioSource interface {
	Record(context.Context, string, string, time.Duration) ([]AudioFrame, error)
}

// NativeAudio records system output and the default microphone concurrently
// from one ScreenCaptureKit stream. Both sources are emitted as independent
// mono 16 kHz WAV chunks so transcription and source attribution stay clear.
type NativeAudio struct{}

func (NativeAudio) Record(ctx context.Context, directory, prefix string, duration time.Duration) ([]AudioFrame, error) {
	frames, err := macosnative.RecordAudio(ctx, directory, prefix, duration.Seconds())
	if err != nil {
		return nil, fmt.Errorf("record system and microphone audio with ScreenCaptureKit: %w", err)
	}
	result := make([]AudioFrame, 0, len(frames))
	for _, frame := range frames {
		result = append(result, AudioFrame{
			Path: frame.Path, Source: frame.Source, DurationMS: frame.DurationMS,
			CaptureError:       frame.CaptureError,
			StartedAt:          unixNanoTime(frame.StartedAtUnixNS),
			SessionStartPTSNS:  frame.SessionStartPTSNS,
			MeasuredDurationMS: frame.MeasuredDurationMS,
		})
	}
	return result, nil
}

// unixNanoTime keeps "the native layer reported no anchor" as a zero time.Time
// rather than 1970, so callers can test it with IsZero and fall back rather than
// silently placing audio half a century ago.
func unixNanoTime(unixNS int64) time.Time {
	if unixNS <= 0 {
		return time.Time{}
	}
	return time.Unix(0, unixNS).UTC()
}

// NativeSpeech transcribes WAV chunks with Apple's on-device SpeechAnalyzer.
// Vocabulary is optional; when set, its terms bias recognition toward names
// and jargon outside the general lexicon.
type NativeSpeech struct {
	Locale     string
	Vocabulary *vocabulary.Loader
	Logger     *slog.Logger
}

func (n NativeSpeech) Transcribe(ctx context.Context, audioPath string) (Transcription, error) {
	native, err := macosnative.TranscribeAudioSegments(ctx, audioPath, n.Locale, n.vocabularyTerms())
	if err != nil {
		return Transcription{}, fmt.Errorf("transcribe audio with SpeechAnalyzer: %w", err)
	}
	result := Transcription{Text: native.Text, Segments: make([]TimedSegment, 0, len(native.Segments))}
	for _, segment := range native.Segments {
		runs := make([]TimedRun, 0, len(segment.Runs))
		for _, run := range segment.Runs {
			runs = append(runs, TimedRun{
				StartMS: run.StartMS, EndMS: run.EndMS,
				Text: run.Text, Confidence: run.Confidence,
			})
		}
		result.Segments = append(result.Segments, TimedSegment{
			StartMS: segment.StartMS, EndMS: segment.EndMS,
			Text: segment.Text, Confidence: segment.Confidence, Runs: runs,
		})
	}
	return result, nil
}

// vocabularyTerms reads the term list for this chunk. A vocabulary failure is
// never allowed to fail transcription: losing biasing on one chunk is
// recoverable, losing the chunk is not. Logging is gated on Snapshot.Changed,
// so a persistently broken file warns once rather than once per chunk even
// though the read itself is retried every chunk.
func (n NativeSpeech) vocabularyTerms() []string {
	if n.Vocabulary == nil {
		return nil
	}
	snapshot := n.Vocabulary.Load()
	if snapshot.Changed {
		switch {
		case snapshot.Err != nil:
			n.logger().Warn("vocabulary unavailable; transcribing without contextual terms",
				"error", snapshot.Err)
		case !snapshot.Exists:
			// Absence is the normal state for a user who has not opted in.
		default:
			n.logger().Info("vocabulary loaded",
				"terms", len(snapshot.Terms), "dropped", snapshot.Dropped)
		}
	}
	return snapshot.Terms
}

func (n NativeSpeech) logger() *slog.Logger {
	if n.Logger == nil {
		return slog.Default()
	}
	return n.Logger
}
