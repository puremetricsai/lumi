package capture

import (
	"context"
	"fmt"
	"strings"
	"sync"

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

// ScreenContext is what the recorder knows about the focused application at one
// capture tick. It is deliberately degradable: App and InputActive come from
// sources that need no Accessibility grant, so a failed Accessibility read
// yields a partial context carrying AccessibilityError rather than nothing at
// all. A degraded snapshot is a value, not an error.
//
// Trusted is nil when trust is unknown — the snapshot failed outright — which is
// distinct from a known-false "trust was revoked". Callers that report a cause
// to the user must branch on all three states.
type ScreenContext struct {
	App         string
	Window      string
	Text        string
	DisplayID   uint32
	InputActive bool
	Trusted     *bool
	// AppSource names where App and the pid behind it came from; TitleSource
	// names where Window came from. They routinely differ — the common case is
	// an app named by the window list whose title was read over Accessibility —
	// so they answer different questions and must not be conflated.
	AppSource          string
	TitleSource        string
	AccessibilityError string
}

// Degraded reports whether anything about this tick is incomplete: either the
// Accessibility read failed, or no application name was resolved at all. A
// degraded tick is not necessarily an unattributed one — the whole point of the
// window-list fallback is that App survives a failed Accessibility read.
func (c ScreenContext) Degraded() bool {
	return c.AccessibilityError != "" || c.App == ""
}

// Unattributed reports the failure that actually costs the index something:
// no application name at all, so the event cannot be found by an app filter.
func (c ScreenContext) Unattributed() bool { return c.App == "" }

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
// It supplies focused-app attribution (App, Window, InputActive); its
// focused-window text is preserved in event metadata when substantive but is not
// the primary screen-text source, since it cannot see beyond the focused window
type AccessibilityContext struct{}

// accessibilityMutex serialises the native Accessibility read. Since audio
// chunks began carrying attribution the snapshot is taken from two goroutines —
// the screen tick and the close of an audio chunk — and the AX machinery it
// drives is process-global, so the lock belongs to the package rather than to an
// instance: AccessibilityContext is a zero-size value that callers construct
// freshly, and a field would guard nothing shared.
var accessibilityMutex sync.Mutex

// Snapshot returns an error only when nothing at all could be read. A snapshot
// whose Accessibility read failed still carries the frontmost application name
// and, where the window list could supply one, a window title; that arrives as a
// populated ScreenContext with AccessibilityError set.
func (AccessibilityContext) Snapshot(ctx context.Context) (ScreenContext, error) {
	accessibilityMutex.Lock()
	defer accessibilityMutex.Unlock()
	snapshot, err := macosnative.Accessibility(ctx)
	if err != nil {
		return ScreenContext{}, fmt.Errorf("read macOS Accessibility tree: %w", err)
	}
	screenContext := ScreenContext{
		App: snapshot.App, Window: snapshot.Window, Text: snapshot.Text,
		DisplayID: snapshot.DisplayID, InputActive: snapshot.InputActive,
		Trusted: snapshot.Trusted, AppSource: snapshot.AppSource,
		TitleSource: snapshot.TitleSource,
	}
	if snapshot.Error != "" {
		screenContext.AccessibilityError = fmt.Sprintf("read macOS Accessibility tree: %s", snapshot.Error)
	}
	return screenContext, nil
}

// VisionText performs on-device Apple Vision text recognition over the full
// display screenshot. It is the primary screen-text source, so the indexed text
// reflects the entire screen (every visible window) rather than only the focused
// application.
type VisionText struct{}

func (VisionText) Extract(ctx context.Context, imagePath string) (string, error) {
	text, err := macosnative.RecognizeText(ctx, imagePath)
	if err != nil {
		return "", fmt.Errorf("recognize text with Apple Vision: %w", err)
	}
	return strings.TrimSpace(text), nil
}
