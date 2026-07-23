package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/spf13/cobra"
)

// stopTimeout bounds how long `record stop` waits for the recorder to exit
// gracefully after SIGTERM. It must exceed the recorder's 5s media-
// preservation window so in-flight media finishes indexing.
const stopTimeout = 20 * time.Second

// recordState is the on-disk record of a background recorder. It lives at
// Paths.RecordState so `status`/`stop` can find the process from any terminal.
type recordState struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Screen    bool      `json:"screen"`
	Audio     bool      `json:"audio"`
	// Args is the child argv (after the binary name), kept for diagnostics.
	Args []string `json:"args"`
	// Log is the file the recorder's output is redirected to.
	Log string `json:"log"`
}

func writeRecordState(paths config.Paths, state recordState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(paths.RecordState, data, 0o600)
}

// readRecordState loads the state file. The bool is false (with a nil error)
// when no recorder has been registered yet.
func readRecordState(paths config.Paths) (recordState, bool, error) {
	data, err := os.ReadFile(paths.RecordState)
	if errors.Is(err, os.ErrNotExist) {
		return recordState{}, false, nil
	}
	if err != nil {
		return recordState{}, false, err
	}
	var state recordState
	if err := json.Unmarshal(data, &state); err != nil {
		return recordState{}, false, fmt.Errorf("parse %s: %w", paths.RecordState, err)
	}
	return state, true, nil
}

func removeRecordState(paths config.Paths) error {
	if err := os.Remove(paths.RecordState); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// processAlive reports whether a process with the given pid is running. It uses
// signal 0, which performs error checking without delivering a signal: nil
// means alive, EPERM means alive-but-not-ours, ESRCH means gone.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (a *app) recordStartCommand() *cobra.Command {
	var f recordFlags
	var foreground bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start recording (in the background, or with --foreground in this terminal)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if foreground {
				return a.runForeground(cmd, f)
			}
			return a.startBackground(cmd, f)
		},
	}
	f.bind(cmd)
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run in this terminal instead of detaching to the background")
	return cmd
}

// startBackground preflights permissions, refuses a duplicate recorder, then
// re-execs `lumi record start --foreground <same flags>` detached into its own
// session with output redirected to the log file.
func (a *app) startBackground(cmd *cobra.Command, f recordFlags) error {
	if f.noScreen && f.noAudio {
		return errors.New("--no-screen and --no-audio cannot be used together")
	}
	paths, err := a.paths()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	if state, ok, err := readRecordState(paths); err != nil {
		return err
	} else if ok && processAlive(state.PID) {
		return fmt.Errorf("recording is already in progress (pid %d); run `lumi record stop` first", state.PID)
	}
	// Surface missing-permission errors to this terminal, not the log file.
	if err := requireRecordingPermissions(cmd.Context(), !f.noScreen, !f.noAudio, !f.noAudio); err != nil {
		return err
	}

	pid, err := a.spawnRecorder(paths, f)
	if err != nil {
		return err
	}
	state := recordState{
		PID:       pid,
		StartedAt: time.Now().UTC(),
		Screen:    !f.noScreen,
		Audio:     !f.noAudio,
		Args:      childArgs(a.dataDir, f),
		Log:       paths.RecordLog,
	}
	if err := writeRecordState(paths, state); err != nil {
		return fmt.Errorf("record state: %w", err)
	}

	// Confirm the child survived startup (permission preflight already passed,
	// but speech-asset download or a native fault can still kill it early).
	if !confirmAlive(pid, time.Second) {
		_ = removeRecordState(paths)
		return fmt.Errorf("recorder exited immediately after start; see %s", paths.RecordLog)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "recording started (pid %d)\nlog: %s\n", pid, paths.RecordLog)
	return nil
}

// childArgs builds the argv (after the binary name) for the detached worker:
// `record start --foreground` plus the forwarded capture flags and --data-dir.
func childArgs(dataDir string, f recordFlags) []string {
	args := []string{"record", "start", "--foreground"}
	if dataDir != "" {
		args = append(args, "--data-dir", dataDir)
	}
	args = append(args,
		"--interval", f.interval.String(),
		"--audio-chunk", f.audioChunk.String(),
		"--speech-locale", f.speechLocale,
	)
	if f.duration > 0 {
		args = append(args, "--duration", f.duration.String())
	}
	if f.noScreen {
		args = append(args, "--no-screen")
	}
	if f.noAudio {
		args = append(args, "--no-audio")
	}
	return args
}

func (a *app) spawnRecorder(paths config.Paths, f recordFlags) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate lumi binary: %w", err)
	}
	logFile, err := os.OpenFile(paths.RecordLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open record log: %w", err)
	}
	defer logFile.Close() // the child holds its own descriptor after Start.

	child := exec.Command(exe, childArgs(a.dataDir, f)...)
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	pid, err := startDetachedProcess(child)
	if err != nil {
		return 0, fmt.Errorf("start background recorder: %w", err)
	}
	return pid, nil
}

// startDetachedProcess starts child and releases its process handle without
// losing the PID needed for state tracking. On Unix, Process.Release mutates
// Process.Pid to -1, so the PID must be captured first.
func startDetachedProcess(child *exec.Cmd) (int, error) {
	if err := child.Start(); err != nil {
		return 0, err
	}
	pid := child.Process.Pid
	// Detach: never Wait, so the child is reaped after this process exits.
	_ = child.Process.Release()
	return pid, nil
}

// confirmAlive polls liveness over the window and returns false only if the
// process is gone by the end, tolerating a race where it hasn't yet appeared.
func confirmAlive(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		alive := processAlive(pid)
		if time.Now().After(deadline) {
			return alive
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (a *app) recordStatusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether a recording is in progress",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			state, ok, err := readRecordState(paths)
			if err != nil {
				return err
			}
			recording := ok && processAlive(state.PID)
			if asJSON {
				payload := map[string]any{"recording": recording}
				if recording {
					payload["pid"] = state.PID
					payload["started_at"] = state.StartedAt
					payload["screen"] = state.Screen
					payload["audio"] = state.Audio
					payload["log"] = state.Log
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(payload)
			}
			out := cmd.OutOrStdout()
			if !recording {
				if ok {
					fmt.Fprintf(out, "not recording (stale state for pid %d)\n", state.PID)
				} else {
					fmt.Fprintln(out, "not recording")
				}
				return nil
			}
			uptime := time.Since(state.StartedAt).Round(time.Second)
			fmt.Fprintf(out, "recording\tpid %d\n", state.PID)
			fmt.Fprintf(out, "started\t%s (%s ago)\n", state.StartedAt.Local().Format("2006-01-02 15:04:05"), uptime)
			fmt.Fprintf(out, "capturing\tscreen=%t audio=%t\n", state.Screen, state.Audio)
			fmt.Fprintf(out, "log\t%s\n", state.Log)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func (a *app) recordStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background recording and wait for it to exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			state, ok, err := readRecordState(paths)
			if err != nil {
				return err
			}
			if !ok || !processAlive(state.PID) {
				if ok {
					// Clear a stale file so `status` reads clean afterwards.
					if err := removeRecordState(paths); err != nil {
						return err
					}
				}
				fmt.Fprintln(cmd.OutOrStdout(), "not recording")
				return nil
			}
			if err := stopProcess(state.PID, stopTimeout); err != nil {
				return err
			}
			if err := removeRecordState(paths); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "recording stopped (pid %d)\n", state.PID)
			return nil
		},
	}
}

// stopProcess sends SIGTERM and waits up to timeout for the process to exit,
// reusing the recorder's graceful-shutdown path so in-flight media is flushed.
func stopProcess(pid int, timeout time.Duration) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find recorder process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal recorder %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		return fmt.Errorf("recorder %d did not stop within %s", pid, timeout)
	}
	return nil
}
