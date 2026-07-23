package cli

import (
	"bufio"
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puremetricsai/lumi/internal/config"
)

// newConfigureTest returns the data dir and a runner that executes the
// configure subcommand directly (bypassing root's platform.Validate) with the
// given stdin.
func newConfigureTest(t *testing.T) (string, func(stdin string, args ...string) (string, error)) {
	t.Helper()
	dataDir := t.TempDir()
	a := &app{dataDir: dataDir}
	run := func(stdin string, args ...string) (string, error) {
		cmd := a.configureCommand()
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stdout)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return stdout.String(), err
	}
	return dataDir, run
}

func readTestConfig(t *testing.T, dataDir string) config.Config {
	t.Helper()
	cfg, err := config.LoadConfig(filepath.Join(dataDir, config.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfigureFlagsPersist(t *testing.T) {
	dataDir, run := newConfigureTest(t)
	if _, err := run("", "--api-key", "secret-key", "--model", "qwen-3-32b"); err != nil {
		t.Fatal(err)
	}
	cfg := readTestConfig(t, dataDir)
	if cfg.CerebrasAPIKey != "secret-key" || cfg.CerebrasModel != "qwen-3-32b" {
		t.Fatalf("persisted config = %+v", cfg)
	}
}

func TestConfigureInteractiveSetsValues(t *testing.T) {
	dataDir, run := newConfigureTest(t)
	if _, err := run("interactive-key\nllama-4-scout\n"); err != nil {
		t.Fatal(err)
	}
	cfg := readTestConfig(t, dataDir)
	if cfg.CerebrasAPIKey != "interactive-key" || cfg.CerebrasModel != "llama-4-scout" {
		t.Fatalf("persisted config = %+v", cfg)
	}
}

func TestConfigureInteractiveBlankKeepsExisting(t *testing.T) {
	dataDir, run := newConfigureTest(t)
	if _, err := run("", "--api-key", "keep-me", "--model", "keep-model"); err != nil {
		t.Fatal(err)
	}
	// Blank answers must preserve both values.
	if _, err := run("\n\n"); err != nil {
		t.Fatal(err)
	}
	cfg := readTestConfig(t, dataDir)
	if cfg.CerebrasAPIKey != "keep-me" || cfg.CerebrasModel != "keep-model" {
		t.Fatalf("blank input clobbered config: %+v", cfg)
	}
}

// TestReadKeyNonTerminalFallback verifies that a non-*os.File reader (piped
// input, tests) keeps the echoed line-reading path instead of touching the
// no-echo terminal branch.
func TestReadKeyNonTerminalFallback(t *testing.T) {
	in := strings.NewReader("piped-secret\nmodel\n")
	reader := bufio.NewReader(in)
	var out bytes.Buffer
	key, err := readKey(in, &out, reader)
	if err != nil {
		t.Fatal(err)
	}
	if key != "piped-secret" {
		t.Fatalf("readKey = %q, want %q", key, "piped-secret")
	}
	// The fallback must not write a synthetic newline the way ReadPassword does.
	if out.Len() != 0 {
		t.Fatalf("fallback path wrote to out: %q", out.String())
	}
	// The bufio reader must retain the un-consumed model line.
	rest, _ := reader.ReadString('\n')
	if strings.TrimSpace(rest) != "model" {
		t.Fatalf("remaining input = %q, want %q", strings.TrimSpace(rest), "model")
	}
}

func TestConfigureShowMasksKey(t *testing.T) {
	_, run := newConfigureTest(t)
	if _, err := run("", "--api-key", "super-secret-1234", "--model", "qwen-3-32b"); err != nil {
		t.Fatal(err)
	}
	out, err := run("", "--show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "super-secret-1234") {
		t.Fatalf("--show leaked the raw API key:\n%s", out)
	}
	if !strings.Contains(out, "1234") || !strings.Contains(out, "qwen-3-32b") {
		t.Fatalf("--show should fingerprint the key and print the model:\n%s", out)
	}
}
