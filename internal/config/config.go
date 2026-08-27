package config

import (
	"fmt"
	"os"
	"path/filepath"
)

type Paths struct {
	Root        string
	Database    string
	Screenshots string
	Audio       string
	// RecordState is the JSON file that tracks a background recorder (pid,
	// start time, what it captures). RecordLog collects the background
	// recorder's stdout/stderr. UpdateLog collects the detached installer's,
	// which outlives the process that started it and is the only record of
	// what an in-app update did. All three live directly under Root.
	RecordState string
	RecordLog   string
	UpdateLog   string
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
		UpdateLog:   filepath.Join(root, "update.log"),
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
