package store

import (
	"encoding/json"
	"fmt"
)

// AudioAttribution says how an audio row's source_apps was earned. It exists so
// a consumer branching on provenance cannot mistake a guess for a fact: the
// three values are not degrees of the same claim but three different kinds of
// evidence, and one of them is the absence of any.
type AudioAttribution string

const (
	// AttributionEmittingProcess means CoreAudio reported those processes
	// holding an active output stream while the chunk recorded. It is the
	// strongest claim available without a process tap, and it is still occupancy
	// rather than audibility: a paused player holding its stream open is
	// reported. See internal/capture/CLAUDE.md.
	AttributionEmittingProcess AudioAttribution = "emitting_process"
	// AttributionForegroundInferred means CoreAudio named no process, but an
	// on-screen window's own title declared it was playing audio.
	//
	// "Named no process" covers both a failed read and a successful one that
	// found nothing, and the common case is the latter. Describing it as a read
	// *failure* overstates how unusual the value is and understates the evidence
	// behind it — a window that says it is playing audio is a real signal, not a
	// consolation prize.
	//
	// The name is about *inference from the window layer*, not about the focused
	// application. Nothing in Lumi may attribute a sound to whatever happened to
	// be focused; that is the defect this whole vocabulary exists to remove.
	AttributionForegroundInferred AudioAttribution = "foreground_inferred"
	// AttributionUnattributed means nothing named a source. Every microphone row
	// carries it unconditionally, and so does a system row when nothing held an
	// output stream or nothing could be sampled.
	AttributionUnattributed AudioAttribution = "unattributed"
)

// ParseAudioAttribution validates a stored or supplied attribution value. The
// empty string is valid and means the row predates the column, which is a
// different thing from AttributionUnattributed: one says nothing was recorded,
// the other says a source was looked for and not found.
func ParseAudioAttribution(value string) (AudioAttribution, error) {
	switch AudioAttribution(value) {
	case "", AttributionEmittingProcess, AttributionForegroundInferred, AttributionUnattributed:
		return AudioAttribution(value), nil
	default:
		return "", fmt.Errorf("invalid audio attribution %q", value)
	}
}

// SourceAppEvidence names what earned one entry its place in a source list.
const (
	// EvidenceProcess is a CoreAudio process object holding an output stream.
	EvidenceProcess = "process"
	// EvidenceWindowMarker is an on-screen window whose title declared it was
	// playing audio.
	EvidenceWindowMarker = "window_marker"
)

// SourceApp is one application observed to be producing sound while a chunk
// recorded.
//
// It carries sample counts rather than a single boolean because emitters are
// sampled repeatedly across the chunk, not once: an application present for one
// sample of twelve and one present throughout are different findings, and
// collapsing them would put a notification blip on equal footing with the video
// that filled the recording. Offsets are relative to the chunk's captured_at.
type SourceApp struct {
	PID      int32  `json:"pid"`
	BundleID string `json:"bundle_id,omitempty"`
	Name     string `json:"name,omitempty"`
	// Window is set only for EvidenceWindowMarker entries, and holds the title
	// with the audio marker removed.
	Window string `json:"window,omitempty"`
	// Evidence is EvidenceProcess or EvidenceWindowMarker. A list never mixes
	// them: the attribution value says which kind earned the row.
	Evidence string `json:"evidence"`
	// Samples is how many observations found this application emitting;
	// Observations is how many observations succeeded at all.
	Samples       int   `json:"samples"`
	Observations  int   `json:"observations"`
	FirstOffsetMS int64 `json:"first_offset_ms"`
	LastOffsetMS  int64 `json:"last_offset_ms"`
}

// DecodeSourceApps reads the source_apps_json column.
//
// It reports presence separately from content, because the two absences mean
// opposite things and nothing downstream could separate them afterwards: an
// empty list is the finding "this was sampled and nothing was emitting", while
// no value at all is "this could not be sampled". That is the same distinction
// an absent active_audio_output_processes key carries against its _error
// sibling, and the same reason a silent chunk is labelled `silent` and not
// `unknown`.
func DecodeSourceApps(raw string) (apps []SourceApp, recorded bool, err error) {
	if raw == "" {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(raw), &apps); err != nil {
		return nil, false, fmt.Errorf("decode source apps %q: %w", raw, err)
	}
	if apps == nil {
		apps = []SourceApp{}
	}
	return apps, true, nil
}

// EncodeSourceApps renders a source list for storage. A nil list is stored as
// the empty string ("not recorded"); an empty non-nil list is stored as "[]"
// ("recorded, nothing emitting"). See DecodeSourceApps.
func EncodeSourceApps(apps []SourceApp) (string, error) {
	if apps == nil {
		return "", nil
	}
	encoded, err := json.Marshal(apps)
	if err != nil {
		return "", fmt.Errorf("encode source apps: %w", err)
	}
	return string(encoded), nil
}
