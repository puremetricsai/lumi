package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

func TestSearchEventsFiltersCombine(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: base, Text: "quarterly roadmap review", App: "Safari", Window: "Quarterly plan", MediaPath: "/tmp/a.jpg", TextSource: "vision", DisplayID: 1},
		store.Event{Kind: store.KindScreen, CapturedAt: base.Add(-72 * time.Hour), Text: "quarterly roadmap review", App: "Safari", Window: "Quarterly plan", MediaPath: "/tmp/b.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: base, Text: "quarterly roadmap review", App: "Ghostty", Window: "zsh", MediaPath: "/tmp/c.jpg"},
		store.Event{Kind: store.KindAudio, CapturedAt: base, Text: "discuss the launch budget", MediaPath: "/tmp/d.wav", AudioSource: "microphone", DurationMS: 30000},
	)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{})
	if len(out.Events) != 4 {
		t.Fatalf("no filters must return everything, got %d", len(out.Events))
	}

	out = callSearch(t, ctx, h, searchEventsInput{Kind: "audio"})
	if len(out.Events) != 1 || out.Events[0].Kind != "audio" {
		t.Fatalf("kind filter failed: %#v", out.Events)
	}

	out = callSearch(t, ctx, h, searchEventsInput{Query: "quarterly roadmap", App: "safari", Since: "24h"})
	if len(out.Events) != 1 {
		t.Fatalf("query+app+since must isolate one event, got %d", len(out.Events))
	}
	got := out.Events[0]
	if got.App != "Safari" || got.Window != "Quarterly plan" || got.MediaPath != "/tmp/a.jpg" {
		t.Fatalf("unexpected record: %#v", got)
	}
	if got.TextSource != "vision" || got.DisplayID != 1 {
		t.Fatalf("provenance columns were dropped: %#v", got)
	}
	if _, err := time.Parse(time.RFC3339, got.CapturedAt); err != nil {
		t.Fatalf("captured_at %q is not RFC3339: %v", got.CapturedAt, err)
	}
	if !strings.Contains(got.CapturedAt, offsetOf(base)) {
		t.Fatalf("captured_at %q is not rendered in the local zone", got.CapturedAt)
	}

	out = callSearch(t, ctx, h, searchEventsInput{Window: "quarterly"})
	if len(out.Events) != 2 {
		t.Fatalf("window substring filter failed: %#v", out.Events)
	}

	out = callSearch(t, ctx, h, searchEventsInput{Limit: 2})
	if len(out.Events) != 2 {
		t.Fatalf("limit failed, got %d", len(out.Events))
	}
}

// offsetOf renders the machine's local UTC offset the way RFC3339 does, so the
// timezone assertion holds on any machine (including a UTC CI box, where it is
// the literal "Z").
func offsetOf(at time.Time) string {
	rendered := at.Local().Format(time.RFC3339)
	return rendered[len(rendered)-6:]
}

func TestSearchEventsMatchAnyFindsWhatAllMisses(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, Text: "postgres index tuning notes", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, Text: "an unrelated grocery list", MediaPath: "/tmp/b.jpg"},
	)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Query: "postgres grocery"})
	if len(out.Events) != 0 {
		t.Fatalf(`the default match must be "all", got %d results`, len(out.Events))
	}
	out = callSearch(t, ctx, h, searchEventsInput{Query: "postgres grocery", Match: "any"})
	if len(out.Events) != 2 {
		t.Fatalf(`match "any" must return both, got %d`, len(out.Events))
	}
}

// TestSearchEventsMatchAnyRanksBodyTextAboveAWindowTitle pins the bm25 column
// weights through the tool boundary: without them a one-word window title
// outranks a page of relevant screen text, and an agent's first page of results
// is all title noise.
func TestSearchEventsMatchAnyRanksBodyTextAboveAWindowTitle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, Text: "budget", App: "Safari", Window: "budget", MediaPath: "/tmp/title.jpg"},
		store.Event{Kind: store.KindScreen, Text: "the launch budget covers contractor time and hosting", App: "Notes", Window: "notes", MediaPath: "/tmp/body.jpg"},
	)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Query: "launch budget", Match: "any"})
	if len(out.Events) != 2 {
		t.Fatalf("expected both events, got %d", len(out.Events))
	}
	if out.Events[0].MediaPath != "/tmp/body.jpg" {
		t.Fatalf("the body-text hit must rank first, got %#v", out.Events[0])
	}
}

func TestSearchEventsRequireTextDropsBlankTranscripts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindAudio, Text: "   \n\t ", MediaPath: "/tmp/silent.wav"},
		store.Event{Kind: store.KindAudio, Text: "discuss the launch budget", MediaPath: "/tmp/speech.wav"},
	)
	h := &handlers{store: s}

	if out := callSearch(t, ctx, h, searchEventsInput{}); len(out.Events) != 2 {
		t.Fatalf("require_text defaults to off, got %d", len(out.Events))
	}
	out := callSearch(t, ctx, h, searchEventsInput{RequireText: true})
	if len(out.Events) != 1 || out.Events[0].MediaPath != "/tmp/speech.wav" {
		t.Fatalf("require_text failed: %#v", out.Events)
	}
}

func TestSearchEventsTruncation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	long := strings.Repeat("a", 1000)
	multibyte := strings.Repeat("日", 800)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, Text: long, App: "Long", MediaPath: "/tmp/long.jpg"},
		store.Event{Kind: store.KindScreen, Text: multibyte, App: "Multi", MediaPath: "/tmp/multi.jpg"},
		store.Event{Kind: store.KindScreen, Text: "short", App: "Short", MediaPath: "/tmp/short.jpg"},
	)
	h := &handlers{store: s}

	byApp := func(out searchEventsOutput, app string) EventRecord {
		t.Helper()
		for _, record := range out.Events {
			if record.App == app {
				return record
			}
		}
		t.Fatalf("no record for app %q", app)
		return EventRecord{}
	}

	out := callSearch(t, ctx, h, searchEventsInput{})
	short := byApp(out, "Short")
	if short.Truncated || short.Text != "short" || short.TextLength != 5 {
		t.Fatalf("under-cap text must be untouched: %#v", short)
	}
	defaulted := byApp(out, "Long")
	if !defaulted.Truncated || len([]rune(defaulted.Text)) != defaultMaxTextChars || defaulted.TextLength != 1000 {
		t.Fatalf("default cap failed: truncated=%v runes=%d length=%d",
			defaulted.Truncated, len([]rune(defaulted.Text)), defaulted.TextLength)
	}
	multi := byApp(out, "Multi")
	if !strings.HasSuffix(multi.Text, "日") || len([]rune(multi.Text)) != defaultMaxTextChars {
		t.Fatalf("multibyte text was cut mid-character: %q", multi.Text)
	}

	atCap := 5
	out = callSearch(t, ctx, h, searchEventsInput{App: "Short", MaxTextChars: &atCap})
	if record := byApp(out, "Short"); record.Truncated {
		t.Fatalf("text exactly at the cap must not be marked truncated: %#v", record)
	}

	none := 0
	out = callSearch(t, ctx, h, searchEventsInput{App: "Long", MaxTextChars: &none})
	if record := byApp(out, "Long"); record.Truncated || record.Text != long {
		t.Fatalf("max_text_chars 0 must mean no cap: truncated=%v len=%d", record.Truncated, len(record.Text))
	}
}

// TestEventRecordAlwaysSerializesTruncated guards the wire form of the pair
// that makes truncation safe. An `omitempty` on Truncated would drop the key
// for every complete text and drop it from the generated schema's required
// list, so an agent would have to infer "not truncated" from a missing field
// while text_length stayed present — exactly the ambiguity the pair exists to
// remove.
func TestEventRecordAlwaysSerializesTruncated(t *testing.T) {
	encoded, err := json.Marshal(newEventRecord(store.Event{Kind: store.KindScreen, Text: "short"}, 600))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	truncated, ok := decoded["truncated"]
	if !ok {
		t.Fatalf("untruncated text dropped the truncated key: %s", encoded)
	}
	if truncated != false {
		t.Fatalf("truncated = %v, want false", truncated)
	}
	if _, ok := decoded["text_length"]; !ok {
		t.Fatalf("text_length must always travel with truncated: %s", encoded)
	}
}

func TestSearchEventsRejectsInvalidEnumsAndTimes(t *testing.T) {
	ctx := context.Background()
	h := &handlers{store: testStore(t)}

	for _, tc := range []struct {
		name  string
		in    searchEventsInput
		wants []string
	}{
		{"kind", searchEventsInput{Kind: "video"}, []string{"screen", "audio", "video"}},
		{"match", searchEventsInput{Match: "some"}, []string{"all", "any", "some"}},
		{"since", searchEventsInput{Since: "yesterday"}, []string{"since", "RFC3339", "duration"}},
		{"until", searchEventsInput{Until: "yesterday"}, []string{"until", "RFC3339", "duration"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := h.searchEvents(ctx, nil, tc.in)
			if err == nil {
				t.Fatal("expected a tool error")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestSearchEventsDistinguishesEmptyStoreFromNoMatch is the difference between
// an agent reporting "you have not recorded anything yet" and "nothing matched"
// — the two need different follow-up actions.
func TestSearchEventsDistinguishesEmptyStoreFromNoMatch(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Query: "anything"})
	if len(out.Events) != 0 {
		t.Fatalf("an empty store must return no events, got %d", len(out.Events))
	}
	if !strings.Contains(out.Notice, "no events") {
		t.Fatalf("empty-store notice = %q, want it to say the store holds no events", out.Notice)
	}

	insertEvents(t, ctx, s, store.Event{Kind: store.KindScreen, Text: "roadmap", MediaPath: "/tmp/a.jpg"})
	out = callSearch(t, ctx, h, searchEventsInput{Query: "kubernetes"})
	if len(out.Events) != 0 {
		t.Fatalf("expected no matches, got %d", len(out.Events))
	}
	if !strings.Contains(out.Notice, "matched") {
		t.Fatalf("no-match notice = %q, want it to say nothing matched the filters", out.Notice)
	}
}

func TestGetEventReturnsUntruncatedTextAndMetadata(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	long := strings.Repeat("b", 2000)
	events := insertEvents(t, ctx, s, store.Event{
		Kind: store.KindScreen, Text: long, App: "Safari", Window: "Quarterly plan",
		MediaPath: "/tmp/a.jpg", TextSource: "vision", DisplayID: 1,
		Metadata: []byte(`{"ocr_ms":42,"focused_window_text":"plan"}`),
	})
	h := &handlers{store: s}

	// The escape hatch only works if search really did flag the cut.
	searched := callSearch(t, ctx, h, searchEventsInput{})
	if !searched.Events[0].Truncated || searched.Events[0].TextLength != 2000 {
		t.Fatalf("search must flag the truncation: %#v", searched.Events[0])
	}

	_, out, err := h.getEvent(ctx, nil, getEventInput{ID: events[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if out.Event.Truncated {
		t.Fatalf("get_event must never truncate: %#v", out.Event)
	}
	if out.Event.Text != long || out.Event.TextLength != 2000 {
		t.Fatalf("text was not returned in full: %d chars", len(out.Event.Text))
	}
	if out.Event.Metadata["ocr_ms"] != float64(42) {
		t.Fatalf("metadata missing or wrong: %#v", out.Event.Metadata)
	}
	if out.Event.MediaPath != "/tmp/a.jpg" {
		t.Fatalf("media_path = %q", out.Event.MediaPath)
	}
}

func TestGetEventUnknownIDIsAToolError(t *testing.T) {
	ctx := context.Background()
	h := &handlers{store: testStore(t)}

	_, _, err := h.getEvent(ctx, nil, getEventInput{ID: 4711})
	if err == nil {
		t.Fatal("an unknown id must be a tool error, not an empty result")
	}
	if !strings.Contains(err.Error(), "4711") {
		t.Fatalf("error %q does not name the id", err)
	}
}

func TestListAppsReportsAppsThenWindows(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: base, App: "Safari", Window: "Plan", Text: "a", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: base.Add(time.Minute), App: "Safari", Window: "Plan", Text: "b", MediaPath: "/tmp/b.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: base.Add(2 * time.Minute), App: "Safari", Window: "Docs", Text: "c", MediaPath: "/tmp/c.jpg"},
		store.Event{Kind: store.KindAudio, CapturedAt: base.Add(3 * time.Minute), Text: "d", MediaPath: "/tmp/d.wav"},
	)
	h := &handlers{store: s}

	_, out, err := h.listApps(ctx, nil, listAppsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("expected Safari and the unattributed bucket, got %#v", out.Entries)
	}
	if out.Entries[0].App != "Safari" || out.Entries[0].Events != 3 {
		t.Fatalf("unexpected first entry: %#v", out.Entries[0])
	}
	if _, err := time.Parse(time.RFC3339, out.Entries[0].LastSeen); err != nil {
		t.Fatalf("last_seen %q is not RFC3339: %v", out.Entries[0].LastSeen, err)
	}
	if out.Entries[1].App != "" || out.Entries[1].Events != 1 {
		t.Fatalf("the empty-app bucket must be reported explicitly: %#v", out.Entries[1])
	}

	app := "safari"
	_, out, err = h.listApps(ctx, nil, listAppsInput{App: &app})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("expected two Safari windows, got %#v", out.Entries)
	}
	if out.Entries[0].Window != "Plan" || out.Entries[0].Events != 2 {
		t.Fatalf("unexpected first window: %#v", out.Entries[0])
	}
}

func TestListAppsHonorsTimeRangeAndRejectsBadTimes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: base.Add(-72 * time.Hour), App: "Old", Text: "a", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: base, App: "Safari", Text: "b", MediaPath: "/tmp/b.jpg"},
	)
	h := &handlers{store: s}

	_, out, err := h.listApps(ctx, nil, listAppsInput{Since: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 || out.Entries[0].App != "Safari" {
		t.Fatalf("since was not applied: %#v", out.Entries)
	}

	if _, _, err := h.listApps(ctx, nil, listAppsInput{Since: "yesterday"}); err == nil {
		t.Fatal("an unparseable since must be a tool error")
	}
}

func TestListAppsOnAnEmptyStoreSaysSo(t *testing.T) {
	ctx := context.Background()
	h := &handlers{store: testStore(t)}

	_, out, err := h.listApps(ctx, nil, listAppsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 0 {
		t.Fatalf("expected no entries, got %#v", out.Entries)
	}
	if !strings.Contains(out.Notice, "no events") {
		t.Fatalf("notice = %q, want it to say the index holds no events", out.Notice)
	}
}

func TestListAppsLimitDefaults(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	// Seed 55 distinct apps: with Limit: 0, the default is 50, so we get 50 back;
	// with an explicit small limit like 20, we get exactly 20. This distinguishes
	// the "default" branch from the "honor explicit" branch. The ceiling clamp
	// (600 → 500) cannot be observed end-to-end without 500+ apps, so it is
	// documented here but not tested (a regression that dropped or flipped the
	// ceiling comparison would pass this test because 55 < 500).
	for i := 1; i <= 55; i++ {
		insertEvents(t, ctx, s, store.Event{
			Kind:       store.KindScreen,
			CapturedAt: base.Add(time.Duration(i) * time.Millisecond),
			App:        fmt.Sprintf("App%02d", i),
			Text:       "text",
			MediaPath:  fmt.Sprintf("/tmp/%d.jpg", i),
		})
	}
	h := &handlers{store: s}

	// Omitted Limit defaults to 50
	_, out, err := h.listApps(ctx, nil, listAppsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 50 {
		t.Fatalf("omitted limit should default to 50, got %d entries", len(out.Entries))
	}

	// Explicit Limit: 0 also defaults to 50
	_, out, err = h.listApps(ctx, nil, listAppsInput{Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 50 {
		t.Fatalf("limit 0 should default to 50, got %d entries", len(out.Entries))
	}

	// Explicit small limit is honored exactly
	_, out, err = h.listApps(ctx, nil, listAppsInput{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 20 {
		t.Fatalf("explicit limit 20 must be honored exactly, got %d entries", len(out.Entries))
	}
}

func TestListAppsEmptyAppFiltersToUnattributedWindows(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	// The empty-string app mode is a distinct code path: App: nil lists apps
	// (grouping by app), while App: pointer-to-"" lists windows within
	// unattributed events. Without this test, a regression that merged the two
	// branches or failed to filter by app would go undetected.
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: base, App: "", Window: "Terminal", Text: "a", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: base.Add(time.Second), App: "", Window: "Editor", Text: "b", MediaPath: "/tmp/b.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: base.Add(2 * time.Second), App: "Safari", Window: "Web", Text: "c", MediaPath: "/tmp/c.jpg"},
	)
	h := &handlers{store: s}

	// Query for windows of the empty-app bucket
	emptyApp := ""
	_, out, err := h.listApps(ctx, nil, listAppsInput{App: &emptyApp})
	if err != nil {
		t.Fatal(err)
	}

	// Must return exactly the two unattributed windows, not the Safari event
	if len(out.Entries) != 2 {
		t.Fatalf("expected 2 unattributed windows, got %d: %#v", len(out.Entries), out.Entries)
	}

	windows := make(map[string]bool)
	for _, entry := range out.Entries {
		if entry.App != "" {
			t.Fatalf("entries filtered by empty app should have App: \"\", got App: %q", entry.App)
		}
		windows[entry.Window] = true
	}

	if !windows["Terminal"] || !windows["Editor"] {
		t.Fatalf("expected Terminal and Editor windows, got %#v", windows)
	}
}

// TestSearchEventsRejectsQueryWithNoSearchableTerms pins finding 1 from the
// final review: FTS5 tokenizes pure punctuation/emoji to nothing, so
// store.Search silently drops the MATCH clause and returns the most recent
// events — indistinguishable from real hits unless the handler catches it
// first. A broken implementation that skips this check would return the
// unrelated "roadmap" event with no error and no notice.
func TestSearchEventsRejectsQueryWithNoSearchableTerms(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s, store.Event{Kind: store.KindScreen, Text: "roadmap", MediaPath: "/tmp/a.jpg"})
	h := &handlers{store: s}

	for _, query := range []string{"???", "🎉", "---", "   ***   "} {
		_, _, err := h.searchEvents(ctx, nil, searchEventsInput{Query: query})
		if err == nil {
			t.Fatalf("query %q must be rejected as a tool error, not silently browse everything", query)
		}
		if !strings.Contains(err.Error(), "searchable term") {
			t.Fatalf("error %q for query %q does not mention searchable terms", err, query)
		}
	}
}

// TestSearchEventsWithAlphanumericQueryStillWorks guards against the guard
// above regressing into rejecting valid queries: any query with at least one
// letter or digit, however mixed with punctuation, must still search normally.
func TestSearchEventsWithAlphanumericQueryStillWorks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s, store.Event{Kind: store.KindScreen, Text: "quarterly roadmap review", MediaPath: "/tmp/a.jpg"})
	h := &handlers{store: s}

	for _, query := range []string{"roadmap", "??roadmap??", "🎉roadmap", "roadmap!"} {
		out := callSearch(t, ctx, h, searchEventsInput{Query: query})
		if len(out.Events) != 1 {
			t.Fatalf("query %q must still find the event, got %d results (notice=%q)", query, len(out.Events), out.Notice)
		}
	}
}

// TestSearchEventsCapNoticeIsAnElseBranch pins finding 2: a full page must
// carry a cap notice so an agent can tell it saw a recency-truncated slice,
// while a partial page and an empty page keep their own distinct notices
// (or none). A broken implementation that always/never sets the cap notice,
// or that clobbers the empty/no-match notices, would fail one of these three.
func TestSearchEventsCapNoticeIsAnElseBranch(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		insertEvents(t, ctx, s, store.Event{
			Kind: store.KindScreen, CapturedAt: base.Add(time.Duration(i) * time.Second),
			Text: "roadmap", MediaPath: "/tmp/a.jpg",
		})
	}
	h := &handlers{store: s}

	// Full page: limit equals the number of matching events.
	full := callSearch(t, ctx, h, searchEventsInput{Limit: 3})
	if len(full.Events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(full.Events))
	}
	if !strings.Contains(full.Notice, "capped") {
		t.Fatalf("full-page notice = %q, want it to mention the cap", full.Notice)
	}

	// Partial page: limit exceeds the number of matching events.
	partial := callSearch(t, ctx, h, searchEventsInput{Limit: 10})
	if len(partial.Events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(partial.Events))
	}
	if partial.Notice != "" {
		t.Fatalf("partial-page notice = %q, want none", partial.Notice)
	}

	// Empty result: the existing no-match notice must still fire, not the cap notice.
	empty := callSearch(t, ctx, h, searchEventsInput{Query: "kubernetes"})
	if len(empty.Events) != 0 {
		t.Fatalf("expected no events, got %d", len(empty.Events))
	}
	if !strings.Contains(empty.Notice, "matched") || strings.Contains(empty.Notice, "capped") {
		t.Fatalf("empty-result notice = %q, want the no-match notice and not the cap notice", empty.Notice)
	}
}

// TestSearchEventsClampsLimitToMax pins the handler-level 500 ceiling: the
// parameter hint documents "capped at 500", so the handler must enforce it
// itself rather than relying on store.Search's own unexported clamp.
func TestSearchEventsClampsLimitToMax(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for i := 0; i < 3; i++ {
		insertEvents(t, ctx, s, store.Event{Kind: store.KindScreen, Text: "roadmap", MediaPath: "/tmp/a.jpg"})
	}
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Limit: 100000})
	if len(out.Events) != 3 {
		t.Fatalf("expected all 3 events, got %d", len(out.Events))
	}
	// With only 3 events stored, a clamp to 500 (not 100000) must still read
	// as a partial page, not a capped one.
	if strings.Contains(out.Notice, "capped") {
		t.Fatalf("notice = %q, an oversized limit clamped to 500 must not read as a full page with only 3 events stored", out.Notice)
	}
}

// TestSearchEventsEmptyStoreNoticeIncludesDatabasePath pins finding 4: a
// mistyped --data-dir yields a fresh empty database that is otherwise
// indistinguishable from "you never recorded anything". Once Options carries
// the resolved database path, the empty-store notice must name it.
func TestSearchEventsEmptyStoreNoticeIncludesDatabasePath(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	h := &handlers{store: s, databasePath: "/tmp/typo-data-dir/lumi.db"}

	out := callSearch(t, ctx, h, searchEventsInput{})
	if !strings.Contains(out.Notice, "/tmp/typo-data-dir/lumi.db") {
		t.Fatalf("empty-store notice = %q, want it to include the database path", out.Notice)
	}

	// list_apps shares the same wording and must carry the path too.
	_, appsOut, err := h.listApps(ctx, nil, listAppsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(appsOut.Notice, "/tmp/typo-data-dir/lumi.db") {
		t.Fatalf("list_apps empty-store notice = %q, want it to include the database path", appsOut.Notice)
	}
}

// audioPair inserts a system+microphone pair sharing one captured_at, the
// invariant CollapseAudioTracks groups on. It returns the two stored events.
func audioPair(t *testing.T, ctx context.Context, s *store.Store, at time.Time, systemText, micText string) []store.Event {
	t.Helper()
	return insertEvents(t, ctx, s,
		store.Event{Kind: store.KindAudio, CapturedAt: at, AudioSource: "system", Text: systemText, MediaPath: "/tmp/sys.wav", DurationMS: 30000},
		store.Event{Kind: store.KindAudio, CapturedAt: at, AudioSource: "microphone", Text: micText, MediaPath: "/tmp/mic.wav", DurationMS: 30000},
	)
}

func TestSearchEventsCollapsesByDefault(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	audioPair(t, ctx, s, base, "system heard the same words", "microphone heard the same words")
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Kind: "audio"})
	if len(out.Events) != 1 {
		t.Fatalf("expected the pair collapsed to one result, got %d", len(out.Events))
	}
	rec := out.Events[0]
	if rec.AudioSource != "system" {
		t.Fatalf("survivor = %q, want system", rec.AudioSource)
	}
	if rec.AudioOrigin != "both" {
		t.Fatalf("audio_origin = %q, want both", rec.AudioOrigin)
	}
	if len(rec.AudioTracks) != 2 {
		t.Fatalf("expected two-entry audio_tracks, got %d", len(rec.AudioTracks))
	}
	if rec.AudioTracks[0].ID != rec.ID {
		t.Fatalf("survivor must be first in audio_tracks; got %d, want %d", rec.AudioTracks[0].ID, rec.ID)
	}
	if !strings.Contains(out.Notice, "collapse_audio_tracks: false") {
		t.Fatalf("notice = %q, want it to name the escape hatch", out.Notice)
	}
}

func TestSearchEventsLoneAudioHasOriginButNoTracks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s, store.Event{
		Kind: store.KindAudio, CapturedAt: time.Now().UTC(), AudioSource: "system",
		Text: "a solitary chunk", MediaPath: "/tmp/lone.wav", DurationMS: 30000,
	})
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Kind: "audio"})
	if len(out.Events) != 1 {
		t.Fatalf("expected one result, got %d", len(out.Events))
	}
	rec := out.Events[0]
	if rec.AudioOrigin != "system" {
		t.Fatalf("audio_origin = %q, want system", rec.AudioOrigin)
	}
	if rec.AudioTracks != nil {
		t.Fatalf("a lone audio row must not carry audio_tracks, got %#v", rec.AudioTracks)
	}
}

func TestSearchEventsCollapseFalseReturnsBothUnmerged(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	audioPair(t, ctx, s, base, "system heard this", "microphone heard this")
	h := &handlers{store: s}

	off := false
	out := callSearch(t, ctx, h, searchEventsInput{Kind: "audio", CollapseAudioTracks: &off})
	if len(out.Events) != 2 {
		t.Fatalf("expected both rows unmerged, got %d", len(out.Events))
	}
	for _, rec := range out.Events {
		if rec.AudioOrigin != "" || rec.AudioTracks != nil {
			t.Fatalf("collapse off must leave audio_origin/audio_tracks empty, got %#v", rec)
		}
	}
	if strings.Contains(out.Notice, "collapsed") {
		t.Fatalf("collapse off must not add a collapse notice, got %q", out.Notice)
	}
}

func TestSearchEventsDroppedTrackResolvesThroughGetEvent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	pair := audioPair(t, ctx, s, base, "system transcript", "the microphone captured its own longer transcript here")
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Kind: "audio"})
	if len(out.Events) != 1 {
		t.Fatalf("expected one collapsed result, got %d", len(out.Events))
	}
	// Find the dropped (microphone) track id in audio_tracks.
	var droppedID int64
	for _, tr := range out.Events[0].AudioTracks {
		if tr.AudioSource == "microphone" {
			droppedID = tr.ID
		}
	}
	if droppedID == 0 {
		t.Fatalf("microphone track id missing from audio_tracks: %#v", out.Events[0].AudioTracks)
	}
	if droppedID != pair[1].ID {
		t.Fatalf("dropped track id = %d, want the microphone row %d", droppedID, pair[1].ID)
	}
	_, got, err := h.getEvent(ctx, nil, getEventInput{ID: droppedID})
	if err != nil {
		t.Fatalf("get_event on dropped track: %v", err)
	}
	if got.Event.Text != "the microphone captured its own longer transcript here" {
		t.Fatalf("get_event returned %q, want the microphone's full text", got.Event.Text)
	}
}

// TestAudioOriginDescribesTheChunkNotTheMatch is the test that fails if
// provenance is ever derived from the matched set alone: a query that hits only
// the microphone track of a bleed pair must still report audio_origin "both".
func TestAudioOriginDescribesTheChunkNotTheMatch(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	pair := audioPair(t, ctx, s, base,
		"revenue was fifty two million dollars",
		"the mic transcribed 52000000 as digits")
	h := &handlers{store: s}

	// "52000000" appears only in the microphone transcript, so only that row
	// matches — a SQL pre-filter judging the non-matching system row "better"
	// would drop the only hit.
	out := callSearch(t, ctx, h, searchEventsInput{Query: "52000000", Kind: "audio"})
	if len(out.Events) != 1 {
		t.Fatalf("expected the microphone hit returned, got %d", len(out.Events))
	}
	rec := out.Events[0]
	if rec.ID != pair[1].ID || rec.AudioSource != "microphone" {
		t.Fatalf("expected the microphone row %d to survive, got id %d source %q", pair[1].ID, rec.ID, rec.AudioSource)
	}
	if rec.AudioOrigin != "both" {
		t.Fatalf("audio_origin = %q, want both — provenance must describe the whole chunk", rec.AudioOrigin)
	}
	var sawSystem bool
	for _, tr := range rec.AudioTracks {
		if tr.AudioSource == "system" && tr.ID == pair[0].ID {
			sawSystem = true
		}
	}
	if !sawSystem {
		t.Fatalf("unmatched system row %d must appear in audio_tracks: %#v", pair[0].ID, rec.AudioTracks)
	}
}

func TestSearchEventsOverFetchKeepsPageFull(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	// 20 distinct chunks, each a system+mic pair with speech in both.
	for i := 0; i < 20; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		audioPair(t, ctx, s, at, fmt.Sprintf("system speech %d", i), fmt.Sprintf("microphone speech %d", i))
	}
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Kind: "audio", Limit: 10})
	if len(out.Events) != 10 {
		t.Fatalf("over-fetch must yield 10 survivors, not 5; got %d", len(out.Events))
	}
	if !strings.Contains(out.Notice, "capped") {
		t.Fatalf("a full collapsed page must still read as capped, got %q", out.Notice)
	}
}

// TestSearchEventsCapNoticeUnderOverFetch covers the two disjuncts of the cap
// condition separately, seeding audio pairs so collapse is actually exercised.
//
//	capped := len(out.Events) == limit || len(rawEvents) == fetchLimit
//
// The first disjunct fires whenever the collapsed page exactly fills the limit.
// The second is only *independently* reachable when 2*limit clamps against
// store.MaxSearchLimit, so fetchLimit < 2*limit and a saturated fetch can
// collapse to fewer survivors than the limit — that is the case (b) isolates.
func TestSearchEventsCapNoticeUnderOverFetch(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	// (a) exactly `limit` survivors: 20 chunks, limit 10 → 10 survivors == limit
	// via the first disjunct.
	for i := 0; i < 20; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		audioPair(t, ctx, s, at, fmt.Sprintf("system speech %d", i), fmt.Sprintf("microphone speech %d", i))
	}
	h := &handlers{store: s}

	a := callSearch(t, ctx, h, searchEventsInput{Kind: "audio", Limit: 10})
	if len(a.Events) != 10 || !strings.Contains(a.Notice, "capped") {
		t.Fatalf("(a) exactly-limit survivors must be capped: got %d events, notice %q", len(a.Events), a.Notice)
	}

	// (c) fewer rows than fetchLimit and fewer survivors than limit → not capped.
	// 20 chunks (40 rows), limit 100 → fetchLimit min(200,500)=200; rawEvents 40
	// != 200 and 20 survivors != 100, so neither disjunct fires.
	c := callSearch(t, ctx, h, searchEventsInput{Kind: "audio", Limit: 100})
	if len(c.Events) != 20 {
		t.Fatalf("(c) expected all 20 chunks, got %d", len(c.Events))
	}
	if strings.Contains(c.Notice, "capped") {
		t.Fatalf("(c) a page short of the limit with an unsaturated fetch must not be capped: %q", c.Notice)
	}

	// (b) saturated raw fetch, survivors < limit → capped by the second disjunct
	// alone. Seed enough pairs that a limit whose 2*limit exceeds the store
	// ceiling clamps fetchLimit to store.MaxSearchLimit. With limit 300,
	// fetchLimit = min(600,500) = 500; 260 pairs = 520 rows saturate that fetch
	// (rawEvents == 500), which collapse to ~250 survivors — short of 300, so the
	// first disjunct is false and only len(rawEvents) == fetchLimit remains.
	s2 := testStore(t)
	for i := 0; i < 260; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		audioPair(t, ctx, s2, at, fmt.Sprintf("system speech %d", i), fmt.Sprintf("microphone speech %d", i))
	}
	h2 := &handlers{store: s2}
	b := callSearch(t, ctx, h2, searchEventsInput{Kind: "audio", Limit: 300})
	if len(b.Events) >= 300 {
		t.Fatalf("(b) expected a page short of the limit (survivors < 300), got %d", len(b.Events))
	}
	if !strings.Contains(b.Notice, "capped") {
		t.Fatalf("(b) a saturated raw fetch must be capped via the second disjunct: got %d events, notice %q", len(b.Events), b.Notice)
	}
}
