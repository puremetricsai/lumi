package llamacpp

import (
	"context"
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
	BaseURL string
	Model   string
	LogPath string // llama-server stdout/stderr is appended here
	PidPath string // pid of a Lumi-launched server is written here
}

// EnsureRunning makes a healthy llama-server available at opts.BaseURL. If one
// is already responding (whether Lumi or the user started it) it returns
// immediately. Otherwise it launches llama-server detached — so it outlives this
// process and keeps the model warm — and waits for /health to report ready.
func EnsureRunning(ctx context.Context, opts Options, notes io.Writer) error {
	if Healthy(ctx, opts.BaseURL) {
		return nil
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
	if opts.PidPath != "" && cmd.Process != nil {
		_ = os.WriteFile(opts.PidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	}
	// Release so it keeps running after Lumi exits.
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return waitReady(ctx, opts.BaseURL, opts.LogPath)
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
