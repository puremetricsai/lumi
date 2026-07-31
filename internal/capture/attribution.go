package capture

import (
	"github.com/puremetricsai/lumi/internal/store"
	"github.com/puremetricsai/lumi/internal/transcript"
)

// AttributionInput is everything the source decision reads. Every field was
// sampled before this call, which is what lets the rule be exercised without
// permissions, without CoreAudio, and without a recording.
type AttributionInput struct {
	// Track is transcript.TrackSystem or transcript.TrackMicrophone.
	Track string
	// Observations is how many samples across the chunk succeeded; Attempts is
	// how many were taken. Observations of zero with Attempts above it means
	// every sample failed, which is a different finding from nothing emitting.
	Observations int
	Attempts     int
	// Processes and Markers are the folded unions over the chunk window, ordered
	// by how much of the chunk each was present for.
	Processes []store.SourceApp
	Markers   []store.SourceApp
	// ProcessErr and MarkerErr are set only when no sample of that kind
	// succeeded. A partial failure is not an error: whatever did read is
	// evidence, and discarding it would lose a real emitter to a transient.
	ProcessErr string
	MarkerErr  string
}

// AttributionVerdict is what to store on one audio row.
type AttributionVerdict struct {
	Attribution store.AudioAttribution
	// SourceApps is nil when nothing was recorded and empty-non-nil when the
	// chunk was sampled and nothing was emitting. store.EncodeSourceApps turns
	// that distinction into "" versus "[]", and nothing downstream can recover it
	// afterwards, so the nil-ness here is load-bearing rather than incidental.
	SourceApps []store.SourceApp
	// Reason is diagnostic only and never a fourth attribution value.
	Reason string
}

// DecideAudioAttribution reports which applications a track's audio should be
// attributed to, and how that claim was earned.
//
// It performs no I/O by design. The rule it encodes is the whole point of this
// change — an audio row used to be stamped with whatever application happened to
// be focused, which was simply wrong whenever the user was not looking at the
// thing making noise — so it has to be checkable directly rather than only
// through a recording.
func DecideAudioAttribution(in AttributionInput) AttributionVerdict {
	// A microphone track records the room. Nothing in that signal names an
	// application, and the only app-shaped value within reach is what the user
	// had focused — a fact about the user, not about the sound. Attributing it
	// would be a fabrication that outlives the audio, since the WAV is deleted on
	// the retention schedule and the claim is not. So this branch takes
	// precedence over every piece of evidence below, including a confident one.
	if in.Track == transcript.TrackMicrophone {
		return AttributionVerdict{
			Attribution: store.AttributionUnattributed,
			Reason:      "microphone audio has no owning application",
		}
	}
	switch {
	case len(in.Processes) > 0:
		// Several processes is not a downgrade. The attribution names how the
		// claim was earned, not how many earned it, and the system track really
		// is the mix of the whole output graph — so naming one of three would be
		// less true, not more certain. Ordering already says which dominated.
		return AttributionVerdict{
			Attribution: store.AttributionEmittingProcess,
			SourceApps:  in.Processes,
		}
	case len(in.Markers) > 0:
		return AttributionVerdict{
			Attribution: store.AttributionForegroundInferred,
			SourceApps:  in.Markers,
			Reason:      "no process held an output stream; an on-screen window declared it was playing audio",
		}
	case in.Observations > 0:
		// Sampled, and nothing was emitting. That is a finding, so the list is
		// empty rather than absent.
		return AttributionVerdict{
			Attribution: store.AttributionUnattributed,
			SourceApps:  []store.SourceApp{},
			Reason:      "no process held an output stream and no window declared audio",
		}
	default:
		// Never sampled, or every sample failed. Absent list, so this stays
		// distinguishable from the case above forever.
		return AttributionVerdict{
			Attribution: store.AttributionUnattributed,
			Reason:      unsampledReason(in),
		}
	}
}

func unsampledReason(in AttributionInput) string {
	switch {
	case in.ProcessErr != "" && in.MarkerErr != "":
		return in.ProcessErr + "; " + in.MarkerErr
	case in.ProcessErr != "":
		return in.ProcessErr
	case in.MarkerErr != "":
		return in.MarkerErr
	case in.Attempts == 0:
		return "audio sources were never sampled"
	default:
		return "no audio source sample succeeded"
	}
}
