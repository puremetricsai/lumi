package llamacpp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llama-server.json")
	want := State{PID: 4242, Model: "unsloth/gemma-4-26B-A4B-it-GGUF:MXFP4_MOE", BaseURL: "http://127.0.0.1:8080"}
	if err := WriteState(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadState(path)
	if !ok || got != want {
		t.Fatalf("ReadState = (%+v, %t), want (%+v, true)", got, ok, want)
	}
	// The state file sits beside a config that may hold an API key; keep the
	// data directory's 0600 convention.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %o, want 600", perm)
	}
}

func TestReadStateMissingOrCorrupt(t *testing.T) {
	dir := t.TempDir()
	if st, ok := ReadState(filepath.Join(dir, "absent.json")); ok {
		t.Fatalf("missing state read as %+v", st)
	}
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, ok := ReadState(corrupt); ok {
		t.Fatalf("corrupt state read as %+v", st)
	}
	// A record with no usable pid is not a record.
	noPid := filepath.Join(dir, "nopid.json")
	if err := os.WriteFile(noPid, []byte(`{"model":"m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, ok := ReadState(noPid); ok {
		t.Fatalf("pid-less state read as %+v", st)
	}
}

// A server launched by the pid-file-only build must still be manageable: its pid
// is adopted, with no model recorded.
func TestReadStateAdoptsLegacyPidFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, legacyPidFileName), []byte("321\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadState(filepath.Join(dir, "llama-server.json"))
	if !ok || got.PID != 321 || got.Model != "" {
		t.Fatalf("ReadState = (%+v, %t), want pid 321 with no model", got, ok)
	}
}

func TestReadLegacyPid(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, contents string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cases := []struct {
		name    string
		path    string
		wantPid int
		wantOK  bool
	}{
		{"valid", writeFile("valid.pid", "12345\n"), 12345, true},
		{"no-trailing-newline", writeFile("bare.pid", "678"), 678, true},
		{"surrounding-whitespace", writeFile("spaced.pid", "  42  \n"), 42, true},
		{"missing", filepath.Join(dir, "does-not-exist.pid"), 0, false},
		{"empty", writeFile("empty.pid", ""), 0, false},
		{"whitespace-only", writeFile("ws.pid", "   \n\t"), 0, false},
		{"non-numeric", writeFile("abc.pid", "abc"), 0, false},
		{"zero", writeFile("zero.pid", "0"), 0, false},
		{"negative", writeFile("neg.pid", "-1"), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ok := readLegacyPid(tc.path)
			if st.PID != tc.wantPid || ok != tc.wantOK {
				t.Fatalf("readLegacyPid(%q) = (%d, %t), want (%d, %t)", tc.path, st.PID, ok, tc.wantPid, tc.wantOK)
			}
		})
	}
}

func TestRemoveStateClearsBothFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llama-server.json")
	legacy := filepath.Join(dir, legacyPidFileName)
	if err := WriteState(path, State{PID: 1, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveState(path); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, legacy} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived RemoveState, stat err = %v", p, err)
		}
	}
	// Removing an already-clean state is not an error.
	if err := RemoveState(path); err != nil {
		t.Fatalf("second RemoveState: %v", err)
	}
}

func TestRestartNeeded(t *testing.T) {
	const model = "ggml-org/gpt-oss-20b-GGUF"
	const url = "http://127.0.0.1:8080"
	cases := []struct {
		name     string
		st       State
		recorded bool
		model    string
		baseURL  string
		want     bool
	}{
		{"same model", State{PID: 1, Model: model, BaseURL: url}, true, model, url, false},
		{"different model", State{PID: 1, Model: "other/repo", BaseURL: url}, true, model, url, true},
		// Unknown is not a known mismatch: reloading weights costs minutes, so
		// an adopted server is noted rather than restarted.
		{"unknown model", State{PID: 1, BaseURL: url}, true, model, url, false},
		// Not ours to restart: a server Lumi did not launch is left alone.
		{"no record", State{}, false, model, url, false},
		// Nothing to switch to; use whatever is loaded.
		{"no configured model", State{PID: 1, Model: "other/repo", BaseURL: url}, true, "", url, false},
		{"whitespace-only model", State{PID: 1, Model: "other/repo", BaseURL: url}, true, "   ", url, false},
		// The same GGUF written two ways is the same model.
		{"path spelled differently", State{PID: 1, Model: "  /models/q.gguf", BaseURL: url}, true, "/models/q.gguf ", url, false},
		// The recorded server is a different one: killing it would leave the
		// server actually answering here untouched.
		{"different endpoint", State{PID: 1, Model: "other/repo", BaseURL: "http://127.0.0.1:9090"}, true, model, url, false},
		// State written before endpoints were recorded names no server here.
		{"unrecorded endpoint", State{PID: 1, Model: "other/repo"}, true, model, url, false},
		// A trailing slash or the default port is not a different endpoint.
		{"same endpoint spelled differently", State{PID: 1, Model: "other/repo", BaseURL: "http://127.0.0.1:8080/"}, true, model, url, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := restartNeeded(c.st, c.recorded, c.model, c.baseURL); got != c.want {
				t.Fatalf("restartNeeded(%+v, %t, %q, %q) = %t, want %t", c.st, c.recorded, c.model, c.baseURL, got, c.want)
			}
		})
	}
}
