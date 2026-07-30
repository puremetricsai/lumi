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
	"time"
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

// TestResolveFrontmostAttributesAppSwitchBoundaries is the regression test for
// events 28603 and 28609, captured 2026-07-28 23:44:20 and 23:44:32. Both were
// stamped "Comet" while the menu bar in their own OCR read "Screen Sharing" and
// "Messages" respectively, and both recorded app_source="window_list" — the
// system-wide Accessibility read had returned nothing, so the resolver fell
// through to the identical topmost layer-0 window, Comet's Gmail tab.
//
// The window list is not wrong about window order; it is answering the wrong
// question at an app-switch boundary, where the front window is still (or
// already) some other app's while activation has moved. Validating candidates
// against per-application kAXFrontmost supplies the activation pid the
// system-wide read could not, which is what these cases assert.
func TestResolveFrontmostAttributesAppSwitchBoundaries(t *testing.T) {
	const comet, screenSharing, messages, self = 5120, 6210, 1198, 4242
	// Identical in both events: Comet's Gmail tab was the topmost layer-0 window.
	windows := windowsJSON(t, window(comet, 0, "Comet"))

	for _, testCase := range []struct {
		name      string
		event     int
		activePID int32
		wantApp   string
	}{
		{"event 28603 was stamped Comet with Screen Sharing in the menu bar", 28603, screenSharing, "Screen Sharing"},
		{"event 28609 was stamped Comet with Messages in the menu bar", 28609, messages, "Messages"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// The activation pid carries the name here only when the app owns a
			// listed window; these apps did not, so the caller names it. What
			// must never happen is inheriting Comet's pid.
			resolved, err := resolveFrontmost(windows, testCase.activePID, 0, "", self)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.PID == comet {
				t.Fatalf("event %d: resolved Comet's pid again — the window list still wins over activation",
					testCase.event)
			}
			if resolved.PID != testCase.activePID {
				t.Errorf("event %d: resolved pid %d, want the active %d (%s)",
					testCase.event, resolved.PID, testCase.activePID, testCase.wantApp)
			}
			if resolved.AppSource != "accessibility" {
				t.Errorf("event %d: app_source = %q, want accessibility — window_list is what misattributed it",
					testCase.event, resolved.AppSource)
			}
		})
	}

	// With no activation source at all, the resolver still falls back to the
	// window list rather than losing attribution: that is the pre-fix behaviour,
	// and it stays the floor rather than becoming the answer.
	resolved, err := resolveFrontmost(windows, 0, 0, "", self)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.PID != comet || resolved.AppSource != "window_list" {
		t.Errorf("without activation, resolved %d via %s, want Comet via window_list",
			resolved.PID, resolved.AppSource)
	}
}

// TestFrontmostCandidatesReachWindowlessApplications covers the gap the
// resolver tests structurally cannot: they supply the activation pid, but the
// question here is whether such a process is *reachable* at all. An application
// with every window minimized or closed owns no window-list entry, so a walk
// seeded only from on-screen owners can never ask it, returns nothing, and the
// resolver falls back to whichever app is visually behind it.
func TestFrontmostCandidatesReachWindowlessApplications(t *testing.T) {
	const comet, finder, self = 9440, 7100, 4242
	const notificationCenter = 1121

	contains := func(candidates []int32, pid int32) bool {
		for _, candidate := range candidates {
			if candidate == pid {
				return true
			}
		}
		return false
	}

	t.Run("a windowless regular application is still asked", func(t *testing.T) {
		candidates, err := frontmostCandidates(
			windowsJSON(t, window(comet, 0, "Comet")), "[9440,7100]", self)
		if err != nil {
			t.Fatal(err)
		}
		if !contains(candidates, finder) {
			t.Errorf("candidates %v omit the windowless application %d — it can never be "+
				"asked whether it is frontmost, so the frame goes to whatever is visible",
				candidates, finder)
		}
	})

	t.Run("on-screen owners are asked before windowless applications", func(t *testing.T) {
		candidates, err := frontmostCandidates(
			windowsJSON(t, window(comet, 0, "Comet")), "[7100,9440]", self)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) == 0 || candidates[0] != comet {
			t.Errorf("candidates %v do not start with the front window's owner %d; the "+
				"frontmost app almost always owns it, and asking it first is what keeps "+
				"this to one round-trip", candidates, comet)
		}
	})

	t.Run("a background agent is never a candidate", func(t *testing.T) {
		// Notification Center answers kAXFrontmost affirmatively. An unfiltered
		// walk was measured attributing frames to it, so eligibility must come
		// from the regular-activation-policy list and nowhere else.
		candidates, err := frontmostCandidates(
			windowsJSON(t, window(comet, 0, "Comet")), "[9440]", self)
		if err != nil {
			t.Fatal(err)
		}
		if contains(candidates, notificationCenter) {
			t.Errorf("candidates %v include background agent %d, which claims frontmost "+
				"spuriously", candidates, notificationCenter)
		}
	})

	t.Run("the recorder itself is never a candidate", func(t *testing.T) {
		candidates, err := frontmostCandidates(
			windowsJSON(t, window(self, 0, "lumi")), "[4242,9440]", self)
		if err != nil {
			t.Fatal(err)
		}
		if contains(candidates, self) {
			t.Errorf("candidates %v include the recorder's own pid", candidates)
		}
	})

	t.Run("candidates are deduplicated and bounded", func(t *testing.T) {
		candidates, err := frontmostCandidates(
			windowsJSON(t, window(comet, 0, "Comet"), window(comet, 0, "Comet")), "[9440,7100]", self)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[int32]bool{}
		for _, candidate := range candidates {
			if seen[candidate] {
				t.Errorf("candidates %v ask pid %d twice", candidates, candidate)
			}
			seen[candidate] = true
		}
		if len(candidates) > 48 {
			t.Errorf("candidates %d exceed the bound; the walk is per-tick AX traffic", len(candidates))
		}
	})

	t.Run("no regular applications leaves the window owners", func(t *testing.T) {
		candidates, err := frontmostCandidates(windowsJSON(t, window(comet, 0, "Comet")), "[]", self)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0] != comet {
			t.Errorf("candidates %v, want just the window owner %d", candidates, comet)
		}
	})
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
	case "accessibility", "accessibility_frontmost", "window_list", "running_application", "workspace":
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
	case "accessibility", "accessibility_frontmost", "window_list", "running_application", "workspace":
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

// TestTranscriptionPayloadDecode covers the wire shape without needing Speech
// Recognition, so CI still catches a Swift-side rename. The absent cases matter:
// the bridge omits confidence when the recognizer reported none, and omits runs
// for an empty-text result — and an empty-text result carrying a span is a
// signal, not noise. It says audio was present here but no words resolved, which
// is what separates "nothing was playing" from "transcription failed".
func TestTranscriptionPayloadDecode(t *testing.T) {
	const payload = `{"text":"Mm-hmm. Yeah.","segments":[
		{"start_ms":3000,"end_ms":3660,"text":"Mm-hmm.","confidence":0.51,
		 "runs":[{"start_ms":3000,"end_ms":3660,"text":"Mm-hmm.","confidence":0.51}]},
		{"start_ms":18660,"end_ms":19680,"text":""},
		{"start_ms":26280,"end_ms":27720,"text":" Yeah."}
	]}`
	var decoded Transcription
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Text != "Mm-hmm. Yeah." {
		t.Errorf("text = %q", decoded.Text)
	}
	if len(decoded.Segments) != 3 {
		t.Fatalf("decoded %d segments, want 3", len(decoded.Segments))
	}
	if got := decoded.Segments[0].Runs[0].Confidence; got != 0.51 {
		t.Errorf("run confidence = %v, want 0.51", got)
	}
	silent := decoded.Segments[1]
	if silent.Text != "" || len(silent.Runs) != 0 {
		t.Errorf("expected an empty-text runless segment, got %#v", silent)
	}
	if silent.StartMS != 18660 || silent.EndMS != 19680 {
		t.Errorf("empty-text segment lost its span: %d..%d", silent.StartMS, silent.EndMS)
	}
	if got := decoded.Segments[2].Confidence; got != 0 {
		t.Errorf("absent confidence decoded as %v, want 0 meaning not reported", got)
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
	if _, err := TranscribeAudio(ctx, audio[0].Path, "en-US", nil); err != nil {
		t.Fatalf("SpeechAnalyzer transcription failed: %v", err)
	}
}

// TestTranscribeAudioSegmentsAgreesWithTranscribeAudio pins the reason both entry
// points share one native ASR path. events.text for every already-indexed row is
// the recognizer's results concatenated with no separator; if the segments path
// ever joined them differently — with a space, say — new rows would stop matching
// old ones and the FTS index would shift for reasons unrelated to any feature.
//
// It also pins that segments tile forward in time, which the attribution code
// relies on when it unions run intervals to measure overlap.
func TestTranscribeAudioSegmentsAgreesWithTranscribeAudio(t *testing.T) {
	if os.Getenv("LUMI_NATIVE_SMOKE") != "1" {
		t.Skip("set LUMI_NATIVE_SMOKE=1 after granting Speech Recognition and installing en-US assets")
	}
	ctx := context.Background()
	directory := t.TempDir()
	audio, err := RecordAudio(ctx, directory, "speech", 2.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(audio) == 0 {
		t.Fatal("RecordAudio returned no frames")
	}
	path := audio[0].Path

	flat, err := TranscribeAudio(ctx, path, "en-US", nil)
	if err != nil {
		t.Fatalf("flat transcription failed: %v", err)
	}
	timed, err := TranscribeAudioSegments(ctx, path, "en-US", nil)
	if err != nil {
		t.Fatalf("segmented transcription failed: %v", err)
	}
	if timed.Text != flat {
		t.Errorf("segmented text differs from flat text:\n flat=%q\ntimed=%q", flat, timed.Text)
	}
	joined := ""
	for _, segment := range timed.Segments {
		joined += segment.Text
	}
	if joined != timed.Text {
		t.Errorf("segments joined with no separator = %q, want %q", joined, timed.Text)
	}
	previousEnd := int64(-1)
	for i, segment := range timed.Segments {
		if segment.EndMS < segment.StartMS {
			t.Errorf("segment %d ends before it starts: %d..%d", i, segment.StartMS, segment.EndMS)
		}
		if segment.StartMS < previousEnd {
			t.Errorf("segment %d starts at %d, before segment %d ended at %d",
				i, segment.StartMS, i-1, previousEnd)
		}
		previousEnd = segment.EndMS
		for j, run := range segment.Runs {
			if run.StartMS < segment.StartMS || run.EndMS > segment.EndMS {
				t.Errorf("segment %d run %d spans %d..%d, outside its segment %d..%d",
					i, j, run.StartMS, run.EndMS, segment.StartMS, segment.EndMS)
			}
		}
	}
}

// TestTranscribeAudioSegmentsOnRealAudio inspects a WAV that actually contains
// speech, which the smoke tests above cannot guarantee — a freshly recorded
// second of a quiet room yields zero segments and makes their assertions
// vacuous. Point it at a captured chunk:
//
//	LUMI_SEGMENTS_AUDIO="$HOME/Library/Application Support/Lumi/audio/…-microphone.wav" \
//	  go test ./internal/macosnative -run RealAudio -v
//
// It stays in the suite because threshold calibration needs exactly this: a way
// to dump real per-word timings for a known chunk.
func TestTranscribeAudioSegmentsOnRealAudio(t *testing.T) {
	path := os.Getenv("LUMI_SEGMENTS_AUDIO")
	if path == "" {
		t.Skip("set LUMI_SEGMENTS_AUDIO to a WAV containing speech")
	}
	result, err := TranscribeAudioSegments(context.Background(), path, "en-US", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Segments) == 0 {
		t.Fatalf("no segments for %s; is it silent?", path)
	}
	words := 0
	for _, segment := range result.Segments {
		words += len(segment.Runs)
	}
	t.Logf("%d segments, %d timed words, %d chars", len(result.Segments), words, len(result.Text))
	for i, segment := range result.Segments {
		t.Logf("  [%02d] %6d..%6d ms conf=%.3f runs=%d %q",
			i, segment.StartMS, segment.EndMS, segment.Confidence, len(segment.Runs), segment.Text)
	}
	if words == 0 {
		t.Error("segments carried no word-level timings; attribution overlap would fall back to result ranges")
	}
}

// TestRecordAudioReportsATimingAnchor covers the ObjC change. Both tracks must
// report the wall clock of their first sample buffer, because a chunk's
// captured_at is taken before ScreenCaptureKit is even asked for shareable
// content and so cannot place the audio. Without this the two tracks' timings
// are not comparable and the whole timed attribution path degrades.
func TestRecordAudioReportsATimingAnchor(t *testing.T) {
	if os.Getenv("LUMI_NATIVE_SMOKE") != "1" {
		t.Skip("set LUMI_NATIVE_SMOKE=1 after granting Lumi permissions")
	}
	ctx := context.Background()
	before := time.Now().Add(-2 * time.Second).UnixNano()
	frames, err := RecordAudio(ctx, t.TempDir(), "anchor", 1.0)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(2 * time.Second).UnixNano()
	if len(frames) == 0 {
		t.Fatal("RecordAudio returned no frames")
	}
	for _, frame := range frames {
		if frame.StartedAtUnixNS == 0 {
			t.Errorf("%s frame reported no started_at_unix_ns", frame.Source)
			continue
		}
		if frame.StartedAtUnixNS < before || frame.StartedAtUnixNS > after {
			t.Errorf("%s frame started_at %d is outside the recording window %d..%d",
				frame.Source, frame.StartedAtUnixNS, before, after)
		}
		if frame.MeasuredDurationMS <= 0 {
			t.Errorf("%s frame reported no measured duration", frame.Source)
		}
	}
	if len(frames) == 2 {
		// Both tracks come from one SCStream, so their session starts share a
		// host timebase. The skew between them is small but real — it is exactly
		// what used to be unrecoverable.
		skew := frames[0].SessionStartPTSNS - frames[1].SessionStartPTSNS
		if skew < 0 {
			skew = -skew
		}
		if skew > int64(time.Second) {
			t.Errorf("session starts differ by %v, far more than capture start skew", time.Duration(skew))
		}
	}
}

// TestAudioSessionChunksAreContiguous is the native half of the regression test
// for the audio that used to fall between chunks. Cycling the stream per chunk
// cost about two seconds of every thirty — the stream was closed for the
// teardown, the file finalisation, and the next stream's setup — so consecutive
// chunks arrived 32s apart while each held 30s of sound, and the missing
// seconds landed mid-sentence.
//
// Two things are asserted, because either alone can be satisfied by a broken
// implementation: chunk starts sit exactly one chunk apart (the grid never
// drifts), and each file actually holds that whole interval (the grid is not
// merely relabelling a recording with holes in it).
func TestAudioSessionChunksAreContiguous(t *testing.T) {
	if os.Getenv("LUMI_NATIVE_SMOKE") != "1" {
		t.Skip("set LUMI_NATIVE_SMOKE=1 after granting Lumi permissions")
	}
	const chunkSeconds = 3.0
	session, err := StartAudioSession(context.Background(), t.TempDir(), "contiguity", chunkSeconds)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	chunks := make([]AudioChunk, 0, 3)
	deadline := time.Now().Add(30 * time.Second)
	for len(chunks) < 3 && time.Now().Before(deadline) {
		chunk, err := session.Next(500 * time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk.Frames) > 0 {
			chunks = append(chunks, chunk)
		}
		if chunk.Closed {
			break
		}
	}
	session.Stop()
	if len(chunks) < 3 {
		t.Fatalf("captured %d chunks, need 3 to measure the boundary between them", len(chunks))
	}

	const chunkMS = int64(chunkSeconds * 1000)
	for i := 1; i < len(chunks); i++ {
		gap := (chunks[i].StartedAtUnixNS - chunks[i-1].StartedAtUnixNS) / int64(time.Millisecond)
		if gap != chunkMS {
			t.Errorf("chunk %d starts %dms after chunk %d, but a chunk covers %dms; "+
				"the difference is audio no file holds", i, gap, i-1, chunkMS)
		}
	}
	// One sample buffer may land on either side of a boundary, so a file can
	// fall short of the full interval by that much and no more. The regression
	// this guards cost whole seconds, so the tolerance separates them cleanly.
	const bufferToleranceMS = 250
	for i, chunk := range chunks {
		for _, frame := range chunk.Frames {
			t.Logf("chunk %d %s: started=%s measured=%dms", i, frame.Source,
				time.Unix(0, frame.StartedAtUnixNS).UTC().Format(time.RFC3339Nano), frame.MeasuredDurationMS)
			if frame.MeasuredDurationMS < chunkMS-bufferToleranceMS {
				t.Errorf("chunk %d %s holds %dms of a %dms interval; %dms of audio was not captured",
					i, frame.Source, frame.MeasuredDurationMS, chunkMS, chunkMS-frame.MeasuredDurationMS)
			}
		}
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

func TestEncodeVocabularyJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		terms []string
		want  string
	}{
		{name: "nil is empty so no context is applied", terms: nil, want: ""},
		{name: "empty is empty", terms: []string{}, want: ""},
		{name: "plain terms", terms: []string{"Acme Corp", "Mostafa"}, want: `["Acme Corp","Mostafa"]`},
		{
			name:  "quotes and non-ASCII survive encoding",
			terms: []string{`say "hello"`, "Zoë", "日本語"},
			want:  `["say \"hello\"","Zoë","日本語"]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeVocabulary(tc.terms)
			if err != nil {
				t.Fatalf("encodeVocabulary: %v", err)
			}
			if got != tc.want {
				t.Fatalf("encodeVocabulary(%q) = %q, want %q", tc.terms, got, tc.want)
			}
		})
	}
}
