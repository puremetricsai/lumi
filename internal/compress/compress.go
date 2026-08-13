// Package compress re-encodes indexed media in place: screenshots to HEIC,
// audio to lossless FLAC, then reclaims the database's free pages.
//
// Files are replaced before their rows are repointed, which is the exact inverse
// of internal/retention's rows-before-files rule, and both orderings are correct
// for what they protect. Pruning deletes a row first because an orphaned file is
// recoverable and a row naming media that no longer exists is not; compressing
// writes and verifies the replacement first because here the unrecoverable state
// is a row naming media that does not exist *yet*.
//
// Compress and prune are complementary: prune decides what history to keep,
// compress decides how densely to keep it. Nothing here deletes an event.
package compress

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/store"
)

// Codec names what a pass produces. CodecNone disables the pass entirely.
type Codec string

const (
	CodecNone Codec = "none"
	CodecHEIC Codec = "heic"
	CodecFLAC Codec = "flac"
)

// Extensions this package understands, lower-cased. Eligibility is a whitelist
// rather than a blacklist so that a container added later — a movie holding many
// frames, say — is skipped by an old build instead of being fed to the JPEG path.
const (
	extJPG  = ".jpg"
	extJPEG = ".jpeg"
	extHEIC = ".heic"
	extWAV  = ".wav"
	extFLAC = ".flac"
)

// DefaultQuality is the HEIC quality `lumi compress` ships with.
//
// Measured over 309 frames sampled across a real index it gives 2.58x with a
// worst-case PSNR of 39.4 dB, and a visual check at this setting found no
// legible difference — small interface text and Arabic diacritics survive. See
// docs/compress.md for the decision to accept a second lossy generation at all.
const DefaultQuality = 0.60

// DefaultMinPSNRDB is the floor a re-encoded image must clear before its
// original is deleted.
//
// It is an encoder-sanity gate, not a quality setting: the point is to catch an
// encode that silently produced the wrong picture, and no real frame comes near
// it. The worst of those 309 measured frames was 39.4 dB, so this sits roughly
// 9 dB below anything the corpus actually contains.
const DefaultMinPSNRDB = 30.0

// DefaultMinHistogramSimilarity catches what PSNR alone can pass: a colour-space
// or channel-order mistake scores far below this while a lossy re-encode of the
// same picture does not. The worst measured frame scored 0.989.
const DefaultMinHistogramSimilarity = 0.95

// ImageTranscoder writes destination from source and reports what the file it
// wrote decodes back to.
//
// Implementations must reopen the destination rather than describing the image
// they encoded: an encoder that reports success having written a truncated file
// is the one failure in this feature that destroys data.
type ImageTranscoder interface {
	Transcode(ctx context.Context, source, destination string, quality float64) (macosnative.ImageVerification, error)
	// Inspect reports what an existing image decodes to, for the recovery case
	// in reconcile where no original survives to compare against.
	Inspect(ctx context.Context, path string) (macosnative.ImageVerification, error)
}

// AudioTranscoder writes destination from source losslessly, and decodes any
// audio file to mono 16-bit samples so a result can be compared against its
// source exactly.
type AudioTranscoder interface {
	Transcode(ctx context.Context, source, destination string) error
	Decode(ctx context.Context, path string) ([]int16, int, error)
}

type Options struct {
	// Before compresses only events captured strictly before this instant.
	//
	// A nil Before is never right for a live index: recompression is what you do
	// to history, and touching the last few minutes races the recorder for files
	// it is still writing and the backfill for originals it still needs. The CLI
	// defaults it to 48 hours ago. It is a pointer anyway so a test can mean
	// "everything" without inventing a sentinel time.
	Before *time.Time

	Screens Codec // CodecHEIC or CodecNone
	Audio   Codec // CodecFLAC or CodecNone

	Quality                float64
	MinPSNRDB              float64
	MinHistogramSimilarity float64

	// Images and Sounds are injected so this package's sequencing, verification
	// and accounting are testable with fakes on any machine — the same reason
	// capture.Recorder takes its processors as interfaces.
	Images ImageTranscoder
	Sounds AudioTranscoder

	// MediaDirs are swept for leftovers from an interrupted run. Typically
	// Paths.Screenshots and Paths.Audio.
	MediaDirs []string

	// Vacuum rebuilds the database after the passes, reclaiming the free pages
	// they and every past prune left behind.
	Vacuum       bool
	DatabasePath string

	DryRun bool
	Logger *slog.Logger
}

// quality treats only an unset (zero) value as "use the default". A caller
// asking for a specific quality gets it; validation that the number is usable
// happens once in Validate rather than being silently corrected here.
func (o Options) quality() float64 {
	if o.Quality == 0 {
		return DefaultQuality
	}
	return o.Quality
}

func (o Options) minPSNR() float64 {
	if o.MinPSNRDB == 0 {
		return DefaultMinPSNRDB
	}
	return o.MinPSNRDB
}

func (o Options) minHistogram() float64 {
	if o.MinHistogramSimilarity == 0 {
		return DefaultMinHistogramSimilarity
	}
	return o.MinHistogramSimilarity
}

func (o Options) logger() *slog.Logger {
	if o.Logger == nil {
		return slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	return o.Logger
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// PassResult accounts for one media kind. Every row a pass looked at lands in
// exactly one counter, so a run that reports nothing compressed still says why.
type PassResult struct {
	Files       int64 `json:"files"`
	BytesBefore int64 `json:"bytes_before"`
	BytesAfter  int64 `json:"bytes_after"`
	// AlreadyDone counts rows whose media is already in the target format, which
	// is what makes a rerun a no-op without any stored state.
	AlreadyDone int64 `json:"already_done"`
	// Skipped counts rows in a format this pass does not handle. Not a failure.
	Skipped int64 `json:"skipped"`
	// MissingFiles counts rows whose media is already gone, which an aged-out
	// history is full of.
	MissingFiles int64 `json:"missing_files"`
	EncodeFailed int64 `json:"encode_failed"`
	// VerifyFailed counts encodes that produced a file the checks rejected. The
	// original is kept in every one of those cases.
	VerifyFailed int64 `json:"verify_failed"`
	// Raced counts rows another writer repointed between the read and the
	// update. Their newly written file is removed and the original left alone.
	Raced int64 `json:"raced"`
	// Conflicted counts rows skipped because compressing them would overwrite
	// or orphan media another row depends on. It is an inconsistency in the
	// index rather than a failure of this run; see preflight.go.
	Conflicted int64 `json:"conflicted"`
	// FlushFailed counts results that encoded and verified but could not be
	// flushed durably. Counted apart from EncodeFailed because it means a
	// filesystem problem rather than a broken encoder.
	FlushFailed int64 `json:"flush_failed"`
}

// ReconcileResult accounts for the leftover sweep.
type ReconcileResult struct {
	Removed int64 `json:"removed"`
	Bytes   int64 `json:"bytes"`
	// Recovered counts rows repointed at a surviving compressed file whose
	// original had already been deleted. A non-zero count means an earlier run
	// was interrupted, which is worth saying out loud.
	Recovered int64 `json:"recovered"`
}

// VacuumResult reports a step that is allowed not to happen.
type VacuumResult struct {
	// Status is one of "done", "disabled", "skipped" or "busy".
	Status      string `json:"status"`
	Detail      string `json:"detail,omitempty"`
	BytesBefore int64  `json:"bytes_before,omitempty"`
	BytesAfter  int64  `json:"bytes_after,omitempty"`
}

type Result struct {
	Screens    PassResult      `json:"screens"`
	Audio      PassResult      `json:"audio"`
	Reconciled ReconcileResult `json:"reconciled"`
	Vacuum     VacuumResult    `json:"vacuum"`
}

// Compress re-encodes eligible media, reconciles anything an interrupted run
// left behind, and then vacuums.
//
// The order is load-bearing at both ends. Reconciling before the passes — or
// from a reference set taken before them — would delete every file the passes
// had just written, because those files are not yet named by any row when the
// set is built. Vacuuming last is what reclaims the FTS churn this run itself
// created, and keeps the exclusive lock it needs out of the passes' way.
func Compress(ctx context.Context, s *store.Store, opts Options) (Result, error) {
	var result Result
	if opts.Before == nil {
		return result, errors.New("compress requires a cutoff; recompressing the frame captured a moment ago races the recorder for it")
	}
	if err := opts.validate(); err != nil {
		return result, err
	}
	if !opts.DryRun {
		usableMediaDir := false
		for _, dir := range opts.MediaDirs {
			if strings.TrimSpace(dir) != "" {
				usableMediaDir = true
				break
			}
		}
		if !usableMediaDir {
			return result, errors.New("destructive compression requires at least one usable media directory")
		}
	}
	if opts.Screens == CodecHEIC && opts.Images == nil {
		return result, errors.New("compressing screenshots needs an image transcoder")
	}
	if opts.Audio == CodecFLAC && opts.Sounds == nil {
		return result, errors.New("compressing audio needs an audio transcoder")
	}

	// The cutoff-bounded set is the only set passes and root containment may
	// touch. Ownership is deliberately broader: a recent row can still own an
	// old row's source or deterministic destination.
	candidates, err := s.Expired(ctx, *opts.Before, 0)
	if err != nil {
		return result, err
	}
	allEvents, err := s.AllEvents(ctx)
	if err != nil {
		return result, err
	}
	// Before anything on disk is touched: refuse a run whose candidate media
	// does not belong to this data directory, and identify collisions against
	// every row in the index.
	report, err := runPreflight(ctx, candidates, allEvents, opts)
	if err != nil {
		return result, err
	}

	if opts.Screens == CodecHEIC {
		result.Screens, err = runPass(ctx, s, opts, candidates, store.KindScreen, screenPass{}, report)
		if err != nil {
			return result, err
		}
	}
	if opts.Audio == CodecFLAC {
		result.Audio, err = runPass(ctx, s, opts, candidates, store.KindAudio, audioPass{}, report)
		if err != nil {
			return result, err
		}
	}

	result.Reconciled, err = reconcile(ctx, s, opts)
	if err != nil {
		return result, err
	}

	result.Vacuum = runVacuum(ctx, s, opts)
	return result, nil
}

// pass is the per-kind half of a compression pass: which extensions it handles,
// and how it encodes and verifies one file. Everything else — ordering, the
// compare-and-swap, accounting, the crash-safe replacement — is shared, because
// getting that sequence right once is the point of this package.
type pass interface {
	// classify reports the destination path for an eligible source, or an empty
	// string when the pass does not handle this file.
	classify(source string) (destination string, eligible bool)
	// done reports whether the file is already in the target format.
	done(source string) bool
	// encode writes destination from source and verifies it, returning an error
	// describing what was wrong when the result is not acceptable.
	encode(ctx context.Context, opts Options, source, destination string) error
}

func runPass(ctx context.Context, s *store.Store, opts Options,
	events []store.Event, kind store.Kind, p pass, report preflightReport) (PassResult, error) {
	var result PassResult
	logger := opts.logger()
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			// Interrupting mid-run costs at most the file in flight, and leaves
			// state a rerun reconciles. Everything committed stays committed.
			return result, err
		}
		if event.Kind != kind {
			continue
		}
		if p.done(event.MediaPath) {
			result.AlreadyDone++
			continue
		}
		if reason, conflicted := report.conflicts[event.ID]; conflicted {
			// Compressing this row would take another row's media with it.
			result.Conflicted++
			logger.Warn("skipping media that another event also depends on",
				"event", event.ID, "media", event.MediaPath, "reason", reason)
			continue
		}
		destination, eligible := p.classify(event.MediaPath)
		if !eligible {
			result.Skipped++
			continue
		}
		info, err := os.Stat(event.MediaPath)
		if err != nil {
			result.MissingFiles++
			continue
		}
		if opts.DryRun {
			// Stops before the encode, which is why a dry run cannot report a
			// ratio: measuring one means doing the work.
			result.Files++
			result.BytesBefore += info.Size()
			continue
		}

		if err := p.encode(ctx, opts, event.MediaPath, destination); err != nil {
			if isVerificationError(err) {
				result.VerifyFailed++
				logger.Warn("compressed file failed verification; keeping the original",
					"event", event.ID, "media", event.MediaPath, "error", err)
			} else {
				result.EncodeFailed++
				logger.Warn("could not compress media; keeping the original",
					"event", event.ID, "media", event.MediaPath, "error", err)
			}
			// A partial or rejected encode is never left on disk to be adopted
			// later by reconcile.
			os.Remove(destination)
			continue
		}
		if err := flushDurably(destination); err != nil {
			result.FlushFailed++
			logger.Warn("could not flush compressed media to disk; keeping the original",
				"event", event.ID, "media", destination, "error", err)
			os.Remove(destination)
			continue
		}

		updated, err := s.UpdateMediaPath(ctx, event.ID, event.MediaPath, destination)
		if err != nil {
			os.Remove(destination)
			return result, err
		}
		if updated == 0 {
			// Another writer moved or deleted this row while we were encoding.
			// The file just written is an instant orphan; remove it and leave
			// whatever won alone.
			result.Raced++
			os.Remove(destination)
			continue
		}

		// Past the compare-and-swap nothing below may end the run. The row is
		// committed and names a file that exists, so every remaining failure is
		// in the recoverable half of the ordering — and aborting here would strand
		// the originals of every file this run had already replaced, none of which
		// is reclaimed until somebody runs compress again.
		result.Files++
		result.BytesBefore += info.Size()
		if compressed, err := os.Stat(destination); err == nil {
			result.BytesAfter += compressed.Size()
		} else {
			// Costs this file's contribution to the reported ratio and nothing
			// else; the replacement itself is already committed.
			logger.Warn("could not measure compressed media; it is missing from the reported ratio",
				"event", event.ID, "media", destination, "error", err)
		}

		// Only now is the original redundant.
		if err := os.Remove(event.MediaPath); err != nil && !os.IsNotExist(err) {
			// A stranded original is precisely reconcile's owner-is-present case:
			// the row names the compressed file, the original is an unreferenced
			// sibling, and the next run drops it.
			logger.Warn("could not remove the original after repointing its event; a later run will sweep it",
				"event", event.ID, "media", event.MediaPath, "error", err)
		}
	}
	return result, nil
}

// verificationError marks a result the checks rejected, as opposed to an encode
// that failed outright. They are counted apart because they mean different
// things: one is a broken input or a busy machine, the other is an encoder
// producing something wrong while reporting success.
type verificationError struct{ error }

func isVerificationError(err error) bool {
	var target verificationError
	return errors.As(err, &target)
}

// flushDurably is a package var purely as a test seam. The dir-sync failure path
// is load-bearing — it is the one place a verified file is thrown away — and
// without a seam it would be unreachable outside a filesystem that fails on
// demand.
var flushDurably = durable

// durable flushes a freshly written file and the directory entry naming it.
//
// The directory sync is not ceremony. Without it the new file's *name* may not
// survive a power loss that the committed row update does, which reintroduces
// exactly the row-pointing-at-nothing case this ordering exists to prevent. A
// failure here is therefore fatal for the file rather than logged and ignored —
// claiming a step is load-bearing and then continuing without it would be
// incoherent. On darwin os.File.Sync issues F_FULLFSYNC, so this is a real
// barrier rather than a handoff to the drive cache.
func durable(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func runVacuum(ctx context.Context, s *store.Store, opts Options) VacuumResult {
	switch {
	case !opts.Vacuum:
		return VacuumResult{Status: "disabled"}
	case opts.DryRun:
		return VacuumResult{Status: "skipped", Detail: "dry run"}
	}
	before := fileSize(opts.DatabasePath)
	if err := s.Vacuum(ctx); err != nil {
		if errors.Is(err, store.ErrVacuumBusy) {
			return VacuumResult{Status: "busy", Detail: "the database is in use"}
		}
		// A vacuum that fails for any other reason is still only maintenance:
		// everything the run compressed is already committed, so reporting it
		// as a failed run would misdescribe what happened.
		return VacuumResult{Status: "skipped", Detail: err.Error()}
	}
	return VacuumResult{Status: "done", BytesBefore: before, BytesAfter: fileSize(opts.DatabasePath)}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// sha256Samples hashes decoded audio so a mismatch reports one short line rather
// than the first differing index out of half a million.
func sha256Samples(samples []int16) [32]byte {
	raw := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(sample))
	}
	return sha256.Sum256(raw)
}

// swapExtension is how every destination path is derived, and it is what makes
// reconcile able to pair a leftover with the row that should have named it.
func swapExtension(path, extension string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + extension
}

func lowerExt(path string) string {
	return strings.ToLower(filepath.Ext(path))
}

// foldPath is the key reconcile pairs a leftover with its row under. It folds
// case because the passes do — they match on lowerExt and write a lower-case
// extension — so an exact-match key would leave frame.JPG's leftover ownerless.
func foldPath(path string) string {
	return strings.ToLower(filepath.Clean(path))
}

// validate rejects settings that would silently disable a check rather than
// applying it. A NaN threshold is the case that matters: every comparison
// against it is false, so `--min-psnr NaN` would turn the verification gate off
// while still reporting that images were verified.
func (o Options) validate() error {
	if math.IsNaN(o.Quality) || o.Quality < 0 || o.Quality > 1 {
		return fmt.Errorf("quality must be between 0 and 1, not %v", o.Quality)
	}
	if math.IsNaN(o.MinPSNRDB) || o.MinPSNRDB < 0 {
		return fmt.Errorf("the PSNR floor must be a non-negative number, not %v", o.MinPSNRDB)
	}
	if math.IsNaN(o.MinHistogramSimilarity) || o.MinHistogramSimilarity < 0 || o.MinHistogramSimilarity > 1 {
		return fmt.Errorf("the histogram floor must be between 0 and 1, not %v", o.MinHistogramSimilarity)
	}
	return nil
}
