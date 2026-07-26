package llamacpp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// binaryName is the llama.cpp server executable.
const binaryName = "llama-server"

// lookPath is indirected so tests can simulate llama-server being present or
// absent without touching PATH.
var lookPath = exec.LookPath

// readyTimeout bounds how long EnsureRunning waits for a freshly launched
// server to report healthy. It is generous because a `-hf` model may download
// on first run.
const readyTimeout = 5 * time.Minute

// Installed reports the llama-server path and whether it is on PATH.
func Installed() (string, bool) {
	path, err := lookPath(binaryName)
	if err != nil {
		return "", false
	}
	return path, true
}

// Healthy reports whether a llama-server at baseURL answers /health with 200.
func Healthy(ctx context.Context, baseURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode == http.StatusOK
}

// hostPort splits a base URL into host and port, defaulting the port to 8080
// when the URL omits it.
func hostPort(baseURL string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", "", fmt.Errorf("parse llama base URL %q: %w", baseURL, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("llama base URL %q has no host", baseURL)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		// No explicit port in the URL.
		host = u.Host
		port = "8080"
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port, nil
}

// sameEndpoint reports whether two base URLs name the same llama-server, so a
// trailing slash or an implicit port does not read as an endpoint change. A base
// URL that is empty or unparseable names nothing and matches nothing: state
// written before endpoints were recorded must not be mistaken for this server.
func sameEndpoint(a, b string) bool {
	hostA, portA, err := hostPort(a)
	if err != nil {
		return false
	}
	hostB, portB, err := hostPort(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(hostA, hostB) && portA == portB
}

// modelArgs turns a configured model into llama-server arguments. A GGUF file
// path (by suffix, an existing file, or a leading path separator) is loaded with
// -m; anything else is treated as a HuggingFace repo id loaded with -hf.
func modelArgs(model string) []string {
	model = strings.TrimSpace(model)
	if looksLikePath(model) {
		return []string{"-m", expandHome(model)}
	}
	return []string{"-hf", model}
}

func looksLikePath(model string) bool {
	if strings.HasSuffix(strings.ToLower(model), ".gguf") {
		return true
	}
	if strings.HasPrefix(model, "/") || strings.HasPrefix(model, "~") ||
		strings.HasPrefix(model, "./") || strings.HasPrefix(model, "../") || model == "." {
		return true
	}
	if _, err := os.Stat(expandHome(model)); err == nil {
		return true
	}
	return false
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// Options configures EnsureRunning.
type Options struct {
	BaseURL   string
	Model     string
	LogPath   string // llama-server stdout/stderr is appended here
	StatePath string // pid and model of a Lumi-launched server are written here
}

// stopTimeout bounds how long EnsureRunning waits for a server it is replacing
// to stop answering before giving up on the port.
const stopTimeout = 20 * time.Second

// EnsureRunning makes a healthy llama-server available at opts.BaseURL serving
// opts.Model. A server Lumi launched with a different model is replaced —
// otherwise a configuration change would silently keep answering from the old
// weights. A server Lumi did not launch is never killed, only noted. Otherwise
// llama-server is launched detached, so it outlives this process and keeps the
// model warm, and EnsureRunning waits for /health to report ready.
func EnsureRunning(ctx context.Context, opts Options, notes io.Writer) error {
	if Healthy(ctx, opts.BaseURL) {
		st, recorded := ReadState(opts.StatePath)
		recorded = recorded && IsServerProcess(st.PID)
		// State describing a different endpoint says nothing about the server
		// answering here: that one is, as far as Lumi knows, foreign.
		if recorded && strings.TrimSpace(st.BaseURL) != "" && !sameEndpoint(st.BaseURL, opts.BaseURL) {
			st, recorded = State{}, false
		}
		if !restartNeeded(st, recorded, opts.Model, opts.BaseURL) {
			noteUnverifiedModel(notes, st, recorded, opts)
			return nil
		}
		if err := replaceRunning(ctx, st, opts, notes); err != nil {
			return err
		}
	}
	bin, ok := Installed()
	if !ok {
		return fmt.Errorf("%s not found on PATH; install llama.cpp (brew install llama.cpp) — https://github.com/ggml-org/llama.cpp", binaryName)
	}
	if strings.TrimSpace(opts.Model) == "" {
		return fmt.Errorf("no llama.cpp model configured; run `lumi configure` to set one (a GGUF path or a HuggingFace repo)")
	}
	host, port, err := hostPort(opts.BaseURL)
	if err != nil {
		return err
	}

	args := append([]string{"--host", host, "--port", port}, modelArgs(opts.Model)...)
	cmd := exec.Command(bin, args...)
	// Detach into its own process group so it survives this process exiting.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if opts.LogPath != "" {
		if logFile, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			defer logFile.Close()
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}
	}
	if notes != nil {
		fmt.Fprintf(notes, "note: starting llama-server at %s (loading model; first run with a HuggingFace repo may download)\n", opts.BaseURL)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch %s: %w", binaryName, err)
	}
	if opts.StatePath != "" && cmd.Process != nil {
		_ = WriteState(opts.StatePath, State{PID: cmd.Process.Pid, Model: opts.Model, BaseURL: opts.BaseURL})
	}
	// Release so it keeps running after Lumi exits.
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return waitReady(ctx, opts.BaseURL, opts.LogPath)
}

// noteUnverifiedModel reports a healthy server whose model Lumi cannot vouch
// for: one it did not launch, or one adopted from a pid file written before
// models were recorded. Both are kept running — but never silently, since the
// answer may come from weights other than the configured ones.
func noteUnverifiedModel(notes io.Writer, st State, recorded bool, opts Options) {
	if notes == nil || strings.TrimSpace(opts.Model) == "" {
		return
	}
	switch {
	case !recorded:
		fmt.Fprintf(notes, "note: the llama-server at %s was not started by Lumi; using it as is, so its model may not be %s\n", opts.BaseURL, opts.Model)
	case strings.TrimSpace(st.Model) == "":
		fmt.Fprintf(notes, "note: the llama-server at %s (pid %d) predates model tracking, so Lumi cannot confirm it is serving %s; run `lumi llama stop` to reload it\n", opts.BaseURL, st.PID, opts.Model)
	case !sameEndpoint(st.BaseURL, opts.BaseURL):
		fmt.Fprintf(notes, "note: the recorded llama-server (pid %d) predates URL tracking, so Lumi cannot confirm the server at %s is serving %s; run `lumi llama stop` to reload it\n", st.PID, opts.BaseURL, opts.Model)
	}
}

// replaceRunning stops a Lumi-launched server so a differently-configured one
// can take its place. The install check comes first: a running server is never
// killed for a model Lumi has no binary to serve.
func replaceRunning(ctx context.Context, st State, opts Options, notes io.Writer) error {
	if _, ok := Installed(); !ok {
		return fmt.Errorf("%s not found on PATH; install llama.cpp (brew install llama.cpp) — https://github.com/ggml-org/llama.cpp", binaryName)
	}
	if notes != nil {
		fmt.Fprintf(notes, "note: restarting llama-server for model %s (it is running %s)\n", opts.Model, orUnknownModel(st.Model))
	}
	proc, err := os.FindProcess(st.PID)
	if err != nil {
		return fmt.Errorf("find llama-server (pid %d): %w", st.PID, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop llama-server (pid %d): %w", st.PID, err)
	}
	return waitStopped(ctx, opts.BaseURL, st.PID)
}

// waitStopped blocks until the replaced server stops answering, so the fresh
// one can bind the port.
func waitStopped(ctx context.Context, baseURL string, pid int) error {
	deadline := time.Now().Add(stopTimeout)
	for {
		if !Healthy(ctx, baseURL) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("llama-server (pid %d) still answering at %s after %s; stop it with `lumi llama stop`", pid, baseURL, stopTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// processAlive reports whether pid names a live process. Signal 0 performs the
// permission and existence checks without delivering anything.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// processCommand reports the executable behind a live pid. Indirected so tests
// can stand in for a llama-server without running one.
var processCommand = func(pid int) (string, bool) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", false
	}
	return name, true
}

// IsServerProcess reports whether pid is a live llama-server. Liveness alone is
// not ownership: pids are reused, and a recorded server that has since exited
// would otherwise hand its pid — and the SIGTERM aimed at it — to whatever
// unrelated process inherited the number. An identity that cannot be read is
// treated as not ours, so an unverifiable pid is never signalled.
func IsServerProcess(pid int) bool {
	if pid <= 0 || !processAlive(pid) {
		return false
	}
	name, ok := processCommand(pid)
	if !ok {
		return false
	}
	return filepath.Base(name) == binaryName
}

func orUnknownModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return "an unrecorded model"
	}
	return model
}

func waitReady(ctx context.Context, baseURL, logPath string) error {
	deadline := time.Now().Add(readyTimeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if Healthy(ctx, baseURL) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			if now.After(deadline) {
				hint := ""
				if logPath != "" {
					hint = fmt.Sprintf(" (see %s)", logPath)
				}
				return fmt.Errorf("llama-server did not become ready within %s%s", readyTimeout, hint)
			}
		}
	}
}
