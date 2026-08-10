package compress

import (
	"context"
	"errors"
	"fmt"
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
