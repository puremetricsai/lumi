package capture

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/vocabulary"
)

type AudioFrame struct {
	Path   string
	Source string
	// DurationMS is the duration that was requested, which is what every
	// indexed row means by it. MeasuredDurationMS is what the file holds.
	DurationMS   int64
	CaptureError string
	// StartedAt is the wall clock of this track's first sample buffer, and the
	// only sound anchor for its file-relative segment timings. It is per track,
	// where the chunk's captured_at is shared by both, so the two differ by the
	// skew between the tracks. Zero when the native layer reported none, which
	// is the case for every chunk captured before this field existed.
	StartedAt time.Time
	// SessionStartPTSNS is the first sample buffer's presentation timestamp.
	// Both tracks are fed by one ScreenCaptureKit stream, so these share a host
	// timebase and their difference is the exact skew between the two files'
	// t=0. Zero when the native layer reported none.
	SessionStartPTSNS  int64
	MeasuredDurationMS int64
}

// TimedSegment is one transcribed phrase with a measured span, relative to the
// start of its own WAV. StartMS/EndMS come from the union of the phrase's word
// timings rather than the recognizer's result range, which overstates speech
// extent; see macosnative.SpeechSegment.
//
// A segment with empty Text and no Runs is a signal rather than noise: audio was
// present across this span but no words resolved.
type TimedSegment struct {
	StartMS    int64
	EndMS      int64
	Text       string
	Confidence float64
	Runs       []TimedRun
}

// TimedRun is one word-level span inside a segment. Runs whose timing would not
// resolve are dropped, so runs need not concatenate back to the segment text.
type TimedRun struct {
	StartMS    int64
	EndMS      int64
	Text       string
	Confidence float64
}

// Transcription is a chunk's transcript and its timed segments. Text is the
// segments' text concatenated with no separator, matching byte-for-byte what
// every already-indexed row holds.
type Transcription struct {
	Text     string
	Segments []TimedSegment
}

// AudioChunk is one recording interval's worth of audio: every track captured
// across the same span, sharing one start instant. That shared instant is what
// makes the pair a chunk — collapse groups audio rows by timestamp, so both
// tracks must be stamped from it and not from two separate clock reads.
type AudioChunk struct {
	// StartedAt is when the chunk's audio begins. Zero when the source could
	// not say, which leaves the caller to fall back to its own clock.
	StartedAt time.Time
	Frames    []AudioFrame
}

// ErrAudioStreamClosed reports that a stream has ended and delivered everything
// it captured. It is the ordinary end of a cancelled recording, not a failure.
var ErrAudioStreamClosed = errors.New("audio capture stream closed")

// AudioSource opens a capture stream that stays open across chunks. It is a
// stream rather than a per-chunk call because the tap has to remain open while a
// finished chunk is being transcribed: closing and reopening it around each one
// left roughly two seconds of every thirty uncaptured, mid-sentence.
type AudioSource interface {
	Open(ctx context.Context, directory string, chunk time.Duration) (AudioStream, error)
}

// AudioStream delivers chunks from a capture session that keeps recording
// between them.
type AudioStream interface {
	// Next blocks until the next chunk is complete. Cancelling ctx stops the
	// capture but never abandons what it already recorded: the chunk in flight
	// is finalised and every queued chunk is still delivered, after which Next
	// returns ErrAudioStreamClosed.
	Next(ctx context.Context) (AudioChunk, error)
	Close() error
}

// NativeAudio records system output and the default microphone concurrently
// from one ScreenCaptureKit stream. Both sources are emitted as independent
// mono 16 kHz WAV chunks so transcription and source attribution stay clear.
type NativeAudio struct{}

func (NativeAudio) Open(ctx context.Context, directory string, chunk time.Duration) (AudioStream, error) {
	session, err := macosnative.StartAudioSession(ctx, directory, fileStamp(time.Now().UTC()), chunk.Seconds())
	if err != nil {
		return nil, fmt.Errorf("record system and microphone audio with ScreenCaptureKit: %w", err)
	}
	return &nativeAudioStream{session: session}, nil
}

// nativeAudioPollInterval bounds how long a Next call sits inside the native
// layer before returning to check for cancellation. The native side has no way
// to interrupt a waiting reader, so promptness on shutdown is bought here.
const nativeAudioPollInterval = 250 * time.Millisecond

type nativeAudioStream struct {
	session  *macosnative.AudioSession
	stopping bool
}

func (s *nativeAudioStream) Next(ctx context.Context) (AudioChunk, error) {
	for {
		if ctx.Err() != nil && !s.stopping {
			s.stopping = true
			s.session.Stop()
		}
		chunk, err := s.session.Next(nativeAudioPollInterval)
		if err != nil {
			return AudioChunk{}, fmt.Errorf("read ScreenCaptureKit audio chunk: %w", err)
		}
		switch {
		case len(chunk.Frames) > 0:
			return s.adopt(chunk), nil
		case chunk.Closed && chunk.CaptureError != "":
			return AudioChunk{}, fmt.Errorf("ScreenCaptureKit audio capture stopped: %s", chunk.CaptureError)
		case chunk.Closed:
			return AudioChunk{}, ErrAudioStreamClosed
		}
	}
}

func (s *nativeAudioStream) Close() error {
	s.session.Close()
	return nil
}

// adopt gives a chunk's files the timestamped names every previously recorded
// chunk carries. The session names them by ordinal because it must open them
// before Go has seen the chunk, and a failed rename keeps the ordinal name
// rather than costing the recording: the path is only ever an opaque handle to
// the bytes.
func (s *nativeAudioStream) adopt(chunk macosnative.AudioChunk) AudioChunk {
	started := unixNanoTime(chunk.StartedAtUnixNS)
	result := AudioChunk{StartedAt: started, Frames: make([]AudioFrame, 0, len(chunk.Frames))}
	for _, frame := range chunk.Frames {
		path := frame.Path
		if !started.IsZero() {
			named := filepath.Join(filepath.Dir(path), fileStamp(started)+"-"+frame.Source+".wav")
			if named != path && os.Rename(path, named) == nil {
				path = named
			}
		}
		result.Frames = append(result.Frames, AudioFrame{
			Path: path, Source: frame.Source, DurationMS: frame.DurationMS,
			CaptureError:       frame.CaptureError,
			StartedAt:          unixNanoTime(frame.StartedAtUnixNS),
			SessionStartPTSNS:  frame.SessionStartPTSNS,
			MeasuredDurationMS: frame.MeasuredDurationMS,
		})
	}
	return result
}

// unixNanoTime keeps "the native layer reported no anchor" as a zero time.Time
// rather than 1970, so callers can test it with IsZero and fall back rather than
// silently placing audio half a century ago.
func unixNanoTime(unixNS int64) time.Time {
	if unixNS <= 0 {
		return time.Time{}
	}
	return time.Unix(0, unixNS).UTC()
}

// NativeSpeech transcribes WAV chunks with Apple's on-device SpeechAnalyzer.
// Vocabulary is optional; when set, its terms bias recognition toward names
// and jargon outside the general lexicon.
type NativeSpeech struct {
	Locale     string
	Vocabulary *vocabulary.Loader
	Logger     *slog.Logger
}

func (n NativeSpeech) Transcribe(ctx context.Context, audioPath string) (Transcription, error) {
	native, err := macosnative.TranscribeAudioSegments(ctx, audioPath, n.Locale, n.vocabularyTerms())
	if err != nil {
		return Transcription{}, fmt.Errorf("transcribe audio with SpeechAnalyzer: %w", err)
	}
	result := Transcription{Text: native.Text, Segments: make([]TimedSegment, 0, len(native.Segments))}
	for _, segment := range native.Segments {
		runs := make([]TimedRun, 0, len(segment.Runs))
		for _, run := range segment.Runs {
			runs = append(runs, TimedRun{
				StartMS: run.StartMS, EndMS: run.EndMS,
				Text: run.Text, Confidence: run.Confidence,
			})
		}
		result.Segments = append(result.Segments, TimedSegment{
			StartMS: segment.StartMS, EndMS: segment.EndMS,
			Text: segment.Text, Confidence: segment.Confidence, Runs: runs,
		})
	}
	return result, nil
}

// vocabularyTerms reads the term list for this chunk. A vocabulary failure is
// never allowed to fail transcription: losing biasing on one chunk is
// recoverable, losing the chunk is not. Logging is gated on Snapshot.Changed,
// so a persistently broken file warns once rather than once per chunk even
// though the read itself is retried every chunk.
func (n NativeSpeech) vocabularyTerms() []string {
	if n.Vocabulary == nil {
		return nil
	}
	snapshot := n.Vocabulary.Load()
	if snapshot.Changed {
		switch {
		case snapshot.Err != nil:
			n.logger().Warn("vocabulary unavailable; transcribing without contextual terms",
				"error", snapshot.Err)
		case !snapshot.Exists:
			// Absence is the normal state for a user who has not opted in.
		default:
			n.logger().Info("vocabulary loaded",
				"terms", len(snapshot.Terms), "dropped", snapshot.Dropped)
		}
	}
	return snapshot.Terms
}

func (n NativeSpeech) logger() *slog.Logger {
	if n.Logger == nil {
		return slog.Default()
	}
	return n.Logger
}
