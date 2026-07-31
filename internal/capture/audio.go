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
	"github.com/puremetricsai/lumi/internal/transcript"
	"github.com/puremetricsai/lumi/internal/vocabulary"
)

type AudioFrame struct {
	Path   string
	Source string
	// DurationMS is the duration that was requested, which is what every
	// indexed row means by it. What the file actually holds is
	// macosnative.AudioFrame.MeasuredDurationMS, which the native smoke test
	// asserts against and nothing on this path needs.
	DurationMS   int64
	CaptureError string
	// StartedAt is the wall clock of this track's first sample buffer, and the
	// only sound anchor for its file-relative segment timings. It is per track,
	// where the chunk's captured_at is shared by both, so the two differ by the
	// skew between the tracks. Zero when the native layer reported none, which
	// is the case for every chunk captured before this field existed.
	StartedAt time.Time
	// MeasuredDurationMS is what the file actually holds, as opposed to
	// DurationMS which is what was requested. It is carried across so the chunk's
	// attribution window matches the audio rather than the configured interval,
	// and so a measured timestamp can be audited against the sound it names.
	// Zero when the native layer reported none.
	MeasuredDurationMS int64
	// SessionStartPTSNS is this track's first-sample presentation timestamp on the
	// shared host timebase. The difference between the two tracks' values is the
	// exact inter-track skew, which is the number that says whether a chunk's
	// shared captured_at is honest. Zero when unreported.
	SessionStartPTSNS int64
}

// TimedSegment is one transcribed phrase with a measured span, relative to the
// start of its own WAV. StartMS/EndMS come from the union of the phrase's word
// timings rather than the recognizer's result range, which overstates speech
// extent; see macosnative.SpeechSegment.
//
// A segment with empty Text and no Runs is a signal rather than noise: audio was
// present across this span but no words resolved.
//
// It is an alias rather than a twin of transcript.TimedSegment because every
// value of this type exists to be handed to that package: a distinct struct with
// identical fields bought nothing but a conversion loop at each hop, and one
// recognizer field added to the bridge would have needed editing in three places
// with only the middle one covered by a test.
type TimedSegment = transcript.TimedSegment

// TimedRun is one word-level span inside a segment. Runs whose timing would not
// resolve are dropped, so runs need not concatenate back to the segment text.
type TimedRun = transcript.TimedRun

// Transcription is a chunk's transcript and its timed segments. Text is the
// segments' text concatenated with no separator, matching byte-for-byte what
// every already-indexed row holds.
type Transcription struct {
	Text     string
	Segments []TimedSegment
	// Source names the recognizer that produced this text, and becomes the row's
	// text_source — the same column a screen row uses to say "vision". Without
	// it a transcript-quality regression cannot be traced to a model.
	//
	// The transcriber names itself rather than the recorder hardcoding it, which
	// is what keeps the two from forking the moment a second recognizer exists.
	// It is set on success only: a failed recognition must never claim a source
	// for text that does not exist.
	Source string
}

// AudioChunk is one recording interval's worth of audio: every track captured
// across the same span, sharing one start instant. That shared instant is what
// makes the pair a chunk — collapse groups audio rows by timestamp, so both
// tracks must be stamped from it and not from two separate clock reads.
type AudioChunk struct {
	// StartedAt is when the chunk's audio begins. Zero when the source could
	// not say, which leaves the caller to fall back to its own clock.
	StartedAt time.Time
	// GridStartedAt is where this chunk sits on the drift-free session grid:
	// the session anchor plus a whole number of chunk durations. StartedAt used
	// to be exactly this value, which is why every audio timestamp in an index
	// shared one sub-second fraction. Keeping it lets a measured stamp be
	// compared against the grid it replaced. Zero when unreported.
	GridStartedAt time.Time
	// StreamOffsetMS is this chunk's exact offset from the start of the capture
	// session. It is the drift-free arithmetic that StartedAt no longer carries,
	// and nil when the source could not say. Zero is a real value — the first
	// chunk of every session has it — so this must stay a pointer.
	StreamOffsetMS *int64
	// ClockAnomaly reports that the measured wall clock disagreed with the grid
	// by more than the guard allows, so the grid value was used instead.
	ClockAnomaly bool
	Frames       []AudioFrame
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

// AudioProcess is one application holding an active audio output stream while a
// chunk was captured. BundleID and Name are each empty when macOS could not
// resolve them, which happens for command-line processes owning no bundle: such
// a process still held a stream, so it is reported with only a PID rather than
// dropped.
type AudioProcess struct {
	PID      int32  `json:"pid"`
	BundleID string `json:"bundle_id,omitempty"`
	Name     string `json:"name,omitempty"`
}

// AudioOutputs lists the processes holding an active audio output stream. It
// answers the question the WAV cannot: the system track is a mix of the whole
// output graph and carries no provenance, so without this a recorded call is
// attributable only to whatever the user happened to have focused.
//
// It reports *stream occupancy, not audible sound*, because that is the
// strongest claim CoreAudio supports without a tap: a paused player that still
// holds its stream open is reported (measured with QuickTime), while the same
// app with its document closed is not. Establishing actual emission would need
// AudioHardwareCreateProcessTap, which this design excludes.
//
// It is separate from AudioSource because it is a read of process state rather
// than a capture, and it is optional on the Recorder for the same reason
// ContextExtractor is — a recorder with neither still indexes audio.
type AudioOutputs interface {
	Active(context.Context) ([]AudioProcess, error)
}

// NativeAudioOutputs reads CoreAudio's process objects. It opens no tap and
// captures no audio, so it needs no permission the recorder does not already
// hold.
type NativeAudioOutputs struct{}

func (NativeAudioOutputs) Active(ctx context.Context) ([]AudioProcess, error) {
	processes, err := macosnative.AudioProcesses(ctx)
	if err != nil {
		return nil, fmt.Errorf("list processes with active audio output: %w", err)
	}
	// Lumi's own output is excluded from the capture by the session
	// (excludesCurrentProcessAudio), so listing it here would claim provenance
	// for sound the recording cannot contain. Nothing else is filtered:
	// dropping the processes macOS cannot name would understate what was
	// audible, which is the failure this list exists to prevent.
	self := int32(os.Getpid())
	result := make([]AudioProcess, 0, len(processes))
	for _, process := range processes {
		if process.PID == self {
			continue
		}
		result = append(result, AudioProcess{
			PID: process.PID, BundleID: process.BundleID, Name: process.Name,
		})
	}
	return result, nil
}

// AudioMarkerWindow is one on-screen window whose own title declared it was
// playing audio while a chunk was captured. Window holds the title with the
// marker removed.
type AudioMarkerWindow = macosnative.AudioMarkerWindow

// AudioMarkers lists the windows that say they are playing sound.
//
// It is a separate seam from AudioOutputs rather than another return value on
// it, because the attribution decision has to tell "no process held an output
// stream" apart from "the window scan failed", and one error return cannot carry
// both. Collapsing them would make a transient scan failure indistinguishable
// from the finding that the machine was quiet.
//
// It is the weaker of the two signals and is consulted only when CoreAudio names
// nobody: a title is a self-report, and an application that never updates its
// title while playing simply is not found. It earned its place by being measured
// — of 117 events captured while a video played in Comet, 116 carried the marker.
type AudioMarkers interface {
	Playing(context.Context) ([]AudioMarkerWindow, error)
}

// NativeAudioMarkers scans the window list. It reads the same
// CGWindowListCopyWindowInfo the attribution snapshot already uses, so it needs
// no permission the recorder does not hold.
type NativeAudioMarkers struct{}

func (NativeAudioMarkers) Playing(ctx context.Context) ([]AudioMarkerWindow, error) {
	windows, err := macosnative.AudioMarkerWindows(ctx)
	if err != nil {
		return nil, fmt.Errorf("scan windows for the audio-playing marker: %w", err)
	}
	return windows, nil
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
	result := AudioChunk{
		StartedAt:     started,
		GridStartedAt: unixNanoTime(chunk.GridStartedAtUnixNS),
		ClockAnomaly:  chunk.ClockAnomaly,
		Frames:        make([]AudioFrame, 0, len(chunk.Frames)),
	}
	if chunk.StreamOffsetNS != nil {
		offset := *chunk.StreamOffsetNS / int64(time.Millisecond)
		result.StreamOffsetMS = &offset
	}
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
			MeasuredDurationMS: frame.MeasuredDurationMS,
			SessionStartPTSNS:  frame.SessionStartPTSNS,
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
	return Transcription{
		Text:     native.Text,
		Segments: TimedSegmentsFrom(native.Segments),
		Source:   SpeechAnalyzerSource,
	}, nil
}

// SpeechAnalyzerSource is the text_source an audio row carries when Apple's
// on-device SpeechAnalyzer produced its transcript, alongside "vision" and
// "accessibility" on screen rows.
const SpeechAnalyzerSource = "speechanalyzer"

// TimedSegmentsFrom carries the bridge's timed segments across the cgo boundary
// into the attribution types.
//
// It is exported because `lumi transcript backfill --retranscribe` re-runs the
// same recognizer over the same WAVs and has to land on the same values the
// recorder did: a second copy of this loop is how the two paths start disagreeing
// about a chunk they both attributed.
func TimedSegmentsFrom(native []macosnative.SpeechSegment) []TimedSegment {
	out := make([]TimedSegment, 0, len(native))
	for _, segment := range native {
		runs := make([]TimedRun, 0, len(segment.Runs))
		for _, run := range segment.Runs {
			runs = append(runs, TimedRun{
				StartMS: run.StartMS, EndMS: run.EndMS,
				Text: run.Text, Confidence: run.Confidence,
			})
		}
		out = append(out, TimedSegment{
			StartMS: segment.StartMS, EndMS: segment.EndMS,
			Text: segment.Text, Confidence: segment.Confidence, Runs: runs,
		})
	}
	return out
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
