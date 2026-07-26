package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultCerebrasModel = "gpt-oss-120b"
	CerebrasEndpoint     = "https://api.cerebras.ai/v1/chat/completions"
	ConfigFileName       = "config.json"

	// Provider names for the `ask` inference backend.
	ProviderCerebras = "cerebras"
	ProviderLlamaCpp = "llama.cpp"
	DefaultProvider  = ProviderCerebras

	// DefaultLlamaBaseURL is where a local llama-server listens by default.
	DefaultLlamaBaseURL = "http://127.0.0.1:8080"
)

// Config is the persisted, user-editable configuration. It lives at
// <Paths.Root>/config.json and replaces the former CEREBRAS_API_KEY /
// LUMI_CEREBRAS_MODEL environment variables.
type Config struct {
	// Provider selects the `ask` inference backend ("cerebras" or "llama.cpp").
	// Empty means the built-in default (Cerebras).
	Provider string `json:"provider,omitempty"`

	CerebrasAPIKey string `json:"cerebras_api_key,omitempty"`
	CerebrasModel  string `json:"cerebras_model,omitempty"`

	// LlamaModel is a GGUF file path or a HuggingFace repo id passed to
	// llama-server; LlamaBaseURL is the server address Lumi talks to (and
	// launches at when it isn't already running).
	LlamaModel   string `json:"llama_model,omitempty"`
	LlamaBaseURL string `json:"llama_base_url,omitempty"`
}

// ResolvedModel returns the configured Cerebras model, falling back to the
// built-in default when none has been set.
func (c Config) ResolvedModel() string {
	if model := strings.TrimSpace(c.CerebrasModel); model != "" {
		return model
	}
	return DefaultCerebrasModel
}

// ResolvedProvider returns the active provider, normalizing case and the
// "llamacpp" spelling, and falling back to the built-in default when unset.
func (c Config) ResolvedProvider() string {
	switch strings.ToLower(strings.TrimSpace(c.Provider)) {
	case "":
		return DefaultProvider
	case ProviderLlamaCpp, "llamacpp", "llama-cpp":
		return ProviderLlamaCpp
	case ProviderCerebras:
		return ProviderCerebras
	default:
		return strings.TrimSpace(c.Provider)
	}
}

// KnownProvider reports whether name resolves to a supported inference
// backend (cerebras or llama.cpp), accepting the same aliases/casing as
// ResolvedProvider. An empty name is known (it means the default).
func KnownProvider(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", ProviderCerebras, ProviderLlamaCpp, "llamacpp", "llama-cpp":
		return true
	default:
		return false
	}
}

// ResolvedLlamaBaseURL returns the configured llama-server base URL, falling
// back to the built-in default.
func (c Config) ResolvedLlamaBaseURL() string {
	if url := strings.TrimSpace(c.LlamaBaseURL); url != "" {
		return url
	}
	return DefaultLlamaBaseURL
}

// ResolvedLlamaModel returns the configured llama.cpp model. There is no
// built-in default — llama-server has no universal model — so this may be empty.
func (c Config) ResolvedLlamaModel() string {
	return strings.TrimSpace(c.LlamaModel)
}

type Paths struct {
	Root        string
	Database    string
	Screenshots string
	Audio       string
	// RecordState is the JSON file that tracks a background recorder (pid,
	// start time, what it captures). RecordLog collects the background
	// recorder's stdout/stderr. Both live directly under Root.
	RecordState string
	RecordLog   string
	// Config is the persisted user configuration (API key, model).
	Config string
	// LlamaLog collects a Lumi-launched llama-server's stdout/stderr;
	// LlamaState records its pid and the model it was launched with, so a
	// configuration change can be told apart from the warm server already
	// answering. Both live directly under Root.
	LlamaLog   string
	LlamaState string
}

func DefaultPaths() (Paths, error) {
	root := os.Getenv("LUMI_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("find home directory: %w", err)
		}
		root = filepath.Join(home, "Library", "Application Support", "Lumi")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve data directory: %w", err)
	}
	return FromRoot(root)
}

func FromRoot(root string) (Paths, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve data directory: %w", err)
	}
	return Paths{
		Root:        root,
		Database:    filepath.Join(root, "lumi.db"),
		Screenshots: filepath.Join(root, "screenshots"),
		Audio:       filepath.Join(root, "audio"),
		RecordState: filepath.Join(root, "record.json"),
		RecordLog:   filepath.Join(root, "record.log"),
		Config:      filepath.Join(root, ConfigFileName),
		LlamaLog:    filepath.Join(root, "llama-server.log"),
		LlamaState:  filepath.Join(root, "llama-server.json"),
	}, nil
}

func (p Paths) Ensure() error {
	for _, dir := range []string{p.Root, p.Screenshots, p.Audio} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// LoadConfig reads the config file at path. A missing file is not an error —
// an unconfigured Lumi is normal — and yields a zero Config.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return c, nil
}

// SaveConfig writes c to path with 0600 permissions, creating the parent
// directory (0700) if needed. The file may hold an API key, so it is never
// world- or group-readable. The write is an atomic replace: data is written to
// a temp file in the same directory (created 0600) and renamed over path, so
// the result is 0600 regardless of any pre-existing file's mode and a crash
// never leaves a partially written config.
func SaveConfig(path string, c Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ConfigFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	// os.CreateTemp makes the file 0600 on Unix, but be explicit so the
	// contract holds regardless of umask or platform quirks.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("set temp config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write config %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("sync config %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close config %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}
