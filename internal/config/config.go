package config

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	DefaultCerebrasModel = "gpt-oss-120b"
	CerebrasEndpoint     = "https://api.cerebras.ai/v1/chat/completions"
)

type Paths struct {
	Root        string
	Database    string
	Screenshots string
	Audio       string
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
