package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigMissingFileIsZero(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing config must not error: %v", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("missing config must be zero, got %+v", cfg)
	}
}

func TestSaveConfigRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", ConfigFileName)
	want := Config{CerebrasAPIKey: "secret-key", CerebrasModel: "qwen-3-32b"}
	if err := SaveConfig(path, want); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config permissions = %o, want 600 (may hold a secret)", perm)
	}

	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
}

func TestSaveConfigForces0600OnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	// Simulate a pre-existing, group/world-readable config file.
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	want := Config{CerebrasAPIKey: "secret-key", CerebrasModel: "qwen-3-32b"}
	if err := SaveConfig(path, want); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config permissions = %o, want 600 (may hold a secret)", perm)
	}

	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
}

func TestResolvedModelFallsBackToDefault(t *testing.T) {
	if got := (Config{}).ResolvedModel(); got != DefaultCerebrasModel {
		t.Fatalf("empty model = %q, want %q", got, DefaultCerebrasModel)
	}
	if got := (Config{CerebrasModel: "custom"}).ResolvedModel(); got != "custom" {
		t.Fatalf("set model = %q, want %q", got, "custom")
	}
}
