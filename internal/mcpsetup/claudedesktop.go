package mcpsetup

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// claudeDesktopName is the display name used in results and error messages.
const claudeDesktopName = "claude-desktop"

// backupSuffix names the copy taken before the first modifying write. The name
// is fixed and overwritten each run on purpose: a timestamped series would
// quietly accumulate copies of a file full of the user's preferences.
const backupSuffix = ".lumi-backup"

// ClaudeDesktop configures Claude Desktop's MCP servers.
//
// Claude Desktop ships no CLI, so its config is read-modify-written in place.
// That file holds far more than mcpServers — this machine has
// coworkUserFilesPath and a large nested preferences object beside it — so the
// write path decodes into json.RawMessage at both the top level and inside
// mcpServers, replaces exactly one key, and leaves every other byte alone.
type ClaudeDesktop struct {
	// ConfigPath is claude_desktop_config.json under Application Support when
	// empty.
	ConfigPath string
	// Required makes an absent Claude Desktop an error rather than a skip.
	Required bool
}

func (d *ClaudeDesktop) Name() string { return claudeDesktopName }

// DefaultDesktopConfigPath is where Claude Desktop keeps its config on macOS.
func DefaultDesktopConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "Claude",
		"claude_desktop_config.json"), nil
}

func (d *ClaudeDesktop) configPath() (string, error) {
	if d.ConfigPath != "" {
		return d.ConfigPath, nil
	}
	return DefaultDesktopConfigPath()
}

// Apply takes no context: it does only local file I/O and spawns nothing.
func (d *ClaudeDesktop) Apply(_ context.Context, spec Spec, opts Options) (Result, error) {
	result := Result{Target: claudeDesktopName}

	path, err := d.configPath()
	if err != nil {
		return result, fmt.Errorf("%s: %w", claudeDesktopName, err)
	}

	// Installation is detected on the directory, not the file: a fresh install
	// has the directory but no config yet. Deliberately no MkdirAll — creating
	// Claude Desktop's support directory on a machine without Claude Desktop
	// leaves litter that is indistinguishable from a broken install.
	dir := filepath.Dir(path)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		result.Status = StatusSkipped
		result.Detail = "Claude Desktop is not installed"
		result.Manual = ManualSnippet(spec)
		if d.Required {
			return result, notInstalledErr(claudeDesktopName, result.Detail)
		}
		return result, nil
	}

	root, servers, mode, existed, err := readDesktopConfig(path)
	if err != nil {
		return result, err
	}

	var existing entry
	found := false
	if raw, ok := servers[spec.Name]; ok {
		if err := json.Unmarshal(raw, &existing); err == nil {
			found = true
		}
	}

	switch {
	case found && existing.matches(spec):
		result.Status = StatusUnchanged
		result.Detail = "already configured"
		return result, nil
	case found && !opts.Force:
		result.Status = StatusConflict
		result.Detail = "an entry with different settings already exists"
		result.Current = existing.commandLine()
		result.Manual = ManualSnippet(spec)
		return result, conflictErr(claudeDesktopName, spec.Name)
	}

	result.Status = StatusAdded
	if found {
		result.Status = StatusReplaced
	}
	result.Detail = spec.CommandLine()

	if opts.DryRun {
		return result, nil
	}

	// Back up only when a modifying write is actually about to happen, so a
	// no-op run leaves no trace.
	if existed {
		if err := copyFile(path, path+backupSuffix, mode); err != nil {
			return result, fmt.Errorf("%s: back up %s: %w", claudeDesktopName, path, err)
		}
	}

	encoded, err := json.Marshal(newEntry(spec))
	if err != nil {
		return result, fmt.Errorf("%s: %w", claudeDesktopName, err)
	}
	servers[spec.Name] = encoded
	rawServers, err := json.Marshal(servers)
	if err != nil {
		return result, fmt.Errorf("%s: %w", claudeDesktopName, err)
	}
	root["mcpServers"] = rawServers

	// Two-space indent matches the file Claude Desktop writes. Go sorts map
	// keys, so the top-level key order comes back alphabetical; the invariant
	// worth holding is that every key and value survives, and an
	// order-preserving JSON rewriter is disproportionate to that.
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return result, fmt.Errorf("%s: %w", claudeDesktopName, err)
	}
	data = append(data, '\n')

	if err := writeFileAtomic(path, data, mode); err != nil {
		return result, fmt.Errorf("%s: write %s: %w", claudeDesktopName, path, err)
	}
	result.Changed = true
	return result, nil
}

// readDesktopConfig loads the config, returning the top-level object, the
// mcpServers map, the mode to write back with, and whether the file existed.
//
// Both maps hold json.RawMessage so that everything Lumi did not write —
// sibling top-level keys, other servers' env and type blocks — round-trips
// byte-for-byte. There is no struct to keep in sync and so nothing to drift.
func readDesktopConfig(path string) (root, servers map[string]json.RawMessage, mode fs.FileMode, existed bool, err error) {
	// 0600 for a file Lumi creates. When the file already exists its mode is
	// preserved instead: silently tightening permissions on another
	// application's file is a side effect nobody asked for.
	mode = 0o600
	root = map[string]json.RawMessage{}
	servers = map[string]json.RawMessage{}

	data, statErr := os.ReadFile(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return root, servers, mode, false, nil
		}
		return nil, nil, 0, false, fmt.Errorf("%s: read %s: %w", claudeDesktopName, path, statErr)
	}
	existed = true
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	if err := json.Unmarshal(data, &root); err != nil {
		// Never repair by rewriting from scratch: that would discard
		// preferences and everything else the user has accumulated.
		return nil, nil, 0, false, fmt.Errorf(
			"%s: %s is not valid JSON (%w); fix or remove it, then re-run", claudeDesktopName, path, err)
	}

	raw, ok := root["mcpServers"]
	if !ok || string(raw) == "null" {
		return root, servers, mode, existed, nil
	}
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, nil, 0, false, fmt.Errorf(
			"%s: %s has an mcpServers value that is not an object; fix it, then re-run",
			claudeDesktopName, path)
	}
	return root, servers, mode, existed, nil
}

// writeFileAtomic writes via a temp file in the same directory plus a rename.
// A same-directory rename is atomic, so a running Claude Desktop can never
// observe a half-written config.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func copyFile(src, dst string, mode fs.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

var _ Target = (*ClaudeDesktop)(nil)
