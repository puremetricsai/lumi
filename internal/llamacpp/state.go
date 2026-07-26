package llamacpp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// legacyPidFileName is what the first llama.cpp provider build wrote instead of
// a state file: a bare pid, with no record of the model. Reading it lets a
// server started by that build still be managed (and, since its model is
// unknown, restarted onto the configured one).
const legacyPidFileName = "llama-server.pid"

// State records what a Lumi-launched llama-server was started with. The model
// is the point: without it, a configuration change cannot be distinguished from
// the warm server already answering, and `ask` would keep using the old model.
type State struct {
	PID     int    `json:"pid"`
	Model   string `json:"model,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

// ReadState returns the recorded state of a Lumi-launched server, if any.
func ReadState(path string) (State, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return readLegacyPid(filepath.Join(filepath.Dir(path), legacyPidFileName))
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil || st.PID <= 0 {
		return State{}, false
	}
	return st, true
}

func readLegacyPid(path string) (State, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return State{}, false
	}
	return State{PID: pid}, true
}

// WriteState records a launched server at path with 0600 permissions, matching
// the rest of the data directory.
func WriteState(path string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// RemoveState clears the record of a Lumi-launched server, including a legacy
// pid file. A missing file is not an error.
func RemoveState(path string) error {
	for _, p := range []string{path, filepath.Join(filepath.Dir(path), legacyPidFileName)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// restartNeeded reports whether a healthy server must be replaced to honour the
// configured model. recorded means the state belongs to a live llama-server Lumi
// launched — a server Lumi did not start is never killed, and with no model
// configured there is nothing to switch to.
//
// An unrecorded model (a server adopted from a legacy pid file) is not a known
// mismatch, and reloading weights takes minutes: EnsureRunning notes the
// uncertainty instead of paying that cost on a server that is probably already
// right. State that names no endpoint, or a different one, says nothing about
// the server answering at baseURL — restarting on it would kill one server and
// then wait for an unrelated one to stop.
func restartNeeded(st State, recorded bool, model, baseURL string) bool {
	if !recorded || strings.TrimSpace(model) == "" || strings.TrimSpace(st.Model) == "" {
		return false
	}
	if !sameEndpoint(st.BaseURL, baseURL) {
		return false
	}
	return !sameModel(st.Model, model)
}

// sameModel compares models the way llama-server sees them, so that ~ expansion
// and stray whitespace do not read as a model change.
func sameModel(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return a == b
	}
	return reflect.DeepEqual(modelArgs(a), modelArgs(b))
}
