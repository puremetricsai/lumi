package capture

import (
	"testing"

	"github.com/puremetricsai/lumi/internal/store"
	"github.com/puremetricsai/lumi/internal/transcript"
)

func comet() store.SourceApp {
	return store.SourceApp{PID: 812, BundleID: "com.perplexity.comet", Name: "Comet",
		Evidence: store.EvidenceProcess, Samples: 11, Observations: 12}
}

func cometWindow() store.SourceApp {
	return store.SourceApp{PID: 812, Name: "Comet", Window: "(45) Why Intelligence Always Escapes - YouTube",
		Evidence: store.EvidenceWindowMarker, Samples: 9, Observations: 12}
}

func TestDecideAudioAttribution(t *testing.T) {
	for _, tc := range []struct {
		name         string
		in           AttributionInput
		want         store.AudioAttribution
		wantApps     int
		wantRecorded bool // whether SourceApps is non-nil
	}{
		{
			name: "system track with an emitting process",
			in: AttributionInput{Track: transcript.TrackSystem, Observations: 12, Attempts: 12,
				Processes: []store.SourceApp{comet()}},
			want: store.AttributionEmittingProcess, wantApps: 1, wantRecorded: true,
		},
		{
			name: "two emitters are still emitting_process",
			in: AttributionInput{Track: transcript.TrackSystem, Observations: 12, Attempts: 12,
				Processes: []store.SourceApp{comet(), {PID: 990, Name: "Music", Evidence: store.EvidenceProcess}}},
			want: store.AttributionEmittingProcess, wantApps: 2, wantRecorded: true,
		},
		{
			name: "window marker fills in when no process held a stream",
			in: AttributionInput{Track: transcript.TrackSystem, Observations: 12, Attempts: 12,
				Markers: []store.SourceApp{cometWindow()}},
			want: store.AttributionForegroundInferred, wantApps: 1, wantRecorded: true,
		},
		{
			name: "a process outranks a window marker",
			in: AttributionInput{Track: transcript.TrackSystem, Observations: 12, Attempts: 12,
				Processes: []store.SourceApp{comet()}, Markers: []store.SourceApp{cometWindow()}},
			want: store.AttributionEmittingProcess, wantApps: 1, wantRecorded: true,
		},
		{
			name: "sampled with nothing emitting records an empty list",
			in:   AttributionInput{Track: transcript.TrackSystem, Observations: 12, Attempts: 12},
			want: store.AttributionUnattributed, wantApps: 0, wantRecorded: true,
		},
		{
			name: "every sample failing records no list at all",
			in: AttributionInput{Track: transcript.TrackSystem, Observations: 0, Attempts: 12,
				ProcessErr: "list processes with active audio output: CoreAudio unavailable"},
			want: store.AttributionUnattributed, wantApps: 0, wantRecorded: false,
		},
		{
			name: "never sampled records no list at all",
			in:   AttributionInput{Track: transcript.TrackSystem},
			want: store.AttributionUnattributed, wantApps: 0, wantRecorded: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideAudioAttribution(tc.in)
			if got.Attribution != tc.want {
				t.Fatalf("attribution = %q, want %q", got.Attribution, tc.want)
			}
			if len(got.SourceApps) != tc.wantApps {
				t.Fatalf("source apps = %#v, want %d", got.SourceApps, tc.wantApps)
			}
			if recorded := got.SourceApps != nil; recorded != tc.wantRecorded {
				t.Fatalf("source apps recorded = %v, want %v — an empty list and an absent one "+
					"are different findings", recorded, tc.wantRecorded)
			}
		})
	}
}

// TestMicrophoneIsNeverAttributed is acceptance criterion 2 at the level of the
// rule itself: no amount of evidence about what the machine was playing may
// attach an application to what the room was saying.
func TestMicrophoneIsNeverAttributed(t *testing.T) {
	for _, in := range []AttributionInput{
		{Track: transcript.TrackMicrophone},
		{Track: transcript.TrackMicrophone, Observations: 12, Attempts: 12,
			Processes: []store.SourceApp{comet()}},
		{Track: transcript.TrackMicrophone, Observations: 12, Attempts: 12,
			Processes: []store.SourceApp{comet()}, Markers: []store.SourceApp{cometWindow()}},
	} {
		got := DecideAudioAttribution(in)
		if got.Attribution != store.AttributionUnattributed {
			t.Fatalf("microphone attribution = %q, want %q", got.Attribution, store.AttributionUnattributed)
		}
		if got.SourceApps != nil {
			t.Fatalf("microphone carried source apps %#v; it must never name one", got.SourceApps)
		}
	}
}
