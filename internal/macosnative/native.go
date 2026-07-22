//go:build darwin && arm64 && cgo

package macosnative

/*
#cgo CFLAGS: -fblocks -fobjc-arc
// LDFLAGS spike (Task 1): -L${SRCDIR} -llumispeech -framework Speech linked
// liblumispeech.a on the first try on this toolchain (macOS 26.5.2, Xcode
// 26.6, Swift 6.3) — no Swift-runtime fallback flags (-L/usr/lib/swift,
// -Wl,-rpath,/usr/lib/swift, -lswiftCore -lswift_Concurrency) were needed.
#cgo LDFLAGS: -framework AppKit -framework ApplicationServices -framework AudioToolbox -framework AVFoundation -framework CoreGraphics -framework CoreMedia -framework CoreVideo -framework Foundation -framework ImageIO -framework ScreenCaptureKit -framework UniformTypeIdentifiers -framework Vision -L${SRCDIR} -llumispeech -framework Speech
#include <stdlib.h>
#include <stdbool.h>

char *lumi_capture_screens_json(const char *directory, const char *prefix, char **error_message);
char *lumi_accessibility_snapshot_json(char **error_message);
char *lumi_vision_recognize(const char *image_path, char **error_message);
char *lumi_permissions_json(char **error_message);
char *lumi_request_permissions_json(bool input_monitoring, char **error_message);
char *lumi_record_audio_json(const char *directory, const char *prefix, double duration_seconds, char **error_message);
void lumi_os_version(int *major, int *minor, int *patch);
char *lumi_speech_ping(void);
char *lumi_transcribe_audio_string(const char *audio_path, const char *locale, char **error_message);
char *lumi_speech_status_json(const char *locale, char **error_message);
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type AccessibilitySnapshot struct {
	App         string `json:"app"`
	Window      string `json:"window"`
	Text        string `json:"text"`
	DisplayID   uint32 `json:"display_id"`
	InputActive bool   `json:"input_active"`
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

// TranscribeAudio transcribes a WAV file with on-device SpeechAnalyzer. It
// mirrors RecognizeText: a nil native return with a populated error message
// becomes a Go error. An empty string with no error is a valid (silent)
// transcript. An empty locale defaults to en-US in the native layer.
func TranscribeAudio(ctx context.Context, audioPath, locale string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	pathC := C.CString(audioPath)
	localeC := C.CString(locale)
	defer C.free(unsafe.Pointer(pathC))
	defer C.free(unsafe.Pointer(localeC))
	var nativeErr *C.char
	result, err := nativeString(C.lumi_transcribe_audio_string(pathC, localeC, &nativeErr), nativeErr)
	if err != nil {
		return "", err
	}
	return result, nil
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

// SpeechPing proves the Swift speech bridge is linked and callable. It needs no
// permissions or assets and exists so a fast, permission-free test can assert
// the cgo↔Swift boundary works.
func SpeechPing() string {
	value := C.lumi_speech_ping()
	if value == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(value))
	return C.GoString(value)
}

// SpeechStatus reports whether the current OS, authorization state, and
// locale asset availability are sufficient for on-device SpeechAnalyzer
// transcription. AssetsInstalled is a proxy backed by
// [SFSpeechRecognizer supportedLocales], the only per-locale signal readable
// from Objective-C; the authoritative asset state comes from Swift's
// AssetInventory at transcribe time.
type SpeechStatus struct {
	OSSupported     bool   `json:"os_supported"`
	Locale          string `json:"locale"`
	AssetsInstalled bool   `json:"assets_installed"`
	Authorization   string `json:"authorization"`
}

func GetSpeechStatus(ctx context.Context, locale string) (SpeechStatus, error) {
	if err := ctx.Err(); err != nil {
		return SpeechStatus{}, err
	}
	localeC := C.CString(locale)
	defer C.free(unsafe.Pointer(localeC))
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_speech_status_json(localeC, &nativeErr), nativeErr)
	if err != nil {
		return SpeechStatus{}, err
	}
	var status SpeechStatus
	if err := json.Unmarshal(result, &status); err != nil {
		return status, fmt.Errorf("decode speech status: %w", err)
	}
	return status, nil
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
