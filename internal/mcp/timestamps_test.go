package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// offsetOfNow is the local zone's offset as RFC3339 renders it, which is what
// every timestamp this package returns must end with.
func offsetOfNow(t *testing.T) string {
	t.Helper()
	stamp := time.Now().Format(time.RFC3339)
	return stamp[len(stamp)-6:]
}

// TestEveryTimestampIsRenderedInTheLocalZone is a whole-payload sweep rather
// than a field-by-field check, so a timestamp added later is covered without
// anyone remembering to come back here.
//
// resume_from is why it exists. It was the one field rendered UTC while
// captured_at, started_at, ended_at and last_seen were local — both parse and
// both round-trip, so nothing failed, but an agent comparing resume_from to a
// turn's started_at as strings reads them hours apart. The notice that offers
// resume_from names covered_until in the same sentence, which put the two
// spellings side by side in one line of prose.
func TestEveryTimestampIsRenderedInTheLocalZone(t *testing.T) {
	if offsetOfNow(t) == "+00:00" {
		t.Skip("the local zone is UTC, so this test cannot tell the two renderings apart")
	}
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Second)
	insertEvents(t, ctx, s, store.Event{
		Kind: store.KindScreen, CapturedAt: base, Text: "roadmap",
		App: "Safari", MediaPath: "/tmp/a.jpg",
	})
	// Six chunks against a cap of two, so the transcript is capped and has a
	// resume point to render.
	for c := range 6 {
		attributedChunk(t, ctx, s, base.Add(time.Duration(c)*time.Hour),
			store.Segment{Origin: store.OriginExternal, SourceTrack: "microphone",
				Text: "phrase", Confidence: 0.9, OrderConfidence: "sequence"})
	}
	h := &handlers{store: s}

	search := callSearch(t, ctx, h, searchEventsInput{})
	apps, _, err := h.listApps(ctx, nil, listAppsInput{})
	if err != nil {
		t.Fatal(err)
	}
	transcript := callTranscript(t, ctx, h, getTranscriptInput{Since: "11h", MaxTurns: 2})
	if transcript.ResumeFrom == "" {
		t.Fatal("the transcript is not capped, so resume_from is untested")
	}

	offset := offsetOfNow(t)
	for name, payload := range map[string]any{
		"search_events":  search,
		"list_apps":      apps,
		"get_transcript": transcript,
	} {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		for path, stamp := range timestamps(decoded, name) {
			if !strings.HasSuffix(stamp, offset) {
				t.Errorf("%s = %q, want the local offset %s", path, stamp, offset)
			}
		}
	}
}

// timestamps walks a decoded payload and returns every string that parses as
// RFC3339, keyed by where it was found. Walking the payload rather than naming
// fields is the point: it covers a timestamp nobody thought to add here.
//
// It looks at values, not key names, so a timestamp embedded in a notice's prose
// is skipped — notices are covered by the tests that assert their wording.
func timestamps(node any, path string) map[string]string {
	found := map[string]string{}
	switch value := node.(type) {
	case string:
		if _, err := time.Parse(time.RFC3339, value); err == nil {
			found[path] = value
		}
	case []any:
		for i, item := range value {
			for k, v := range timestamps(item, path+"["+strconv.Itoa(i)+"]") {
				found[k] = v
			}
		}
	case map[string]any:
		for key, item := range value {
			// A notice is prose that quotes timestamps in sentences; the wording
			// tests own it.
			if key == "notice" {
				continue
			}
			for k, v := range timestamps(item, path+"."+key) {
				found[k] = v
			}
		}
	}
	return found
}
