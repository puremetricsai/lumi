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

// TestInputMonitoringDistinguishesDeniedFromNotDetermined pins the tri-state
// contract that IOHIDCheckAccess can answer and CGPreflightListenEventAccess
// cannot. "denied" and "not_determined" need opposite remedies — System
// Settings versus `permissions --request` — so collapsing them into one string
// leaves the operator with no way to tell which one they are in.
//
// Screen Recording and Accessibility are deliberately absent: CGPreflight-
// ScreenCaptureAccess and AXIsProcessTrusted return a bare BOOL, so
// "denied_or_not_determined" is the most precise answer macOS permits.
func TestInputMonitoringDistinguishesDeniedFromNotDetermined(t *testing.T) {
	permissions, err := PermissionStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	switch permissions.InputMonitoring {
	case "granted", "denied", "not_determined":
	default:
		t.Errorf("input monitoring reported %q, want a tri-state value; a conflated "+
			"status cannot tell the operator whether to open System Settings or "+
			"run `permissions --request`", permissions.InputMonitoring)
	}
}

// TestInputMonitoringStateNames pins the mapping itself, independent of this
// machine's TCC state. The status-from-live-TCC test above can only observe a
// conflated value on a machine where Input Monitoring is not granted, so it
// would pass vacuously on a developer box that has already granted it.
func TestInputMonitoringStateNames(t *testing.T) {
	for access, want := range map[int]string{
		hidAccessGranted: "granted",
		hidAccessDenied:  "denied",
		hidAccessUnknown: "not_determined",
	} {
		if got := hidAccessName(access); got != want {
			t.Errorf("hidAccessName(%d) = %q, want %q", access, got, want)
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
	if len(audio) == 0 {
		t.Fatal("RecordAudio returned no frames")
	}
	// A short real recording may be silent, so only require successful analysis.
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
