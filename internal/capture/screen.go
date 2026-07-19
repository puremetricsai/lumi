package capture

import (
	"context"
	"fmt"
	"strings"

	"github.com/puremetricsai/lumi/internal/macosnative"
)

// ScreenFrame is a captured image and the stable CoreGraphics display ID that
// produced it. A source enumerates displays on every capture, so display
// hotplug does not require restarting the recorder.
type ScreenFrame struct {
	Path         string
	DisplayID    uint32
	Width        int
	Height       int
	CaptureError string
}

type ScreenContext struct {
	App         string
	Window      string
	Text        string
	DisplayID   uint32
	InputActive bool
}

type ScreenSource interface {
	Capture(context.Context, string, string) ([]ScreenFrame, error)
}

type TextExtractor interface {
	Extract(context.Context, string) (string, error)
}

type ContextExtractor interface {
	Snapshot(context.Context) (ScreenContext, error)
}

// NativeScreens captures every currently connected display with
// ScreenCaptureKit's SCScreenshotManager.
type NativeScreens struct{}

func (NativeScreens) Capture(ctx context.Context, directory, prefix string) ([]ScreenFrame, error) {
	frames, err := macosnative.CaptureScreens(ctx, directory, prefix)
	if err != nil {
		return nil, fmt.Errorf("capture displays with ScreenCaptureKit: %w", err)
	}
	result := make([]ScreenFrame, 0, len(frames))
	for _, frame := range frames {
		result = append(result, ScreenFrame{
			Path: frame.Path, DisplayID: frame.DisplayID, Width: frame.Width, Height: frame.Height,
			CaptureError: frame.CaptureError,
		})
	}
	return result, nil
}

// AccessibilityContext reads the focused application's Accessibility tree.
// It is the primary screen-text source; Vision is only invoked when the tree
// is unavailable or contains no useful text.
type AccessibilityContext struct{}

func (AccessibilityContext) Snapshot(ctx context.Context) (ScreenContext, error) {
	snapshot, err := macosnative.Accessibility(ctx)
	if err != nil {
		return ScreenContext{}, fmt.Errorf("read macOS Accessibility tree: %w", err)
	}
	return ScreenContext{
		App: snapshot.App, Window: snapshot.Window, Text: snapshot.Text,
		DisplayID: snapshot.DisplayID, InputActive: snapshot.InputActive,
	}, nil
}

// VisionText performs on-device Apple Vision text recognition.
type VisionText struct{}

func (VisionText) Extract(ctx context.Context, imagePath string) (string, error) {
	text, err := macosnative.RecognizeText(ctx, imagePath)
	if err != nil {
		return "", fmt.Errorf("recognize text with Apple Vision: %w", err)
	}
	return strings.TrimSpace(text), nil
}

func commandError(action string, err error, output []byte) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}
