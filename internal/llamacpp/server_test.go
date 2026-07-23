package llamacpp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
