package mcpsetup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiSetup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent")
	p := &Pi{AgentDir: root, LookPath: func(string) (string, error) { return "/usr/bin/pi", nil }}
	spec := Spec{Name: "lumi", Command: `/Applications/Lumi.app/Contents/MacOS/lumi`, Args: []string{"mcp", "--data-dir", `/tmp/lumi "quoted"`}}
	file := filepath.Join(root, "extensions", "lumi.ts")

	result, err := p.Apply(context.Background(), spec, Options{DryRun: true})
	if err != nil || result.Status != StatusAdded || result.Changed || result.AfterChange != "" {
		t.Fatalf("dry run: %+v, %v", result, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("dry run created directory: %v", err)
	}
	if !strings.Contains(result.Manual, `"--data-dir","/tmp/lumi \"quoted\""`) || !strings.Contains(result.ManualHint, file) {
		t.Fatalf("invalid snippet: %s, %s", result.Manual, result.ManualHint)
	}

	result, err = p.Apply(context.Background(), spec, Options{})
	if err != nil || result.Status != StatusAdded || !result.Changed || result.AfterChange == "" {
		t.Fatalf("install: %+v, %v", result, err)
	}
	data, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(data, []byte(result.Manual)) {
		t.Fatalf("installed extension differs from manual snippet: %v", err)
	}
	result, err = p.Apply(context.Background(), spec, Options{})
	if err != nil || result.Status != StatusUnchanged || result.Changed || result.AfterChange != "" {
		t.Fatalf("idempotent setup: %+v, %v", result, err)
	}

	old := []byte("// custom extension\n")
	if err := os.WriteFile(file, old, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	result, err = p.Apply(context.Background(), spec, Options{})
	if err == nil || result.Status != StatusConflict || result.Current != file {
		t.Fatalf("conflict: %+v, %v", result, err)
	}
	unchanged, _ := os.ReadFile(file)
	if !bytes.Equal(old, unchanged) {
		t.Fatal("conflict overwrote custom extension")
	}
	result, err = p.Apply(context.Background(), spec, Options{Force: true})
	if err != nil || result.Status != StatusReplaced || !result.Changed {
		t.Fatalf("force: %+v, %v", result, err)
	}
	backup, err := os.ReadFile(file + backupSuffix)
	if err != nil || !bytes.Equal(backup, old) {
		t.Fatalf("previous extension not backed up: %v", err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("replacement did not preserve permissions: %v, %v", info, err)
	}
}

func TestPiSourceDoesNotReplacePlaceholdersInPaths(t *testing.T) {
	source := piSource(Spec{Command: "/tmp/__LUMI_ARGS__/lumi", Args: []string{"mcp"}})
	if !bytes.Contains(source, []byte(`/tmp/__LUMI_ARGS__/lumi`)) || !bytes.Contains(source, []byte(`const args = ["mcp"]`)) {
		t.Fatalf("substitution changed the supplied binary path: %s", source)
	}
}

func TestPiAgentDirOverride(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom-agent")
	t.Setenv(piAgentDirVar, root)
	p := &Pi{LookPath: func(string) (string, error) { return "/usr/bin/pi", nil }}
	result, err := p.Apply(context.Background(), testSpec(), Options{})
	if err != nil || result.Status != StatusAdded || !result.Changed {
		t.Fatalf("custom agent directory: %+v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "extensions", "lumi.ts")); err != nil {
		t.Fatalf("extension missing from custom agent directory: %v", err)
	}
}

// Lumi.app never sees a PI_CODING_AGENT_DIR exported in ~/.zshrc, so the
// user's shell answers when this process has none. Either value is held to the
// same tilde and absolute rules.
func TestPiAgentDirResolution(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		process, shell, want string
	}{
		"default":        {want: filepath.Join(home, ".pi", "agent")},
		"shell only":     {shell: "/shell/pi", want: "/shell/pi"},
		"process wins":   {process: "/process/pi", shell: "/shell/pi", want: "/process/pi"},
		"process tilde":  {process: "~/pi-test", want: filepath.Join(home, "pi-test")},
		"shell tilde":    {shell: "~", want: home},
		"relative":       {process: "relative/pi-agent"},
		"shell relative": {shell: "relative/pi-agent"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(piAgentDirVar, tc.process)
			stubUserPiAgentDir(t, tc.shell)
			got, err := (&Pi{}).agentDir()
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted a relative agent directory: %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("agentDir = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// A custom --name skips an installed Pi reached via "all", so the other clients'
// registration still exits zero; named explicitly, it fails.
func TestPiCustomServerNameSkipsOrFails(t *testing.T) {
	for name, required := range map[string]bool{"implicit": false, "explicit": true} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "agent")
			p := &Pi{AgentDir: root, LookPath: func(string) (string, error) { return "/usr/bin/pi", nil }, Required: required}
			result, err := p.Apply(context.Background(), Spec{Name: "other", Command: testSpec().Command}, Options{})
			if required {
				if err == nil || result.Status != StatusFailed || !strings.Contains(err.Error(), "--name") {
					t.Fatalf("explicit custom name: %+v, %v", result, err)
				}
			} else if err != nil || result.Status != StatusSkipped || !strings.Contains(result.Detail, "fixed name") {
				t.Fatalf("implicit custom name: %+v, %v", result, err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("custom name created directory: %v", err)
			}
		})
	}
}

func TestPiAbsent(t *testing.T) {
	p := &Pi{AgentDir: filepath.Join(t.TempDir(), "absent"), LookPath: func(string) (string, error) { return "", errors.New("missing") }}
	spec := testSpec()
	result, err := p.Apply(context.Background(), spec, Options{})
	if err != nil || result.Status != StatusSkipped || result.Manual == "" {
		t.Fatalf("absent: %+v, %v", result, err)
	}
	spec.Name = "other"
	result, err = p.Apply(context.Background(), spec, Options{})
	if err != nil || result.Status != StatusSkipped {
		t.Fatalf("absent Pi with custom name: %+v, %v", result, err)
	}
	p.Required = true
	result, err = p.Apply(context.Background(), spec, Options{})
	if err == nil || result.Status != StatusSkipped {
		t.Fatalf("required: %+v, %v", result, err)
	}
}
