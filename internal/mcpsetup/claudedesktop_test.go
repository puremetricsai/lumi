package mcpsetup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// realWorldConfig mirrors the shape of an actual claude_desktop_config.json:
// mcpServers beside unrelated top-level keys, and a second server carrying
// fields Lumi's own entry type does not model.
const realWorldConfig = `{
  "coworkUserFilesPath": "/Users/someone/Claude",
  "preferences": {
    "sidebarMode": "chat",
    "localAgentModeTrustedFolders": ["/Users/someone/code"],
    "coworkWebSearchEnabled": true
  },
  "mcpServers": {
    "other": {
      "type": "stdio",
      "command": "/usr/local/bin/other",
      "args": ["serve"],
      "env": {"TOKEN": "secret"}
    },
    "lumi": {
      "command": "lumi",
      "args": ["mcp"]
    }
  }
}
`

// newDesktop writes body to a temp Claude support directory and returns a
// target plus the config path. An empty body leaves the file absent.
func newDesktop(t *testing.T, body string) (*ClaudeDesktop, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "claude_desktop_config.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	return &ClaudeDesktop{ConfigPath: path}, path
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return parsed
}

// The load-bearing test: everything Lumi did not write survives untouched.
func TestDesktopPreservesUnknownTopLevelKeys(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, realWorldConfig)
	before := readJSON(t, path)

	if _, err := target.Apply(context.Background(), testSpec(), Options{Force: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after := readJSON(t, path)

	for key, want := range before {
		if key == "mcpServers" {
			continue
		}
		if !reflect.DeepEqual(after[key], want) {
			t.Errorf("top-level %q changed:\n before %#v\n after  %#v", key, want, after[key])
		}
	}

	beforeServers := before["mcpServers"].(map[string]any)
	afterServers, ok := after["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers is %T, want an object", after["mcpServers"])
	}
	// A sibling server's env and type must round-trip; Lumi's entry type does
	// not model everything a client may store.
	if !reflect.DeepEqual(afterServers["other"], beforeServers["other"]) {
		t.Errorf("sibling server changed:\n before %#v\n after  %#v",
			beforeServers["other"], afterServers["other"])
	}

	lumi := afterServers["lumi"].(map[string]any)
	if lumi["command"] != "/abs/lumi" {
		t.Errorf("command = %v, want the absolute path", lumi["command"])
	}
}

func TestDesktopIsIdempotent(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, realWorldConfig)

	if _, err := target.Apply(context.Background(), testSpec(), Options{Force: true}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	afterFirst, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if result.Status != StatusUnchanged {
		t.Errorf("second run status = %q, want unchanged", result.Status)
	}
	if result.Changed {
		t.Error("second run reported a change")
	}

	afterSecond, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Byte equality is the strongest available statement of "wrote nothing".
	if string(afterFirst) != string(afterSecond) {
		t.Errorf("second run rewrote the file:\n%s\n---\n%s", afterFirst, afterSecond)
	}
}

func TestDesktopConflictLeavesTheFileUntouched(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, realWorldConfig)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err == nil {
		t.Fatal("Apply succeeded over a differing entry, want a conflict")
	}
	if result.Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", result.Status)
	}
	if result.Current != "lumi mcp" {
		t.Errorf("Current = %q, want the existing entry rendered", result.Current)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Error("config changed despite the conflict")
	}
	if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
		t.Error("a backup was written for a run that changed nothing")
	}
}

// An entry under Lumi's name that does not decode is still the user's entry.
// Treating it as absent would overwrite it with no --force and no backup.
func TestDesktopRefusesToOverwriteAnUnreadableEntry(t *testing.T) {
	t.Parallel()
	const body = `{"mcpServers":{"lumi":"not an object"}}`
	target, path := newDesktop(t, body)

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err == nil {
		t.Fatal("Apply overwrote an unreadable entry without --force")
	}
	if result.Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", result.Status)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != body {
		t.Errorf("config changed despite the conflict:\n%s", after)
	}
}

// --force is still the way through: the user has said to replace it.
func TestDesktopForceReplacesAnUnreadableEntry(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, `{"mcpServers":{"lumi":"not an object"}}`)

	result, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusReplaced || !result.Changed {
		t.Fatalf("got %+v, want replaced and changed", result)
	}
	servers := readJSON(t, path)["mcpServers"].(map[string]any)
	entry, ok := servers["lumi"].(map[string]any)
	if !ok {
		t.Fatalf("lumi entry = %#v, want an object", servers["lumi"])
	}
	if entry["command"] != "/abs/lumi" {
		t.Errorf("command = %v, want the spec's binary", entry["command"])
	}
}

// A config file holding literal `null` unmarshals into a nil map. Assigning
// into it panics, so this pins that the file is handled rather than crashed on.
func TestDesktopHandlesANullConfigFile(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, "null\n")

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusAdded || !result.Changed {
		t.Fatalf("got %+v, want added and changed", result)
	}
	servers, ok := readJSON(t, path)["mcpServers"].(map[string]any)
	if !ok || servers["lumi"] == nil {
		t.Errorf("entry was not written over the null config: %v", readJSON(t, path))
	}
	// The original file still had to be preserved before being replaced.
	backup, err := os.ReadFile(path + backupSuffix)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != "null\n" {
		t.Errorf("backup = %q, want the pre-run file", backup)
	}
}

func TestDesktopForceReplacesAndBacksUp(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, realWorldConfig)

	result, err := target.Apply(context.Background(), testSpec(), Options{Force: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusReplaced || !result.Changed {
		t.Fatalf("got %+v, want replaced and changed", result)
	}

	backup, err := os.ReadFile(path + backupSuffix)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != realWorldConfig {
		t.Errorf("backup is not the pre-run file:\n%s", backup)
	}
}

func TestDesktopWritesNoBackupOnNoOp(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, realWorldConfig)
	if _, err := target.Apply(context.Background(), testSpec(), Options{Force: true}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	backupPath := path + backupSuffix
	first, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}

	if _, err := target.Apply(context.Background(), testSpec(), Options{}); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	second, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(first) != string(second) {
		t.Error("the no-op run overwrote the backup")
	}
}

func TestDesktopCreatesFileWhenOnlyTheDirExists(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, "")

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusAdded {
		t.Fatalf("status = %q, want added", result.Status)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 for a file Lumi created", info.Mode().Perm())
	}
	parsed := readJSON(t, path)
	if len(parsed) != 1 || parsed["mcpServers"] == nil {
		t.Errorf("new file holds %v, want only mcpServers", parsed)
	}
	if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
		t.Error("backed up a file that did not exist")
	}
}

// Setup must never create Claude Desktop's support directory: the litter would
// be indistinguishable from a broken install.
func TestDesktopSkipsWhenDirectoryMissing(t *testing.T) {
	t.Parallel()
	for name, required := range map[string]bool{"implicit": false, "explicit": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "Claude")
			path := filepath.Join(dir, "claude_desktop_config.json")
			target := &ClaudeDesktop{ConfigPath: path, Required: required}

			result, err := target.Apply(context.Background(), testSpec(), Options{})
			if result.Status != StatusSkipped {
				t.Fatalf("status = %q, want skipped", result.Status)
			}
			if result.Manual == "" {
				t.Error("skip result carries no manual snippet")
			}
			if required && err == nil {
				t.Error("explicit target skipped without an error")
			}
			if !required && err != nil {
				t.Errorf("implicit skip returned %v, want nil", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Error("setup created the Claude support directory")
			}
		})
	}
}

func TestDesktopPreservesExistingFileMode(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, realWorldConfig)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := target.Apply(context.Background(), testSpec(), Options{Force: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want the original 0644 preserved", info.Mode().Perm())
	}
}

func TestDesktopRefusesInvalidJSON(t *testing.T) {
	t.Parallel()
	broken := `{"preferences": {"sidebarMode": "chat"},}`
	target, path := newDesktop(t, broken)

	if _, err := target.Apply(context.Background(), testSpec(), Options{Force: true}); err == nil {
		t.Fatal("Apply succeeded over invalid JSON, want an error")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Repairing by rewriting would discard preferences.
	if string(after) != broken {
		t.Error("setup rewrote a file it could not parse")
	}
}

func TestDesktopRefusesNonObjectMCPServers(t *testing.T) {
	t.Parallel()
	body := `{"mcpServers": []}`
	target, path := newDesktop(t, body)

	if _, err := target.Apply(context.Background(), testSpec(), Options{Force: true}); err == nil {
		t.Fatal("Apply succeeded over a non-object mcpServers, want an error")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != body {
		t.Error("setup overwrote an mcpServers value it did not understand")
	}
}

func TestDesktopTreatsNullMCPServersAsAbsent(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, `{"preferences":{"sidebarMode":"chat"},"mcpServers":null}`)

	result, err := target.Apply(context.Background(), testSpec(), Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != StatusAdded {
		t.Fatalf("status = %q, want added", result.Status)
	}
	parsed := readJSON(t, path)
	if parsed["preferences"] == nil {
		t.Error("preferences were dropped")
	}
}

func TestDesktopUsesTwoSpaceIndent(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, realWorldConfig)
	if _, err := target.Apply(context.Background(), testSpec(), Options{Force: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "\n  \"mcpServers\"") {
		t.Errorf("mcpServers is not indented by two spaces:\n%s", text)
	}
	if strings.Contains(text, "\t") {
		t.Error("output contains tabs")
	}
	if !strings.HasSuffix(text, "\n") {
		t.Error("output has no trailing newline")
	}
}

func TestDesktopDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body string
		opts Options
		want Status
	}{
		"would add":     {`{"preferences":{}}`, Options{DryRun: true}, StatusAdded},
		"would replace": {realWorldConfig, Options{DryRun: true, Force: true}, StatusReplaced},
		"conflict":      {realWorldConfig, Options{DryRun: true}, StatusConflict},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			target, path := newDesktop(t, tc.body)

			result, _ := target.Apply(context.Background(), testSpec(), tc.opts)
			if result.Status != tc.want {
				t.Errorf("status = %q, want %q", result.Status, tc.want)
			}
			if result.Changed {
				t.Error("Changed = true under --dry-run")
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(after) != tc.body {
				t.Errorf("--dry-run wrote to the config:\n%s", after)
			}
			if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
				t.Error("--dry-run wrote a backup")
			}
		})
	}
}

// The atomic write must not leave temp files behind on the success path.
func TestDesktopLeavesNoTempFiles(t *testing.T) {
	t.Parallel()
	target, path := newDesktop(t, realWorldConfig)
	if _, err := target.Apply(context.Background(), testSpec(), Options{Force: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
