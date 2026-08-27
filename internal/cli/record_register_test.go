package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A pid is the wrong thing to hand someone whose menu bar is holding the
// recorder: `lumi record stop` is not how they stop it.
func TestAlreadyRecordingErrorNamesTheOwningApp(t *testing.T) {
	appOwned := alreadyRecordingError(recordState{
		PID:        4242,
		Executable: "/Applications/Lumi.app/Contents/MacOS/lumi",
	}).Error()
	for _, want := range []string{"Lumi.app", "4242", "quit the app"} {
		if !strings.Contains(appOwned, want) {
			t.Errorf("app-owned refusal = %q, want it to mention %q", appOwned, want)
		}
	}

	// Everything else keeps the message it has always had. A terminal user must
	// not be told to quit an app that is not involved.
	terminal := alreadyRecordingError(recordState{
		PID:        99,
		Executable: "/usr/local/bin/lumi",
	}).Error()
	want := "recording is already in progress (pid 99); run `lumi record stop` first"
	if terminal != want {
		t.Errorf("terminal refusal = %q, want %q", terminal, want)
	}

	// A state written by a build that predates the Executable field carries no
	// path at all, and must still produce the original message.
	legacy := alreadyRecordingError(recordState{PID: 7}).Error()
	if !strings.Contains(legacy, "run `lumi record stop` first") || strings.Contains(legacy, ".app") {
		t.Errorf("legacy refusal = %q, want the original wording", legacy)
	}
}

// The state file is the recorder's registration, so cleanup must never remove
// somebody else's. `record stop` already removes the state of the process it
// stopped, and a new recorder can register before the stopped one's deferred
// cleanup runs.
func TestRemoveOwnRecordStateLeavesAnotherRecordersRegistration(t *testing.T) {
	paths := testPaths(t)
	stranger := recordState{PID: os.Getpid() + 1, StartedAt: time.Now().UTC(), Screen: true}
	if err := writeRecordState(paths, stranger); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnRecordState(paths); err != nil {
		t.Fatalf("removeOwnRecordState: %v", err)
	}
	got, ok, err := readRecordState(paths)
	if err != nil || !ok {
		t.Fatalf("readRecordState = (ok=%v, err=%v), want the stranger's state intact", ok, err)
	}
	if got.PID != stranger.PID {
		t.Errorf("state pid = %d, want the stranger's %d", got.PID, stranger.PID)
	}

	// Its own registration is removed.
	if err := writeRecordState(paths, recordState{PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnRecordState(paths); err != nil {
		t.Fatalf("removeOwnRecordState on own state: %v", err)
	}
	if _, ok, _ := readRecordState(paths); ok {
		t.Error("own registration survived removeOwnRecordState")
	}

	// A missing file is not an error; the recorder may never have registered.
	if err := removeOwnRecordState(paths); err != nil {
		t.Errorf("removeOwnRecordState on a missing file: %v", err)
	}
}

// Registering from the detached child as well as from the parent would give one
// recorder two writers of one file.
func TestRegisterStateRequiresForeground(t *testing.T) {
	cmd := (&app{}).recordStartCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--register-state"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--register-state without --foreground was accepted; want an error")
	}
	if !strings.Contains(err.Error(), "--foreground") {
		t.Errorf("error = %q, want it to name --foreground", err)
	}
}

// The flag has to default off, or every existing --foreground caller silently
// starts registering and refusing.
func TestRegisterStateDefaultsOff(t *testing.T) {
	cmd := (&app{}).recordStartCommand()
	flag := cmd.Flags().Lookup("register-state")
	if flag == nil {
		t.Fatal("record start has no --register-state flag")
	}
	if flag.DefValue != "false" {
		t.Errorf("--register-state default = %q, want false", flag.DefValue)
	}
}

// A registered foreground recorder must be refused for the same reason a
// detached one is: two recorders on one store both write it.
func TestRunForegroundRefusesWhenAnotherRecorderIsLive(t *testing.T) {
	paths := testPaths(t)
	// This process is unambiguously alive, so processAlive says yes without a
	// spawned helper.
	if err := writeRecordState(paths, recordState{
		PID:        os.Getpid(),
		Executable: "/Applications/Lumi.app/Contents/MacOS/lumi",
	}); err != nil {
		t.Fatal(err)
	}

	a := &app{dataDir: paths.Root}
	cmd := a.recordStartCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := a.runForeground(cmd, recordFlags{speechLocale: "en-US"}, true)
	if err == nil {
		t.Fatal("runForeground started over a live recorder; want a refusal")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("error = %q, want a duplicate-recorder refusal", err)
	}
	if !strings.Contains(err.Error(), "Lumi.app") {
		t.Errorf("error = %q, want it to name the owning app", err)
	}

	// The refusal comes before anything is opened or downloaded, so it must not
	// have created a database on the way out.
	if _, err := os.Stat(paths.Database); !os.IsNotExist(err) {
		t.Errorf("a refused start created %s", paths.Database)
	}
}

// Without the flag, a foreground recorder registers nothing and refuses nobody
// — exactly what it did before this flag existed.
//
// The capture flags matter here. `--no-screen --no-audio` is rejected on the
// first line of runForeground, so a test using both never reaches the duplicate
// check at all and would keep passing if a refusal were added to the
// unregistered path later. This one captures screen, and is stopped instead by
// a data directory that cannot be created — a failure that happens *after* the
// point a refusal would have been raised.
func TestRunForegroundWithoutRegisterStateIgnoresALiveRecorder(t *testing.T) {
	paths := testPaths(t)
	if err := writeRecordState(paths, recordState{PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}

	// A data directory whose parent is a regular file: paths.Ensure fails, so
	// nothing is ever captured or written.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{dataDir: filepath.Join(blocker, "lumi")}
	cmd := a.recordStartCommand()
	cmd.SetContext(context.Background())

	err := a.runForeground(cmd, recordFlags{noAudio: true, speechLocale: "en-US"}, false)
	if err == nil {
		t.Fatal("runForeground succeeded against an uncreatable data directory")
	}
	if strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("unregistered --foreground was refused: %v", err)
	}
	// It must have failed *past* the flag validation, or this proves nothing.
	if strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("test never reached the duplicate check: %v", err)
	}
}

// The registration itself — the write and the retirement — was previously
// reachable only by actually recording, so nothing pinned it.
func TestRegisterForegroundRecorderWritesAndRetiresItsState(t *testing.T) {
	paths := testPaths(t)
	release, err := registerForegroundRecorder(paths, paths.Root, recordFlags{
		audioChunk: 30 * time.Second, interval: 2 * time.Second, speechLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("registerForegroundRecorder: %v", err)
	}

	state, ok, err := readRecordState(paths)
	if err != nil || !ok {
		t.Fatalf("readRecordState = (ok=%v, err=%v), want a registration", ok, err)
	}
	if state.PID != os.Getpid() {
		t.Errorf("registered pid = %d, want this process %d", state.PID, os.Getpid())
	}
	if state.Screen != true || state.Audio != true {
		t.Errorf("registered capture = screen:%t audio:%t, want both true", state.Screen, state.Audio)
	}
	// A foreground recorder opens no log of its own; claiming one would send
	// `record status` readers to a file nobody writes.
	if state.Log != "" {
		t.Errorf("registered log = %q, want empty for a foreground recorder", state.Log)
	}
	if !slices.Contains(state.Args, "--register-state") {
		t.Errorf("registered args = %v, want them to record --register-state", state.Args)
	}

	// A second registration is refused while the first is live.
	if _, err := registerForegroundRecorder(paths, paths.Root, recordFlags{}); err == nil {
		t.Error("a second registration was accepted while the first was live")
	} else if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("second registration error = %q, want a duplicate refusal", err)
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, ok, _ := readRecordState(paths); ok {
		t.Error("registration survived its release")
	}
}

// `record stop` removes the registration after the process it signalled is
// gone. A recorder that retires its own registration frees the path the moment
// it exits, so an unconditional removal there deletes whatever a recorder
// started in that gap has since written.
func TestRecordStopOnlyRemovesTheRegistrationItStopped(t *testing.T) {
	paths := testPaths(t)
	newcomer := recordState{PID: os.Getpid(), StartedAt: time.Now().UTC(), Screen: true}
	if err := writeRecordState(paths, newcomer); err != nil {
		t.Fatal(err)
	}
	// The recorder that was stopped is a different, now-dead pid.
	if err := removeRecordStateFor(paths, os.Getpid()+1); err != nil {
		t.Fatalf("removeRecordStateFor: %v", err)
	}
	got, ok, err := readRecordState(paths)
	if err != nil || !ok {
		t.Fatalf("readRecordState = (ok=%v, err=%v), want the newcomer intact", ok, err)
	}
	if got.PID != newcomer.PID {
		t.Errorf("state pid = %d, want the newcomer's %d", got.PID, newcomer.PID)
	}
}

// `record status` must keep printing exactly what it printed before, for the
// states that existed before. The log line is only suppressed for a foreground
// registration, which writes no log of its own.
func TestRecordStatusStillPrintsTheLogLineForADetachedRecorder(t *testing.T) {
	paths := testPaths(t)
	startedAt := time.Now().UTC()
	if err := writeRecordState(paths, recordState{
		PID:       os.Getpid(),
		StartedAt: startedAt,
		Screen:    true,
		Audio:     true,
		Log:       paths.RecordLog,
	}); err != nil {
		t.Fatal(err)
	}
	a := &app{dataDir: paths.Root}
	cmd := a.recordStatusCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("record status: %v", err)
	}
	// Compared in full, not by substring: an added or reordered line is a
	// change to output that scripts read.
	want := "recording\tpid " + strconv.Itoa(os.Getpid()) + "\n" +
		"started\t" + startedAt.Local().Format("2006-01-02 15:04:05") + " (0s ago)\n" +
		"capturing\tscreen=true audio=true\n" +
		"log\t" + paths.RecordLog + "\n"
	if out.String() != want {
		t.Errorf("status =\n%q\nwant\n%q", out.String(), want)
	}

	// --json must still carry the log key for a detached recorder.
	cmdJSON := a.recordStatusCommand()
	var jsonOut bytes.Buffer
	cmdJSON.SetOut(&jsonOut)
	cmdJSON.SetErr(&jsonOut)
	cmdJSON.SetArgs([]string{"--json"})
	if err := cmdJSON.Execute(); err != nil {
		t.Fatalf("record status --json: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(jsonOut.Bytes(), &payload); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	if payload["log"] != paths.RecordLog {
		t.Errorf("json log = %v, want %q", payload["log"], paths.RecordLog)
	}

	// A foreground registration carries no log, and status must not print an
	// empty one.
	if err := writeRecordState(paths, recordState{
		PID: os.Getpid(), StartedAt: time.Now().UTC(), Screen: true,
		Executable: filepath.Join("/Applications", appBundleName, "Contents/MacOS/lumi"),
	}); err != nil {
		t.Fatal(err)
	}
	cmd = a.recordStatusCommand()
	out.Reset()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("record status: %v", err)
	}
	if strings.Contains(out.String(), "log\t") {
		t.Errorf("status = %q, want no log line for a foreground registration", out.String())
	}
	if !strings.Contains(out.String(), "recording\tpid") {
		t.Errorf("status = %q, want it to still report the recording", out.String())
	}

	cmdJSON2 := a.recordStatusCommand()
	jsonOut.Reset()
	cmdJSON2.SetOut(&jsonOut)
	cmdJSON2.SetErr(&jsonOut)
	cmdJSON2.SetArgs([]string{"--json"})
	if err := cmdJSON2.Execute(); err != nil {
		t.Fatalf("record status --json: %v", err)
	}
	payload = nil
	if err := json.Unmarshal(jsonOut.Bytes(), &payload); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	if _, present := payload["log"]; present {
		t.Errorf("json carries a log key for a foreground registration: %v", payload["log"])
	}
	if payload["recording"] != true {
		t.Errorf("json recording = %v, want true", payload["recording"])
	}
}
