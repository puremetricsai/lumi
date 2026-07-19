package capture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

type Transcriber struct {
	Binary    string
	ModelPath string
	Language  string
}

func (t Transcriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	if t.ModelPath == "" {
		return "", errors.New("whisper model path is required")
	}
	binary := t.Binary
	if binary == "" {
		binary = "whisper-cli"
	}
	prefix := strings.TrimSuffix(audioPath, ".wav") + "-transcript"
	args := []string{"-m", t.ModelPath, "-f", audioPath, "-otxt", "-of", prefix, "-nt", "-np"}
	if t.Language != "" {
		args = append(args, "-l", t.Language)
	}
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		return "", commandError("transcribe audio", err, output)
	}
	transcriptPath := prefix + ".txt"
	contents, err := os.ReadFile(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("read whisper transcript: %w", err)
	}
	if err := os.Remove(transcriptPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove temporary transcript: %w", err)
	}
	return strings.TrimSpace(string(contents)), nil
}
