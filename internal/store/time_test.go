package store

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestFormatCapturedAtIsFixedWidth pins the property the layout exists for. A
// variable-width rendering is what makes lexicographic comparison disagree with
// chronological order, and every range filter in this package is lexicographic.
func TestFormatCapturedAtIsFixedWidth(t *testing.T) {
	base := time.Date(2026, 7, 30, 19, 33, 48, 0, time.UTC)
	widths := map[int]time.Time{}
	for _, ns := range []int{0, 1, 120000000, 123456000, 123456789, 999999999} {
		at := base.Add(time.Duration(ns))
		widths[len(FormatCapturedAt(at))] = at
	}
	if len(widths) != 1 {
		t.Fatalf("captured_at renderings differ in width: %v", widths)
	}
}

// TestLexicographicOrderMatchesChronologicalOrder is the defect the fixed-width
// layout fixes. Sorting the stored strings must give the same sequence as
// sorting the instants; under time.RFC3339Nano's trailing-zero trimming it did
// not, because ".12Z" sorts after ".123456789Z" while being the earlier moment.
func TestLexicographicOrderMatchesChronologicalOrder(t *testing.T) {
	base := time.Date(2026, 7, 30, 19, 33, 48, 0, time.UTC)
	instants := []time.Time{
		base,
		base.Add(1),
		base.Add(12 * time.Millisecond),
		base.Add(120 * time.Millisecond),
		base.Add(123456 * time.Microsecond),
		base.Add(123456789),
		base.Add(999999999),
		base.Add(time.Second),
	}
	rendered := make([]string, len(instants))
	for i, at := range instants {
		rendered[i] = FormatCapturedAt(at)
	}
	sorted := append([]string(nil), rendered...)
	sort.Strings(sorted)
	for i := range rendered {
		if rendered[i] != sorted[i] {
			t.Fatalf("byte order disagrees with time order at %d: chronological %q, lexicographic %q",
				i, rendered[i], sorted[i])
		}
	}
}

// TestLegacyNanosecondRowsStillResolveTheirAudioTracks is the regression for the
// hazard that killed two earlier designs of this change: a caller rebuilding a
// captured_at key for an equality lookup cannot reproduce the stored bytes,
// because the index holds rows written under time.RFC3339Nano's trimmed
// rendering *and* rows written under CapturedAtLayout.
//
// It writes the legacy rendering through raw SQL rather than Insert, because
// Insert is exactly what no longer produces it. When this breaks, AudioTracksAt
// returns nothing for a real two-track chunk and OriginOf calls it "silent" —
// silently, for every chunk recorded before this change.
func TestLegacyNanosecondRowsStillResolveTheirAudioTracks(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A whole-second instant is the worst case: RFC3339Nano omits the fraction
	// entirely, so the legacy and fixed-width renderings share no suffix at all.
	for _, at := range []time.Time{
		time.Date(2026, 7, 29, 9, 13, 16, 493218000, time.UTC), // trailing zeros trimmed
		time.Date(2026, 7, 29, 9, 14, 0, 0, time.UTC),          // no fraction at all
	} {
		legacy := at.UTC().Format(time.RFC3339Nano)
		if legacy == FormatCapturedAt(at) {
			t.Fatalf("fixture is not a legacy rendering: %q", legacy)
		}
		for _, track := range []struct{ source, text, path string }{
			{"system", "system heard this", "sys.wav"},
			{"microphone", "mic heard this", "mic.wav"},
		} {
			_, err := s.db.ExecContext(ctx, `
INSERT INTO events(kind, captured_at, text, app, window, media_path, duration_ms,
                   text_source, display_id, audio_source, metadata_json)
VALUES ('audio', ?, ?, '', '', ?, 30000, '', 0, ?, '{}')`,
				legacy, track.text, track.path, track.source)
			if err != nil {
				t.Fatal(err)
			}
		}

		tracks, err := s.AudioTracksAt(ctx, []time.Time{at})
		if err != nil {
			t.Fatal(err)
		}
		got := tracks[FormatCapturedAt(at)]
		if len(got) != 2 {
			t.Fatalf("legacy row at %q resolved %d tracks, want 2", legacy, len(got))
		}
		if origin := OriginOf(got); origin != AudioOriginBoth {
			t.Fatalf("legacy two-track chunk at %q reported origin %q, want %q",
				legacy, origin, AudioOriginBoth)
		}
	}
}

// TestBoundsCoverBothStoredRenderings is the regression for a fixed-width bound
// silently excluding a legacy row that sits exactly on it. ".12Z" sorts *above*
// ".120000000Z" — same instant, opposite order — so an upper bound rendered one
// way drops a row rendered the other.
func TestBoundsCoverBothStoredRenderings(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// An instant with trailing zeros is the case where the two renderings differ.
	at := time.Date(2026, 7, 30, 19, 33, 48, 120000000, time.UTC)
	legacy := at.UTC().Format(time.RFC3339Nano)
	if legacy == FormatCapturedAt(at) {
		t.Fatalf("fixture is not a legacy rendering: %q", legacy)
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO events(kind, captured_at, text, app, window, media_path, duration_ms,
                   text_source, display_id, audio_source, metadata_json)
VALUES ('screen', ?, 'legacy boundary row', '', '', 'legacy.jpg', 0, 'vision', 1, '', '{}')`,
		legacy); err != nil {
		t.Fatal(err)
	}
	// A new row at the same instant, rendered the current way.
	fresh := Event{Kind: KindScreen, CapturedAt: at, Text: "fresh boundary row", MediaPath: "fresh.jpg"}
	if err := s.Insert(ctx, &fresh); err != nil {
		t.Fatal(err)
	}

	// Both rows sit exactly on the bound, and both must be returned by an
	// inclusive filter from either side.
	for _, tc := range []struct {
		name        string
		opts        SearchOptions
		wantAtLeast int
	}{
		{"until on the instant", SearchOptions{Until: &at, Limit: 50}, 2},
		{"since on the instant", SearchOptions{Since: &at, Limit: 50}, 2},
		{"both on the instant", SearchOptions{Since: &at, Until: &at, Limit: 50}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Search(ctx, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) < tc.wantAtLeast {
				t.Fatalf("got %d rows, want %d — a bound excluded a row stored under the "+
					"other rendering", len(got), tc.wantAtLeast)
			}
		})
	}
}

// TestMicrophoneRowsCannotBeAttributedAtTheStore enforces the repository-wide
// guarantee at the persistence boundary rather than trusting one caller. The
// decision function already refuses, but a guarantee that rests on a single
// writer is one a second writer can break silently — and an invented speaker
// outlives its evidence, since the WAV is pruned and the claim is not.
func TestMicrophoneRowsCannotBeAttributedAtTheStore(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	apps, err := EncodeSourceApps([]SourceApp{{PID: 812, Name: "Comet", Evidence: EvidenceProcess}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		event Event
	}{
		{"attribution", Event{Kind: KindAudio, MediaPath: "m.wav", AudioSource: "microphone",
			AudioAttribution: string(AttributionEmittingProcess)}},
		{"source list", Event{Kind: KindAudio, MediaPath: "m.wav", AudioSource: "microphone",
			AudioAttribution: string(AttributionUnattributed), SourceApps: apps}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := tc.event
			if err := s.Insert(ctx, &event); err == nil {
				t.Fatal("a microphone row was allowed to name a source")
			}
		})
	}

	// The legitimate shapes still insert: unattributed with an empty list
	// ("sampled, nothing emitting") and with none ("could not sample").
	empty, err := EncodeSourceApps([]SourceApp{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sourceApps := range []string{empty, ""} {
		event := Event{Kind: KindAudio, MediaPath: "m.wav", AudioSource: "microphone",
			AudioAttribution: string(AttributionUnattributed), SourceApps: sourceApps}
		if err := s.Insert(ctx, &event); err != nil {
			t.Fatalf("a correctly unattributed microphone row was rejected: %v", err)
		}
	}
}

// TestUndecodableSourceAppsAreRejected is the regression for validating syntax
// instead of shape. `["Comet"]` is valid JSON and invalid provenance: it parses,
// fails to decode, and — before this guard — was stored anyway. The MCP boundary
// then drops an undecodable list silently, so the row reads as though it simply
// had no source, and a microphone row carrying one slipped past the invariant
// that microphone audio never names an application.
func TestUndecodableSourceAppsAreRejected(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const malformed = `["Comet"]`
	if _, _, err := DecodeSourceApps(malformed); err == nil {
		t.Fatal("fixture decodes cleanly; it cannot probe this guard")
	}
	for _, tc := range []struct {
		name  string
		event Event
	}{
		{"microphone", Event{Kind: KindAudio, MediaPath: "m.wav", AudioSource: "microphone",
			AudioAttribution: string(AttributionUnattributed), SourceApps: malformed}},
		{"system", Event{Kind: KindAudio, MediaPath: "s.wav", AudioSource: "system",
			AudioAttribution: string(AttributionEmittingProcess), SourceApps: malformed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := tc.event
			if err := s.Insert(ctx, &event); err == nil {
				t.Fatal("an undecodable source list was stored")
			}
		})
	}

	// Well-formed provenance is unaffected, including both meanings of "no apps".
	valid, err := EncodeSourceApps([]SourceApp{{PID: 812, Name: "Comet", Evidence: EvidenceProcess}})
	if err != nil {
		t.Fatal(err)
	}
	for _, apps := range []string{valid, "[]", ""} {
		event := Event{Kind: KindAudio, MediaPath: "s.wav", AudioSource: "system",
			AudioAttribution: string(AttributionEmittingProcess), SourceApps: apps}
		if err := s.Insert(ctx, &event); err != nil {
			t.Fatalf("well-formed source list %q was rejected: %v", apps, err)
		}
	}
}
