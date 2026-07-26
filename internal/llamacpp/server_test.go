package llamacpp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestInstalledOverride(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()

	lookPath = func(string) (string, error) { return "/usr/local/bin/llama-server", nil }
	if path, ok := Installed(); !ok || path == "" {
		t.Fatalf("expected installed, got %q %v", path, ok)
	}

	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, ok := Installed(); ok {
		t.Fatal("expected not installed")
	}
}

func TestHostPort(t *testing.T) {
	cases := []struct {
		in, host, port string
		wantErr        bool
	}{
		{in: "http://127.0.0.1:8080", host: "127.0.0.1", port: "8080"},
		{in: "http://localhost:9090", host: "localhost", port: "9090"},
		{in: "http://example.com", host: "example.com", port: "8080"},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		host, port, err := hostPort(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("hostPort(%q) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("hostPort(%q): %v", c.in, err)
		}
		if host != c.host || port != c.port {
			t.Fatalf("hostPort(%q) = %q,%q want %q,%q", c.in, host, port, c.host, c.port)
		}
	}
}

func TestModelArgs(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "weights.bin")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		model string
		want  []string
	}{
		{"ggml-org/gpt-oss-20b-GGUF", []string{"-hf", "ggml-org/gpt-oss-20b-GGUF"}},
		{"/models/qwen.gguf", []string{"-m", "/models/qwen.gguf"}},
		{"model.GGUF", []string{"-m", "model.GGUF"}},
		{"./local/model.gguf", []string{"-m", "./local/model.gguf"}},
		{existing, []string{"-m", existing}},
	}
	for _, c := range cases {
		if got := modelArgs(c.model); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("modelArgs(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestHealthyAndEnsureRunningWhenUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if !Healthy(context.Background(), srv.URL) {
		t.Fatal("expected healthy")
	}
	// EnsureRunning must return nil without launching anything when a server is
	// already responding.
	if err := EnsureRunning(context.Background(), Options{BaseURL: srv.URL, Model: "unused"}, nil); err != nil {
		t.Fatalf("EnsureRunning against a live server: %v", err)
	}
}

// healthyServer is a stand-in llama-server that only answers /health.
func healthyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// asServerProcess makes every live pid read as a llama-server, so tests can use
// ordinary stand-in processes where ownership is not what is under test.
func asServerProcess(t *testing.T) {
	t.Helper()
	orig := processCommand
	t.Cleanup(func() { processCommand = orig })
	processCommand = func(int) (string, bool) { return "/opt/homebrew/bin/" + binaryName, true }
}

// livePid returns the pid of a process that outlives the test, so state can
// record a "running" server without ever naming the test runner itself.
func livePid(t *testing.T) int {
	t.Helper()
	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatalf("start placeholder process: %v", err)
	}
	t.Cleanup(func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	})
	return proc.Process.Pid
}

func TestEnsureRunningWarmServerWithMatchingModel(t *testing.T) {
	asServerProcess(t)
	srv := healthyServer(t)
	statePath := filepath.Join(t.TempDir(), "llama-server.json")
	if err := WriteState(statePath, State{PID: livePid(t), Model: "repo/model", BaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	// lookPath must never be consulted: a matching warm server is used as is.
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) {
		t.Fatal("matching warm server must not be relaunched")
		return "", nil
	}

	var notes bytes.Buffer
	if err := EnsureRunning(context.Background(), Options{
		BaseURL: srv.URL, Model: "repo/model", StatePath: statePath,
	}, &notes); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if notes.Len() != 0 {
		t.Fatalf("warm path should be silent, wrote: %q", notes.String())
	}
}

// A server adopted from the pre-state-file pid file keeps running — reloading
// weights is expensive and it is probably already correct — but the uncertainty
// is reported.
func TestEnsureRunningNotesAdoptedServer(t *testing.T) {
	asServerProcess(t)
	srv := healthyServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "llama-server.pid"),
		[]byte(strconv.Itoa(livePid(t))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) {
		t.Fatal("an adopted server must not be restarted")
		return "", nil
	}

	var notes bytes.Buffer
	if err := EnsureRunning(context.Background(), Options{
		BaseURL: srv.URL, Model: "repo/model", StatePath: filepath.Join(dir, "llama-server.json"),
	}, &notes); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if !strings.Contains(notes.String(), "predates model tracking") {
		t.Fatalf("expected a note about the unverified model, got: %q", notes.String())
	}
}

func TestEnsureRunningLeavesForeignServerAlone(t *testing.T) {
	srv := healthyServer(t)
	statePath := filepath.Join(t.TempDir(), "llama-server.json")
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) {
		t.Fatal("a server Lumi did not launch must never be replaced")
		return "", nil
	}

	var notes bytes.Buffer
	if err := EnsureRunning(context.Background(), Options{
		BaseURL: srv.URL, Model: "repo/model", StatePath: statePath,
	}, &notes); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	// Silence would hide that the answer may come from a different model.
	if !strings.Contains(notes.String(), "not started by Lumi") {
		t.Fatalf("expected a note about the foreign server, got: %q", notes.String())
	}
}

// TestEnsureRunningRestartsOnModelChange exercises the full switch: a
// Lumi-launched server running the old model is signalled, waited out, and the
// launch path is reached for the new one. The stand-in server reports healthy
// only while the recorded process holds its marker file, so /health goes down
// exactly when the recorded process does — the way a real llama-server would.
func TestEnsureRunningRestartsOnModelChange(t *testing.T) {
	asServerProcess(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "alive")
	proc := exec.Command("sh", "-c",
		`trap 'rm -f "$1"; exit 0' TERM; touch "$1"; while :; do sleep 0.05; done`, "sh", marker)
	if err := proc.Start(); err != nil {
		t.Fatalf("start stand-in server process: %v", err)
	}
	t.Cleanup(func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	})
	waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(marker); r.URL.Path == "/health" && err == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	statePath := filepath.Join(dir, "llama-server.json")
	if err := WriteState(statePath, State{PID: proc.Process.Pid, Model: "old/model", BaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	// Stop short of running a real binary: reaching the launch path is the
	// assertion, and starting a nonexistent one fails immediately.
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) { return filepath.Join(dir, "not-a-real-binary"), nil }

	var notes bytes.Buffer
	err := EnsureRunning(context.Background(), Options{
		BaseURL: srv.URL, Model: "new/model", StatePath: statePath,
	}, &notes)
	if err == nil || !strings.Contains(err.Error(), "launch") {
		t.Fatalf("expected the relaunch attempt to surface, got err = %v; notes = %q", err, notes.String())
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("the old server was not stopped, marker stat err = %v", statErr)
	}
	if !strings.Contains(notes.String(), "new/model") || !strings.Contains(notes.String(), "old/model") {
		t.Fatalf("expected a note naming both models, got: %q", notes.String())
	}
}

// TestEnsureRunningKeepsServerWhenBinaryMissing guards the ordering: a running
// server must not be killed for a model change Lumi cannot then serve.
func TestEnsureRunningKeepsServerWhenBinaryMissing(t *testing.T) {
	asServerProcess(t)
	srv := healthyServer(t)
	statePath := filepath.Join(t.TempDir(), "llama-server.json")
	pid := livePid(t)
	if err := WriteState(statePath, State{PID: pid, Model: "old/model", BaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) { return "", errors.New("not found") }

	err := EnsureRunning(context.Background(), Options{
		BaseURL: srv.URL, Model: "new/model", StatePath: statePath,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("expected the missing-binary error, got %v", err)
	}
	if !processAlive(pid) {
		t.Fatal("the running server was killed even though it could not be relaunched")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHealthyFalseOn503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if Healthy(context.Background(), srv.URL) {
		t.Fatal("503 must not be healthy")
	}
}

func TestEnsureRunningNotInstalled(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) { return "", errors.New("not found") }

	// Use a port that nothing listens on so the health check fails fast.
	err := EnsureRunning(context.Background(), Options{BaseURL: "http://127.0.0.1:1", Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected error when llama-server is not installed")
	}
}

// TestEnsureRunningIgnoresStateForAnotherEndpoint covers a base-URL change onto
// a second, already-healthy server. The recorded server belongs to the old URL:
// terminating it would not free this one, and EnsureRunning would then wait out
// stopTimeout on a server that never stops.
func TestEnsureRunningIgnoresStateForAnotherEndpoint(t *testing.T) {
	asServerProcess(t)
	recorded := healthyServer(t)
	other := healthyServer(t)
	pid := livePid(t)
	statePath := filepath.Join(t.TempDir(), "llama-server.json")
	if err := WriteState(statePath, State{PID: pid, Model: "old/model", BaseURL: recorded.URL}); err != nil {
		t.Fatal(err)
	}
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) {
		t.Fatal("a server recorded at another URL must not be replaced")
		return "", nil
	}

	var notes bytes.Buffer
	if err := EnsureRunning(context.Background(), Options{
		BaseURL: other.URL, Model: "new/model", StatePath: statePath,
	}, &notes); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if !processAlive(pid) {
		t.Fatal("the server recorded at another URL was killed")
	}
	if !strings.Contains(notes.String(), "not started by Lumi") {
		t.Fatalf("expected a note about the unrelated server, got: %q", notes.String())
	}
}

// TestEnsureRunningIgnoresReusedPid covers the recorded server exiting and its
// pid being handed to something else: the automatic restart must not signal a
// process that is no longer llama-server.
func TestEnsureRunningIgnoresReusedPid(t *testing.T) {
	orig := processCommand
	defer func() { processCommand = orig }()
	processCommand = func(int) (string, bool) { return "/bin/sleep", true }

	srv := healthyServer(t)
	pid := livePid(t)
	statePath := filepath.Join(t.TempDir(), "llama-server.json")
	if err := WriteState(statePath, State{PID: pid, Model: "old/model", BaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	origLook := lookPath
	defer func() { lookPath = origLook }()
	lookPath = func(string) (string, error) {
		t.Fatal("a reused pid must not drive a restart")
		return "", nil
	}

	var notes bytes.Buffer
	if err := EnsureRunning(context.Background(), Options{
		BaseURL: srv.URL, Model: "new/model", StatePath: statePath,
	}, &notes); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if !processAlive(pid) {
		t.Fatal("an unrelated process holding the recorded pid was signalled")
	}
	if !strings.Contains(notes.String(), "not started by Lumi") {
		t.Fatalf("expected a note that the server is unverified, got: %q", notes.String())
	}
}

func TestIsServerProcess(t *testing.T) {
	pid := livePid(t)
	orig := processCommand
	defer func() { processCommand = orig }()

	processCommand = func(int) (string, bool) { return "/opt/homebrew/bin/" + binaryName, true }
	if !IsServerProcess(pid) {
		t.Fatal("a live llama-server must be recognised")
	}
	processCommand = func(int) (string, bool) { return "/bin/sleep", true }
	if IsServerProcess(pid) {
		t.Fatal("an unrelated process must not pass as llama-server")
	}
	// An identity that cannot be read is not ownership.
	processCommand = func(int) (string, bool) { return "", false }
	if IsServerProcess(pid) {
		t.Fatal("an unreadable identity must not pass as llama-server")
	}
	processCommand = func(int) (string, bool) { return binaryName, true }
	if IsServerProcess(0) || IsServerProcess(-1) {
		t.Fatal("a nonsense pid must never be treated as a server")
	}
}

func TestSameEndpoint(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/", true},
		{"http://LOCALHOST:8080", "http://localhost:8080", true},
		{"http://example.com", "http://example.com:8080", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:9090", false},
		{"http://127.0.0.1:8080", "http://localhost:8080", false},
		// Nothing matches an unrecorded endpoint.
		{"", "http://127.0.0.1:8080", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := sameEndpoint(c.a, c.b); got != c.want {
			t.Fatalf("sameEndpoint(%q, %q) = %t, want %t", c.a, c.b, got, c.want)
		}
	}
}
