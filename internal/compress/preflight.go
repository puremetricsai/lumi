package compress

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/puremetricsai/lumi/internal/store"
)

// preflight refuses to start a run whose inputs would make the replacement
// sequence unsafe, before anything on disk has been touched.
//
// Both checks exist because the sequence assumes something the schema does not
// enforce: that this run owns every file it is about to rewrite. Discovering
// otherwise halfway through means half the work is already committed.
type preflightReport struct {
	// conflicts maps an event id to why it must be skipped rather than
	// compressed. A conflict is an inconsistency in the index, not a failure of
	// this run, so the run continues without those rows.
	conflicts map[int64]string
}

// resolvedPath resolves an existing path in full. A missing final component is
// allowed because old indexes routinely name aged-out media and destinations do
// not exist yet; in that case its existing parent is resolved instead. Every
// other resolution error is fatal rather than weakening containment.
func resolvedPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	// A dangling final symlink is not an ordinary absent destination. Treating
	// it as a basename inside the root would hide where it attempts to lead.
	if info, lstatErr := os.Lstat(absolute); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", err
	}
	parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
	if parentErr != nil {
		return "", parentErr
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func resolvedRoots(mediaDirs []string) ([]string, error) {
	roots := make([]string, 0, len(mediaDirs))
	seen := make(map[string]bool, len(mediaDirs))
	for _, dir := range mediaDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		root, err := resolvedPath(dir)
		if err != nil {
			return nil, fmt.Errorf("resolve media directory %s: %w", dir, err)
		}
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("inspect media directory %s: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("media directory %s is not a directory", dir)
		}
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots, nil
}

func pathWithinRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// checkRoots reports pass candidates whose source or possible destination lies
// outside the directories this run was given. Only the cutoff-bounded candidate
// set belongs here; all events are used separately for ownership, because a
// recent unrelated foreign path must not block valid old history.
func checkRoots(events []store.Event, mediaDirs []string) error {
	roots, err := resolvedRoots(mediaDirs)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return nil
	}

	var outside []string
	count := 0
	for _, event := range events {
		if event.MediaPath == "" {
			continue
		}
		paths := []string{event.MediaPath}
		switch lowerExt(event.MediaPath) {
		case extJPG, extJPEG:
			paths = append(paths, swapExtension(event.MediaPath, extHEIC))
		case extWAV:
			paths = append(paths, swapExtension(event.MediaPath, extFLAC))
		}
		for _, candidate := range paths {
			path, err := resolvedPath(candidate)
			if err != nil {
				return fmt.Errorf("resolve media path %s: %w", candidate, err)
			}
			within := false
			for _, root := range roots {
				if pathWithinRoot(path, root) {
					within = true
					break
				}
			}
			if within {
				continue
			}
			count++
			if len(outside) < 3 {
				outside = append(outside, path)
			}
		}
	}
	if count == 0 {
		return nil
	}
	return fmt.Errorf("%d indexed media paths lie outside this data directory (for example %s); "+
		"compress rewrites and deletes the files it reads, so it will not touch media belonging to "+
		"another index. media_path is stored absolute, so a copied data directory still names the "+
		"original's files — see docs/compress.md",
		count, strings.Join(outside, ", "))
}

type ownedPath struct {
	event    store.Event
	clean    string
	resolved string
	info     os.FileInfo
}

func inspectOwnedPath(event store.Event) ownedPath {
	clean, err := filepath.Abs(event.MediaPath)
	if err != nil {
		clean = filepath.Clean(event.MediaPath)
	} else {
		clean = filepath.Clean(clean)
	}
	resolved, _ := filepath.EvalSymlinks(clean)
	info, _ := os.Stat(clean)
	return ownedPath{event: event, clean: clean, resolved: filepath.Clean(resolved), info: info}
}

func (p ownedPath) keys() []string {
	keys := []string{"path:" + strings.ToLower(p.clean)}
	if p.resolved != "." && p.resolved != "" {
		resolved := "resolved:" + strings.ToLower(p.resolved)
		if resolved != keys[0] {
			keys = append(keys, resolved)
		}
	}
	return keys
}

// equivalentPath uses filesystem identity whenever both paths exist. Lower-case
// keys merely select paths worth comparing; they never decide equivalence, so a
// case-sensitive volume holding distinct frame.jpg and FRAME.JPG keeps both.
func equivalentPath(a, b ownedPath) bool {
	if a.clean == b.clean {
		return true
	}
	return a.info != nil && b.info != nil && os.SameFile(a.info, b.info)
}

// findConflicts reports pass candidates that must not be compressed because any
// event, including one newer than the cutoff, would lose media if they were.
func findConflicts(events []store.Event) map[int64]string {
	conflicts := make(map[int64]string)
	owners := make([]ownedPath, 0, len(events))
	buckets := make(map[string][]int, len(events))
	for _, event := range events {
		if event.MediaPath == "" {
			continue
		}
		owner := inspectOwnedPath(event)
		index := len(owners)
		owners = append(owners, owner)
		for _, key := range owner.keys() {
			buckets[key] = append(buckets[key], index)
		}
	}

	seenPairs := make(map[[2]int]bool)
	for _, bucket := range buckets {
		for i := 0; i < len(bucket); i++ {
			for j := i + 1; j < len(bucket); j++ {
				pair := [2]int{bucket[i], bucket[j]}
				if seenPairs[pair] {
					continue
				}
				seenPairs[pair] = true
				first, second := owners[pair[0]], owners[pair[1]]
				if !equivalentPath(first, second) {
					continue
				}
				conflicts[first.event.ID] = "multiple events share this media file"
				conflicts[second.event.ID] = "multiple events share this media file"
			}
		}
	}

	for _, event := range events {
		if event.MediaPath == "" {
			continue
		}
		var extensions []string
		switch {
		case event.Kind == store.KindScreen && (lowerExt(event.MediaPath) == extJPG || lowerExt(event.MediaPath) == extJPEG):
			extensions = []string{extHEIC}
		case event.Kind == store.KindAudio && lowerExt(event.MediaPath) == extWAV:
			extensions = []string{extFLAC}
		}
		for _, extension := range extensions {
			destinationEvent := event
			destinationEvent.MediaPath = swapExtension(event.MediaPath, extension)
			destination := inspectOwnedPath(destinationEvent)
			if equivalentPath(destination, inspectOwnedPath(event)) {
				continue
			}
			visited := make(map[int]bool)
			for _, key := range destination.keys() {
				for _, index := range buckets[key] {
					if visited[index] {
						continue
					}
					visited[index] = true
					other := owners[index]
					if other.event.ID == event.ID || !equivalentPath(destination, other) {
						continue
					}
					conflicts[event.ID] = fmt.Sprintf("event %d already holds the file this would be compressed to", other.event.ID)
				}
			}
		}
	}
	return conflicts
}

func runPreflight(_ context.Context, candidates, allEvents []store.Event, opts Options) (preflightReport, error) {
	if err := checkRoots(candidates, opts.MediaDirs); err != nil {
		return preflightReport{}, err
	}
	return preflightReport{conflicts: findConflicts(allEvents)}, nil
}
