package capture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type AudioRecorder struct {
	Binary string
	Device string
}

func (r AudioRecorder) Record(ctx context.Context, destination string, duration time.Duration) error {
	binary := r.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	device := r.Device
	if device == "" {
		device = "0"
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "avfoundation", "-i", ":" + device,
		"-t", fmt.Sprintf("%.3f", duration.Seconds()),
		"-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-y", destination,
	}
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return commandError("record audio", err, output)
	}
	return nil
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
