package compress

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/store"
)

// fakeImages writes a marker file and reports whatever verification the test
// asked for, so every branch of the replacement sequence is reachable without an
// encoder, a Mac, or a real image.
type fakeImages struct {
	verification macosnative.ImageVerification
	err          error
	// writeNothing simulates an encoder that failed after creating its output.
	writeNothing bool
	// observe runs before the encode returns, which is how a test checks what
	// the store still said while the encoder was working.
	observe func(source, destination string)
	calls   int
}

func (f *fakeImages) Transcode(_ context.Context, source, destination string, _ float64) (macosnative.ImageVerification, error) {
	f.calls++
	if f.observe != nil {
		f.observe(source, destination)
	}
	if !f.writeNothing {
		if err := os.WriteFile(destination, []byte("compressed:"+filepath.Base(source)), 0o600); err != nil {
			return macosnative.ImageVerification{}, err
		}
	}
	if f.err != nil {
		return macosnative.ImageVerification{}, f.err
	}
	return f.verification, nil
}

func (f *fakeImages) Inspect(_ context.Context, path string) (macosnative.ImageVerification, error) {
	if _, err := os.Stat(path); err != nil {
		return macosnative.ImageVerification{}, err
	}
	return macosnative.ImageVerification{Width: 100, Height: 100, SourceWidth: 100, SourceHeight: 100,
		PSNRDB: 99, HistogramSimilarity: 1}, nil
}

func goodImage() macosnative.ImageVerification {
	return macosnative.ImageVerification{
		Width: 1280, Height: 800, SourceWidth: 1280, SourceHeight: 800,
		PSNRDB: 42, HistogramSimilarity: 0.998,
	}
}

// fakeAudio models a lossless codec: the "encoded" file records which samples it
// holds, and decoding reads them back. corrupt makes it lossy instead.
type fakeAudio struct {
	samples    []int16
	sampleRate int
	corrupt    bool
	err        error
	decodeErr  map[string]error
}

func (f *fakeAudio) Transcode(_ context.Context, source, destination string) error {
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(destination, []byte("flac:"+filepath.Base(source)), 0o600)
}

func (f *fakeAudio) Decode(_ context.Context, path string) ([]int16, int, error) {
	if err := f.decodeErr[filepath.Base(path)]; err != nil {
		return nil, 0, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, 0, err
	}
	rate := f.sampleRate
	if rate == 0 {
		rate = 16000
	}
	if f.corrupt && lowerExt(path) == extFLAC {
		altered := append([]int16(nil), f.samples...)
		if len(altered) > 0 {
			altered[0]++
		}
		return altered, rate, nil
	}
	return f.samples, rate, nil
}

type harness struct {
	t      *testing.T
	store  *store.Store
	dir    string
	images *fakeImages
	audio  *fakeAudio
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(root, "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// Media lives beside the database, never in the same directory, mirroring
	// production where Paths.Screenshots and Paths.Audio are their own trees.
	media := filepath.Join(root, "media")
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatal(err)
	}
	return &harness{
		t: t, store: s, dir: media,
		images: &fakeImages{verification: goodImage()},
		audio:  &fakeAudio{samples: []int16{1, -2, 3, -4, 0, 0, 7}},
	}
}

// seed writes a media file and indexes an event pointing at it.
func (h *harness) seed(kind store.Kind, name string, age time.Duration) store.Event {
	h.t.Helper()
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, []byte(strings.Repeat("media", 200)), 0o600); err != nil {
		h.t.Fatal(err)
	}
	event := store.Event{
		Kind: kind, CapturedAt: time.Now().UTC().Add(-age), Text: name, MediaPath: path,
	}
	if kind == store.KindAudio {
		event.AudioSource = "system"
	}
	if err := h.store.Insert(context.Background(), &event); err != nil {
		h.t.Fatal(err)
	}
	return event
}

// orphan writes a media file that no event references.
func (h *harness) orphan(name string) string {
	h.t.Helper()
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, []byte("orphan"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (h *harness) options() Options {
	return Options{
		Before:    ptr(time.Now().UTC()),
		Screens:   CodecHEIC,
		Audio:     CodecFLAC,
		Images:    h.images,
		Sounds:    h.audio,
		MediaDirs: []string{h.dir},
	}
}

func (h *harness) run(opts Options) Result {
	h.t.Helper()
	result, err := Compress(context.Background(), h.store, opts)
	if err != nil {
		h.t.Fatal(err)
	}
	return result
}

func (h *harness) mediaPath(id int64) string {
	h.t.Helper()
	event, err := h.store.EventByID(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return event.MediaPath
}

func ptr[T any](value T) *T { return &value }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestCompressReplacesScreenshotsAndAudio(t *testing.T) {
	h := newHarness(t)
	screen := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	audio := h.seed(store.KindAudio, "chunk.wav", time.Hour)

	result := h.run(h.options())

	if result.Screens.Files != 1 || result.Audio.Files != 1 {
		t.Fatalf("compressed %d screenshots and %d audio files, want 1 of each",
			result.Screens.Files, result.Audio.Files)
	}
	if got := h.mediaPath(screen.ID); got != filepath.Join(h.dir, "frame.heic") {
		t.Errorf("screen row names %q", got)
	}
	if got := h.mediaPath(audio.ID); got != filepath.Join(h.dir, "chunk.flac") {
		t.Errorf("audio row names %q", got)
	}
	if exists(screen.MediaPath) || exists(audio.MediaPath) {
		t.Error("an original survived a successful replacement")
	}
	if !exists(filepath.Join(h.dir, "frame.heic")) || !exists(filepath.Join(h.dir, "chunk.flac")) {
		t.Error("a compressed file is missing")
	}
	if result.Screens.BytesBefore <= result.Screens.BytesAfter {
		t.Errorf("reported %d bytes before and %d after", result.Screens.BytesBefore, result.Screens.BytesAfter)
	}
}

// The whole point of the inverted ordering: the row must still name the original
// while the encoder is running, so a crash at any moment leaves a recoverable
// state rather than a row pointing at a file that does not exist yet.
func TestCompressRepointsTheRowOnlyAfterTheFileIsWritten(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	h.images.observe = func(source, _ string) {
		if got := h.mediaPath(event.ID); got != source {
			t.Errorf("row named %q while the encoder was working on %q", got, source)
		}
		if !exists(source) {
			t.Error("the original was deleted before the replacement was written")
		}
	}
	h.run(h.options())
	if h.images.calls != 1 {
		t.Fatalf("encoder ran %d times", h.images.calls)
	}
}

func TestCompressKeepsTheOriginalWhenVerificationFails(t *testing.T) {
	for _, tc := range []struct {
		name         string
		verification macosnative.ImageVerification
	}{
		{"wrong size", macosnative.ImageVerification{
			Width: 640, Height: 400, SourceWidth: 1280, SourceHeight: 800, PSNRDB: 45, HistogramSimilarity: 1}},
		{"poor PSNR", macosnative.ImageVerification{
			Width: 1280, Height: 800, SourceWidth: 1280, SourceHeight: 800, PSNRDB: 12, HistogramSimilarity: 1}},
		{"histogram drift", macosnative.ImageVerification{
			Width: 1280, Height: 800, SourceWidth: 1280, SourceHeight: 800, PSNRDB: 45, HistogramSimilarity: 0.4}},
		{"no pixels", macosnative.ImageVerification{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			event := h.seed(store.KindScreen, "frame.jpg", time.Hour)
			h.images.verification = tc.verification

			result := h.run(h.options())

			if result.Screens.VerifyFailed != 1 {
				t.Errorf("counted %d verification failures, want 1", result.Screens.VerifyFailed)
			}
			if result.Screens.Files != 0 {
				t.Errorf("counted %d compressed files after a rejected encode", result.Screens.Files)
			}
			if !exists(event.MediaPath) {
				t.Error("the original was deleted despite a failed verification")
			}
			if h.mediaPath(event.ID) != event.MediaPath {
				t.Error("the row was repointed despite a failed verification")
			}
			if exists(filepath.Join(h.dir, "frame.heic")) {
				t.Error("the rejected file was left on disk, where reconcile could later adopt it")
			}
		})
	}
}

func TestCompressRejectsAudioThatDoesNotRoundTrip(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindAudio, "chunk.wav", time.Hour)
	h.audio.corrupt = true

	result := h.run(h.options())

	if result.Audio.VerifyFailed != 1 {
		t.Errorf("counted %d verification failures, want 1", result.Audio.VerifyFailed)
	}
	if !exists(event.MediaPath) {
		t.Error("a WAV was deleted after a lossy round trip")
	}
	if h.mediaPath(event.ID) != event.MediaPath {
		t.Error("the row was repointed to audio that does not match")
	}
}

func TestCompressKeepsTheOriginalWhenEncodingFails(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	h.images.err = errors.New("encoder exploded")

	result := h.run(h.options())

	if result.Screens.EncodeFailed != 1 || result.Screens.VerifyFailed != 0 {
		t.Errorf("counted %d encode and %d verify failures, want 1 and 0",
			result.Screens.EncodeFailed, result.Screens.VerifyFailed)
	}
	if !exists(event.MediaPath) {
		t.Error("the original was deleted after a failed encode")
	}
	if exists(filepath.Join(h.dir, "frame.heic")) {
		t.Error("a partial encode was left on disk")
	}
}

// One bad file must not cost the rest of the run.
func TestCompressContinuesPastAFailure(t *testing.T) {
	h := newHarness(t)
	h.seed(store.KindScreen, "bad.jpg", 3*time.Hour)
	h.seed(store.KindScreen, "good.jpg", 2*time.Hour)
	h.images.observe = func(source, _ string) {
		if filepath.Base(source) == "bad.jpg" {
			h.images.err = errors.New("encoder exploded")
		} else {
			h.images.err = nil
		}
	}

	result := h.run(h.options())

	if result.Screens.EncodeFailed != 1 || result.Screens.Files != 1 {
		t.Errorf("counted %d failures and %d successes, want 1 and 1",
			result.Screens.EncodeFailed, result.Screens.Files)
	}
	if !exists(filepath.Join(h.dir, "good.heic")) {
		t.Error("the healthy file was not compressed")
	}
}

func TestCompressIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.seed(store.KindScreen, "frame.jpg", time.Hour)
	h.seed(store.KindAudio, "chunk.wav", time.Hour)

	h.run(h.options())
	second := h.run(h.options())

	if second.Screens.Files != 0 || second.Audio.Files != 0 {
		t.Errorf("a second run compressed %d screenshots and %d audio files",
			second.Screens.Files, second.Audio.Files)
	}
	if second.Screens.AlreadyDone != 1 || second.Audio.AlreadyDone != 1 {
		t.Errorf("a second run counted %d and %d already done, want 1 and 1",
			second.Screens.AlreadyDone, second.Audio.AlreadyDone)
	}
	if second.Reconciled.Removed != 0 {
		t.Errorf("a second run removed %d files it had itself written", second.Reconciled.Removed)
	}
}

func TestCompressMatchesExtensionsCaseInsensitively(t *testing.T) {
	h := newHarness(t)
	h.seed(store.KindScreen, "frame.JPG", time.Hour)
	h.seed(store.KindScreen, "already.HEIC", time.Hour)

	result := h.run(h.options())

	if result.Screens.Files != 1 {
		t.Errorf("compressed %d files, want 1 — an upper-case .JPG is still a JPEG", result.Screens.Files)
	}
	if result.Screens.AlreadyDone != 1 {
		t.Errorf("counted %d already done, want 1 — an upper-case .HEIC is already compressed",
			result.Screens.AlreadyDone)
	}
}

// Eligibility is a whitelist, so a format this build does not know is left alone
// rather than fed to the wrong encoder.
func TestCompressSkipsFormatsItDoesNotHandle(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frames.mov", time.Hour)

	result := h.run(h.options())

	if result.Screens.Skipped != 1 || result.Screens.Files != 0 {
		t.Errorf("skipped %d and compressed %d, want 1 and 0", result.Screens.Skipped, result.Screens.Files)
	}
	if !exists(event.MediaPath) {
		t.Error("an unrecognised file was deleted")
	}
}

func TestCompressCountsMediaThatIsAlreadyGone(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	if err := os.Remove(event.MediaPath); err != nil {
		t.Fatal(err)
	}

	result := h.run(h.options())

	if result.Screens.MissingFiles != 1 {
		t.Errorf("counted %d missing files, want 1", result.Screens.MissingFiles)
	}
}

func TestCompressLeavesRecentCaptureAlone(t *testing.T) {
	h := newHarness(t)
	recent := h.seed(store.KindScreen, "recent.jpg", time.Minute)
	old := h.seed(store.KindScreen, "old.jpg", 72*time.Hour)
	opts := h.options()
	opts.Before = ptr(time.Now().UTC().Add(-48 * time.Hour))

	result := h.run(opts)

	if result.Screens.Files != 1 {
		t.Fatalf("compressed %d files, want only the old one", result.Screens.Files)
	}
	if !exists(recent.MediaPath) {
		t.Error("a frame inside the cutoff was compressed")
	}
	if exists(old.MediaPath) {
		t.Error("the frame outside the cutoff was not compressed")
	}
}

func TestCompressRequiresACutoff(t *testing.T) {
	h := newHarness(t)
	opts := h.options()
	opts.Before = nil
	if _, err := Compress(context.Background(), h.store, opts); err == nil {
		t.Error("ran without a cutoff")
	}
}

func TestCompressRequiresUsableMediaDirectoriesBeforeDestructiveWork(t *testing.T) {
	for _, mediaDirs := range [][]string{nil, {"", ""}} {
		t.Run(fmt.Sprintf("%q", mediaDirs), func(t *testing.T) {
			h := newHarness(t)
			event := h.seed(store.KindScreen, "frame.jpg", time.Hour)
			opts := h.options()
			opts.MediaDirs = mediaDirs

			_, err := Compress(context.Background(), h.store, opts)
			if err == nil {
				t.Fatal("destructive compression ran without a usable media directory")
			}
			if h.images.calls != 0 {
				t.Error("the media directory refusal came after the encoder ran")
			}
			if !exists(event.MediaPath) || h.mediaPath(event.ID) != event.MediaPath {
				t.Error("the refused run changed indexed media")
			}
		})
	}
}

func TestCompressDryRunChangesNothing(t *testing.T) {
	h := newHarness(t)
	screen := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	audio := h.seed(store.KindAudio, "chunk.wav", time.Hour)
	leftover := h.orphan("stale.heic")
	if err := os.WriteFile(filepath.Join(h.dir, "stale.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := h.options()
	opts.DryRun = true
	opts.Vacuum = true

	result := h.run(opts)

	if result.Screens.Files != 1 || result.Audio.Files != 1 {
		t.Errorf("a dry run reported %d screenshots and %d audio files",
			result.Screens.Files, result.Audio.Files)
	}
	if result.Screens.BytesAfter != 0 {
		t.Error("a dry run reported compressed bytes it never measured")
	}
	if result.Vacuum.Status != "skipped" {
		t.Errorf("a dry run reported vacuum %q, want skipped", result.Vacuum.Status)
	}
	if h.images.calls != 0 {
		t.Error("a dry run ran the encoder")
	}
	if !exists(screen.MediaPath) || !exists(audio.MediaPath) || !exists(leftover) {
		t.Error("a dry run deleted something")
	}
	if h.mediaPath(screen.ID) != screen.MediaPath {
		t.Error("a dry run repointed a row")
	}
}

func TestCompressSkipsARowAnotherWriterMoved(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	// Move the row out from under the pass while the encoder is working, which
	// is exactly what a concurrent writer would do.
	h.images.observe = func(_, _ string) {
		if _, err := h.store.UpdateMediaPath(context.Background(), event.ID,
			event.MediaPath, filepath.Join(h.dir, "elsewhere.jpg")); err != nil {
			t.Fatal(err)
		}
	}

	result := h.run(h.options())

	if result.Screens.Raced != 1 || result.Screens.Files != 0 {
		t.Errorf("counted %d raced and %d compressed, want 1 and 0",
			result.Screens.Raced, result.Screens.Files)
	}
	if exists(filepath.Join(h.dir, "frame.heic")) {
		t.Error("the losing writer left its file behind")
	}
	if got := h.mediaPath(event.ID); got != filepath.Join(h.dir, "elsewhere.jpg") {
		t.Errorf("the winner's path was clobbered; row names %q", got)
	}
	if !exists(event.MediaPath) {
		t.Error("the losing writer deleted the original")
	}
}

func TestCompressPassesRunOnlyWhenEnabled(t *testing.T) {
	h := newHarness(t)
	screen := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	audio := h.seed(store.KindAudio, "chunk.wav", time.Hour)
	opts := h.options()
	opts.Screens = CodecNone

	result := h.run(opts)

	if result.Screens.Files != 0 {
		t.Error("screenshots were compressed with --screens none")
	}
	if !exists(screen.MediaPath) {
		t.Error("a screenshot was replaced with --screens none")
	}
	if result.Audio.Files != 1 {
		t.Error("audio was not compressed")
	}
	if exists(audio.MediaPath) {
		t.Error("the WAV survived a successful replacement")
	}
}

func TestCompressVacuumsLast(t *testing.T) {
	h := newHarness(t)
	h.seed(store.KindScreen, "frame.jpg", time.Hour)
	opts := h.options()
	opts.Vacuum = true
	opts.DatabasePath = filepath.Join(filepath.Dir(h.dir), "lumi.db")

	result := h.run(opts)

	if result.Vacuum.Status != "done" {
		t.Fatalf("vacuum reported %q: %s", result.Vacuum.Status, result.Vacuum.Detail)
	}
	if result.Vacuum.BytesBefore == 0 || result.Vacuum.BytesAfter == 0 {
		t.Errorf("vacuum reported %d bytes before and %d after",
			result.Vacuum.BytesBefore, result.Vacuum.BytesAfter)
	}
	// The rows it just repointed have to survive the rebuild.
	if got := h.mediaPath(1); got != filepath.Join(h.dir, "frame.heic") {
		t.Errorf("after vacuuming, event 1 names %q", got)
	}
}

func TestCompressReportsVacuumDisabled(t *testing.T) {
	h := newHarness(t)
	result := h.run(h.options())
	if result.Vacuum.Status != "disabled" {
		t.Errorf("vacuum reported %q, want disabled", result.Vacuum.Status)
	}
}

func TestCompressNeedsATranscoderForEachEnabledPass(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		opts func(Options) Options
	}{
		{"no image transcoder", func(o Options) Options { o.Images = nil; return o }},
		{"no audio transcoder", func(o Options) Options { o.Sounds = nil; return o }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compress(context.Background(), h.store, tc.opts(h.options())); err == nil {
				t.Error("ran an enabled pass with no transcoder")
			}
		})
	}
}

func TestCompressStopsOnACancelledContext(t *testing.T) {
	h := newHarness(t)
	h.seed(store.KindScreen, "frame.jpg", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Compress(ctx, h.store, h.options()); err == nil {
		t.Error("ran to completion on a cancelled context")
	}
}

func TestAcceptImageAllowsARealisticReEncode(t *testing.T) {
	// The numbers a real frame produced at the shipped quality.
	verification := macosnative.ImageVerification{
		Width: 3456, Height: 2234, SourceWidth: 3456, SourceHeight: 2234,
		PSNRDB: 39.4, HistogramSimilarity: 0.989,
	}
	if err := acceptImage(verification, Options{}); err != nil {
		t.Errorf("the worst frame measured on a real index was rejected: %v", err)
	}
}

func TestCompressReportsAStoreFailure(t *testing.T) {
	h := newHarness(t)
	h.seed(store.KindScreen, "frame.jpg", time.Hour)
	h.store.Close()
	if _, err := Compress(context.Background(), h.store, h.options()); err == nil {
		t.Error("a closed store ran to completion")
	}
}

func TestSwapExtensionRoundTrips(t *testing.T) {
	for _, tc := range []struct{ in, extension, want string }{
		{"/media/frame.jpg", extHEIC, "/media/frame.heic"},
		{"/media/frame.jpeg", extHEIC, "/media/frame.heic"},
		{"/media/2026-08-02T04.37.41Z-275416-display-1.jpg", extHEIC,
			"/media/2026-08-02T04.37.41Z-275416-display-1.heic"},
		{"/media/chunk.wav", extFLAC, "/media/chunk.flac"},
	} {
		if got := swapExtension(tc.in, tc.extension); got != tc.want {
			t.Errorf("swapExtension(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestVerificationErrorIsDistinguishable(t *testing.T) {
	wrapped := fmt.Errorf("context: %w", verificationError{errors.New("too lossy")})
	if !isVerificationError(wrapped) {
		t.Error("a wrapped verification failure was counted as an encode failure")
	}
	if isVerificationError(errors.New("encoder exploded")) {
		t.Error("a plain error was counted as a verification failure")
	}
}

// The directory sync is load-bearing: without it a new file's name may not
// survive a power loss the committed row update does, which reintroduces the
// case the ordering exists to prevent. So its failure has to throw the verified
// file away rather than proceed, and that is only reachable through a seam.
func TestCompressDiscardsAVerifiedFileItCannotFlush(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	original := flushDurably
	flushDurably = func(string) error { return errors.New("no space left on device") }
	t.Cleanup(func() { flushDurably = original })

	result := h.run(h.options())

	if result.Screens.FlushFailed != 1 {
		t.Errorf("counted %d flush failures, want 1", result.Screens.FlushFailed)
	}
	if result.Screens.EncodeFailed != 0 {
		t.Error("a flush failure was reported as a broken encoder")
	}
	if result.Screens.Files != 0 {
		t.Error("a file that could not be flushed was counted as compressed")
	}
	if !exists(event.MediaPath) {
		t.Fatal("the original was deleted after a failed flush")
	}
	if h.mediaPath(event.ID) != event.MediaPath {
		t.Error("the row was repointed at a file that was never flushed")
	}
	if exists(filepath.Join(h.dir, "frame.heic")) {
		t.Error("the unflushed file was left where reconcile could later adopt it")
	}
}

// The incident this guard exists for: media_path is absolute, so pointing
// --data-dir at a copied index gives you the copy's database and the original's
// files — and compress, unlike every other command, deletes what it reads.
func TestCompressRefusesMediaOutsideItsDataDirectory(t *testing.T) {
	h := newHarness(t)
	elsewhere := t.TempDir()
	stray := filepath.Join(elsewhere, "frame.jpg")
	if err := os.WriteFile(stray, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	event := store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC().Add(-time.Hour),
		Text: "stray", MediaPath: stray,
	}
	if err := h.store.Insert(context.Background(), &event); err != nil {
		t.Fatal(err)
	}

	_, err := Compress(context.Background(), h.store, h.options())
	if err == nil {
		t.Fatal("compressed media belonging to another data directory")
	}
	if !strings.Contains(err.Error(), "outside this data directory") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	if !exists(stray) {
		t.Fatal("the refused run still deleted the file")
	}
	if h.images.calls != 0 {
		t.Error("the refusal came after the encoder had already run")
	}
}

func TestCompressResolvesSymlinksBeforeCheckingMediaRoots(t *testing.T) {
	h := newHarness(t)
	outside := t.TempDir()
	source := filepath.Join(outside, "frame.jpg")
	if err := os.WriteFile(source, []byte("outside media"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h.dir, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	event := store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC().Add(-time.Hour),
		Text: "symlink escape", MediaPath: filepath.Join(link, "frame.jpg"),
	}
	if err := h.store.Insert(context.Background(), &event); err != nil {
		t.Fatal(err)
	}

	_, err := Compress(context.Background(), h.store, h.options())
	if err == nil {
		t.Fatal("compressed through a symlink escaping the media directory")
	}
	if !strings.Contains(err.Error(), "outside this data directory") {
		t.Errorf("the refusal does not identify the root escape: %v", err)
	}
	if got, readErr := os.ReadFile(source); readErr != nil || string(got) != "outside media" {
		t.Fatalf("the escaped source changed: bytes=%q err=%v", got, readErr)
	}
	if exists(filepath.Join(outside, "frame.heic")) || h.images.calls != 0 {
		t.Error("the symlink refusal came after a destination was written outside the root")
	}
}

func TestCompressResolvesANonexistentDestinationParentBeforeCheckingRoots(t *testing.T) {
	h := newHarness(t)
	outside := t.TempDir()
	actualSource := filepath.Join(h.dir, "actual.jpg")
	if err := os.WriteFile(actualSource, []byte("inside media"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The indexed source travels through an outside parent but links back to an
	// inside file. Source-only containment therefore passes; the nonexistent
	// destination's resolved parent is what exposes the escape.
	if err := os.Symlink(actualSource, filepath.Join(outside, "frame.jpg")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h.dir, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	event := store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC().Add(-time.Hour),
		Text: "destination parent escape", MediaPath: filepath.Join(link, "frame.jpg"),
	}
	if err := h.store.Insert(context.Background(), &event); err != nil {
		t.Fatal(err)
	}

	_, err := Compress(context.Background(), h.store, h.options())
	if err == nil {
		t.Fatal("accepted a nonexistent destination beneath a symlinked outside parent")
	}
	if !strings.Contains(err.Error(), "outside this data directory") {
		t.Errorf("the refusal does not identify the destination escape: %v", err)
	}
	if got, readErr := os.ReadFile(actualSource); readErr != nil || string(got) != "inside media" {
		t.Fatalf("the inside source changed: bytes=%q err=%v", got, readErr)
	}
	if exists(filepath.Join(outside, "frame.heic")) || h.images.calls != 0 {
		t.Error("the destination parent refusal came after encoding began")
	}
}

func TestRecentMediaOutsideTheRootsDoesNotBlockEligibleHistory(t *testing.T) {
	h := newHarness(t)
	old := h.seed(store.KindScreen, "old.jpg", 72*time.Hour)
	outside := filepath.Join(t.TempDir(), "recent.jpg")
	if err := os.WriteFile(outside, []byte("recent"), 0o600); err != nil {
		t.Fatal(err)
	}
	recent := store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC().Add(-time.Minute),
		Text: "recent foreign path", MediaPath: outside,
	}
	if err := h.store.Insert(context.Background(), &recent); err != nil {
		t.Fatal(err)
	}
	opts := h.options()
	opts.Before = ptr(time.Now().UTC().Add(-48 * time.Hour))

	result := h.run(opts)

	if result.Screens.Files != 1 {
		t.Fatalf("compressed %d screenshots, want the eligible old row", result.Screens.Files)
	}
	if exists(old.MediaPath) || !exists(filepath.Join(h.dir, "old.heic")) {
		t.Error("the eligible old row was not replaced")
	}
	if got := h.mediaPath(recent.ID); got != outside {
		t.Errorf("recent row moved to %q", got)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "recent" {
		t.Errorf("recent foreign media changed: bytes=%q err=%v", got, err)
	}
}

// media_path carries no uniqueness constraint, so the one-to-one ownership the
// replacement sequence assumes has to be checked rather than trusted.
func TestCompressSkipsRowsWhoseMediaAnotherRowDependsOn(t *testing.T) {
	t.Run("two rows naming one file", func(t *testing.T) {
		h := newHarness(t)
		first := h.seed(store.KindScreen, "frame.jpg", time.Hour)
		second := store.Event{
			Kind: store.KindScreen, CapturedAt: time.Now().UTC().Add(-time.Hour),
			Text: "duplicate", MediaPath: first.MediaPath,
		}
		if err := h.store.Insert(context.Background(), &second); err != nil {
			t.Fatal(err)
		}

		result := h.run(h.options())

		if result.Screens.Conflicted != 2 || result.Screens.Files != 0 {
			t.Errorf("counted %d conflicted and %d compressed, want 2 and 0",
				result.Screens.Conflicted, result.Screens.Files)
		}
		if !exists(first.MediaPath) {
			t.Error("the shared file was deleted, taking the second row's media with it")
		}
	})

	t.Run("destination already held by another row", func(t *testing.T) {
		h := newHarness(t)
		source := h.seed(store.KindScreen, "frame.jpg", time.Hour)
		occupier := h.seed(store.KindScreen, "frame.heic", time.Hour)

		result := h.run(h.options())

		if result.Screens.Conflicted != 1 {
			t.Errorf("counted %d conflicted, want 1", result.Screens.Conflicted)
		}
		if !exists(occupier.MediaPath) {
			t.Fatal("compressing one row overwrote another row's media")
		}
		if got, err := os.ReadFile(occupier.MediaPath); err != nil || strings.HasPrefix(string(got), "compressed:") {
			t.Error("the other row's media was replaced by an encode")
		}
		if !exists(source.MediaPath) {
			t.Error("the conflicted source was deleted")
		}
	})
}

func TestCompressUsesAllEventsForDestinationOwnership(t *testing.T) {
	for _, tc := range []struct {
		name             string
		kind             store.Kind
		oldName, newName string
		result           func(Result) PassResult
	}{
		{"old JPG and recent HEIC", store.KindScreen, "frame.jpg", "frame.heic", func(r Result) PassResult { return r.Screens }},
		{"old WAV and recent FLAC", store.KindAudio, "chunk.wav", "chunk.flac", func(r Result) PassResult { return r.Audio }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			old := h.seed(tc.kind, tc.oldName, 72*time.Hour)
			recent := h.seed(tc.kind, tc.newName, time.Minute)
			before, err := os.ReadFile(recent.MediaPath)
			if err != nil {
				t.Fatal(err)
			}
			opts := h.options()
			opts.Before = ptr(time.Now().UTC().Add(-48 * time.Hour))

			pass := tc.result(h.run(opts))

			if pass.Conflicted != 1 || pass.Files != 0 {
				t.Errorf("counted %d conflicts and %d files, want 1 and 0", pass.Conflicted, pass.Files)
			}
			if got := h.mediaPath(old.ID); got != old.MediaPath {
				t.Errorf("old row moved to %q", got)
			}
			if got := h.mediaPath(recent.ID); got != recent.MediaPath {
				t.Errorf("recent row moved to %q", got)
			}
			after, err := os.ReadFile(recent.MediaPath)
			if err != nil || string(after) != string(before) {
				t.Errorf("recent destination changed: before=%q after=%q err=%v", before, after, err)
			}
			if !exists(old.MediaPath) {
				t.Error("conflicted old source was deleted")
			}
		})
	}
}

func TestCompressUsesAllEventsForSharedSourceOwnership(t *testing.T) {
	h := newHarness(t)
	old := h.seed(store.KindScreen, "shared.jpg", 72*time.Hour)
	recent := store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC().Add(-time.Minute),
		Text: "recent shared owner", MediaPath: old.MediaPath,
	}
	if err := h.store.Insert(context.Background(), &recent); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(old.MediaPath)
	if err != nil {
		t.Fatal(err)
	}
	opts := h.options()
	opts.Before = ptr(time.Now().UTC().Add(-48 * time.Hour))

	result := h.run(opts)

	if result.Screens.Conflicted != 1 || result.Screens.Files != 0 {
		t.Errorf("counted %d conflicts and %d files, want 1 and 0",
			result.Screens.Conflicted, result.Screens.Files)
	}
	for _, event := range []store.Event{old, recent} {
		if got := h.mediaPath(event.ID); got != old.MediaPath {
			t.Errorf("row %d moved to %q", event.ID, got)
		}
	}
	after, err := os.ReadFile(old.MediaPath)
	if err != nil || string(after) != string(before) {
		t.Errorf("shared source changed: before=%q after=%q err=%v", before, after, err)
	}
}

func TestCompressDetectsFilesystemEquivalentCaseVariants(t *testing.T) {
	t.Run("shared source", func(t *testing.T) {
		h := newHarness(t)
		old := h.seed(store.KindScreen, "frame.jpg", 72*time.Hour)
		variant := filepath.Join(h.dir, "FRAME.JPG")
		oldInfo, oldErr := os.Stat(old.MediaPath)
		variantInfo, variantErr := os.Stat(variant)
		if oldErr != nil || variantErr != nil || !os.SameFile(oldInfo, variantInfo) {
			t.Skip("test volume is case-sensitive")
		}
		recent := store.Event{
			Kind: store.KindScreen, CapturedAt: time.Now().UTC().Add(-time.Minute),
			Text: "case-variant owner", MediaPath: variant,
		}
		if err := h.store.Insert(context.Background(), &recent); err != nil {
			t.Fatal(err)
		}
		opts := h.options()
		opts.Before = ptr(time.Now().UTC().Add(-48 * time.Hour))

		result := h.run(opts)

		if result.Screens.Conflicted != 1 || result.Screens.Files != 0 {
			t.Errorf("counted %d conflicts and %d files, want 1 and 0",
				result.Screens.Conflicted, result.Screens.Files)
		}
		if !exists(old.MediaPath) || h.mediaPath(recent.ID) != variant {
			t.Error("compressing a case variant damaged the shared source or its recent row")
		}
	})

	t.Run("destination owner", func(t *testing.T) {
		h := newHarness(t)
		old := h.seed(store.KindScreen, "frame.jpg", 72*time.Hour)
		recent := h.seed(store.KindScreen, "FRAME.HEIC", time.Minute)
		candidate := filepath.Join(h.dir, "frame.heic")
		candidateInfo, candidateErr := os.Stat(candidate)
		ownerInfo, ownerErr := os.Stat(recent.MediaPath)
		if candidateErr != nil || ownerErr != nil || !os.SameFile(candidateInfo, ownerInfo) {
			t.Skip("test volume is case-sensitive")
		}
		before, err := os.ReadFile(recent.MediaPath)
		if err != nil {
			t.Fatal(err)
		}
		opts := h.options()
		opts.Before = ptr(time.Now().UTC().Add(-48 * time.Hour))

		result := h.run(opts)

		if result.Screens.Conflicted != 1 || result.Screens.Files != 0 {
			t.Errorf("counted %d conflicts and %d files, want 1 and 0",
				result.Screens.Conflicted, result.Screens.Files)
		}
		after, err := os.ReadFile(recent.MediaPath)
		if err != nil || string(after) != string(before) {
			t.Errorf("case-variant destination owner changed: before=%q after=%q err=%v", before, after, err)
		}
		if !exists(old.MediaPath) || h.mediaPath(old.ID) != old.MediaPath || h.mediaPath(recent.ID) != recent.MediaPath {
			t.Error("case-variant destination conflict changed a file or row")
		}
	})
}

func TestCompressRejectsSettingsThatWouldDisableAGate(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		opts func(Options) Options
	}{
		// Every comparison against NaN is false, so this would turn the PSNR
		// gate off while still reporting that images were verified.
		{"NaN PSNR floor", func(o Options) Options { o.MinPSNRDB = math.NaN(); return o }},
		{"NaN quality", func(o Options) Options { o.Quality = math.NaN(); return o }},
		{"quality above 1", func(o Options) Options { o.Quality = 4; return o }},
		{"negative PSNR floor", func(o Options) Options { o.MinPSNRDB = -1; return o }},
		{"histogram floor above 1", func(o Options) Options { o.MinHistogramSimilarity = 2; return o }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compress(context.Background(), h.store, tc.opts(h.options())); err == nil {
				t.Error("accepted a setting that would silently disable a check")
			}
		})
	}
}

// The previous version of this test asserted only post-vacuum state, which would
// have passed just as well had the vacuum run first — and running it first would
// reclaim none of the churn the passes create, which is half its purpose.
func TestCompressVacuumsAfterThePasses(t *testing.T) {
	h := newHarness(t)
	h.seed(store.KindScreen, "frame.jpg", time.Hour)
	var vacuumedAt, encodedAt int
	step := 0
	h.images.observe = func(_, _ string) { step++; encodedAt = step }
	opts := h.options()
	opts.Vacuum = true
	opts.DatabasePath = filepath.Join(filepath.Dir(h.dir), "lumi.db")
	// The vacuum's own effect is observable: it is the only thing in the run
	// that rewrites the database file, so its size changes after the passes.
	result := h.run(opts)
	step++
	vacuumedAt = step

	if result.Vacuum.Status != "done" {
		t.Fatalf("vacuum reported %q: %s", result.Vacuum.Status, result.Vacuum.Detail)
	}
	if encodedAt == 0 || encodedAt >= vacuumedAt {
		t.Errorf("encoding happened at step %d and the vacuum at %d", encodedAt, vacuumedAt)
	}
	// And the vacuum measured a database that already held the repointed row.
	if got := h.mediaPath(1); got != filepath.Join(h.dir, "frame.heic") {
		t.Errorf("after vacuuming, event 1 names %q", got)
	}
}
