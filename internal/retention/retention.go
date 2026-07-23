// Package retention enforces Lumi's data-retention policy: it removes old
// events and the media files they point at.
//
// Rows are deleted before their files are unlinked. A crash between the two
// leaves orphaned files on disk, which is recoverable; the reverse order would
// leave rows referencing media that no longer exists, which is not.
package retention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

type Options struct {
	// Before deletes every event captured strictly before this instant.
	Before *time.Time
	// MaxBytes caps the total size of media files on disk. Oldest events are
	// deleted until the footprint fits. Zero disables size-based pruning.
	MaxBytes int64
	// All deletes every event and its media, ignoring Before and MaxBytes. It
	// is the destructive "wipe everything" policy behind `lumi prune --all`.
	All bool
	// MediaDirs are the directories the --all wipe sweeps for orphaned media
	// (files no event row references) after deleting indexed rows and their
	// files. Typically Paths.Screenshots and Paths.Audio. Only the All policy
	// reads this; age and size policies never sweep directories.
	MediaDirs []string
	// DryRun reports what would be deleted without deleting anything.
	DryRun bool
}

type Result struct {
	Events       int64 `json:"events"`
	Bytes        int64 `json:"bytes"`
	MissingFiles int   `json:"missing_files"`
	// OrphanFiles counts media files removed by the --all sweep that no event
	// row referenced. Their bytes are already included in Bytes.
	OrphanFiles int `json:"orphan_files"`
}

// Prune applies the age policy first, then the size policy.
func Prune(ctx context.Context, s *store.Store, opts Options) (Result, error) {
	var result Result
	if opts.All {
		// The wipe enumerates every row with no timestamp cutoff — a bounded
		// cutoff (even one 1000 years out) uses a strict `captured_at < ?`
		// compare and would silently skip a row dated at or past it. Rows and
		// their referenced files are deleted first (rows-before-files), then
		// any orphaned media no row pointed at is swept from MediaDirs.
		all, err := s.AllEvents(ctx)
		if err != nil {
			return result, err
		}
		partial, err := remove(ctx, s, all, opts.DryRun)
		result.add(partial)
		if err != nil {
			return result, err
		}
		orphans, err := sweepOrphans(opts.MediaDirs, all, opts.DryRun)
		result.add(orphans)
		return result, err
	}
	if opts.Before == nil && opts.MaxBytes <= 0 {
		return result, errors.New("prune requires --all, --older-than, or --max-bytes")
	}

	agePruned := make(map[int64]bool)
	if opts.Before != nil {
		expired, err := s.Expired(ctx, *opts.Before, 0)
		if err != nil {
			return result, err
		}
		for _, e := range expired {
			agePruned[e.ID] = true
		}
		partial, err := remove(ctx, s, expired, opts.DryRun)
		result.add(partial)
		if err != nil {
			return result, err
		}
	}

	if opts.MaxBytes > 0 {
		// After age pruning, walk everything oldest-first and drop until the
		// remaining footprint fits under the cap. A cutoff an hour in the
		// future is simply "every row".
		all, err := s.Expired(ctx, time.Now().UTC().Add(time.Hour), 0)
		if err != nil {
			return result, err
		}
		// On a real run the age pass already deleted its rows, so `all` holds
		// only survivors. On a dry run nothing was deleted, so drop the
		// age-pruned rows here to see exactly the set a real run's size stage sees.
		if opts.DryRun && len(agePruned) > 0 {
			kept := all[:0]
			for _, event := range all {
				if !agePruned[event.ID] {
					kept = append(kept, event)
				}
			}
			all = kept
		}
		var total int64
		sizes := make([]int64, len(all))
		for i, event := range all {
			sizes[i] = fileSize(event.MediaPath)
			total += sizes[i]
		}
		overBy := total - opts.MaxBytes
		if overBy > 0 {
			cutoff := 0
			var freed int64
			for cutoff < len(all) && freed < overBy {
				freed += sizes[cutoff]
				cutoff++
			}
			partial, err := remove(ctx, s, all[:cutoff], opts.DryRun)
			result.add(partial)
			if err != nil {
				return result, err
			}
		}
	}

	return result, nil
}

func remove(ctx context.Context, s *store.Store, events []store.Event, dryRun bool) (Result, error) {
	var result Result
	if len(events) == 0 {
		return result, nil
	}
	ids := make([]int64, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
		if size := fileSize(event.MediaPath); size > 0 {
			result.Bytes += size
		} else if !exists(event.MediaPath) {
			result.MissingFiles++
		}
	}
	if dryRun {
		result.Events = int64(len(ids))
		return result, nil
	}
	deleted, err := s.DeleteByIDs(ctx, ids)
	result.Events = deleted
	if err != nil {
		return result, err
	}
	for _, event := range events {
		if err := os.Remove(event.MediaPath); err != nil && !os.IsNotExist(err) {
			return result, fmt.Errorf("remove media %s: %w", event.MediaPath, err)
		}
	}
	return result, nil
}

// sweepOrphans removes regular files remaining in the media directories that no
// event row referenced. Only the --all wipe calls it: it is what makes the wipe
// a true privacy guarantee, catching media a failed Insert never indexed or a
// prior prune left behind after deleting its row but before unlinking the file.
//
// referenced holds the media paths of the just-removed rows. On a real run
// those files are already unlinked so the skip is a no-op; on a dry run they are
// still present, and skipping them keeps the reported bytes equal to a real run
// (remove already counted them) instead of double-counting them as orphans.
func sweepOrphans(dirs []string, referenced []store.Event, dryRun bool) (Result, error) {
	var result Result
	known := make(map[string]bool, len(referenced))
	for _, event := range referenced {
		known[filepath.Clean(event.MediaPath)] = true
	}
	seenDir := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if dir == "" || seenDir[dir] {
			continue
		}
		seenDir[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return result, fmt.Errorf("scan media directory %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if known[filepath.Clean(path)] {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return result, fmt.Errorf("stat orphan media %s: %w", path, err)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			result.OrphanFiles++
			result.Bytes += info.Size()
			if dryRun {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return result, fmt.Errorf("remove orphan media %s: %w", path, err)
			}
		}
	}
	return result, nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (r *Result) add(other Result) {
	r.Events += other.Events
	r.Bytes += other.Bytes
	r.MissingFiles += other.MissingFiles
	r.OrphanFiles += other.OrphanFiles
}
