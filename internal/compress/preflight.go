package compress

import (
	"context"
	"fmt"
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

// checkRoots reports events whose media lies outside the directories this run
// was given.
//
// This is the check that would have prevented a real incident. `media_path` is
// stored absolute, so copying a data directory and pointing `--data-dir` at the
// copy gives you the copy's *database* and the original's *files* — and compress,
// unlike every other command, rewrites and deletes what it reads. A copied index
// therefore destroys the originals it was supposed to leave alone.
//
// It fails the whole run rather than skipping the offending rows: media outside
// the roots means the caller is not looking at the index they think they are,
// and quietly compressing the subset that happens to be inside would be acting
// on that misunderstanding rather than reporting it.
func checkRoots(events []store.Event, mediaDirs []string) error {
	if len(mediaDirs) == 0 {
		return nil
	}
	roots := make([]string, 0, len(mediaDirs))
	for _, dir := range mediaDirs {
		if dir == "" {
			continue
		}
		absolute, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("resolve media directory %s: %w", dir, err)
		}
		roots = append(roots, filepath.Clean(absolute)+string(filepath.Separator))
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
		absolute, err := filepath.Abs(event.MediaPath)
		if err != nil {
			return fmt.Errorf("resolve media path %s: %w", event.MediaPath, err)
		}
		path := filepath.Clean(absolute)
		within := false
		for _, root := range roots {
			if strings.HasPrefix(path+string(filepath.Separator), root) {
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
	if count == 0 {
		return nil
	}
	return fmt.Errorf("%d indexed events name media outside this data directory (for example %s); "+
		"compress rewrites and deletes the files it reads, so it will not touch media belonging to "+
		"another index. media_path is stored absolute, so a copied data directory still names the "+
		"original's files — see docs/compress.md",
		count, strings.Join(outside, ", "))
}

// findConflicts reports events this run must not compress because another row
// would lose its media if it did.
//
// `events.media_path` carries no uniqueness constraint, and destinations are
// derived deterministically by swapping the extension, so two situations break
// the one-to-one assumption the replacement sequence rests on:
//
//   - Two rows naming the same file. Compressing the first deletes the media the
//     second still names.
//   - A row whose destination is already named by a different row. Writing it
//     overwrites that row's media, and the FLAC encoder truncates an existing
//     destination outright.
//
// Neither occurs in an index Lumi wrote — capture names files uniquely per
// display per instant, and per chunk per track — so this is a guard against an
// index that has been edited or merged, not against ordinary operation. The
// affected rows are skipped and counted rather than failing the run, because the
// inconsistency belongs to those rows and not to the rest of the history.
func findConflicts(events []store.Event) map[int64]string {
	conflicts := make(map[int64]string)
	owners := make(map[string][]store.Event, len(events))
	for _, event := range events {
		if event.MediaPath == "" {
			continue
		}
		key := filepath.Clean(event.MediaPath)
		owners[key] = append(owners[key], event)
	}
	for _, sharing := range owners {
		if len(sharing) < 2 {
			continue
		}
		for _, event := range sharing {
			conflicts[event.ID] = fmt.Sprintf("%d events share this media file", len(sharing))
		}
	}
	for _, event := range events {
		if event.MediaPath == "" {
			continue
		}
		for _, extension := range []string{extHEIC, extFLAC} {
			destination := filepath.Clean(swapExtension(event.MediaPath, extension))
			if destination == filepath.Clean(event.MediaPath) {
				continue
			}
			for _, other := range owners[destination] {
				if other.ID == event.ID {
					continue
				}
				conflicts[event.ID] = fmt.Sprintf("event %d already holds the file this would be compressed to", other.ID)
			}
		}
	}
	return conflicts
}

func runPreflight(_ context.Context, events []store.Event, opts Options) (preflightReport, error) {
	if err := checkRoots(events, opts.MediaDirs); err != nil {
		return preflightReport{}, err
	}
	return preflightReport{conflicts: findConflicts(events)}, nil
}
