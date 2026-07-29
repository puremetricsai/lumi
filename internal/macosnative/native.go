//go:build darwin && arm64 && cgo

package macosnative

/*
#cgo CFLAGS: -fblocks -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework ApplicationServices -framework AudioToolbox -framework AVFoundation -framework CoreGraphics -framework CoreMedia -framework CoreVideo -framework Foundation -framework ImageIO -framework IOKit -framework ScreenCaptureKit -framework UniformTypeIdentifiers -framework Vision -L${SRCDIR} -llumispeech -framework Speech
#include <stdlib.h>
#include <stdbool.h>

char *lumi_capture_screens_json(const char *directory, const char *prefix, char **error_message);
char *lumi_accessibility_snapshot_json(char **error_message);
char *lumi_vision_recognize(const char *image_path, char **error_message);
char *lumi_permissions_json(char **error_message);
char *lumi_hid_access_name(int access);
char *lumi_request_permissions_json(bool input_monitoring, char **error_message);
char *lumi_record_audio_json(const char *directory, const char *prefix, double duration_seconds, char **error_message);
void lumi_os_version(int *major, int *minor, int *patch);
char *lumi_transcribe_audio_string(const char *audio_path, const char *locale, double timeout_seconds, char **error_message);
char *lumi_speech_ensure_assets(const char *locale, double timeout_seconds, char **error_message);
int lumi_speech_assets_installed(const char *locale);
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unsafe"
)

// ScreenFrame describes one display image produced atomically by
// CaptureScreens. Displays are enumerated for every call, making capture
// naturally responsive to display hotplug events.
type ScreenFrame struct {
	Path         string `json:"path"`
	DisplayID    uint32 `json:"display_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	CaptureError string `json:"capture_error,omitempty"`
}

// AccessibilitySnapshot is total: App and InputActive are read from sources that
// need no Accessibility grant, so they survive an AX failure. Error is set when
// the focused-window read failed and the snapshot is therefore degraded — that
// is a value, not an error return.
//
// Trusted is a pointer because a nil-vs-false distinction matters: a decoded
// snapshot always carries trust, but a zero-valued struct (returned when the
// snapshot failed outright) must not be mistaken for "trust was revoked".
type AccessibilitySnapshot struct {
	App         string `json:"app"`
	Window      string `json:"window"`
	Text        string `json:"text"`
	DisplayID   uint32 `json:"display_id"`
	InputActive bool   `json:"input_active"`
	Trusted     *bool  `json:"trusted,omitempty"`
	TitleSource string `json:"title_source,omitempty"`
	Error       string `json:"error,omitempty"`
}

type Permissions struct {
	ScreenRecording   string `json:"screen_recording"`
	Accessibility     string `json:"accessibility"`
	InputMonitoring   string `json:"input_monitoring"`
	Microphone        string `json:"microphone"`
	SpeechRecognition string `json:"speech_recognition"`
}

type AudioFrame struct {
	Path         string `json:"path"`
	Source       string `json:"source"`
	DurationMS   int64  `json:"duration_ms"`
	CaptureError string `json:"capture_error,omitempty"`
}

func CaptureScreens(ctx context.Context, directory, prefix string) ([]ScreenFrame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directoryC := C.CString(directory)
	prefixC := C.CString(prefix)
	defer C.free(unsafe.Pointer(directoryC))
	defer C.free(unsafe.Pointer(prefixC))
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_capture_screens_json(directoryC, prefixC, &nativeErr), nativeErr)
	if err != nil {
		return nil, err
	}
	var frames []ScreenFrame
	if err := json.Unmarshal(result, &frames); err != nil {
		return nil, fmt.Errorf("decode native screen capture result: %w", err)
	}
	if len(frames) == 0 {
		return nil, errors.New("ScreenCaptureKit returned no displays")
	}
	return frames, nil
}

func Accessibility(ctx context.Context) (AccessibilitySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return AccessibilitySnapshot{}, err
	}
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_accessibility_snapshot_json(&nativeErr), nativeErr)
	if err != nil {
		return AccessibilitySnapshot{}, err
	}
	var snapshot AccessibilitySnapshot
	if err := json.Unmarshal(result, &snapshot); err != nil {
		return snapshot, fmt.Errorf("decode Accessibility snapshot: %w", err)
	}
	return snapshot, nil
}

func RecognizeText(ctx context.Context, imagePath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	pathC := C.CString(imagePath)
	defer C.free(unsafe.Pointer(pathC))
	var nativeErr *C.char
	result, err := nativeString(C.lumi_vision_recognize(pathC, &nativeErr), nativeErr)
	if err != nil {
		return "", err
	}
	return result, nil
}

// TranscribeAudio transcribes a WAV file with on-device SpeechAnalyzer.
func TranscribeAudio(ctx context.Context, audioPath, locale string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	timeout := nativeTimeout(ctx, 2*time.Minute)
	type transcription struct {
		text string
		err  error
	}
	done := make(chan transcription, 1)
	go func() {
		pathC := C.CString(audioPath)
		localeC := C.CString(locale)
		var nativeErr *C.char
		result, err := nativeString(
			C.lumi_transcribe_audio_string(pathC, localeC, C.double(timeout.Seconds()), &nativeErr), nativeErr)
		C.free(unsafe.Pointer(pathC))
		C.free(unsafe.Pointer(localeC))
		done <- transcription{text: result, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		return r.text, r.err
	}
}

// EnsureSpeechAssets downloads missing SpeechAnalyzer assets for locale.
func EnsureSpeechAssets(ctx context.Context, locale string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := nativeTimeout(ctx, 10*time.Minute)
	done := make(chan error, 1)
	go func() {
		localeC := C.CString(locale)
		var nativeErr *C.char
		_, err := nativeString(
			C.lumi_speech_ensure_assets(localeC, C.double(timeout.Seconds()), &nativeErr), nativeErr)
		C.free(unsafe.Pointer(localeC))
		done <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func nativeTimeout(ctx context.Context, fallback time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < fallback {
			return remaining
		}
	}
	return fallback
}

// IOHIDCheckAccess's tri-state, mirrored so tests can exercise every branch of
// the mapping on a machine that only ever reports one of them.
const (
	hidAccessGranted = 0
	hidAccessDenied  = 1
	hidAccessUnknown = 2
)

func hidAccessName(access int) string {
	name := C.lumi_hid_access_name(C.int(access))
	if name == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(name))
	return C.GoString(name)
}

func PermissionStatus(ctx context.Context) (Permissions, error) {
	if err := ctx.Err(); err != nil {
		return Permissions{}, err
	}
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_permissions_json(&nativeErr), nativeErr)
	if err != nil {
		return Permissions{}, err
	}
	var permissions Permissions
	if err := json.Unmarshal(result, &permissions); err != nil {
		return permissions, fmt.Errorf("decode native permission status: %w", err)
	}
	return permissions, nil
}

// RequestPermissions invokes the system TCC request flows. Accessibility and
// Input Monitoring may require the user to finish approval in System Settings
// and restart the invoking binary before their status changes to granted.
func RequestPermissions(ctx context.Context, inputMonitoring bool) (Permissions, error) {
	if err := ctx.Err(); err != nil {
		return Permissions{}, err
	}
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_request_permissions_json(C.bool(inputMonitoring), &nativeErr), nativeErr)
	if err != nil {
		return Permissions{}, err
	}
	var permissions Permissions
	if err := json.Unmarshal(result, &permissions); err != nil {
		return permissions, fmt.Errorf("decode requested native permission status: %w", err)
	}
	return permissions, nil
}

func RecordAudio(ctx context.Context, directory, prefix string, durationSeconds float64) ([]AudioFrame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directoryC := C.CString(directory)
	prefixC := C.CString(prefix)
	defer C.free(unsafe.Pointer(directoryC))
	defer C.free(unsafe.Pointer(prefixC))
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_record_audio_json(
		directoryC, prefixC, C.double(durationSeconds), &nativeErr), nativeErr)
	if err != nil {
		return nil, err
	}
	var frames []AudioFrame
	if err := json.Unmarshal(result, &frames); err != nil {
		return nil, fmt.Errorf("decode native audio capture result: %w", err)
	}
	if len(frames) == 0 {
		return nil, errors.New("ScreenCaptureKit returned no audio")
	}
	return frames, nil
}

func OSVersion() (major, minor, patch int, err error) {
	var nativeMajor, nativeMinor, nativePatch C.int
	C.lumi_os_version(&nativeMajor, &nativeMinor, &nativePatch)
	return int(nativeMajor), int(nativeMinor), int(nativePatch), nil
}

func SpeechAssetsInstalled(ctx context.Context, locale string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	localeC := C.CString(locale)
	defer C.free(unsafe.Pointer(localeC))
	switch C.lumi_speech_assets_installed(localeC) {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, errors.New("inspect SpeechAnalyzer assets: timed out")
	}
}

func nativeJSON(value, errorMessage *C.char) ([]byte, error) {
	text, err := nativeString(value, errorMessage)
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

func nativeString(value, errorMessage *C.char) (string, error) {
	if errorMessage != nil {
		defer C.free(unsafe.Pointer(errorMessage))
	}
	if value == nil {
		if errorMessage != nil {
			return "", errors.New(C.GoString(errorMessage))
		}
		return "", errors.New("native macOS operation failed")
	}
	defer C.free(unsafe.Pointer(value))
	return C.GoString(value), nil
}
