package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateSetsUserVersion(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if want := len(migrations); version != want {
		t.Fatalf("user_version = %d, want %d", version, want)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lumi.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e := Event{Kind: KindScreen, Text: "roadmap", MediaPath: "a.jpg"}
	if err := first.Insert(ctx, &e); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening a migrated database must succeed: %v", err)
	}
	defer second.Close()

	got, err := second.Search(ctx, SearchOptions{Query: "roadmap"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the pre-existing row to survive migration, got %d rows", len(got))
	}
}

// A database created by the pre-migration build has the full schema but
// user_version = 0. Opening it must not fail and must not duplicate FTS rows.
func TestMigrateUpgradesLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lumi.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, migrations[0].SQL); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO events(kind, captured_at, text, media_path)
		 VALUES ('screen', '2026-07-19T10:00:00Z', 'legacy note', 'old.jpg')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("opening a legacy database must succeed: %v", err)
	}
	defer s.Close()

	got, err := s.Search(ctx, SearchOptions{Query: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("legacy row should be findable exactly once, got %d rows", len(got))
	}
}

func TestCaptureProvenanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	event := Event{
		Kind: KindScreen, Text: "native text", MediaPath: "display.jpg",
		TextSource: "accessibility", DisplayID: 42,
	}
	if err := s.Insert(ctx, &event); err != nil {
		t.Fatal(err)
	}
	got, err := s.Search(ctx, SearchOptions{Query: "native"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TextSource != "accessibility" || got[0].DisplayID != 42 {
		t.Fatalf("capture provenance did not round trip: %#v", got)
	}

	audio := Event{
		Kind: KindAudio, Text: "meeting", MediaPath: "system.wav", AudioSource: "system",
	}
	if err := s.Insert(ctx, &audio); err != nil {
		t.Fatal(err)
	}
	got, err = s.Search(ctx, SearchOptions{Query: "meeting"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AudioSource != "system" {
		t.Fatalf("audio provenance did not round trip: %#v", got)
	}
}

// TestAudioAttributionRoundTrip covers migration 5's three columns, including
// the two distinctions the schema exists to preserve: a nil stream offset is not
// zero, and an unrecorded source list is not an empty one.
func TestAudioAttributionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	offset := int64(60000)
	apps, err := EncodeSourceApps([]SourceApp{{
		PID: 812, BundleID: "com.perplexity.comet", Name: "Comet",
		Evidence: EvidenceProcess, Samples: 11, Observations: 12, LastOffsetMS: 27500,
	}})
	if err != nil {
		t.Fatal(err)
	}
	attributed := Event{
		Kind: KindAudio, Text: "emitter", MediaPath: "system.wav", AudioSource: "system",
		AudioAttribution: string(AttributionEmittingProcess), SourceApps: apps,
		StreamOffsetMS: &offset,
	}
	if err := s.Insert(ctx, &attributed); err != nil {
		t.Fatal(err)
	}
	got, err := s.EventByID(ctx, attributed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AudioAttribution != string(AttributionEmittingProcess) {
		t.Fatalf("attribution did not round trip: %q", got.AudioAttribution)
	}
	if got.StreamOffsetMS == nil || *got.StreamOffsetMS != offset {
		t.Fatalf("stream offset did not round trip: %v", got.StreamOffsetMS)
	}
	decoded, recorded, err := DecodeSourceApps(got.SourceApps)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded || len(decoded) != 1 || decoded[0].Name != "Comet" {
		t.Fatalf("source apps did not round trip: %#v", decoded)
	}

	// An empty list is the finding "sampled, nothing emitting"; no value at all
	// is "could not sample". Collapsing them is unrecoverable afterwards.
	empty, err := EncodeSourceApps([]SourceApp{})
	if err != nil {
		t.Fatal(err)
	}
	silent := Event{
		Kind: KindAudio, Text: "quiet", MediaPath: "quiet.wav", AudioSource: "system",
		AudioAttribution: string(AttributionUnattributed), SourceApps: empty,
	}
	if err := s.Insert(ctx, &silent); err != nil {
		t.Fatal(err)
	}
	unsampled := Event{
		Kind: KindAudio, Text: "unknown", MediaPath: "unknown.wav", AudioSource: "system",
		AudioAttribution: string(AttributionUnattributed),
	}
	if err := s.Insert(ctx, &unsampled); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id           int64
		wantRecorded bool
	}{{silent.ID, true}, {unsampled.ID, false}} {
		row, err := s.EventByID(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		decoded, recorded, err := DecodeSourceApps(row.SourceApps)
		if err != nil {
			t.Fatal(err)
		}
		if recorded != tc.wantRecorded || len(decoded) != 0 {
			t.Fatalf("event %d: recorded=%v (want %v), apps=%#v", tc.id, recorded, tc.wantRecorded, decoded)
		}
	}
	if row, err := s.EventByID(ctx, unsampled.ID); err != nil || row.StreamOffsetMS != nil {
		t.Fatalf("an unrecorded stream offset must stay nil, not zero: %v %v", row.StreamOffsetMS, err)
	}
}

// TestScreenEventsCannotCarryAudioAttribution keeps the column from forking its
// meaning by row kind, which is the defect it was added to undo.
func TestScreenEventsCannotCarryAudioAttribution(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	event := Event{
		Kind: KindScreen, Text: "screen", MediaPath: "d.jpg",
		AudioAttribution: string(AttributionEmittingProcess),
	}
	if err := s.Insert(ctx, &event); err == nil {
		t.Fatal("a screen row must not be allowed to claim an audio attribution")
	}
	invalid := Event{
		Kind: KindAudio, Text: "audio", MediaPath: "a.wav", AudioAttribution: "probably_comet",
	}
	if err := s.Insert(ctx, &invalid); err == nil {
		t.Fatal("an unknown attribution value must be rejected")
	}
}
