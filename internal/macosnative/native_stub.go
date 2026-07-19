//go:build !darwin || !arm64 || !cgo

package macosnative

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("native capture requires Apple Silicon macOS with cgo enabled")

type ScreenFrame struct {
	Path         string `json:"path"`
	DisplayID    uint32 `json:"display_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	CaptureError string `json:"capture_error,omitempty"`
}

type AccessibilitySnapshot struct {
	App         string `json:"app"`
	Window      string `json:"window"`
	Text        string `json:"text"`
	DisplayID   uint32 `json:"display_id"`
	InputActive bool   `json:"input_active"`
}

type Permissions struct {
	ScreenRecording string `json:"screen_recording"`
	Accessibility   string `json:"accessibility"`
	InputMonitoring string `json:"input_monitoring"`
	Microphone      string `json:"microphone"`
}

type AudioFrame struct {
	Path         string `json:"path"`
	Source       string `json:"source"`
	DurationMS   int64  `json:"duration_ms"`
	CaptureError string `json:"capture_error,omitempty"`
}

func CaptureScreens(context.Context, string, string) ([]ScreenFrame, error) {
	return nil, errUnsupported
}

func Accessibility(context.Context) (AccessibilitySnapshot, error) {
	return AccessibilitySnapshot{}, errUnsupported
}

func RecognizeText(context.Context, string) (string, error) { return "", errUnsupported }

func PermissionStatus(context.Context) (Permissions, error) { return Permissions{}, errUnsupported }

func RequestPermissions(context.Context, bool) (Permissions, error) {
	return Permissions{}, errUnsupported
}

func RecordAudio(context.Context, string, string, float64) ([]AudioFrame, error) {
	return nil, errUnsupported
}

func OSVersion() (int, int, int, error) { return 0, 0, 0, errUnsupported }
