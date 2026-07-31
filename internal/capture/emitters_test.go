package capture

import (
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
	"github.com/puremetricsai/lumi/internal/transcript"
)

var foldStart = time.Date(2026, 7, 30, 19, 33, 0, 0, time.UTC)

func at(seconds int) time.Time { return foldStart.Add(time.Duration(seconds) * time.Second) }

// TestAFailedProcessReadIsNeverFoldedIntoAQuietFinding is the regression for the
// shared observation count.
//
// If CoreAudio fails on every sample while the window scan reads fine and simply
// finds nothing, a shared "did this observation work" flag makes the chunk look
// sampled — and DecideAudioAttribution then records an *empty* source list,
// asserting that no process held an output stream when no process list was ever
// read. The two are opposite findings and nothing downstream can separate them
// afterwards.
func TestAFailedProcessReadIsNeverFoldedIntoAQuietFinding(t *testing.T) {
	observations := []EmitterObservation{
		{At: at(0), ProcessErr: "CoreAudio unavailable"},
		{At: at(1), ProcessErr: "CoreAudio unavailable"},
	}
	input := foldEmitters(observations, foldStart)
	if input.ProcessErr == "" {
		t.Fatal("a source that never read successfully must report its error")
	}
	if input.Observations != 0 {
		t.Fatalf("Observations = %d; a sample missing the process list cannot support "+
			"\"nothing was emitting\"", input.Observations)
	}
	verdict := DecideAudioAttribution(input.withTrack(transcript.TrackSystem))
	if verdict.SourceApps != nil {
		t.Fatalf("an unread process list produced %#v; it must record no list at all",
			verdict.SourceApps)
	}
}

// withTrack keeps the table tests readable.
func (in AttributionInput) withTrack(track string) AttributionInput {
	in.Track = track
	return in
}

// TestAPartialFailureStillReportsWhatItRead keeps the other half honest: one bad
// sample must not discard evidence the rest of the window collected.
func TestAPartialFailureStillReportsWhatItRead(t *testing.T) {
	comet := AudioProcess{PID: 812, BundleID: "com.perplexity.comet", Name: "Comet"}
	input := foldEmitters([]EmitterObservation{
		{At: at(0), ProcessErr: "CoreAudio unavailable"},
		{At: at(1), Processes: []AudioProcess{comet}},
		{At: at(2), Processes: []AudioProcess{comet}},
	}, foldStart)
	if input.ProcessErr != "" {
		t.Fatalf("a partial failure reported an error: %q", input.ProcessErr)
	}
	if len(input.Processes) != 1 || input.Processes[0].Name != "Comet" {
		t.Fatalf("processes = %#v, want Comet", input.Processes)
	}
	// The denominator counts only samples of this evidence kind, so the failed
	// read is not silently counted as a sample Comet was absent from.
	if input.Processes[0].Observations != 2 || input.Processes[0].Samples != 2 {
		t.Fatalf("samples %d of %d observations; want 2 of 2",
			input.Processes[0].Samples, input.Processes[0].Observations)
	}
}

// TestSampledAndQuietStaysDistinctFromUnsampled pins the pair at the fold level.
func TestSampledAndQuietStaysDistinctFromUnsampled(t *testing.T) {
	quiet := foldEmitters([]EmitterObservation{{At: at(0)}, {At: at(1)}}, foldStart)
	if quiet.Observations != 2 || quiet.ProcessErr != "" || quiet.MarkerErr != "" {
		t.Fatalf("a clean quiet window reported %#v", quiet)
	}
	if apps := DecideAudioAttribution(quiet.withTrack(transcript.TrackSystem)).SourceApps; apps == nil {
		t.Fatal("a sampled quiet chunk must record an empty list, not an absent one")
	}

	unread := foldEmitters([]EmitterObservation{
		{At: at(0), ProcessErr: "boom", WindowErr: "boom"},
	}, foldStart)
	if apps := DecideAudioAttribution(unread.withTrack(transcript.TrackSystem)).SourceApps; apps != nil {
		t.Fatalf("an unsampled chunk recorded %#v; it must record no list at all", apps)
	}
}

// TestFoldOrdersByPresenceAcrossTheChunk pins that the list says which
// application dominated rather than which happened to be seen first.
func TestFoldOrdersByPresenceAcrossTheChunk(t *testing.T) {
	music := AudioProcess{PID: 990, BundleID: "com.apple.Music", Name: "Music"}
	comet := AudioProcess{PID: 812, BundleID: "com.perplexity.comet", Name: "Comet"}
	input := foldEmitters([]EmitterObservation{
		{At: at(0), Processes: []AudioProcess{music}},
		{At: at(1), Processes: []AudioProcess{comet}},
		{At: at(2), Processes: []AudioProcess{comet}},
		{At: at(3), Processes: []AudioProcess{comet}},
	}, foldStart)
	if len(input.Processes) != 2 || input.Processes[0].Name != "Comet" {
		t.Fatalf("processes = %#v, want Comet first", input.Processes)
	}
	if input.Processes[0].FirstOffsetMS != 1000 || input.Processes[0].LastOffsetMS != 3000 {
		t.Fatalf("Comet spanned %d-%dms, want 1000-3000",
			input.Processes[0].FirstOffsetMS, input.Processes[0].LastOffsetMS)
	}
	if input.Processes[0].Evidence != store.EvidenceProcess {
		t.Fatalf("evidence = %q", input.Processes[0].Evidence)
	}
}

// TestRingKeepsTheNewestObservations covers appendRing's wraparound.
func TestRingKeepsTheNewestObservations(t *testing.T) {
	timeline := newEmitterTimeline(3, 3)
	for i := 0; i < 6; i++ {
		timeline.observeEmitters(EmitterObservation{At: at(i)})
	}
	got, _ := timeline.window(at(0), at(10))
	if len(got) != 3 {
		t.Fatalf("ring held %d observations, want 3", len(got))
	}
	for i, observation := range got {
		if want := at(i + 3); !observation.At.Equal(want) {
			t.Fatalf("observation %d at %v, want %v (oldest first, newest kept)", i, observation.At, want)
		}
	}
}

// TestChunkSpanPrefersWhatTheFileHolds is the regression for a window that
// outlived its audio. A chunk closed early — at shutdown, or when a stream fault
// forces a reopen — holds less than was requested, and taking the maximum of the
// two durations let the request stretch the window over emitters that only
// started after the sound stopped.
func TestChunkSpanPrefersWhatTheFileHolds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chunk AudioChunk
		want  time.Duration
	}{
		{
			name: "a partial chunk is bounded by what it measured",
			chunk: AudioChunk{Frames: []AudioFrame{
				{Source: "system", DurationMS: 30000, MeasuredDurationMS: 8000},
				{Source: "microphone", DurationMS: 30000, MeasuredDurationMS: 8000},
			}},
			want: 8 * time.Second,
		},
		{
			name: "a measurement longer than the request still wins",
			chunk: AudioChunk{Frames: []AudioFrame{
				{Source: "system", DurationMS: 30000, MeasuredDurationMS: 30120},
			}},
			want: 30120 * time.Millisecond,
		},
		{
			name: "the longest measurement across tracks is used",
			chunk: AudioChunk{Frames: []AudioFrame{
				{Source: "system", DurationMS: 30000, MeasuredDurationMS: 8000},
				{Source: "microphone", DurationMS: 30000, MeasuredDurationMS: 9000},
			}},
			want: 9 * time.Second,
		},
		{
			name: "an unmeasured chunk falls back to what was requested",
			chunk: AudioChunk{Frames: []AudioFrame{
				{Source: "system", DurationMS: 30000},
			}},
			want: 30 * time.Second,
		},
		{
			name:  "a chunk reporting neither falls back to the configured interval",
			chunk: AudioChunk{Frames: []AudioFrame{{Source: "system"}}},
			want:  30 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chunkSpan(tc.chunk, 30*time.Second); got != tc.want {
				t.Fatalf("chunkSpan = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestForegroundInferredCoversASuccessfulEmptyRead pins what the value actually
// means. The window marker is consulted whenever CoreAudio names nobody, and the
// common case is a clean read that found nothing — not a failed one. Describing
// it as a read failure overstates how unusual the value is.
func TestForegroundInferredCoversASuccessfulEmptyRead(t *testing.T) {
	input := foldEmitters([]EmitterObservation{
		{At: at(0), Windows: []AudioMarkerWindow{{PID: 812, Name: "Comet", Window: "YouTube"}}},
		{At: at(1), Windows: []AudioMarkerWindow{{PID: 812, Name: "Comet", Window: "YouTube"}}},
	}, foldStart)
	if input.ProcessErr != "" {
		t.Fatalf("fixture reports a process error: %q — this must be a clean empty read",
			input.ProcessErr)
	}
	if input.Observations == 0 {
		t.Fatal("fixture was not counted as sampled")
	}
	verdict := DecideAudioAttribution(input.withTrack(transcript.TrackSystem))
	if verdict.Attribution != store.AttributionForegroundInferred {
		t.Fatalf("attribution = %q, want %q", verdict.Attribution, store.AttributionForegroundInferred)
	}
	if len(verdict.SourceApps) != 1 || verdict.SourceApps[0].Evidence != store.EvidenceWindowMarker {
		t.Fatalf("source apps = %#v", verdict.SourceApps)
	}
}
