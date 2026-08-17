package compress

import (
	"context"
	"fmt"

	"github.com/puremetricsai/lumi/internal/macosnative"
)

// screenPass re-encodes captured JPEGs as HEIC.
//
// This is the one lossy step in Lumi: the capture already wrote a JPEG, so a
// HEIC is a second generation and the pixels it drops are gone. That is an
// explicit, recorded decision — docs/compress.md carries both sides of it — and
// it is why this pass verifies against thresholds while the audio pass verifies
// against the bytes.
type screenPass struct{}

func (screenPass) done(source string) bool { return lowerExt(source) == extHEIC }

func (screenPass) classify(source string) (string, bool) {
	switch lowerExt(source) {
	case extJPG, extJPEG:
		return swapExtension(source, extHEIC), true
	default:
		return "", false
	}
}

func (screenPass) encode(ctx context.Context, opts Options, source, destination string) error {
	verification, err := opts.Images.Transcode(ctx, source, destination, opts.quality())
	if err != nil {
		return err
	}
	return acceptImage(verification, opts)
}

// acceptImage applies the three gates an image must clear before its original is
// deleted. They are independent on purpose: a truncated encode fails the first,
// a garbage one fails the second, and a colour-space or channel-order mistake
// can pass the second while failing the third.
func acceptImage(verification macosnative.ImageVerification, opts Options) error {
	if verification.Width != verification.SourceWidth || verification.Height != verification.SourceHeight {
		return verificationError{fmt.Errorf("re-encoded to %dx%d from %dx%d",
			verification.Width, verification.Height, verification.SourceWidth, verification.SourceHeight)}
	}
	if verification.Width == 0 || verification.Height == 0 {
		return verificationError{fmt.Errorf("re-encoded to an image with no pixels")}
	}
	if verification.PSNRDB < opts.minPSNR() {
		return verificationError{fmt.Errorf("PSNR %.1f dB is below the %.1f dB floor",
			verification.PSNRDB, opts.minPSNR())}
	}
	if verification.HistogramSimilarity < opts.minHistogram() {
		return verificationError{fmt.Errorf("histogram similarity %.4f is below the %.4f floor",
			verification.HistogramSimilarity, opts.minHistogram())}
	}
	return nil
}

// audioPass re-encodes captured WAVs as lossless FLAC.
//
// Lossless is what makes this pass uncontroversial: the samples are recoverable
// bit for bit, so "never lose captured media" is satisfied outright and there is
// no fidelity threshold to argue about. It pays for itself on silence — a silent
// system track is 960 KB of zeroes on disk and a few hundred bytes of FLAC, and
// silence is roughly half of what gets captured.
type audioPass struct{}

func (audioPass) done(source string) bool { return lowerExt(source) == extFLAC }

func (audioPass) classify(source string) (string, bool) {
	if lowerExt(source) == extWAV {
		return swapExtension(source, extFLAC), true
	}
	return "", false
}

func (audioPass) encode(ctx context.Context, opts Options, source, destination string) error {
	// The source is decoded before the encode, so a WAV that cannot be read is
	// reported as an encode failure rather than as a verification one — nothing
	// was verified. It also fails before anything has been written.
	original, originalRate, err := opts.Sounds.Decode(ctx, source)
	if err != nil {
		return err
	}
	// A chunk shorter than the encoder's minimum block cannot be turned into a
	// readable FLAC — AVFoundation writes a bare header with no audio and the
	// verifying reopen then fails with an "unsupported file type" that reads as a
	// broken encoder. It is a hard property of the codec, not a fidelity call, so
	// the input is declined before anything is written and left as the WAV it is.
	// The sub-second tail chunks a stopped recording leaves behind are where this
	// bites. → internal/macosnative MinFLACFrames.
	if len(original) < macosnative.MinFLACFrames {
		return skipError{fmt.Errorf("audio is %d frames, below the %d-frame FLAC minimum",
			len(original), macosnative.MinFLACFrames)}
	}
	if err := opts.Sounds.Transcode(ctx, source, destination); err != nil {
		return err
	}
	// Both sides go through the same decoder, so any behaviour it has is applied
	// to both and cancels out; what is left is the codec's own fidelity, which
	// for FLAC must be exact.
	encoded, encodedRate, err := opts.Sounds.Decode(ctx, destination)
	if err != nil {
		return verificationError{fmt.Errorf("re-encoded audio would not decode: %w", err)}
	}
	if encodedRate != originalRate {
		return verificationError{fmt.Errorf("re-encoded at %d Hz from %d Hz", encodedRate, originalRate)}
	}
	if len(encoded) != len(original) {
		return verificationError{fmt.Errorf("re-encoded to %d samples from %d", len(encoded), len(original))}
	}
	if sha256Samples(encoded) != sha256Samples(original) {
		return verificationError{fmt.Errorf("re-encoded audio does not match the original sample for sample")}
	}
	return nil
}
