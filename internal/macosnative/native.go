//go:build darwin && arm64 && cgo

package macosnative

/*
#cgo CFLAGS: -fblocks -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework ApplicationServices -framework AudioToolbox -framework AVFoundation -framework CoreGraphics -framework CoreMedia -framework CoreVideo -framework Foundation -framework ImageIO -framework IOKit -framework ScreenCaptureKit -framework UniformTypeIdentifiers -framework Vision -L${SRCDIR} -llumispeech -framework Speech
#include <stdlib.h>
#include <stdbool.h>
#include <stdint.h>

char *lumi_capture_screens_json(const char *directory, const char *prefix, char **error_message);
char *lumi_accessibility_snapshot_json(char **error_message);
char *lumi_resolve_frontmost_json(const char *windows_json, int active_pid, int workspace_pid,
                                  const char *workspace_app, int self_pid,
                                  char **error_message);
char *lumi_frontmost_diagnostic_json(char **error_message);
char *lumi_frontmost_candidates_json(const char *windows_json, const char *regular_pids_json,
                                     int self_pid, char **error_message);
char *lumi_vision_recognize(const char *image_path, char **error_message);
char *lumi_permissions_json(char **error_message);
char *lumi_hid_access_name(int access);
char *lumi_request_permissions_json(bool input_monitoring, char **error_message);
int64_t lumi_audio_session_start(const char *directory, const char *prefix, double chunk_seconds, char **error_message);
char *lumi_audio_session_next_json(int64_t handle, double timeout_seconds, char **error_message);
void lumi_audio_session_stop(int64_t handle);
void lumi_audio_session_close(int64_t handle);
void lumi_os_version(int *major, int *minor, int *patch);
char *lumi_transcribe_audio_string(const char *audio_path, const char *locale, const char *vocabulary_json, double timeout_seconds, char **error_message);
char *lumi_transcribe_audio_segments_json(const char *audio_path, const char *locale, const char *vocabulary_json, double timeout_seconds, char **error_message);
char *lumi_speech_ensure_assets(const char *locale, double timeout_seconds, char **error_message);
int lumi_speech_assets_installed(const char *locale);
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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
	AppSource   string `json:"app_source,omitempty"`
	TitleSource string `json:"title_source,omitempty"`
	Error       string `json:"error,omitempty"`
}

// FrontmostProcess is one source's answer to "which process is frontmost".
type FrontmostProcess struct {
	PID int32  `json:"pid"`
	App string `json:"app"`
}

// FrontmostResolution is FrontmostProcess plus which source produced it.
type FrontmostResolution struct {
	PID       int32  `json:"pid"`
	App       string `json:"app"`
	AppSource string `json:"app_source"`
}

// FrontmostDiagnosticReport compares the three frontmost sources. Agree
// concerns only NSWorkspace against the window list: it is false whenever they
// name different processes, which is the signature of a stale NSWorkspace in a
// process that runs no run loop. Accessibility is reported separately, and is
// whichever activation stage answered — the system-wide read or the per-
// application frontmost validation — because it is unavailable often enough
// that its absence is not itself a disagreement.
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
	Path   string `json:"path"`
	Source string `json:"source"`
	// DurationMS is the duration that was *requested*, not the duration
	// captured. Every indexed row carries it with that meaning; use
	// MeasuredDurationMS to learn what the file actually holds.
	DurationMS   int64  `json:"duration_ms"`
	CaptureError string `json:"capture_error,omitempty"`
	// StartedAtUnixNS is the wall-clock instant of this track's first sample
	// buffer, which is the only sound anchor for its file-relative timings. It
	// is per track, and so sits a track's worth of skew away from the chunk
	// boundary both tracks share. Zero means the native side reported none.
	StartedAtUnixNS int64 `json:"started_at_unix_ns,omitempty"`
	// SessionStartPTSNS is the first sample buffer's presentation timestamp.
	// Both tracks come from one SCStream, so their PTS values share a host
	// timebase and the difference between them is the exact skew between the
	// two files' t=0. Zero means the native side reported none.
	SessionStartPTSNS int64 `json:"session_start_pts_ns,omitempty"`
	// MeasuredDurationMS is the span actually written, from the first sample
	// buffer to the last. Zero means the native side reported none.
	MeasuredDurationMS int64 `json:"measured_duration_ms,omitempty"`
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

// FrontmostDiagnostic reports what each frontmost source sees, for
// `lumi native-smoke`. It shares a resolver with Accessibility, so the two
// cannot disagree about what the recorder will actually attribute.
func FrontmostDiagnostic(ctx context.Context) (FrontmostDiagnosticReport, error) {
	if err := ctx.Err(); err != nil {
		return FrontmostDiagnosticReport{}, err
	}
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_frontmost_diagnostic_json(&nativeErr), nativeErr)
	if err != nil {
		return FrontmostDiagnosticReport{}, err
	}
	var report FrontmostDiagnosticReport
	if err := json.Unmarshal(result, &report); err != nil {
		return report, fmt.Errorf("decode frontmost diagnostic: %w", err)
	}
	return report, nil
}

// resolveFrontmost drives the pure native resolver directly, so its branch order
// can be tested without a live session. Unexported: it exists for the test.
func resolveFrontmost(windowsJSON string, activePID, workspacePID int32, workspaceApp string, selfPID int32) (FrontmostResolution, error) {
	windowsC := C.CString(windowsJSON)
	appC := C.CString(workspaceApp)
	defer C.free(unsafe.Pointer(windowsC))
	defer C.free(unsafe.Pointer(appC))
	var nativeErr *C.char
	result, err := nativeJSON(
		C.lumi_resolve_frontmost_json(windowsC, C.int(activePID), C.int(workspacePID), appC,
			C.int(selfPID), &nativeErr),
		nativeErr)
	if err != nil {
		return FrontmostResolution{}, err
	}
	var resolution FrontmostResolution
	if err := json.Unmarshal(result, &resolution); err != nil {
		return resolution, fmt.Errorf("decode frontmost resolution: %w", err)
	}
	return resolution, nil
}

// frontmostCandidates drives the pure candidate enumeration directly, so which
// processes are eligible for the frontmost walk is testable without a live
// session. Unexported: it exists for the test.
func frontmostCandidates(windowsJSON, regularPIDsJSON string, selfPID int32) ([]int32, error) {
	windowsC := C.CString(windowsJSON)
	regularC := C.CString(regularPIDsJSON)
	defer C.free(unsafe.Pointer(windowsC))
	defer C.free(unsafe.Pointer(regularC))
	var nativeErr *C.char
	result, err := nativeJSON(
		C.lumi_frontmost_candidates_json(windowsC, regularC, C.int(selfPID), &nativeErr), nativeErr)
	if err != nil {
		return nil, err
	}
	var candidates []int32
	if err := json.Unmarshal(result, &candidates); err != nil {
		return nil, fmt.Errorf("decode frontmost candidates: %w", err)
	}
	return candidates, nil
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

// SpeechRun is one timed span inside a segment — in practice a single word. The
// bridge drops runs whose time range will not resolve, so a segment's runs need
// not concatenate back to its Text; Text is always the authority.
type SpeechRun struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
	// Confidence is the recognizer's own per-run score. Zero means it reported
	// none, which is not the same as low confidence.
	Confidence float64 `json:"confidence,omitempty"`
}

// SpeechSegment is one transcriber result: a phrase with a measured span.
//
// StartMS/EndMS are the union of the segment's run intervals, deliberately not
// the transcriber's own result range — that range extends to the finalization
// boundary and was measured overstating speech extent roughly tenfold, which
// would attribute overlap to microphone speech that merely sat nearby.
//
// A segment with empty Text and no Runs is meaningful rather than noise: it says
// audio was present across this span but no words resolved.
type SpeechSegment struct {
	StartMS    int64       `json:"start_ms"`
	EndMS      int64       `json:"end_ms"`
	Text       string      `json:"text"`
	Confidence float64     `json:"confidence,omitempty"`
	Runs       []SpeechRun `json:"runs,omitempty"`
}

// Transcription is a WAV's transcript plus its timed segments. Text is the
// segments' text concatenated with no separator, byte-identical to what
// TranscribeAudio returns for the same file — both go through one native path.
type Transcription struct {
	Text     string          `json:"text"`
	Segments []SpeechSegment `json:"segments"`
}

// TranscribeAudio transcribes a WAV file with on-device SpeechAnalyzer. Terms
// bias recognition toward phrases outside the general lexicon; an empty list
// applies no context at all, leaving behaviour identical to no vocabulary.
func TranscribeAudio(ctx context.Context, audioPath, locale string, terms []string) (string, error) {
	return transcribeNative(ctx, audioPath, locale, terms,
		func(pathC, localeC, vocabularyC *C.char, timeout C.double, nativeErr **C.char) *C.char {
			return C.lumi_transcribe_audio_string(pathC, localeC, vocabularyC, timeout, nativeErr)
		})
}

// TranscribeAudioSegments transcribes a WAV file and returns the transcript
// together with per-phrase spans and per-word timings. It shares the native ASR
// path with TranscribeAudio, so the transcripts cannot diverge and vocabulary
// biasing applies identically to both.
func TranscribeAudioSegments(ctx context.Context, audioPath, locale string, terms []string) (Transcription, error) {
	raw, err := transcribeNative(ctx, audioPath, locale, terms,
		func(pathC, localeC, vocabularyC *C.char, timeout C.double, nativeErr **C.char) *C.char {
			return C.lumi_transcribe_audio_segments_json(pathC, localeC, vocabularyC, timeout, nativeErr)
		})
	if err != nil {
		return Transcription{}, err
	}
	var result Transcription
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return Transcription{}, fmt.Errorf("decode native transcription result: %w", err)
	}
	return result, nil
}

// transcribeNative runs one of the two Swift entry points on its own goroutine so
// a cancelled context returns promptly even though the native call itself cannot
// be interrupted. Both callers share it to keep cancellation, timeout, and
// vocabulary encoding identical.
func transcribeNative(ctx context.Context, audioPath, locale string, terms []string,
	call func(pathC, localeC, vocabularyC *C.char, timeout C.double, nativeErr **C.char) *C.char,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	vocabularyJSON, err := encodeVocabulary(terms)
	if err != nil {
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
		vocabularyC := C.CString(vocabularyJSON)
		var nativeErr *C.char
		result, err := nativeString(
			call(pathC, localeC, vocabularyC, C.double(timeout.Seconds()), &nativeErr), nativeErr)
		C.free(unsafe.Pointer(pathC))
		C.free(unsafe.Pointer(localeC))
		C.free(unsafe.Pointer(vocabularyC))
		done <- transcription{text: result, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		return r.text, r.err
	}
}

// encodeVocabulary renders terms as a JSON array for the Swift bridge, matching
// how lumi_resolve_frontmost_json and lumi_frontmost_candidates_json already
// take JSON arrays across this boundary. An empty list encodes to "" so the
// Swift side can skip AnalysisContext entirely.
func encodeVocabulary(terms []string) (string, error) {
	if len(terms) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(terms)
	if err != nil {
		return "", fmt.Errorf("encode vocabulary terms: %w", err)
	}
	return string(encoded), nil
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

// AudioChunk is one slice of a continuously running capture session. Exactly one
// of Frames, TimedOut, and Closed is meaningful per read: a chunk, nothing yet,
// or the end of the session.
type AudioChunk struct {
	// StartedAtUnixNS is the wall clock of this chunk's boundary, derived by
	// offsetting the session anchor rather than by reading the clock at
	// rotation, so consecutive chunks are exactly one chunk duration apart and
	// the grid cannot drift over a recording of any length.
	//
	// Because a sample buffer is never split, the chunk's first sample can sit
	// up to one buffer (~100ms) before its boundary. That offset is bounded and
	// does not accumulate, and a caller needing the sample-accurate instant has
	// each track's own StartedAtUnixNS. The alternative — stamping the chunk
	// from its first sample — would be exact but jittery, and would give up the
	// uniform spacing that makes coverage arithmetic exact.
	StartedAtUnixNS int64        `json:"started_at_unix_ns"`
	Frames          []AudioFrame `json:"frames"`
	TimedOut        bool         `json:"timeout"`
	Closed          bool         `json:"closed"`
	// CaptureError is set alongside Closed when the stream ended because
	// ScreenCaptureKit failed rather than because it was stopped.
	CaptureError string `json:"capture_error,omitempty"`
}

// AudioSession is one continuously open ScreenCaptureKit audio stream, sliced
// into chunks by presentation timestamp. Holding the stream open is the whole
// point: stopping and restarting it per chunk left roughly two seconds of every
// thirty uncaptured, and the loss landed mid-sentence.
type AudioSession struct {
	handle C.int64_t

	mu     sync.Mutex
	closed bool
}

// StartAudioSession opens the stream. Files are named by chunk ordinal under
// prefix, because the session opens them before any caller has seen the chunk.
func StartAudioSession(ctx context.Context, directory, prefix string, chunkSeconds float64) (*AudioSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directoryC := C.CString(directory)
	prefixC := C.CString(prefix)
	defer C.free(unsafe.Pointer(directoryC))
	defer C.free(unsafe.Pointer(prefixC))
	var nativeErr *C.char
	handle := C.lumi_audio_session_start(directoryC, prefixC, C.double(chunkSeconds), &nativeErr)
	if handle == 0 {
		if err := nativeError(nativeErr); err != nil {
			return nil, fmt.Errorf("start ScreenCaptureKit audio session: %w", err)
		}
		return nil, errors.New("start ScreenCaptureKit audio session")
	}
	return &AudioSession{handle: handle}, nil
}

// Next waits up to timeout for the next finished chunk. It reports queued chunks
// even after Stop, so cancelling a recording never discards audio that was
// already captured.
func (s *AudioSession) Next(timeout time.Duration) (AudioChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return AudioChunk{Closed: true}, nil
	}
	var nativeErr *C.char
	result, err := nativeJSON(
		C.lumi_audio_session_next_json(s.handle, C.double(timeout.Seconds()), &nativeErr), nativeErr)
	if err != nil {
		return AudioChunk{}, err
	}
	var chunk AudioChunk
	if err := json.Unmarshal(result, &chunk); err != nil {
		return AudioChunk{}, fmt.Errorf("decode native audio chunk: %w", err)
	}
	return chunk, nil
}

// Stop ends capture and finalises the chunk in flight, which then arrives from
// Next like any other. It is safe to call more than once.
func (s *AudioSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	C.lumi_audio_session_stop(s.handle)
}

// Close stops the session and releases it. Chunks still queued are dropped, so
// callers that care about them drain with Next after Stop first.
func (s *AudioSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	C.lumi_audio_session_close(s.handle)
}

// RecordAudio captures a single chunk and stops, which is what the native smoke
// test wants. It runs on the same session the recorder uses so there is only one
// capture path to keep correct.
func RecordAudio(ctx context.Context, directory, prefix string, durationSeconds float64) ([]AudioFrame, error) {
	session, err := StartAudioSession(ctx, directory, prefix, durationSeconds)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	deadline := time.Now().Add(time.Duration(durationSeconds*float64(time.Second)) + 30*time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			session.Stop()
		}
		chunk, err := session.Next(250 * time.Millisecond)
		if err != nil {
			return nil, err
		}
		if len(chunk.Frames) > 0 {
			return chunk.Frames, nil
		}
		if chunk.Closed {
			if chunk.CaptureError != "" {
				return nil, fmt.Errorf("ScreenCaptureKit audio capture stopped: %s", chunk.CaptureError)
			}
			break
		}
	}
	return nil, errors.New("ScreenCaptureKit returned no audio")
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

// nativeError consumes an error message from a native call that reports failure
// some way other than by returning NULL, and always frees it.
func nativeError(errorMessage *C.char) error {
	if errorMessage == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(errorMessage))
	return errors.New(C.GoString(errorMessage))
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
