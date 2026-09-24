package mcpsetup

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const piName = "pi"

// piAgentDirVar relocates Pi's agent directory. Read from this process first,
// then from the user's shell, which is where Pi itself would have read it.
const piAgentDirVar = "PI_CODING_AGENT_DIR"

//go:embed pi.ts
var piExtension []byte

// Pi installs a personal extension; it does not have an MCP config file.
type Pi struct {
	AgentDir string // Defaults to PI_CODING_AGENT_DIR, else ~/.pi/agent.
	LookPath func(string) (string, error)
	Required bool
}

func (*Pi) Name() string { return piName }

func (p *Pi) agentDir() (string, error) {
	if p.AgentDir != "" {
		return p.AgentDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := os.Getenv(piAgentDirVar)
	if dir == "" {
		dir = userPiAgentDir()
	}
	if dir == "" {
		return filepath.Join(home, ".pi", "agent"), nil
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(home, dir[1:])
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s must be absolute for Lumi.app to locate Pi's extensions", piAgentDirVar)
	}
	return dir, nil
}

func piSource(spec Spec) []byte {
	binary, _ := json.Marshal(spec.Command)
	args, _ := json.Marshal(spec.Args)
	return []byte(strings.NewReplacer("__LUMI_BINARY__", string(binary), "__LUMI_ARGS__", string(args)).Replace(string(piExtension)))
}

func (p *Pi) Apply(_ context.Context, spec Spec, opts Options) (Result, error) {
	result := Result{Target: piName}
	path, err := p.agentDir()
	if err != nil {
		result.Status = StatusFailed
		return result, fmt.Errorf("pi: locate agent directory: %w", err)
	}
	file := filepath.Join(path, "extensions", "lumi.ts")
	wanted := piSource(spec)
	result.Manual = string(wanted)
	result.ManualHint = "save this as " + file + " and reload Pi"

	look := p.LookPath
	if look == nil {
		look = lookCLI
	}
	if _, err := look("pi"); err != nil {
		if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
			result.Status = StatusSkipped
			result.Detail = "Pi is not installed"
			if p.Required {
				return result, notInstalledErr(piName, result.Detail)
			}
			return result, nil
		}
	}

	if spec.Name != DefaultName {
		// Reached via --client all, this is a skip like an absent client, so a
		// custom name still configures the others and exits zero.
		result.Detail = "Pi's extension uses the fixed name lumi"
		if !p.Required {
			result.Status = StatusSkipped
			return result, nil
		}
		result.Status = StatusFailed
		return result, fmt.Errorf("pi: --name %q is not supported; use --client to select an MCP client for custom names", spec.Name)
	}

	current, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		result.Status = StatusFailed
		return result, fmt.Errorf("pi: read %s: %w", file, err)
	}
	found := err == nil
	switch {
	case found && bytes.Equal(current, wanted):
		result.Status = StatusUnchanged
		result.Detail = "already configured"
		return result, nil
	case found && !opts.Force:
		result.Status = StatusConflict
		result.Detail = "an extension with different contents already exists"
		result.Current = file
		return result, fmt.Errorf("pi: refusing to overwrite %s; use --force to replace it", file)
	}

	result.Status = StatusAdded
	if found {
		result.Status = StatusReplaced
	}
	result.Detail = file
	if opts.DryRun {
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return result, fmt.Errorf("pi: create extension directory: %w", err)
	}
	mode := os.FileMode(0o600)
	if found {
		info, err := os.Stat(file)
		if err != nil {
			return result, fmt.Errorf("pi: stat extension: %w", err)
		}
		mode = info.Mode().Perm()
		if err := copyFile(file, file+backupSuffix, mode); err != nil {
			return result, fmt.Errorf("pi: back up extension: %w", err)
		}
	}
	if err := writeFileAtomic(file, wanted, mode); err != nil {
		return result, fmt.Errorf("pi: write extension: %w", err)
	}
	result.Changed = true
	result.AfterChange = "Reload Pi to load the new extension."
	return result, nil
}

var _ Target = (*Pi)(nil)
