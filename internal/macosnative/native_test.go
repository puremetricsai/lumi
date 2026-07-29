//go:build darwin && arm64 && cgo

package macosnative

import (
	"context"
	"encoding/json"
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

// windowsJSON builds a CGWindowListCopyWindowInfo-shaped array. The keys are the
// literal CoreGraphics constant strings, which fails safe: if a constant ever
// diverged from its literal the resolver would stop matching and these tests
// would fail, rather than passing vacuously.
func windowsJSON(t *testing.T, windows ...map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(windows)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func window(pid int, layer int, owner string) map[string]any {
	entry := map[string]any{"kCGWindowOwnerPID": pid, "kCGWindowLayer": layer}
	if owner != "" {
		entry["kCGWindowOwnerName"] = owner
	}
	return entry
}

// TestResolveFrontmostPrefersTheFrontOnScreenWindow pins the branch order that
// decides which app an event is attributed to. It exists because asserting the
// live resolution cannot catch the regression it guards: NSWorkspace is correct
// in any foreground process, so a live assertion passes vacuously in every test
// run and only fails in the recorder daemon, where nothing is asserting.
func TestResolveFrontmostPrefersTheFrontOnScreenWindow(t *testing.T) {
	const ghostty, comet, controlCenter, self = 8123, 9440, 501, 4242
	const finder = 7100
	for _, testCase := range []struct {
		name          string
		windows       string
		activePID     int32
		wantPID       int32
		wantApp       string
		wantAppSource string
	}{
		{
			// Accessibility is the only source that answers activation rather
			// than visibility. Without it, an app whose windows are all closed
			// or minimized is attributed to whatever is visually behind it —
			// which is not "what the user was working in" (CLAUDE.md).
			name:          "an active app with no window still wins over the front window",
			windows:       windowsJSON(t, window(comet, 0, "Comet")),
			activePID:     finder,
			wantPID:       finder,
			wantApp:       "",
			wantAppSource: "accessibility",
		},
		{
			name:          "an active app is named from its own window entry",
			windows:       windowsJSON(t, window(comet, 0, "Comet"), window(finder, 0, "Finder")),
			activePID:     finder,
			wantPID:       finder,
			wantApp:       "Finder",
			wantAppSource: "accessibility",
		},
		{
			name:          "an active app matching the workspace borrows its name",
			windows:       windowsJSON(t, window(comet, 0, "Comet")),
			activePID:     ghostty,
			wantPID:       ghostty,
			wantApp:       "Ghostty",
			wantAppSource: "accessibility",
		},
		{
			// Accessibility is unreliable per-application, so an unavailable
			// activation pid must fall through rather than blank attribution.
			name:          "an unavailable activation pid falls through to the window list",
			windows:       windowsJSON(t, window(comet, 0, "Comet")),
			activePID:     0,
			wantPID:       comet,
			wantApp:       "Comet",
			wantAppSource: "window_list",
		},
		{
			// The reported bug: the daemon was launched by Ghostty, so
			// NSWorkspace froze there while Comet held the front window.
			name:          "stale workspace pid loses to the front window",
			windows:       windowsJSON(t, window(comet, 0, "Comet"), window(ghostty, 0, "Ghostty")),
			wantPID:       comet,
			wantApp:       "Comet",
			wantAppSource: "window_list",
		},
		{
			name:          "menu bar and overlays are skipped",
			windows:       windowsJSON(t, window(controlCenter, 25, "Control Center"), window(comet, 0, "Comet")),
			wantPID:       comet,
			wantApp:       "Comet",
			wantAppSource: "window_list",
		},
		{
			name:          "the recorder's own window is skipped",
			windows:       windowsJSON(t, window(self, 0, "lumi"), window(comet, 0, "Comet")),
			wantPID:       comet,
			wantApp:       "Comet",
			wantAppSource: "window_list",
		},
		{
			name:          "a zero owner pid is skipped",
			windows:       windowsJSON(t, window(0, 0, ""), window(comet, 0, "Comet")),
			wantPID:       comet,
			wantApp:       "Comet",
			wantAppSource: "window_list",
		},
		{
			name:          "the window list still wins when it agrees",
			windows:       windowsJSON(t, window(ghostty, 0, "Ghostty")),
			wantPID:       ghostty,
			wantApp:       "Ghostty",
			wantAppSource: "window_list",
		},
		{
			// The name is borrowable here and only here: both sources mean the
			// same process, so there is no app to confuse it with.
			name:          "an unnamed front window borrows a matching workspace name",
			windows:       windowsJSON(t, window(ghostty, 0, "")),
			wantPID:       ghostty,
			wantApp:       "Ghostty",
			wantAppSource: "window_list",
		},
		{
			// The subtle one. Borrowing "Ghostty" for Comet's pid would name one
			// app while reading another's title — the original bug in a form no
			// other case here catches.
			name:          "an unnamed front window keeps its own pid rather than borrowing a name",
			windows:       windowsJSON(t, window(comet, 0, "")),
			wantPID:       comet,
			wantApp:       "",
			wantAppSource: "window_list",
		},
		{
			name:          "an empty list falls back to the workspace",
			windows:       "[]",
			wantPID:       ghostty,
			wantApp:       "Ghostty",
			wantAppSource: "workspace",
		},
		{
			name:          "a list with no layer-0 window falls back to the workspace",
			windows:       windowsJSON(t, window(controlCenter, 25, "Control Center")),
			wantPID:       ghostty,
			wantApp:       "Ghostty",
			wantAppSource: "workspace",
		},
		{
			// An uncopyable window list arrives as JSON null, not as an array.
			name:          "an absent list falls back to the workspace",
			windows:       "null",
			wantPID:       ghostty,
			wantApp:       "Ghostty",
			wantAppSource: "workspace",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolved, err := resolveFrontmost(testCase.windows, testCase.activePID, ghostty, "Ghostty", self)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.PID != testCase.wantPID {
				t.Errorf("resolved pid %d, want %d — attribution would follow the wrong process",
					resolved.PID, testCase.wantPID)
			}
			if resolved.App != testCase.wantApp {
				t.Errorf("resolved app %q, want %q — the event would name an app that is not frontmost",
					resolved.App, testCase.wantApp)
			}
			if resolved.AppSource != testCase.wantAppSource {
				t.Errorf("resolved app source %q, want %q", resolved.AppSource, testCase.wantAppSource)
			}
		})
	}
}

// TestResolveFrontmostNamesSomethingWheneverASourceCan pins the other half of
// the attribution invariant: the fix must not buy correctness by returning
// nothing. Only a total failure of both sources may resolve to nothing at all.
func TestResolveFrontmostNamesSomethingWheneverASourceCan(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		windows      string
		workspacePID int32
		workspaceApp string
	}{
		{"window list only", windowsJSON(t, window(9440, 0, "Comet")), 0, ""},
		{"workspace only", "[]", 8123, "Ghostty"},
		{"both", windowsJSON(t, window(9440, 0, "Comet")), 8123, "Ghostty"},
		{"an unnamed window still yields a pid", windowsJSON(t, window(9440, 0, "")), 0, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolved, err := resolveFrontmost(testCase.windows, 0, testCase.workspacePID, testCase.workspaceApp, 4242)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.App == "" && resolved.PID == 0 {
				t.Error("resolved nothing while a source could name the frontmost process")
			}
		})
	}
}

func TestResolveFrontmostRejectsMalformedWindows(t *testing.T) {
	if _, err := resolveFrontmost("{not json", 0, 8123, "Ghostty", 4242); err == nil {
		t.Error("malformed window list resolved without an error")
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
	// Provenance must always be recorded, so a wrong app in the index can be
	// traced to the source that named it.
	switch snapshot.AppSource {
	case "accessibility", "window_list", "running_application", "workspace":
	default:
		t.Errorf("Accessibility named %q with unknown app source %q", snapshot.App, snapshot.AppSource)
	}
}

// TestFrontmostDiagnosticIsInternallyConsistent checks only what one call can
// prove. It deliberately does not compare against a separate Accessibility
// snapshot: those are two native calls, and a focus change between them makes
// them differ for a legitimate reason, so such a test fails intermittently
// while proving nothing. That the two share a resolver is guaranteed by
// construction and pinned by the pure-resolver tests above.
func TestFrontmostDiagnosticIsInternallyConsistent(t *testing.T) {
	report, err := FrontmostDiagnostic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	switch report.Resolved.AppSource {
	case "accessibility", "window_list", "running_application", "workspace":
	default:
		t.Errorf("diagnostic resolved via unknown source %q", report.Resolved.AppSource)
	}
	if report.Resolved.PID == 0 && report.Resolved.App == "" {
		t.Error("diagnostic resolved no frontmost process at all")
	}
	// Whichever source won must be the one the verdict's pid came from.
	switch report.Resolved.AppSource {
	case "accessibility":
		if report.Resolved.PID != report.Accessibility.PID {
			t.Errorf("resolved via accessibility with pid %d, but that source reported %d",
				report.Resolved.PID, report.Accessibility.PID)
		}
	case "window_list":
		if report.Resolved.PID != report.WindowList.PID {
			t.Errorf("resolved via the window list with pid %d, but that source reported %d",
				report.Resolved.PID, report.WindowList.PID)
		}
	case "workspace":
		if report.Resolved.PID != report.Workspace.PID {
			t.Errorf("resolved via the workspace with pid %d, but that source reported %d",
				report.Resolved.PID, report.Workspace.PID)
		}
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
