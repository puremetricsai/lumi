package compress

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/puremetricsai/lumi/internal/store"
)

// compressionSiblings maps each format to the one it converts to or from, which
// is the whole basis of what reconcile is allowed to touch.
var compressionSiblings = map[string][]string{
	extHEIC: {extJPG, extJPEG},
	extJPG:  {extHEIC},
	extJPEG: {extHEIC},
	extFLAC: {extWAV},
	extWAV:  {extFLAC},
}

// reconcile cleans up after an interrupted run.
//
// Extension-based idempotence covers *re-running* compress; it cannot see a
// crash. A crash before the row update strands the compressed file, and a crash
// after it strands the original, and neither is visible to a rerun that only
// asks whether a row's path already ends in .heic. Without this, every
// interruption permanently leaks a file.
//
// It is deliberately not a general orphan sweep, and that distinction is the
// most important thing in this package. Captured media exists on disk *before*
// its row does — a frame is written, compared, then inserted; a WAV exists for a
// whole chunk plus transcription latency before its Insert — so a sweep that
// removed any unreferenced file would delete media the recorder had not indexed
// yet, which is the "never lose captured media" rule broken directly. That is
// also why the general sweep lives behind `prune --all`'s irreversible
// confirmation and stays there.
//
// What is safe is far narrower: a file is touched only when its
// extension-swapped sibling is named by a row. Freshly captured media has no
// such sibling, so it is untouchable by construction rather than by a check that
// could be got wrong — no recorder gate, no age bound, no flag.
//
// The third case below is the one that earns the design. A power loss can drop
// the committed row update while the unlink of the original persists, because
// media is flushed with a full barrier and SQLite's WAL commit by default is
// not. Deleting the leftover would destroy the only surviving copy; adopting it
// turns that tail case into a repair.
func reconcile(ctx context.Context, s *store.Store, opts Options) (ReconcileResult, error) {
	var result ReconcileResult
	if len(opts.MediaDirs) == 0 {
		return result, nil
	}
	events, err := s.AllEvents(ctx)
	if err != nil {
		return result, err
	}
	// Built here rather than passed in: a reference set assembled before the
	// passes would not name anything they had just written, and reconcile would
	// delete the entire run's output.
	referenced := make(map[string]store.Event, len(events))
	for _, event := range events {
		referenced[filepath.Clean(event.MediaPath)] = event
	}

	logger := opts.logger()
	seen := make(map[string]bool, len(opts.MediaDirs))
	for _, dir := range opts.MediaDirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return result, fmt.Errorf("scan media directory %s: %w", dir, err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if _, ok := referenced[filepath.Clean(path)]; ok {
				continue
			}
			siblings, ok := compressionSiblings[lowerExt(path)]
			if !ok {
				// Not a format this package produces or consumes, so nothing
				// here can have created it.
				continue
			}
			for _, extension := range siblings {
				owner, ok := referenced[filepath.Clean(swapExtension(path, extension))]
				if !ok {
					continue
				}
				partial, err := settle(ctx, s, opts, path, owner, logger)
				if err != nil {
					return result, err
				}
				result.Removed += partial.Removed
				result.Bytes += partial.Bytes
				result.Recovered += partial.Recovered
				break
			}
		}
	}
	return result, nil
}

// settle decides what to do with one leftover whose sibling row was found.
func settle(ctx context.Context, s *store.Store, opts Options, leftover string,
	owner store.Event, logger *slog.Logger) (ReconcileResult, error) {
	var result ReconcileResult
	size := fileSize(leftover)

	_, ownerErr := os.Stat(owner.MediaPath)
	switch {
	case ownerErr != nil && !os.IsNotExist(ownerErr):
		// "Could not stat" is not "is gone", and conflating them is the one
		// single-process path in this package that destroys data. A transient
		// EACCES or EIO on a *present* original would otherwise adopt the
		// unverified leftover, and the next run — seeing the original as an
		// unreferenced sibling of a referenced file — would delete the only copy
		// that was ever verified. Leave both files alone and say so.
		logger.Warn("could not tell whether an event's media is still there; leaving its leftover alone",
			"event", owner.ID, "media", owner.MediaPath, "leftover", leftover, "error", ownerErr)
		return result, nil
	case ownerErr == nil:
		// The row's own media is present, so this leftover is a redundant copy
		// from an interrupted run — either an unverified compressed file written
		// before the update, or an original left behind after it. Both are safe
		// to drop, and the compressed one is dropped rather than adopted because
		// nothing ever verified it.
		result.Removed++
		result.Bytes += size
		if opts.DryRun {
			return result, nil
		}
		if err := os.Remove(leftover); err != nil && !os.IsNotExist(err) {
			return ReconcileResult{}, fmt.Errorf("remove leftover media %s: %w", leftover, err)
		}
		return result, nil
	}

	// The row names media that is gone while this file survives. Deleting it
	// would destroy the last copy, so adopt it instead — but only after checking
	// it decodes, since an interrupted encode can leave a truncated file and
	// there is no original left to compare it against.
	if !canAdopt(ctx, opts, leftover) {
		logger.Warn("leaving a leftover file that does not decode; its event's media is already gone",
			"event", owner.ID, "media", leftover)
		return result, nil
	}
	result.Recovered++
	if opts.DryRun {
		return result, nil
	}
	updated, err := s.UpdateMediaPath(ctx, owner.ID, owner.MediaPath, leftover)
	if err != nil {
		return ReconcileResult{}, err
	}
	if updated == 0 {
		// Somebody repointed the row while we were deciding; theirs wins.
		result.Recovered--
	}
	return result, nil
}

// canAdopt reports whether a leftover is intact enough to become a row's media.
//
// Audio is checked by decoding it, images by decoding and confirming they have
// pixels. Neither can be compared against an original, because the original is
// what is missing — this is the weakest check in the package and it is applied
// only where the alternative is deleting the last copy of the data.
func canAdopt(ctx context.Context, opts Options, path string) bool {
	switch lowerExt(path) {
	case extHEIC, extJPG, extJPEG:
		if opts.Images == nil {
			return false
		}
		verification, err := opts.Images.Inspect(ctx, path)
		return err == nil && verification.Width > 0 && verification.Height > 0
	case extFLAC, extWAV:
		if opts.Sounds == nil {
			return false
		}
		samples, rate, err := opts.Sounds.Decode(ctx, path)
		return err == nil && rate > 0 && len(samples) > 0
	default:
		return false
	}
}
