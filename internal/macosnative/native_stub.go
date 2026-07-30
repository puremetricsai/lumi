//go:build !darwin || !arm64 || !cgo

package macosnative

import (
	"context"
	"errors"
	"time"
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
	Trusted     *bool  `json:"trusted,omitempty"`
	AppSource   string `json:"app_source,omitempty"`
	TitleSource string `json:"title_source,omitempty"`
	Error       string `json:"error,omitempty"`
}

type FrontmostProcess struct {
	PID int32  `json:"pid"`
	App string `json:"app"`
}

type FrontmostResolution struct {
	PID       int32  `json:"pid"`
	App       string `json:"app"`
	AppSource string `json:"app_source"`
}

type FrontmostDiagnosticReport struct {
	Accessibility FrontmostProcess    `json:"accessibility"`
	Workspace     FrontmostProcess    `json:"workspace"`
	WindowList    FrontmostProcess    `json:"window_list"`
	Resolved      FrontmostResolution `json:"resolved"`
	Agree         bool                `json:"agree"`
}

type Permissions struct {
	ScreenRecording   string `json:"screen_recording"`
	Accessibility     string `json:"accessibility"`
	InputMonitoring   string `json:"input_monitoring"`
	Microphone        string `json:"microphone"`
	SpeechRecognition string `json:"speech_recognition"`
}

type AudioFrame struct {
	Path               string `json:"path"`
	Source             string `json:"source"`
	DurationMS         int64  `json:"duration_ms"`
	CaptureError       string `json:"capture_error,omitempty"`
	StartedAtUnixNS    int64  `json:"started_at_unix_ns,omitempty"`
	SessionStartPTSNS  int64  `json:"session_start_pts_ns,omitempty"`
	MeasuredDurationMS int64  `json:"measured_duration_ms,omitempty"`
}

type AudioProcess struct {
	PID      int32  `json:"pid"`
	BundleID string `json:"bundle_id,omitempty"`
	Name     string `json:"name,omitempty"`
}

type SpeechRun struct {
	StartMS    int64   `json:"start_ms"`
	EndMS      int64   `json:"end_ms"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence,omitempty"`
}

type SpeechSegment struct {
	StartMS    int64       `json:"start_ms"`
	EndMS      int64       `json:"end_ms"`
	Text       string      `json:"text"`
	Confidence float64     `json:"confidence,omitempty"`
	Runs       []SpeechRun `json:"runs,omitempty"`
}

type Transcription struct {
	Text     string          `json:"text"`
	Segments []SpeechSegment `json:"segments"`
}

func CaptureScreens(context.Context, string, string) ([]ScreenFrame, error) {
	return nil, errUnsupported
}

func Accessibility(context.Context) (AccessibilitySnapshot, error) {
	return AccessibilitySnapshot{}, errUnsupported
}

func FrontmostDiagnostic(context.Context) (FrontmostDiagnosticReport, error) {
	return FrontmostDiagnosticReport{}, errUnsupported
}

func RecognizeText(context.Context, string) (string, error) { return "", errUnsupported }

func TranscribeAudio(context.Context, string, string, []string) (string, error) {
	return "", errUnsupported
}

func TranscribeAudioSegments(context.Context, string, string, []string) (Transcription, error) {
	return Transcription{}, errUnsupported
}

func EnsureSpeechAssets(context.Context, string) error { return errUnsupported }

func PermissionStatus(context.Context) (Permissions, error) { return Permissions{}, errUnsupported }

func RequestPermissions(context.Context, bool) (Permissions, error) {
	return Permissions{}, errUnsupported
}

func RecordAudio(context.Context, string, string, float64) ([]AudioFrame, error) {
	return nil, errUnsupported
}

func AudioProcesses(context.Context) ([]AudioProcess, error) {
	return nil, errUnsupported
}

// AudioChunk mirrors the darwin definition so callers compile everywhere.
type AudioChunk struct {
	StartedAtUnixNS int64
	Frames          []AudioFrame
	Closed          bool
	CaptureError    string
}

type AudioSession struct{}

func StartAudioSession(context.Context, string, string, float64) (*AudioSession, error) {
	return nil, errUnsupported
}

func (*AudioSession) Next(time.Duration) (AudioChunk, error) { return AudioChunk{}, errUnsupported }

func (*AudioSession) Stop() {}

func (*AudioSession) Close() {}

func OSVersion() (int, int, int, error) { return 0, 0, 0, errUnsupported }

func SpeechAssetsInstalled(context.Context, string) (bool, error) { return false, errUnsupported }
