package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/puremetricsai/lumi/internal/store"
)

// TestMetadataDropsWhatIsAlreadyOnTheWire covers every denied key at once, so a
// key added to the list without a reason shows up here as an unexplained line.
func TestMetadataDropsWhatIsAlreadyOnTheWire(t *testing.T) {
	blob := map[string]any{
		"display_id": 5, "text_source": "vision", "audio_source": "system",
		"active_audio_output_processes": []any{map[string]any{"pid": 812}},
		"audio_marker_windows":          []any{map[string]any{"window": "playing"}},
		"accessibility_text":            strings.Repeat("x", 4000),
		"frame_similarity":              0.31, "accessibility_trusted": true,
		"width": 2560, "height": 1440, "grid_started_at": "20260731T162832Z",
		"audio_source_samples": 14, "audio_source_sample_attempts": 15,
		"foreground_samples": 6, "foreground_apps_seen": []any{"Ghostty"},
	}
	encoded, err := json.Marshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeMetadata(encoded); got != nil {
		t.Fatalf("every key here duplicates the wire or describes capture; got %#v", got)
	}
}

// TestMetadataKeepsEveryDiagnostic is the denylist's whole justification. These
// keys say why text is missing or doubtful, and a processor added later will
// write one nothing here has heard of — which is why this is a denylist and not
// an allowlist.
func TestMetadataKeepsEveryDiagnostic(t *testing.T) {
	blob := map[string]any{
		"processor_error":                     "vision timed out",
		"capture_error":                       "stream reopened",
		"accessibility_error":                 "not trusted",
		"active_audio_output_processes_error": "CoreAudio read failed",
		"audio_marker_windows_error":          "window list was null",
		"clock_anomaly":                       true,
		"audio_attribution_reason":            "one process held an output stream",
		"app_source":                          "accessibility",
		"attribution_source":                  "accessibility",
		// Stands for a key written by a processor that does not exist yet.
		"ocr_ms": 42,
		// Denied, to prove the filter is running at all.
		"display_id": 5,
	}
	encoded, err := json.Marshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeMetadata(encoded)
	if _, present := got["display_id"]; present {
		t.Fatalf("the filter did not run: %#v", got)
	}
	for key := range blob {
		if key == "display_id" {
			continue
		}
		if _, present := got[key]; !present {
			t.Fatalf("metadata dropped the diagnostic %q: %#v", key, got)
		}
	}
}

// TestMetadataPreservesAnUnreadableBlob keeps the escape hatch: a blob that is
// not a JSON object is the strongest signal something went wrong upstream, and
// dropping it would lose the only account of it.
func TestMetadataPreservesAnUnreadableBlob(t *testing.T) {
	got := decodeMetadata([]byte(`"not an object"`))
	if got["_raw"] != `"not an object"` {
		t.Fatalf("_raw = %#v", got)
	}
	if decodeMetadata(nil) != nil {
		t.Fatal("an absent blob must stay absent")
	}
}

// TestGetEventReturnsOneTextNotTwo is the case that prompted the accessibility
// text's removal. get_event promises text in full, and shipped the OCR text
// beside a differently-sourced rendering of the same frame as long again — with
// text_length describing only the first and nothing naming the second.
func TestGetEventReturnsOneTextNotTwo(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	ocr := strings.Repeat("o", 3793)
	events := insertEvents(t, ctx, s, store.Event{
		Kind: store.KindScreen, Text: ocr, App: "Ghostty", MediaPath: "/tmp/a.jpg",
		TextSource: "vision", DisplayID: 5,
		Metadata: []byte(`{"accessibility_text":"` + strings.Repeat("a", 4600) +
			`","app_source":"accessibility"}`),
	})
	h := &handlers{store: s}

	_, out, err := h.getEvent(ctx, nil, getEventInput{ID: events[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if out.Event.TextLength != len(ocr) {
		t.Fatalf("text_length = %d, want %d", out.Event.TextLength, len(ocr))
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "accessibility_text") {
		t.Fatal("get_event still ships a second copy of the screen")
	}
	// The Accessibility read's verdict still reaches a caller; only its text is gone.
	if out.Event.Metadata["app_source"] != "accessibility" {
		t.Fatalf("app_source was lost with it: %#v", out.Event.Metadata)
	}
	// Nothing else on the wire may exceed what text_length accounts for by the
	// margin a whole second transcript would.
	if len(encoded) > 2*len(ocr) {
		t.Fatalf("payload is %d bytes for %d characters of text", len(encoded), len(ocr))
	}
}

// TestPresenceIsAbsentWhenNothingWasObserved keeps a measured absence apart from
// never having read: zero observations means no sampler ever succeeded, which is
// not the same finding as "present for none of the chunk".
func TestPresenceIsAbsentWhenNothingWasObserved(t *testing.T) {
	apps, err := store.EncodeSourceApps([]store.SourceApp{{
		BundleID: "com.example.app", Name: "Example",
		Evidence: store.EvidenceProcess, Samples: 0, Observations: 0,
	}})
	if err != nil {
		t.Fatal(err)
	}
	record := newEventRecord(store.Event{
		Kind: store.KindAudio, AudioSource: "system", SourceApps: apps,
	}, 0, nil)
	if len(record.SourceApp) != 1 {
		t.Fatalf("source_app = %#v", record.SourceApp)
	}
	encoded, err := json.Marshal(record.SourceApp[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "presence") {
		t.Fatalf("unobserved presence must be absent, not 0: %s", encoded)
	}
}
