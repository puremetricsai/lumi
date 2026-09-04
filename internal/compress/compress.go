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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/seal"
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

	// Workers is how many files may be re-encoded at once. Zero means the default
	// below; 1 restores the strictly sequential behaviour.
	Workers int

	// Images and Sounds are injected so this package's sequencing, verification
	// and accounting are testable with fakes on any machine — the same reason
	// capture.Recorder takes its processors as interfaces.
	Images ImageTranscoder
	Sounds AudioTranscoder

	// MediaDirs are swept for leftovers from an interrupted run. Typically
	// Paths.Screenshots and Paths.Audio.
	MediaDirs []string

	// Cipher unseals a source before it is re-encoded and seals the replacement
	// before the row is repointed. Its zero value is a pass-through, and when it
	// is unset this package behaves exactly as it did before encryption existed
	// — including writing the encode straight to its destination rather than
	// through a temporary file.
	Cipher seal.Key

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

// MaxDefaultWorkers caps how many files the default concurrency will re-encode at
// once, however many cores the machine has.
//
// Eight is where the measured curve flattens, not a guess. Over 180 real 3456x2234
// frames on an 18-logical-core machine (6 performance cores), wall time went
// 37.6s at 1 worker → 22.5s at 2 → 14.4s at 4 → 11.4s at 6 → 9.9s at 8, and then
// stopped: 10.1s at 10, 9.4s at 12, 9.3s at 18. So the cap keeps 3.8x of the
// available 4.0x.
//
// What it declines is memory for nothing. Peak RSS over the same runs was 1.37 GB
// at 1 worker, 2.02 GB at 8 and 2.72 GB at 18 — about 93 MB per additional file in
// flight, since verifying one image holds two full RGBA buffers plus both decoded
// images. Past eight that buys ~4% of throughput, and the baseline is already
// large because a run materialises every event twice.
const MaxDefaultWorkers = 8

// workers resolves the concurrency for a run. Zero means unset, matching how
// Quality and the two floors treat it; validate rejects a negative rather than
// silently reading it as unset.
//
// An explicit count is honoured however large, deliberately. MaxDefaultWorkers
// caps what this package picks on the caller's behalf, and clamping a number the
// caller typed would be the same quiet disagreement as silently correcting
// --quality: the flag would report a concurrency the run did not use. The cost of
// an absurd one is memory — roughly 93 MB per file in flight — and it is bounded
// below by the amount of work, since replaceFiles never starts more workers than
// there are files.
func (o Options) workers() int {
	if o.Workers > 0 {
		return o.Workers
	}
	return min(runtime.GOMAXPROCS(0), MaxDefaultWorkers)
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
	// Skipped counts rows the pass left alone by choice rather than by failure:
	// a format it does not handle, or a handled one whose contents the codec
	// cannot represent — audio shorter than the FLAC encoder's minimum block.
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

// work is one file a pass has decided to replace.
type work struct {
	event       store.Event
	destination string
	// size is measured during selection but only *reported* once the row is
	// committed, so a file that fails or loses the compare-and-swap still
	// contributes nothing to the ratio.
	size int64
}

// selectWork applies every eligibility gate exactly once and accounts for each
// row that does not survive them.
//
// It is separate from the encoding so that a pass can say how many *files* it is
// about to replace rather than how many rows it has looked at. That distinction
// is the whole value of the progress line: on a real index the row count is
// dominated by aged-out and already-compressed rows costing microseconds each, so
// "6,024 of 13,211 rows" reads as 45% of a run that has barely started, while
// "108 of 223 files" is the truth. Getting the honest denominator any other way
// means a second copy of these gates, which is the drift this package's CLAUDE.md
// warns about everywhere else.
//
// Nothing that was ordered moves. Every gate here is a read-only check, and the
// per-file sequence — encode, verify, fsync, compare-and-swap, unlink — is
// untouched and still strictly one file at a time. The single behavioural
// difference is that a file something else deletes *during* the run is now
// counted as an encode failure rather than as a missing file: a race that was
// always present, and one that is only ever mislabelled by this, never
// mishandled.
func selectWork(ctx context.Context, events []store.Event, kind store.Kind,
	p pass, report preflightReport, logger *slog.Logger) ([]work, PassResult, error) {
	var result PassResult
	var items []work
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, result, err
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
		items = append(items, work{event: event, destination: destination, size: info.Size()})
	}
	return items, result, nil
}

func runPass(ctx context.Context, s *store.Store, opts Options,
	events []store.Event, kind store.Kind, p pass, report preflightReport) (PassResult, error) {
	logger := opts.logger()
	items, result, err := selectWork(ctx, events, kind, p, report, logger)
	if err != nil {
		return result, err
	}
	if opts.DryRun {
		// Stops before the encode, which is why a dry run cannot report a ratio:
		// measuring one means doing the work. For the same reason Files here counts
		// path-eligible candidates, not what a real run will replace: a real run
		// decodes each one and may reclassify it — a short audio chunk into Skipped
		// (below the FLAC minimum), or any file into a failure counter. Applying
		// those gates would mean the decode a dry run exists to avoid.
		for _, item := range items {
			result.Files++
			result.BytesBefore += item.size
		}
		return result, nil
	}

	tracker := newProgress(logger, kind, int64(len(items)))
	defer tracker.done()
	// result is threaded by pointer, not returned, so that every worker folds its
	// outcome into the same copy of the counters — which is what makes "every file
	// lands in exactly one counter" checkable with several files in flight.
	err = replaceFiles(ctx, s, opts, p, items, &result, tracker, logger)
	return result, err
}

// outcomeKind is what the per-file sequence did with one file. It exists so the
// sequence and the accounting are separate: every file lands in exactly one
// counter, and with several files in flight that promise is only checkable if the
// counters are touched in one place.
type outcomeKind int

const (
	outcomeReplaced outcomeKind = iota
	outcomeEncodeFailed
	outcomeVerifyFailed
	outcomeFlushFailed
	outcomeRaced
	// outcomeSkipped is a file the pass declined to encode after looking at its
	// contents — an input the codec cannot represent rather than one it botched.
	// It reaches record here rather than selectWork because deciding it needs the
	// decoded frame count, which only the per-file sequence has.
	outcomeSkipped
)

type outcome struct {
	kind outcomeKind
	// bytesAfter is set only for outcomeReplaced, and is zero when the committed
	// replacement could not be measured.
	bytesAfter int64
}

// record folds one file's outcome into a pass's counters. BytesBefore is added
// here rather than at selection so a file that failed or lost the compare-and-swap
// still contributes nothing to the reported ratio.
func (r *PassResult) record(produced outcome, size int64) {
	switch produced.kind {
	case outcomeReplaced:
		r.Files++
		r.BytesBefore += size
		r.BytesAfter += produced.bytesAfter
	case outcomeEncodeFailed:
		r.EncodeFailed++
	case outcomeVerifyFailed:
		r.VerifyFailed++
	case outcomeSkipped:
		r.Skipped++
	case outcomeFlushFailed:
		r.FlushFailed++
	case outcomeRaced:
		r.Raced++
	}
}

// replaceOne runs the crash-safe replacement sequence for a single file: encode,
// verify, flush the file and its parent directory, repoint the row with a
// compare-and-swap, and only then unlink the original.
//
// Every step and its ordering is unchanged from when this ran inline; it was
// lifted out so that several files can be in flight without the sequence itself
// being written more than once. A returned error is fatal for the pass — only a
// store failure qualifies — and everything else is an outcome the caller counts.
func replaceOne(ctx context.Context, s *store.Store, opts Options, p pass,
	item work, logger *slog.Logger) (outcome, error) {
	event, destination := item.event, item.destination

	// The encoders take paths and read them with framework calls, so a sealed
	// source needs a plaintext copy for the length of the encode. TempCopy
	// returns the original path when there is nothing to unseal, so this costs
	// nothing when encryption is off.
	source, releaseSource, err := opts.Cipher.TempCopy(event.MediaPath)
	if err != nil {
		logger.Warn("could not decrypt media for compression; keeping the original",
			"event", event.ID, "media", event.MediaPath, "error", err)
		return outcome{kind: outcomeEncodeFailed}, nil
	}
	defer releaseSource()

	// With a key set the encode lands in a temporary file and is sealed into
	// place afterwards, because a pass verifies by reopening what it wrote and
	// the encoders cannot read a sealed file. With no key it writes straight to
	// its destination, exactly as before.
	encoded, releaseEncoded, err := stagedDestination(opts.Cipher, destination)
	if err != nil {
		logger.Warn("could not stage a compressed file; keeping the original",
			"event", event.ID, "media", event.MediaPath, "error", err)
		return outcome{kind: outcomeEncodeFailed}, nil
	}
	defer releaseEncoded()

	if err := p.encode(ctx, opts, source, encoded); err != nil {
		var kind outcomeKind
		switch {
		case isSkipError(err):
			// A declined input, not a failure: the pass looked at the contents and
			// found something this codec cannot represent. Left alone and counted as
			// Skipped, without a per-file line — naming every sub-second clip would
			// be the log spam this decision exists to remove, and the summary's
			// Skipped count is the honest aggregate.
			kind = outcomeSkipped
		case isVerificationError(err):
			kind = outcomeVerifyFailed
			logger.Warn("compressed file failed verification; keeping the original",
				"event", event.ID, "media", event.MediaPath, "error", err)
		default:
			kind = outcomeEncodeFailed
			logger.Warn("could not compress media; keeping the original",
				"event", event.ID, "media", event.MediaPath, "error", err)
		}
		// A partial, rejected or declined encode is never left on disk to be adopted
		// later by reconcile.
		os.Remove(destination)
		return outcome{kind: kind}, nil
	}
	// Seal the verified encode into its final name, then read it back through
	// the key and compare.
	//
	// This second verification is not redundant with the pass's own. A pass
	// verifies by reopening what it wrote — which, with a key set, is the
	// plaintext temporary, not what landed on disk. Without this check the
	// sequence "good encode, bad seal, unlink the original" is unguarded, and it
	// is the one sequence in this package that destroys data.
	if err := placeEncoded(opts.Cipher, encoded, destination); err != nil {
		logger.Warn("could not encrypt the compressed file; keeping the original",
			"event", event.ID, "media", destination, "error", err)
		os.Remove(destination)
		return outcome{kind: outcomeVerifyFailed}, nil
	}
	if err := flushDurably(destination); err != nil {
		logger.Warn("could not flush compressed media to disk; keeping the original",
			"event", event.ID, "media", destination, "error", err)
		os.Remove(destination)
		return outcome{kind: outcomeFlushFailed}, nil
	}

	updated, err := s.UpdateMediaPath(ctx, event.ID, event.MediaPath, destination)
	if err != nil {
		os.Remove(destination)
		return outcome{}, err
	}
	if updated == 0 {
		// Another writer moved or deleted this row while we were encoding. The
		// file just written is an instant orphan; remove it and leave whatever won
		// alone. No sibling worker can be that writer: each item is dispatched
		// once, and preflight has already established that no two of them share a
		// source or a destination.
		os.Remove(destination)
		return outcome{kind: outcomeRaced}, nil
	}

	// Past the compare-and-swap nothing below may end the pass. The row is
	// committed and names a file that exists, so every remaining failure is in the
	// recoverable half of the ordering — and aborting here would strand the
	// originals of every file already replaced, none of which is reclaimed until
	// somebody runs compress again.
	produced := outcome{kind: outcomeReplaced}
	if compressed, err := os.Stat(destination); err == nil {
		produced.bytesAfter = compressed.Size()
	} else {
		// Costs this file's contribution to the reported ratio and nothing else;
		// the replacement itself is already committed.
		logger.Warn("could not measure compressed media; it is missing from the reported ratio",
			"event", event.ID, "media", destination, "error", err)
	}

	// Only now is the original redundant.
	if err := os.Remove(event.MediaPath); err != nil && !os.IsNotExist(err) {
		// A stranded original is precisely reconcile's owner-is-present case: the
		// row names the compressed file, the original is an unreferenced sibling,
		// and the next run drops it.
		logger.Warn("could not remove the original after repointing its event; a later run will sweep it",
			"event", event.ID, "media", event.MediaPath, "error", err)
	}
	return produced, nil
}

// replaceFiles runs the replacement sequence over items with up to
// opts.workers() files in flight.
//
// Concurrency here is safe for a reason that does not extend to a second
// *process*, and the distinction is the whole of it. The hazard the
// single-instance lock in internal/cli exists to prevent is two agents deriving
// the same destination: the loser of the compare-and-swap unlinks a file the
// winner's row already names, which is unrecoverable. Two processes each build
// their own candidate list and can therefore both hold the same row. Workers
// inside one pass cannot: the list is built once, each item is dispatched to
// exactly one worker, and findConflicts has already skipped any row sharing a
// source or a derived destination with another. So the property parallelism needs
// was already established by preflight, and is not a new assumption.
//
// Nothing about a single file's ordering changes — one worker carries one file
// through the whole sequence — and the per-file crash safety that ordering buys
// is untouched, because it never depended on what other files were doing.
//
// What does change is the cost of interruption: cancelling now abandons up to
// workers files rather than one. Each is left exactly as an interrupted
// sequential run leaves its single file, and reconcile repairs all of them by the
// same sibling rule, so this is a larger amount of the same recoverable state.
func replaceFiles(parent context.Context, s *store.Store, opts Options, p pass,
	items []work, result *PassResult, tracker *progress, logger *slog.Logger) error {
	// A derived context so the first store failure stops the other workers.
	// parent.Err() stays the authority on cancellation, which is why it is kept
	// separate rather than read back off this one.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	jobs := make(chan work)
	// One mutex covers the counters, the progress tracker and the first error.
	// Contention is immaterial against ~100ms of encoding per file, and a single
	// lock keeps "every file lands in exactly one counter" true by construction.
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup

	// Never more workers than there is work: a goroutine past len(items) can only
	// ever observe a closed channel, and spawning one for each of --workers 200 on
	// a pass with three files makes the count in the flag read as something the run
	// did. The concurrency an explicit count asks for is untouched.
	for range min(opts.workers(), len(items)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Workers drain rather than return, because a feeder blocked mid-send
			// would otherwise wait on a channel nobody is reading.
			for item := range jobs {
				if ctx.Err() != nil {
					continue
				}
				produced, err := replaceOne(ctx, s, opts, p, item, logger)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					result.record(produced, item.size)
				}
				tracker.step()
				mu.Unlock()
			}
		}()
	}

	for _, item := range items {
		if ctx.Err() != nil {
			// Interrupting mid-pass costs at most the files in flight, and leaves
			// state a rerun reconciles. Everything committed stays committed.
			break
		}
		jobs <- item
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return parent.Err()
}

// progressInterval is how often a running pass says where it has got to. It is
// a package var purely as a test seam: the default has to be long enough that an
// ordinary small run stays as quiet as it was before progress existed, which
// makes the reporting path unreachable in a test that does not move it.
var progressInterval = 5 * time.Second

// progress reports a long pass's advance through the files selectWork gave it.
//
// The passes are silent by construction: every counter is returned at the end,
// and the logger is otherwise reached only by a failure. A run over a full index
// encodes thousands of frames at roughly a tenth of a second each and fsyncs each
// replacement twice, so twenty minutes with nothing on screen was the normal,
// healthy case — and indistinguishable from a hang. That is not a cosmetic
// problem: interrupting costs far more than the file in flight, because screens
// run before audio and a cancelled pass returns before the next one starts, so
// giving up on a slow screen pass silently forfeits the audio pass too — and
// audio is the lossless one, which measures 7x.
//
// It counts files rather than rows because selectWork has already applied the
// gates; see there for why the honest denominator is worth a separate phase.
type progress struct {
	logger *slog.Logger
	noun   string
	files  int64
	// seen counts files the pass has finished with, whatever became of them. It is
	// deliberately not the count of successful replacements: a run where every file
	// fails verification — a --min-psnr set above what the corpus can meet, or an
	// encoder that is simply broken — is exactly the run a user suspects of
	// hanging, and a numerator that stood still there would fail at the one job
	// this has. The outcome breakdown is the summary's business.
	seen  int64
	start time.Time
	last  time.Time
	// reported records whether this pass ever spoke, so done can stay quiet for a
	// pass that finished inside one interval.
	reported bool
}

func newProgress(logger *slog.Logger, kind store.Kind, files int64) *progress {
	now := time.Now()
	return &progress{logger: logger, noun: passNoun(kind), files: files, start: now, last: now}
}

// step records one attempted file and reports where the pass is, at most once per
// progressInterval.
//
// Rate-limited by elapsed time rather than by file count so that the line arrives
// at a readable cadence whatever the media costs to encode — a 5K frame and a
// silent audio chunk differ by two orders of magnitude — and so a pass can never
// bury the warnings the logger is actually there for.
func (p *progress) step() {
	p.seen++
	now := time.Now()
	if now.Sub(p.last) < progressInterval {
		return
	}
	p.last = now
	p.reported = true
	p.logger.Info("compressing "+p.noun, "done", p.seen, "of", p.files,
		"elapsed", now.Sub(p.start).Round(time.Second))
}

// done closes out a pass that had been reporting, including one that a cancelled
// context ended early — where it is the only account of how far the run got
// before the counters were returned.
func (p *progress) done() {
	if !p.reported {
		return
	}
	p.logger.Info("finished compressing "+p.noun, "done", p.seen, "of", p.files,
		"elapsed", time.Since(p.start).Round(time.Second))
}

// passNoun names a kind the way `lumi compress`'s own summary does, so a
// progress line and the final line describe the same thing in the same words.
func passNoun(kind store.Kind) string {
	if kind == store.KindAudio {
		return "audio files"
	}
	return "screenshots"
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

// skipError marks an input a pass has decided not to attempt at all, as opposed
// to one it tried and failed on. It is counted as Skipped, the same bucket as an
// unhandled format, because both mean "left alone, not a failure" — the audio
// pass raises it for a chunk shorter than macosnative.MinFLACFrames, which the
// FLAC encoder cannot represent, so encoding it would write a stub, fail to
// reopen, and be miscounted as an encoder producing something wrong.
type skipError struct{ error }

func isSkipError(err error) bool {
	var target skipError
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
	// Said before the call rather than after it: VACUUM rewrites the whole file
	// under an exclusive lock and is the run's last step, so it is the stretch most
	// easily read as a hang — the passes have stopped reporting and the summary has
	// not been printed yet. One line, because unlike a pass there is no progress to
	// have: SQLite does not report any.
	opts.logger().Info("rebuilding the database to reclaim free pages",
		"database", opts.DatabasePath, "bytes", before)
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
	// Rejected rather than clamped: a negative would fall through workers()' "zero
	// means unset" branch and silently run at the default, which is the same class
	// of quiet disagreement as --quality 0.
	if o.Workers < 0 {
		return fmt.Errorf("workers cannot be negative; zero means the default, not %d", o.Workers)
	}
	return nil
}
