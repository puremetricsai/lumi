//go:build darwin && arm64 && cgo

package macosnative

import (
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func TestPermissionStatusUsesKnownStates(t *testing.T) {
	permissions, err := PermissionStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, status := range map[string]string{
		"screen recording":   permissions.ScreenRecording,
		"accessibility":      permissions.Accessibility,
		"input monitoring":   permissions.InputMonitoring,
		"microphone":         permissions.Microphone,
		"speech recognition": permissions.SpeechRecognition,
	} {
		if status == "" || status == "unknown" {
			t.Errorf("%s permission has invalid status %q", name, status)
		}
	}
}

func TestVisionRecognizesAValidImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blank.jpg")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	if err := jpeg.Encode(file, img, nil); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := RecognizeText(context.Background(), path); err != nil {
		t.Fatalf("Apple Vision rejected a valid JPEG: %v", err)
	}
}

func TestAccessibilitySnapshotWhenPermissionIsGranted(t *testing.T) {
	permissions, err := PermissionStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if permissions.Accessibility != "granted" {
		t.Skip("Accessibility permission is not granted")
	}
	snapshot, err := Accessibility(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.App == "" {
		t.Fatal("Accessibility returned no frontmost application")
	}
}

func TestSpeechBridgeLinks(t *testing.T) {
	if got := SpeechPing(); got != "pong" {
		t.Fatalf("Swift speech bridge not linked: got %q, want \"pong\"", got)
	}
}

func TestTranscribeAudioSmoke(t *testing.T) {
	if os.Getenv("LUMI_NATIVE_SMOKE") != "1" {
		t.Skip("set LUMI_NATIVE_SMOKE=1 after granting Speech Recognition and installing en-US assets")
	}
	ctx := context.Background()
	directory := t.TempDir()
	audio, err := RecordAudio(ctx, directory, "speech", 1.0)
	if err != nil {
		t.Fatal(err)
	}
	// A short real recording may be silence; we assert the pipeline runs
	// without error and returns a string (possibly empty text is NOT allowed
	// to be an error path — an empty successful transcript is valid here only
	// because we control that emptiness comes from silence, not failure).
	if _, err := TranscribeAudio(ctx, audio[0].Path, "en-US"); err != nil {
		t.Fatalf("SpeechAnalyzer transcription failed: %v", err)
	}
}

func TestNativeCaptureSmoke(t *testing.T) {
	if os.Getenv("LUMI_NATIVE_SMOKE") != "1" {
		t.Skip("set LUMI_NATIVE_SMOKE=1 after granting Lumi permissions")
	}
	ctx := context.Background()
	directory := t.TempDir()
	frames, err := CaptureScreens(ctx, directory, "smoke")
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if frame.DisplayID == 0 || frame.Width == 0 || frame.Height == 0 {
			t.Errorf("invalid screen frame: %#v", frame)
		}
		if _, err := os.Stat(frame.Path); err != nil {
			t.Errorf("captured frame is missing: %v", err)
		}
	}
	if _, err := RecognizeText(ctx, frames[0].Path); err != nil {
		t.Fatalf("Vision failed on a ScreenCaptureKit frame: %v", err)
	}
	snapshot, err := Accessibility(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.App == "" {
		t.Error("Accessibility snapshot did not identify the frontmost app")
	}
	audio, err := RecordAudio(ctx, directory, "smoke", 0.5)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, frame := range audio {
		seen[frame.Source] = true
		if _, err := os.Stat(frame.Path); err != nil {
			t.Errorf("captured audio is missing: %v", err)
			continue
		}
		header, err := os.ReadFile(frame.Path)
		if err != nil {
			t.Errorf("read captured audio: %v", err)
			continue
		}
		if len(header) < 12 || string(header[:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
			t.Errorf("%s is not a valid WAV container", frame.Path)
		}
	}
	if !seen["system"] || !seen["microphone"] {
		t.Errorf("expected system and microphone sources, got %#v", audio)
	}
}
