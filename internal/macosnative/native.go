//go:build darwin && arm64 && cgo

package macosnative

/*
#cgo CFLAGS: -fblocks -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework ApplicationServices -framework AudioToolbox -framework AVFoundation -framework CoreAudio -framework CoreGraphics -framework CoreMedia -framework CoreVideo -framework Foundation -framework ImageIO -framework IOKit -framework ScreenCaptureKit -framework UniformTypeIdentifiers -framework Vision -L${SRCDIR} -llumispeech -framework Speech
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
char *lumi_audio_processes_json(char **error_message);
char *lumi_audio_marker_windows_json(char **error_message);
char *lumi_audio_marker_windows_in_json(const char *windows_json, char **error_message);
int64_t lumi_audio_session_start(const char *directory, const char *prefix, double chunk_seconds,
                                 int32_t level_window_ms, char **error_message);
char *lumi_audio_session_levels_json(int64_t handle, char **error_message);
char *lumi_audio_session_next_json(int64_t handle, double timeout_seconds, char **error_message);
void lumi_audio_session_stop(int64_t handle);
void lumi_audio_session_close(int64_t handle);
void lumi_os_version(int *major, int *minor, int *patch);
char *lumi_transcribe_audio_string(const char *audio_path, const char *locale, double timeout_seconds, char **error_message);
char *lumi_transcribe_audio_segments_json(const char *audio_path, const char *locale, double timeout_seconds, char **error_message);
char *lumi_speech_ensure_assets(const char *locale, double timeout_seconds, char **error_message);
int lumi_speech_assets_installed(const char *locale);
char *lumi_image_inspect_json(const char *image_path, char **error_message);
char *lumi_image_transcode_heic_json(const char *source_path, const char *destination_path,
                                     double quality, char **error_message);
char *lumi_audio_encode_flac_json(const char *source_path, const char *destination_path,
                                  char **error_message);
uint8_t *lumi_audio_decode_pcm16(const char *path, int64_t *frames, int32_t *sample_rate,
                                 char **error_message);
*/
import "C"

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/puremetricsai/lumi/internal/transcript"
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

// AudioProcess is one application holding an active audio output stream.
// BundleID and Name are omitted by the native side when they could not be
// resolved, so a process with only a PID is a real answer rather than a
// malformed one — it still held a stream.
type AudioProcess struct {
	PID      int32  `json:"pid"`
	BundleID string `json:"bundle_id,omitempty"`
	Name     string `json:"name,omitempty"`
}

// AudioProcesses lists the processes holding an active audio output stream at
// this instant. An empty slice means none did, which is an answer and not a
// failure.
//
// It reports stream occupancy rather than audible sound: a paused player still
// answers yes. CoreAudio exposes no per-process level, so nothing stronger is
// available without a tap.
//
// This reads CoreAudio's process objects; it never opens a tap, so it captures
// no audio and needs no grant beyond what the process already holds. Callers
// filter it — notably of their own pid, which the audio session excludes from
// the recording anyway.
func AudioProcesses(ctx context.Context) ([]AudioProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_audio_processes_json(&nativeErr), nativeErr)
	if err != nil {
		return nil, err
	}
	var processes []AudioProcess
	if err := json.Unmarshal(result, &processes); err != nil {
		return nil, fmt.Errorf("decode audio process list: %w", err)
	}
	return processes, nil
}

// AudioMarkerWindow is one on-screen window whose own title declares it is
// playing audio. Window carries the title with the marker removed.
type AudioMarkerWindow struct {
	PID      int32  `json:"pid"`
	BundleID string `json:"bundle_id,omitempty"`
	Name     string `json:"name,omitempty"`
	Window   string `json:"window,omitempty"`
}

// AudioMarkerWindows scans every on-screen window for the marker Chromium
// appends to a title while a tab is playing sound.
//
// It is the fallback for when CoreAudio names no emitter, and it deliberately
// looks past the focused window: the case worth catching is a browser playing in
// the background while the user works elsewhere, which is precisely the case a
// focused-window reading gets wrong.
//
// An empty slice means no window declared audio. Only windows carrying the
// marker are returned, and no other window's title leaves the native side.
func AudioMarkerWindows(ctx context.Context) ([]AudioMarkerWindow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_audio_marker_windows_json(&nativeErr), nativeErr)
	if err != nil {
		return nil, err
	}
	var windows []AudioMarkerWindow
	if err := json.Unmarshal(result, &windows); err != nil {
		return nil, fmt.Errorf("decode audio marker window list: %w", err)
	}
	return windows, nil
}

// AudioMarkerWindowsIn runs the same scan over a supplied window list. It exists
// for the reason the other *In resolvers do: asserting the live scan would pass
// vacuously in any process that happens to have nothing playing, so it could only
// fail where nothing is asserting.
func AudioMarkerWindowsIn(windowsJSON string) ([]AudioMarkerWindow, error) {
	cWindows := C.CString(windowsJSON)
	defer C.free(unsafe.Pointer(cWindows))
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_audio_marker_windows_in_json(cWindows, &nativeErr), nativeErr)
	if err != nil {
		return nil, err
	}
	var windows []AudioMarkerWindow
	if err := json.Unmarshal(result, &windows); err != nil {
		return nil, fmt.Errorf("decode audio marker window list: %w", err)
	}
	return windows, nil
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

// TranscribeAudio transcribes a WAV file with on-device SpeechAnalyzer.
func TranscribeAudio(ctx context.Context, audioPath, locale string) (string, error) {
	return transcribeNative(ctx, audioPath, locale,
		func(pathC, localeC *C.char, timeout C.double, nativeErr **C.char) *C.char {
			return C.lumi_transcribe_audio_string(pathC, localeC, timeout, nativeErr)
		})
}

// TranscribeAudioSegments transcribes a WAV file and returns the transcript
// together with per-phrase spans and per-word timings. It shares the native ASR
// path with TranscribeAudio, so the transcripts cannot diverge.
func TranscribeAudioSegments(ctx context.Context, audioPath, locale string) (Transcription, error) {
	raw, err := transcribeNative(ctx, audioPath, locale,
		func(pathC, localeC *C.char, timeout C.double, nativeErr **C.char) *C.char {
			return C.lumi_transcribe_audio_segments_json(pathC, localeC, timeout, nativeErr)
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
// be interrupted. Both callers share it to keep cancellation and timeout
// identical.
func transcribeNative(ctx context.Context, audioPath, locale string,
	call func(pathC, localeC *C.char, timeout C.double, nativeErr **C.char) *C.char,
) (string, error) {
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
			call(pathC, localeC, C.double(timeout.Seconds()), &nativeErr), nativeErr)
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

// AudioChunk is one slice of a continuously running capture session. A read
// returns a chunk (Frames), nothing yet (neither), or the end of the session
// (Closed). The native layer also reports a poll timeout, which is decoded away
// deliberately: "no chunk yet" and "no chunk yet, and the poll expired" call for
// the same thing from every caller, and a field nobody branches on invites one.
type AudioChunk struct {
	// StartedAtUnixNS is the wall clock of this chunk's boundary, *measured* at
	// rotation by reading the host clock and ageing it back to the boundary
	// presentation timestamp.
	//
	// It used to be arithmetic on the session anchor — anchor + N×chunkDuration —
	// which made every audio timestamp in an index share one sub-second fraction.
	// That is uniform but not observed: clock drift could not be detected, a
	// dropped chunk renumbered silently instead of leaving a visible hole, and
	// correlation with screen events degraded over a long session with nothing to
	// show for it. Ageing the clock rather than reading NSDate on arrival is what
	// keeps this from being the old "read time.Now() between chunks" bug, which
	// made every stamp absorb the previous chunk's processing time.
	//
	// The uniform grid is not lost, only moved: StreamOffsetNS carries the exact,
	// drift-free distance from the session anchor, and GridStartedAtUnixNS carries
	// the grid point itself, so coverage arithmetic stays exact.
	//
	// Because a sample buffer is never split, the chunk's first sample can sit up
	// to one buffer (~100ms) before its boundary. That offset is bounded and does
	// not accumulate, and a caller needing the sample-accurate instant has each
	// track's own StartedAtUnixNS.
	StartedAtUnixNS int64 `json:"started_at_unix_ns"`
	// GridStartedAtUnixNS is where this chunk sits on the drift-free grid. It is
	// what StartedAtUnixNS used to be, kept so a measured stamp can be compared
	// against the arithmetic it replaced. Zero when unreported.
	GridStartedAtUnixNS int64 `json:"grid_started_at_unix_ns"`
	// StreamOffsetNS is the boundary's exact distance from the session anchor. It
	// is a pointer because zero is a real value — every session's first chunk has
	// it — and must stay distinct from "not reported".
	StreamOffsetNS *int64 `json:"stream_offset_ns"`
	// ClockAnomaly reports that the measured read disagreed with the grid by more
	// than the guard allows, or stepped backwards, so the grid value was used.
	// A backwards step is guarded because turn continuation across a chunk
	// boundary requires a strictly positive gap.
	ClockAnomaly bool         `json:"clock_anomaly"`
	Frames       []AudioFrame `json:"frames"`
	Closed       bool         `json:"closed"`
	// CaptureError is set alongside Closed when the stream ended because
	// ScreenCaptureKit failed rather than because it was stopped.
	CaptureError string `json:"capture_error,omitempty"`
}

// TrackLevels is one drain of the live meters: the energy each track received
// since the previous call, window by window.
//
// The figures are mean squares of samples normalised so that full scale is 1.0,
// which is what wav.DBFSFromMeanSquare consumes. They are measured on the stream
// as it arrives — before the writer downmixes it to Lumi's mono 16kHz WAV — so a
// live reading and the stored file's envelope will differ slightly. That is the
// signal genuinely being different, not the measurement disagreeing: the formula
// and the floor are the same one, in internal/wav.
type TrackLevels struct {
	WindowMS   int                  `json:"window_ms"`
	System     []float64            `json:"system"`
	Microphone []float64            `json:"microphone"`
	Diag       map[string][]float64 `json:"diag"`
}

// AudioSession is one continuously open ScreenCaptureKit audio stream, sliced
// into chunks by presentation timestamp. Holding the stream open is the whole
// point: stopping and restarting it per chunk left roughly two seconds of every
// thirty uncaptured, and the loss landed mid-sentence.
type AudioSession struct {
	handle C.int64_t

	mu sync.Mutex
	// closed is atomic rather than guarded by mu, so Levels never contends for
	// it. Next holds mu for the whole of its 250ms wait, and a level poll that
	// queued behind that wait would be exactly the stutter live metering exists
	// to remove.
	closed atomic.Bool
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
	handle := C.lumi_audio_session_start(directoryC, prefixC, C.double(chunkSeconds),
		C.int32_t(transcript.EnvelopeWindowMS), &nativeErr)
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
	if s.closed.Load() {
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

// Levels reports the sound each track has received since the last call, as a
// windowed energy envelope per track at transcript.EnvelopeWindowMS.
//
// It returns energy, not decibels, and the conversion is wav.DBFSFromMeanSquare
// — the native side holds no copy of the formula or the silence floor. Each
// window is handed over exactly once, so polling slowly loses resolution but
// never loses sound, up to the native ring's depth.
//
// An empty envelope for a track is not silence. It means no window finished in
// the interval, which is what a caller polling faster than the window length
// sees; silence completes windows too, at the floor.
//
// Deliberately does not take mu: see the field's comment. Close sets closed
// before it releases the handle, so this can still be in flight when the handle
// goes — and that is left as a race on purpose. The native side answers an
// unknown handle with an error rather than touching freed memory, handles are
// never reused, and the caller logs a level failure at debug and carries on.
// Taking mu to close the window would put a level poll behind Next's 250ms wait,
// which costs the smooth meter this exists for.
func (s *AudioSession) Levels() (TrackLevels, error) {
	if s.closed.Load() {
		return TrackLevels{}, nil
	}
	var nativeErr *C.char
	result, err := nativeJSON(C.lumi_audio_session_levels_json(s.handle, &nativeErr), nativeErr)
	if err != nil {
		return TrackLevels{}, err
	}
	var levels TrackLevels
	if err := json.Unmarshal(result, &levels); err != nil {
		return TrackLevels{}, fmt.Errorf("decode native audio levels: %w", err)
	}
	if levels.WindowMS != transcript.EnvelopeWindowMS {
		return TrackLevels{}, fmt.Errorf(
			"native audio levels were measured at %dms, want transcript.EnvelopeWindowMS (%dms)",
			levels.WindowMS, transcript.EnvelopeWindowMS)
	}
	return levels, nil
}

// Stop ends capture and finalises the chunk in flight, which then arrives from
// Next like any other. It is safe to call more than once.
func (s *AudioSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return
	}
	C.lumi_audio_session_stop(s.handle)
}

// Close stops the session and releases it. Chunks still queued are dropped, so
// callers that care about them drain with Next after Stop first.
func (s *AudioSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return
	}
	s.closed.Store(true)
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

// ImageVerification is what a written image decodes back to, measured against
// the source that produced it.
//
// Every field is read from the file on disk rather than from the encoder's
// in-memory result, because a truncated write still finalises successfully.
// Callers compare Width/Height against Source* to catch that, and use PSNRDB and
// HistogramSimilarity to catch an encode that produced the wrong picture at the
// right size. The two are independent on purpose: a colour-space or channel-order
// mistake can pass one and fail the other.
type ImageVerification struct {
	Width       int `json:"width"`
	Height      int `json:"height"`
	SourceWidth int `json:"source_width"`
	// SourceHeight, with SourceWidth, is the size of the image handed to the
	// encoder. InspectImage reports it equal to Width/Height, since it has no
	// second image to compare against.
	SourceHeight        int     `json:"source_height"`
	PSNRDB              float64 `json:"psnr_db"`
	HistogramSimilarity float64 `json:"histogram_similarity"`
	Bytes               int64   `json:"bytes"`
}

// TranscodeImageHEIC re-encodes an image as HEIC at the given quality (0..1) and
// reports what the written file decodes back to. It does not delete the source.
func TranscodeImageHEIC(ctx context.Context, sourcePath, destinationPath string, quality float64) (ImageVerification, error) {
	if err := ctx.Err(); err != nil {
		return ImageVerification{}, err
	}
	sourceC := C.CString(sourcePath)
	defer C.free(unsafe.Pointer(sourceC))
	destinationC := C.CString(destinationPath)
	defer C.free(unsafe.Pointer(destinationC))
	var nativeErr *C.char
	raw, err := nativeJSON(
		C.lumi_image_transcode_heic_json(sourceC, destinationC, C.double(quality), &nativeErr), nativeErr)
	if err != nil {
		return ImageVerification{}, fmt.Errorf("transcode %s to HEIC: %w", sourcePath, err)
	}
	var verification ImageVerification
	if err := json.Unmarshal(raw, &verification); err != nil {
		return ImageVerification{}, fmt.Errorf("decode HEIC verification: %w", err)
	}
	return verification, nil
}

// InspectImage reports what an existing image decodes to without re-encoding it.
// It fills SourceWidth/SourceHeight from the same image and reports a perfect
// PSNR and histogram, so a caller holding no original can apply one set of
// checks to both cases.
func InspectImage(ctx context.Context, path string) (ImageVerification, error) {
	if err := ctx.Err(); err != nil {
		return ImageVerification{}, err
	}
	pathC := C.CString(path)
	defer C.free(unsafe.Pointer(pathC))
	var nativeErr *C.char
	raw, err := nativeJSON(C.lumi_image_inspect_json(pathC, &nativeErr), nativeErr)
	if err != nil {
		return ImageVerification{}, fmt.Errorf("inspect image %s: %w", path, err)
	}
	var verification ImageVerification
	if err := json.Unmarshal(raw, &verification); err != nil {
		return ImageVerification{}, fmt.Errorf("decode image inspection: %w", err)
	}
	return verification, nil
}

// AudioEncoding describes a file an encoder wrote.
type AudioEncoding struct {
	Bytes      int64 `json:"bytes"`
	Frames     int64 `json:"frames"`
	SampleRate int   `json:"sample_rate"`
}

// EncodeAudioFLAC re-encodes an audio file as lossless FLAC. It does not delete
// the source, and it does not verify the result: FLAC being lossless, the
// caller's comparison of decoded samples is exact and is the stronger check.
func EncodeAudioFLAC(ctx context.Context, sourcePath, destinationPath string) (AudioEncoding, error) {
	if err := ctx.Err(); err != nil {
		return AudioEncoding{}, err
	}
	sourceC := C.CString(sourcePath)
	defer C.free(unsafe.Pointer(sourceC))
	destinationC := C.CString(destinationPath)
	defer C.free(unsafe.Pointer(destinationC))
	var nativeErr *C.char
	raw, err := nativeJSON(C.lumi_audio_encode_flac_json(sourceC, destinationC, &nativeErr), nativeErr)
	if err != nil {
		return AudioEncoding{}, fmt.Errorf("encode %s as FLAC: %w", sourcePath, err)
	}
	var encoding AudioEncoding
	if err := json.Unmarshal(raw, &encoding); err != nil {
		return AudioEncoding{}, fmt.Errorf("decode FLAC encoding report: %w", err)
	}
	return encoding, nil
}

// DecodeMonoPCM16 decodes any file AVFoundation can open into mono 16-bit
// samples at the file's own sample rate.
//
// It exists because internal/wav reads mono 16-bit PCM RIFF and nothing else, by
// design — it is pure Go so it builds and tests anywhere. Once `lumi compress`
// stores a chunk as FLAC, something still has to measure that chunk's energy;
// this is the half that has to know about containers, and internal/wav keeps the
// half that measures samples.
func DecodeMonoPCM16(ctx context.Context, path string) ([]int16, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	pathC := C.CString(path)
	defer C.free(unsafe.Pointer(pathC))
	var frames C.int64_t
	var sampleRate C.int32_t
	var nativeErr *C.char
	buffer := C.lumi_audio_decode_pcm16(pathC, &frames, &sampleRate, &nativeErr)
	if buffer == nil {
		if err := nativeError(nativeErr); err != nil {
			return nil, 0, fmt.Errorf("decode %s: %w", path, err)
		}
		return nil, 0, fmt.Errorf("decode %s: native audio decode failed", path)
	}
	defer C.free(unsafe.Pointer(buffer))
	if nativeErr != nil {
		C.free(unsafe.Pointer(nativeErr))
	}
	// The bridge owns none of these numbers once it has returned, so they are
	// checked here rather than trusted: C.GoBytes takes an int length, and a
	// negative or overlarge count would read outside the buffer.
	// Bounds are checked before the multiplication, not after: frames is int64
	// and C.GoBytes takes an int length, so a count above MaxInt32/2 would wrap
	// negative and slip past a check made on the product.
	if frames < 0 || sampleRate <= 0 {
		return nil, 0, fmt.Errorf("decode %s: native decoder reported %d frames at %d Hz", path, int64(frames), int32(sampleRate))
	}
	if int64(frames) > int64(math.MaxInt32)/2 {
		return nil, 0, fmt.Errorf("decode %s: %d frames is too many to copy", path, int64(frames))
	}
	byteCount := int64(frames) * 2
	raw := C.GoBytes(unsafe.Pointer(buffer), C.int(byteCount))
	samples := make([]int16, frames)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return samples, int(sampleRate), nil
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
