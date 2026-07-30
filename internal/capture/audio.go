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
	Path         string
	Source       string
	DurationMS   int64
	CaptureError string
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
			CaptureError: frame.CaptureError,
		})
	}
	return result, nil
}

// NativeSpeech transcribes WAV chunks with Apple's on-device SpeechAnalyzer.
// Vocabulary is optional; when set, its terms bias recognition toward names
// and jargon outside the general lexicon.
type NativeSpeech struct {
	Locale     string
	Vocabulary *vocabulary.Loader
	Logger     *slog.Logger
}

func (n NativeSpeech) Transcribe(ctx context.Context, audioPath string) (string, error) {
	text, err := macosnative.TranscribeAudio(ctx, audioPath, n.Locale, n.vocabularyTerms())
	if err != nil {
		return "", fmt.Errorf("transcribe audio with SpeechAnalyzer: %w", err)
	}
	return text, nil
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
