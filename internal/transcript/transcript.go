// Package transcript decides where captured sound came from and assembles it
// into one ordered, attributed transcript.
//
// Lumi records every audio chunk twice, from the machine's output and from the
// microphone. Because the speakers are in the same room as the microphone, the
// microphone track holds *both* streams: whatever the machine played, bleeding
// back acoustically, plus whoever was actually talking. The system track holds
// only what the machine played.
//
// That asymmetry is the whole basis of attribution, and it is one-directional by
// architecture: the system track is a tap on the audio output graph, so room
// sound has no path into it. Measured across ten captured chunks where the user
// spoke continuously for thirty seconds, the system track held *exactly zero
// samples* in nine of them. So anything in the system track is machine-origin
// with no inference required, and the entire problem reduces to one question:
//
//	which spans of the microphone transcript are re-recordings of the system one?
//
// Everything else in the microphone track is room audio by elimination. There is
// no speaker identification anywhere here and no attempt at any — the labels name
// provenance, not people.
//
// The package is pure: no database, no cgo, no filesystem. Callers convert their
// own types into a Chunk and convert the returned Segments back.
package transcript

import "time"

// Origin is where a segment's sound came from.
//
// These name provenance rather than identity, and the distinction is load-bearing:
// a large share of captured system audio is a video, music, or a notification
// rather than a person, so calling it "remote" would assert something the data
// cannot support.
type Origin string

const (
	// OriginInternal is sound this machine produced — the far side of a call, a
	// video, music, a notification. Not necessarily a person.
	OriginInternal Origin = "internal"
	// OriginExternal is sound the microphone picked up from the room, typically
	// the user.
	OriginExternal Origin = "external"
	// OriginUnknown means provenance could not be determined: machine audio was
	// present but produced no transcript, so microphone speech overlapping it
	// might be a re-recording of words nobody can see.
	OriginUnknown Origin = "unknown"
	// OriginSilent marks a chunk that was attributed and found to hold no speech
	// at all. It appears only on a wordless marker segment and never on text.
	//
	// It is deliberately not OriginUnknown. Unknown is a warning — machine audio
	// played that nobody can see, so a re-recording may be hidden in the
	// microphone track — and silence asserts the opposite. Merging them would put
	// the loudest doubt in the vocabulary on the quietest thing in the index.
	OriginSilent Origin = "silent"
)

// Capture track names. These say which WAV a segment was read from, which is a
// different question from its Origin — they differ exactly where bleed was found.
// They live here because Track.Source is the field they are assigned to, and
// because both writers of attribution, the recorder and the backfill, have to
// name the two tracks identically or a chunk's system track becomes unfindable
// depending on which path attributed it.
const (
	TrackSystem     = "system"
	TrackMicrophone = "microphone"
)

// OrderConfidence says how much to trust a segment's position in the transcript.
type OrderConfidence string

const (
	// OrderExact means both tracks carried timestamps, so position is measured.
	OrderExact OrderConfidence = "exact"
	// OrderSequence means position is the microphone transcript's own reading
	// order. That order is reliable — the microphone is a single stream holding
	// everything audible, so one recognizer pass over it is chronological by
	// construction — but absolute times are unknown.
	OrderSequence OrderConfidence = "sequence"
	// OrderApproximate means machine audio absent from the microphone transcript
	// was placed by interpolating between its neighbouring matched anchors. This
	// is the only tier that admits guessing.
	OrderApproximate OrderConfidence = "approximate"
)

// Method records which path produced a segment, so a transcript can explain
// itself and a backfill can be re-run selectively.
type Method string

const (
	// MethodTimed used word-level timings from both tracks.
	MethodTimed Method = "timed"
	// MethodText had no timings and aligned the transcripts as text.
	MethodText Method = "text"
	// MethodExternalOnly had no usable system track.
	MethodExternalOnly Method = "external_only"
	// MethodInternalOnly had no usable microphone track.
	MethodInternalOnly Method = "internal_only"
	// MethodSilent ran no attribution path because neither track held words.
	MethodSilent Method = "silent"
)

// TimedRun is one word-level span within a segment, relative to its own WAV.
type TimedRun struct {
	StartMS    int64
	EndMS      int64
	Text       string
	Confidence float64
}

// TimedSegment is one transcribed phrase with a measured span, relative to the
// start of its own WAV.
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

// Track is one WAV's transcription plus what places it on a shared time axis.
type Track struct {
	// Source is the capture track, "system" or "microphone". This is where the
	// audio was *read from*, which is not the same question as its Origin.
	Source string
	// StartedAt is the wall clock of this track's first sample buffer. Zero when
	// unknown, which is the case for every chunk captured before the recorder
	// began reporting it.
	StartedAt time.Time
	// Text is the transcript as stored, verbatim.
	Text string
	// Segments is nil when only text is available.
	Segments []TimedSegment
	// Envelope is per-window energy in dBFS over the whole track, and
	// EnvelopeWindowMS the window it was measured with. Nil when not measured.
	//
	// This is what separates a track that was silent from one that carried sound
	// but produced no words — the recognizer returns nothing in both cases, and
	// they need opposite conclusions.
	Envelope         []float64
	EnvelopeWindowMS int
}

// HasEnergyData reports whether the envelope can be consulted.
func (t *Track) HasEnergyData() bool {
	return t != nil && len(t.Envelope) > 0 && t.EnvelopeWindowMS > 0
}

// EnvelopeWindowMS is the resolution an energy measurement must be taken at to be
// read correctly by internalAudible. 100ms is short enough to isolate a
// notification blip, which was measured occupying a single window in an otherwise
// silent thirty seconds; a whole-file RMS would let that one blip mark the chunk
// ambiguous.
//
// It is exported because the measurement itself needs a filesystem and this
// package has none, so both writers take it elsewhere and have to agree.
const EnvelopeWindowMS = 100

// NeedsInternalEnergy reports whether measuring the system track's energy could
// change this chunk's verdict.
//
// Only externalOnly consults the envelope, and only for a chunk whose microphone
// spoke while the system track came back empty — the recognizer returns nothing
// both for a silent track and for one carrying audio it could not transcribe, and
// those need opposite conclusions. Every other shape either never reads the
// envelope or is decided before it would be. The rule lives here rather than in
// each writer because the recorder and the backfill both have to skip exactly the
// same chunks: measuring where the recorder does not is wasted file I/O on the
// common case, and failing to measure where it does is a different verdict for
// the same audio.
func NeedsInternalEnergy(chunk Chunk) bool {
	return chunk.System != nil && !hasSpeech(chunk.System) && hasSpeech(chunk.Microphone)
}

// IsSilent reports whether attribution concluded the chunk held no speech. The
// marker is the whole result when it is produced at all, so the first segment
// decides.
//
// It is exported because the verdict is half of the gate that keeps a failed
// transcription from being recorded as silence, and both writers apply it.
func IsSilent(segments []Segment) bool {
	return len(segments) > 0 && segments[0].Method == MethodSilent
}

// Chunk is one capture window's two tracks. Either may be nil.
type Chunk struct {
	CapturedAt time.Time
	System     *Track
	Microphone *Track
}

// Segment is one attributed piece of a chunk's audio.
type Segment struct {
	// Seq is the reading position within the chunk, dense from zero. It is the
	// ordering key, because the text-only path produces no absolute times.
	Seq int
	// Origin is the attribution verdict.
	Origin Origin
	// SourceTrack is which WAV this text was read from, "system" or
	// "microphone". It differs from Origin exactly when bleed was found.
	SourceTrack string
	// Text is sliced from the verbatim transcript, never rebuilt from tokens.
	Text string
	// StartedAt and EndedAt are absolute; zero when unknown.
	StartedAt, EndedAt time.Time
	// StartOffsetMS and EndOffsetMS are relative to the segment's own WAV; nil
	// when unknown.
	StartOffsetMS, EndOffsetMS *int64
	// IsBleed marks a microphone segment found to be a re-recording of machine
	// audio. Such segments are stored — nothing captured is discarded — but
	// excluded from an assembled transcript so a phrase appears once.
	IsBleed bool
	// Confidence combines the recognizer's own score with penalties for every
	// way the attribution could be wrong.
	Confidence      float64
	OrderConfidence OrderConfidence
	Method          Method
	// Runs carries the word timings this segment was derived from, so a later
	// pass can refine a split without re-transcribing.
	Runs []TimedRun
}

// Options tunes attribution. DefaultOptions returns the calibrated values; the
// struct exists so a backfill can sweep them without editing code.
type Options struct {
	// MinOverlapFraction is how much of a microphone segment must coincide with
	// machine audio before bleed is even considered.
	MinOverlapFraction float64
	// BleedSimilarity is the match ratio at or above which a microphone span is
	// judged a re-recording.
	BleedSimilarity float64
	// CrosstalkSimilarity is the ratio below which coinciding speech is judged
	// genuinely simultaneous rather than a re-recording.
	CrosstalkSimilarity float64

	// Confidence multipliers, each naming a distinct way the label could be
	// wrong. They are multiplicative so several doubts compound.
	CrosstalkPenalty             float64
	AmbiguousPenalty             float64
	TextPathPenalty              float64
	UntranscribedInternalPenalty float64

	// InternalAudibleDBFS is the level at or below which the system track is
	// treated as carrying nothing that could have bled into the microphone.
	//
	// It is a presence test, not a loudness test. Capture gain varies enough
	// between sessions that "loud enough to be speech" is not portable — clear
	// speech measured -26 dBFS in one recording and -68 dBFS in another. What is
	// portable is that a digital output tap reads *exactly zero* when nothing is
	// playing, so any real signal is unmistakable.
	InternalAudibleDBFS float64

	// DefaultASRConfidence is used when the recognizer reported no score of its
	// own, so a missing score is not read as low confidence.
	DefaultASRConfidence float64

	// MaxAlignLagMS is the slack allowed when comparing the two tracks' times.
	// It absorbs writer-start skew and acoustic delay; measured skew between the
	// two writers was 61 ms, and speaker-to-microphone flight time is under a
	// millisecond per foot.
	MaxAlignLagMS int64
}

// Calibrated defaults. The similarity thresholds were measured rather than
// chosen, by TestCalibrateBleedThresholds over 61 real captured pairs:
//
//	0.0-0.1   9 ####
//	0.1-0.5   0          <- empty across four consecutive buckets
//	0.5-0.6   1
//	0.6-0.7   4
//	0.7-0.8   4
//	0.8-0.9   9
//	0.9-1.0  34 ################
//
// The distribution is sharply bimodal with a 0.4-wide empty valley, so both
// thresholds sit in dead space and neither is delicate. Re-run the calibration
// before moving them.
//
// The same measurement found that 17 of the 51 bleed chunks — a third — matched
// under 80% of the microphone transcript, meaning real room speech sat alongside
// the re-recording in the same window. That is why a matched span is split out of
// a segment rather than the whole segment being labelled: on a third of bleed
// chunks, labelling wholesale would either bury the user's own words or index the
// machine's twice.
const (
	MinOverlapFraction  = 0.5
	BleedSimilarity     = 0.60
	CrosstalkSimilarity = 0.35
	InternalAudibleDBFS = -65.0
)

// DefaultOptions returns the calibrated attribution settings.
func DefaultOptions() Options {
	return Options{
		MinOverlapFraction:           MinOverlapFraction,
		BleedSimilarity:              BleedSimilarity,
		CrosstalkSimilarity:          CrosstalkSimilarity,
		CrosstalkPenalty:             0.60,
		AmbiguousPenalty:             0.50,
		TextPathPenalty:              0.60,
		UntranscribedInternalPenalty: 0.50,
		InternalAudibleDBFS:          InternalAudibleDBFS,
		DefaultASRConfidence:         0.80,
		MaxAlignLagMS:                250,
	}
}

// withDefaults fills zero fields so a caller can pass a partial Options without
// silently disabling every threshold.
func (o Options) withDefaults() Options {
	defaults := DefaultOptions()
	if o.MinOverlapFraction == 0 {
		o.MinOverlapFraction = defaults.MinOverlapFraction
	}
	if o.BleedSimilarity == 0 {
		o.BleedSimilarity = defaults.BleedSimilarity
	}
	if o.CrosstalkSimilarity == 0 {
		o.CrosstalkSimilarity = defaults.CrosstalkSimilarity
	}
	if o.CrosstalkPenalty == 0 {
		o.CrosstalkPenalty = defaults.CrosstalkPenalty
	}
	if o.AmbiguousPenalty == 0 {
		o.AmbiguousPenalty = defaults.AmbiguousPenalty
	}
	if o.TextPathPenalty == 0 {
		o.TextPathPenalty = defaults.TextPathPenalty
	}
	if o.UntranscribedInternalPenalty == 0 {
		o.UntranscribedInternalPenalty = defaults.UntranscribedInternalPenalty
	}
	if o.InternalAudibleDBFS == 0 {
		o.InternalAudibleDBFS = defaults.InternalAudibleDBFS
	}
	if o.DefaultASRConfidence == 0 {
		o.DefaultASRConfidence = defaults.DefaultASRConfidence
	}
	if o.MaxAlignLagMS == 0 {
		o.MaxAlignLagMS = defaults.MaxAlignLagMS
	}
	return o
}
