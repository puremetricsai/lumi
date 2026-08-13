package compress

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// errTruncated stands in for the half-written file an interrupted encode leaves.
var errTruncated = errors.New("truncated file")

// A crash before the row update leaves a compressed file no row names, beside
// the original the row still names. The compressed one is dropped rather than
// adopted, because nothing ever verified it.
func TestReconcileRemovesACompressedFileFromAnInterruptedRun(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	leftover := h.orphan("frame.heic")
	opts := h.options()
	opts.Screens = CodecNone
	opts.Audio = CodecNone

	result := h.run(opts)

	if result.Reconciled.Removed != 1 {
		t.Errorf("removed %d leftovers, want 1", result.Reconciled.Removed)
	}
	if exists(leftover) {
		t.Error("the unverified leftover survived")
	}
	if !exists(event.MediaPath) {
		t.Error("the original the row names was removed")
	}
	if h.mediaPath(event.ID) != event.MediaPath {
		t.Error("the row was repointed at an unverified file")
	}
}

// The passes match extensions case-insensitively and always write the lower-case
// one, so a row naming frame.JPG is compressed to frame.heic. Pairing the two
// therefore has to fold case as well: an exact-match lookup finds no owner for
// the leftover, which then leaks on this run and every run after it.
func TestReconcilePairsALeftoverWithARowWhoseExtensionIsUpperCase(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frame.JPG", time.Hour)
	leftover := h.orphan("frame.heic")
	opts := h.options()
	opts.Screens = CodecNone
	opts.Audio = CodecNone

	result := h.run(opts)

	if result.Reconciled.Removed != 1 {
		t.Errorf("removed %d leftovers, want 1: the leftover found no owner and leaked",
			result.Reconciled.Removed)
	}
	if exists(leftover) {
		t.Error("the unverified leftover survived")
	}
	if !exists(event.MediaPath) || h.mediaPath(event.ID) != event.MediaPath {
		t.Error("the row's own media was disturbed")
	}
}

// A crash after the row update leaves the original behind while the row already
// names the compressed file.
func TestReconcileRemovesAnOriginalLeftAfterTheRowMoved(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frame.heic", time.Hour)
	leftover := h.orphan("frame.jpg")
	opts := h.options()
	opts.Screens = CodecNone
	opts.Audio = CodecNone

	result := h.run(opts)

	if result.Reconciled.Removed != 1 {
		t.Errorf("removed %d leftovers, want 1", result.Reconciled.Removed)
	}
	if exists(leftover) {
		t.Error("the superseded original survived")
	}
	if !exists(event.MediaPath) || h.mediaPath(event.ID) != event.MediaPath {
		t.Error("the row's own media was disturbed")
	}
}

// The case that earns the design. A power loss can drop the committed row update
// while the unlink of the original persists, because media is flushed with a
// full barrier and SQLite's WAL commit by default is not. Deleting the leftover
// would destroy the last copy of the frame.
func TestReconcileAdoptsASurvivingFileWhoseOriginalIsGone(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindScreen, "frame.jpg", time.Hour)
	if err := os.Remove(event.MediaPath); err != nil {
		t.Fatal(err)
	}
	survivor := h.orphan("frame.heic")
	opts := h.options()
	opts.Screens = CodecNone
	opts.Audio = CodecNone

	result := h.run(opts)

	if result.Reconciled.Recovered != 1 {
		t.Errorf("recovered %d rows, want 1", result.Reconciled.Recovered)
	}
	if result.Reconciled.Removed != 0 {
		t.Error("the last surviving copy was deleted")
	}
	if !exists(survivor) {
		t.Fatal("the surviving file was removed")
	}
	if got := h.mediaPath(event.ID); got != survivor {
		t.Errorf("the row still names %q rather than the survivor", got)
	}
}

func TestReconcileAdoptsASurvivingFLAC(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindAudio, "chunk.wav", time.Hour)
	if err := os.Remove(event.MediaPath); err != nil {
		t.Fatal(err)
	}
	survivor := h.orphan("chunk.flac")
	opts := h.options()
	opts.Screens = CodecNone
	opts.Audio = CodecNone

	result := h.run(opts)

	if result.Reconciled.Recovered != 1 {
		t.Errorf("recovered %d rows, want 1", result.Reconciled.Recovered)
	}
	if got := h.mediaPath(event.ID); got != survivor {
		t.Errorf("the row names %q rather than the surviving FLAC", got)
	}
}

// A truncated encode is not adopted, because there is no original left to
// compare it against and a broken file would replace a recoverable absence with
// an unrecoverable claim that the media is fine.
func TestReconcileWillNotAdoptAFileThatDoesNotDecode(t *testing.T) {
	h := newHarness(t)
	event := h.seed(store.KindAudio, "chunk.wav", time.Hour)
	if err := os.Remove(event.MediaPath); err != nil {
		t.Fatal(err)
	}
	survivor := h.orphan("chunk.flac")
	h.audio.decodeErr = map[string]error{"chunk.flac": errTruncated}
	opts := h.options()
	opts.Screens = CodecNone
	opts.Audio = CodecNone

	result := h.run(opts)

	if result.Reconciled.Recovered != 0 {
		t.Errorf("adopted %d undecodable files", result.Reconciled.Recovered)
	}
	if !exists(survivor) {
		t.Error("the undecodable file was deleted rather than left for inspection")
	}
	if h.mediaPath(event.ID) != event.MediaPath {
		t.Error("the row was repointed at a file that does not decode")
	}
}

// THE regression test for this package. Captured media exists on disk before its
// row does — a frame is written, compared, then inserted; a WAV exists for a
// whole chunk plus transcription latency before its Insert. A general orphan
// sweep would delete it. Only a file whose extension-swapped sibling is named by
// a row may ever be touched, and freshly captured media has no such sibling.
func TestReconcileNeverTouchesMediaThatHasNoIndexedSibling(t *testing.T) {
	h := newHarness(t)
	// Exactly what a live recorder leaves between writing and indexing.
	inFlightFrame := h.orphan("20260810T120000Z-1-display-1.jpg")
	inFlightAudio := h.orphan("20260810T120000Z-000001-system.wav")
	inFlightFLAC := h.orphan("something-nobody-indexed.flac")
	unrelated := h.orphan("notes.txt")
	opts := h.options()
	opts.Screens = CodecNone
	opts.Audio = CodecNone

	result := h.run(opts)

	if result.Reconciled.Removed != 0 {
		t.Fatalf("removed %d unreferenced files; reconcile must never be a general orphan sweep",
			result.Reconciled.Removed)
	}
	for _, path := range []string{inFlightFrame, inFlightAudio, inFlightFLAC, unrelated} {
		if !exists(path) {
			t.Errorf("%s was deleted", filepath.Base(path))
		}
	}
}

// Reconcile runs after the passes, so its reference set names what they just
// wrote. Built any earlier — or from a set captured before them — it would
// delete the entire run's output.
func TestReconcileDoesNotDeleteWhatThePassesJustWrote(t *testing.T) {
	h := newHarness(t)
	h.seed(store.KindScreen, "frame.jpg", time.Hour)
	h.seed(store.KindAudio, "chunk.wav", time.Hour)

	result := h.run(h.options())

	if result.Reconciled.Removed != 0 {
		t.Errorf("reconcile removed %d files the same run had just written", result.Reconciled.Removed)
	}
	if !exists(filepath.Join(h.dir, "frame.heic")) || !exists(filepath.Join(h.dir, "chunk.flac")) {
		t.Fatal("reconcile deleted the run's own output")
	}
}

func TestCompressRejectsAMissingMediaDirectory(t *testing.T) {
	h := newHarness(t)
	opts := h.options()
	opts.MediaDirs = []string{filepath.Join(h.dir, "does-not-exist")}
	if _, err := Compress(context.Background(), h.store, opts); err == nil {
		t.Fatal("destructive compression accepted a missing media directory")
	}
	if h.images.calls != 0 {
		t.Error("the unusable media directory was rejected only after encoding began")
	}
}
