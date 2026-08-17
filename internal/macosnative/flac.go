package macosnative

// MinFLACFrames is the fewest frames EncodeAudioFLAC can turn into a readable
// file, and it is a hard property of AVFoundation's FLAC encoder rather than a
// tunable.
//
// The encoder buffers samples and emits nothing until it has accumulated one
// full block plus its lookahead — 4096 + 512 frames — and it does *not* flush a
// shorter remainder when the file is closed. Feed it fewer than 4608 frames and
// the file on disk is a bare 42-byte STREAMINFO header with no audio; reopening
// that stub fails with kAudioFileUnsupportedFileTypeError ('typ?', OSStatus
// 1954115647). The threshold is exactly 4608 and is independent of sample
// rate: measured at 8 kHz, 16 kHz and 48 kHz, 4607 frames produce the stub and
// 4608 round-trips bit for bit.
//
// It lives here because it is a fact about this package's encoder; internal/compress
// reads it to decline the round trip up front rather than write, fail to reopen,
// and count a known limitation as a broken-encoder verification failure.
const MinFLACFrames = 4608
