package capture

import (
	"context"
	"fmt"
	"time"

	"github.com/puremetricsai/lumi/internal/macosnative"
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

// NativeSpeech transcribes WAV chunks with Apple's on-device SpeechAnalyzer via
// the macosnative bridge. Locale selects the recognition assets; empty defaults
// to en-US.
type NativeSpeech struct {
	Locale string
}

func (n NativeSpeech) Transcribe(ctx context.Context, audioPath string) (string, error) {
	locale := n.Locale
	if locale == "" {
		locale = "en-US"
	}
	text, err := macosnative.TranscribeAudio(ctx, audioPath, locale)
	if err != nil {
		return "", fmt.Errorf("transcribe audio with SpeechAnalyzer: %w", err)
	}
	return text, nil
}
