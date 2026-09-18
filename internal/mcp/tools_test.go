package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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
	if got.App != "Safari" || got.Window != "Quarterly plan" || got.MediaFile != "a.jpg" {
		t.Fatalf("unexpected record: %#v", got)
	}
	if out.MediaDir["screen"] != "/tmp" {
		t.Fatalf("media_dir must hoist the screen directory: %#v", out.MediaDir)
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
	if out.Events[0].MediaFile != "body.jpg" {
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
	if len(out.Events) != 1 || out.Events[0].MediaFile != "speech.wav" {
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
	encoded, err := json.Marshal(newEventRecord(store.Event{Kind: store.KindScreen, Text: "short"}, 600, nil))
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
		// display_id and text_source are here as well as in their own columns:
		// the denylist has to drop the copies without touching a key it has
		// never heard of, which is what ocr_ms and focused_window_text stand for.
		Metadata: []byte(`{"ocr_ms":42,"focused_window_text":"plan","display_id":1,"text_source":"vision"}`),
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
	for _, key := range []string{"display_id", "text_source"} {
		if _, present := out.Event.Metadata[key]; present {
			t.Fatalf("metadata %q duplicates a top-level field: %#v", key, out.Event.Metadata)
		}
	}
	if out.Event.MediaFile != "a.jpg" || out.MediaDir != "/tmp" {
		t.Fatalf("media_dir %q + media_file %q must rejoin to the stored path",
			out.MediaDir, out.Event.MediaFile)
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

// Since audio chunks began carrying the focused application, one app's entry
// sums two modalities that answer different questions: which application the
// screen text was read from, and which one merely happened to be focused while
// unrelated sound was captured. Without kind an agent cannot tell them apart,
// and list_apps exists precisely so filter values are not guesses.
func TestListAppsSeparatesScreenFromAudioByKind(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: base, App: "Zed", Window: "notes.md", Text: "a", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: base.Add(time.Minute), App: "Zed", Window: "notes.md", Text: "b", MediaPath: "/tmp/b.jpg"},
		store.Event{Kind: store.KindAudio, CapturedAt: base.Add(2 * time.Minute), App: "Zed", Window: "notes.md", Text: "narration", MediaPath: "/tmp/c.wav"},
	)
	h := &handlers{store: s}

	_, out, err := h.listApps(ctx, nil, listAppsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 || out.Entries[0].Events != 3 {
		t.Fatalf("an omitted kind must span both modalities: %#v", out.Entries)
	}

	_, out, err = h.listApps(ctx, nil, listAppsInput{Kind: "screen"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 || out.Entries[0].App != "Zed" || out.Entries[0].Events != 2 {
		t.Fatalf(`kind "screen" must count only screen events: %#v`, out.Entries)
	}

	_, out, err = h.listApps(ctx, nil, listAppsInput{Kind: "audio"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 || out.Entries[0].App != "Zed" || out.Entries[0].Events != 1 {
		t.Fatalf(`kind "audio" must count only audio events: %#v`, out.Entries)
	}

	// Window mode narrows the same way, so an agent that found an app in the
	// screen inventory can ask which windows it was actually read from.
	app := "Zed"
	_, out, err = h.listApps(ctx, nil, listAppsInput{App: &app, Kind: "screen"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 || out.Entries[0].Window != "notes.md" || out.Entries[0].Events != 2 {
		t.Fatalf("kind must narrow window mode too: %#v", out.Entries)
	}

	if _, _, err := h.listApps(ctx, nil, listAppsInput{Kind: "video"}); err == nil {
		t.Fatal("an unknown kind must be a tool error, not a silent fall-through to both")
	}
}

// A kind that matches nothing must not be explained as a time-range problem:
// the remedy is to drop the kind, not to widen since/until.
func TestListAppsNoticeNamesTheKindFilter(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s, store.Event{Kind: store.KindScreen,
		CapturedAt: time.Now().UTC().Truncate(time.Second), App: "Zed", Text: "a", MediaPath: "/tmp/a.jpg"})
	h := &handlers{store: s}

	_, out, err := h.listApps(ctx, nil, listAppsInput{Kind: "audio"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 0 {
		t.Fatalf("expected no entries, got %#v", out.Entries)
	}
	if !strings.Contains(out.Notice, "kind") {
		t.Fatalf("notice = %q, want it to name the kind filter as a cause", out.Notice)
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
		// Distinct text per frame: these three exercise the cap notice, and
		// collapsing is now the default, so identical screens would fold to one
		// and the counts under test would be the fold's rather than the cap's.
		insertEvents(t, ctx, s, store.Event{
			Kind: store.KindScreen, CapturedAt: base.Add(time.Duration(i) * time.Second),
			Text: fmt.Sprintf("roadmap milestone %d", i), MediaPath: "/tmp/a.jpg",
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
		insertEvents(t, ctx, s, store.Event{Kind: store.KindScreen,
			Text: fmt.Sprintf("roadmap milestone %d", i), MediaPath: "/tmp/a.jpg"})
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

// audioPair inserts a system+microphone pair sharing one captured_at — the
// stamp both tracks of a chunk are written with — and returns the two events.
func audioPair(t *testing.T, ctx context.Context, s *store.Store, at time.Time, systemText, micText string) []store.Event {
	t.Helper()
	return insertEvents(t, ctx, s,
		store.Event{Kind: store.KindAudio, CapturedAt: at, AudioSource: "system", Text: systemText, MediaPath: "/tmp/sys.wav", DurationMS: 30000},
		store.Event{Kind: store.KindAudio, CapturedAt: at, AudioSource: "microphone", Text: micText, MediaPath: "/tmp/mic.wav", DurationMS: 30000},
	)
}

// TestSearchEventsKeepsBothTracksOfAChunk is the regression for the defect that
// removed audio collapse. search_events used to merge a chunk's two rows into
// one on the strength of a shared captured_at and return only the system track,
// under a notice calling them "duplicate audio events". A shared captured_at is
// a shared 30-second *interval*, never evidence of a shared *sound*: reported
// from a live index, the microphone was carrying an entirely separate talk and
// every word of it was dropped, while audio_tracks reported its text_length and
// not its text. The result read as finished, which is what made it worse than
// returning both rows.
//
// Both rows must come back, each with its own text and its own audio_source, and
// nothing may describe them as duplicates. Whether one track re-recorded the
// other is internal/transcript's question, decided per segment against word
// timings and an energy envelope, and reaching an agent through get_transcript.
func TestSearchEventsKeepsBothTracksOfAChunk(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	const (
		systemText = "the quarterly revenue review starts on the next slide"
		micText    = "the cluster manager talks to the control plane not the data plane"
	)
	pair := audioPair(t, ctx, s, base, systemText, micText)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Kind: "audio"})
	if len(out.Events) != 2 {
		t.Fatalf("both tracks of a chunk must be returned, got %d", len(out.Events))
	}

	bySource := map[string]EventRecord{}
	for _, rec := range out.Events {
		bySource[rec.AudioSource] = rec
	}
	for _, want := range []struct {
		source, text string
		id           int64
	}{
		{"system", systemText, pair[0].ID},
		{"microphone", micText, pair[1].ID},
	} {
		rec, ok := bySource[want.source]
		if !ok {
			t.Fatalf("no %s row in the results: %#v", want.source, out.Events)
		}
		if rec.ID != want.id {
			t.Errorf("%s row id = %d, want %d", want.source, rec.ID, want.id)
		}
		// The whole point: the text is present, not reduced to a length.
		if rec.Text != want.text {
			t.Errorf("%s text = %q, want %q", want.source, rec.Text, want.text)
		}
	}
	if strings.Contains(out.Notice, "duplicate") || strings.Contains(out.Notice, "collaps") {
		t.Fatalf("nothing may call the two tracks duplicates; notice = %q", out.Notice)
	}
}

// TestSearchEventsMicrophoneOnlyHitIsNotReplaced pins the other half: a query
// landing only in the microphone transcript returns that row. It used to be at
// risk from a collapse consulting the non-matching system row and judging it the
// better copy, which would drop the only hit.
//
// It is also the case audioProvenanceContract must not mis-describe. One row of
// a pair arriving alone is ordinary, so the description says the pair is never
// *merged* rather than that both are *returned* — the second reading would let
// an agent take this single row for the whole chunk.
func TestSearchEventsMicrophoneOnlyHitIsNotReplaced(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	pair := audioPair(t, ctx, s, base,
		"revenue was fifty two million dollars",
		"the mic transcribed 52000000 as digits")
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Query: "52000000", Kind: "audio"})
	if len(out.Events) != 1 {
		t.Fatalf("expected the microphone hit alone, got %d", len(out.Events))
	}
	rec := out.Events[0]
	if rec.ID != pair[1].ID || rec.AudioSource != "microphone" {
		t.Fatalf("expected microphone row %d, got id %d source %q", pair[1].ID, rec.ID, rec.AudioSource)
	}
	if rec.Text != "the mic transcribed 52000000 as digits" {
		t.Fatalf("microphone text = %q, want it verbatim", rec.Text)
	}
}

// TestSearchEventsRequireTextReturnsLoneTracks is the measurement behind the
// wording of audioProvenanceContract. Every filter search_events applies is a
// predicate on one row, so require_text keeps a chunk's speaking track and drops
// its silent one — and a silent system track beside a speaking microphone is the
// common case, not an edge case. Measured on a live index, a require_text window
// returned 20 chunks and every one of them as a single row.
//
// Nothing may therefore promise that both rows of a pair are returned. The
// guarantee is that they are never merged.
func TestSearchEventsRequireTextReturnsLoneTracks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		// A silent system track beside a speaking microphone: the shape a quiet
		// room with the speakers idle actually records.
		audioPair(t, ctx, s, base.Add(time.Duration(i)*time.Second),
			"", fmt.Sprintf("someone in the room said something %d", i))
	}
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Kind: "audio", RequireText: true})
	if len(out.Events) != 3 {
		t.Fatalf("expected the three speaking tracks, got %d", len(out.Events))
	}
	for _, rec := range out.Events {
		if rec.AudioSource != "microphone" {
			t.Errorf("require_text should leave only the speaking track, got %q", rec.AudioSource)
		}
	}

	// Same rows, no require_text: the silent counterparts come back too, which
	// is what makes this a property of the filter rather than of the index.
	all := callSearch(t, ctx, h, searchEventsInput{Kind: "audio"})
	if len(all.Events) != 6 {
		t.Fatalf("without require_text both tracks of each chunk should be present, got %d", len(all.Events))
	}
}

// TestSearchEventsCapNoticeFiresOnAFullAudioPage pins the cap notice over audio
// rows, which now reach the page one per track rather than one per chunk.
func TestSearchEventsCapNoticeFiresOnAFullAudioPage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	// 20 chunks = 40 rows, so a limit of 10 is comfortably saturated.
	for i := 0; i < 20; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		audioPair(t, ctx, s, at, fmt.Sprintf("system speech %d", i), fmt.Sprintf("microphone speech %d", i))
	}
	h := &handlers{store: s}

	full := callSearch(t, ctx, h, searchEventsInput{Kind: "audio", Limit: 10})
	if len(full.Events) != 10 {
		t.Fatalf("expected a full page of 10, got %d", len(full.Events))
	}
	if !strings.Contains(full.Notice, "capped") {
		t.Fatalf("a full page must read as capped, got %q", full.Notice)
	}

	// A page short of the limit must not.
	short := callSearch(t, ctx, h, searchEventsInput{Kind: "audio", Limit: 100})
	if len(short.Events) != 40 {
		t.Fatalf("expected all 40 rows, got %d", len(short.Events))
	}
	if strings.Contains(short.Notice, "capped") {
		t.Fatalf("a page short of the limit must not read as capped: %q", short.Notice)
	}
}

// TestAudioRecordsRenameAppToForeground pins the boundary rename. The SQL columns
// keep their names — FTS5 and the app filter depend on them — so this is the only
// place an agent is told that an audio row's app is a focus field and not a
// source field.
func TestAudioRecordsRenameAppToForeground(t *testing.T) {
	audio := newEventRecord(store.Event{
		ID: 1, Kind: store.KindAudio, CapturedAt: time.Now(), App: "Ghostty", Window: "lumi — zsh",
		AudioSource: "system", AudioAttribution: string(store.AttributionEmittingProcess),
	}, 0, nil)
	if audio.App != "" || audio.Window != "" {
		t.Fatalf("an audio record still carries app/window: %q/%q", audio.App, audio.Window)
	}
	if audio.ForegroundApp != "Ghostty" || audio.ForegroundWindow != "lumi — zsh" {
		t.Fatalf("audio foreground fields = %q/%q", audio.ForegroundApp, audio.ForegroundWindow)
	}

	screen := newEventRecord(store.Event{
		ID: 2, Kind: store.KindScreen, CapturedAt: time.Now(), App: "Zed", Window: "lumi — .env",
	}, 0, nil)
	if screen.App != "Zed" || screen.Window != "lumi — .env" {
		t.Fatalf("screen record lost app/window: %q/%q", screen.App, screen.Window)
	}
	if screen.ForegroundApp != "" || screen.Attribution != "" {
		t.Fatal("a screen record must not carry audio attribution fields")
	}
}

// TestMicrophoneRecordCarriesNoSourceApp is acceptance criterion 2 at the wire.
func TestMicrophoneRecordCarriesNoSourceApp(t *testing.T) {
	record := newEventRecord(store.Event{
		ID: 1, Kind: store.KindAudio, CapturedAt: time.Now(), App: "Ghostty",
		AudioSource: "microphone", AudioAttribution: string(store.AttributionUnattributed),
	}, 0, nil)
	if record.Attribution != string(store.AttributionUnattributed) {
		t.Fatalf("microphone attribution = %q", record.Attribution)
	}
	if len(record.SourceApp) != 0 {
		t.Fatalf("microphone record named a source: %#v", record.SourceApp)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "\"source_app\"") {
		t.Fatalf("microphone record serialized a source_app key: %s", encoded)
	}
}

// TestSourceAppReachesTheWire covers the emitting-process path end to end through
// the record conversion.
func TestSourceAppReachesTheWire(t *testing.T) {
	apps, err := store.EncodeSourceApps([]store.SourceApp{{
		PID: 812, BundleID: "com.perplexity.comet", Name: "Comet",
		Evidence: store.EvidenceProcess, Samples: 11, Observations: 12,
	}})
	if err != nil {
		t.Fatal(err)
	}
	record := newEventRecord(store.Event{
		ID: 1, Kind: store.KindAudio, CapturedAt: time.Now(), App: "Ghostty",
		AudioSource: "system", AudioAttribution: string(store.AttributionEmittingProcess),
		SourceApps: apps,
	}, 0, nil)
	if len(record.SourceApp) != 1 || record.SourceApp[0].Name != "Comet" {
		t.Fatalf("source_app = %#v, want Comet", record.SourceApp)
	}
	if record.SourceApp[0].Evidence != store.EvidenceProcess {
		t.Fatalf("evidence = %q", record.SourceApp[0].Evidence)
	}
	// 11 of 12 samples is the case the pair existed for and the one live data
	// never shows: an app present for most of a chunk but not all of it.
	if record.SourceApp[0].Presence != 0.92 {
		t.Fatalf("presence = %v, want 11/12 rounded to 0.92: %#v",
			record.SourceApp[0].Presence, record.SourceApp[0])
	}
	// The pid is the store's, and stops here.
	encoded, err := json.Marshal(record.SourceApp[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pid", "samples", "observations"} {
		if strings.Contains(string(encoded), `"`+key+`"`) {
			t.Fatalf("source_app still serializes %q: %s", key, encoded)
		}
	}
	// The emitter and the focused app must remain separate answers.
	if record.ForegroundApp != "Ghostty" {
		t.Fatalf("foreground_app = %q, want Ghostty", record.ForegroundApp)
	}
}

// TestToolDescriptionsStateTheMicrophoneCaveat pins the provenance contract to
// where it is now delivered, and pins the saving that moving it bought.
//
// It used to assert these phrases were in search_events' description, which
// meant every client loaded 1753 characters on every tools/list — a third of
// Lumi's whole description payload — including the ones that only ever read
// screen text. They now ride the notice of a page that actually holds an audio
// row, so the test is two-sided: present when there is an audio row to explain,
// and ABSENT otherwise. The absence half is the one that fails if the contract
// creeps back into the description, which is the only way the saving is lost.
func TestToolDescriptionsStateTheMicrophoneCaveat(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	pair := audioPair(t, ctx, s, base, "the quarterly numbers are on the next slide", "and the room agreed")
	screenRows := insertEvents(t, ctx, s, store.Event{
		Kind: store.KindScreen, CapturedAt: base.Add(time.Minute),
		App: "Comet", Text: "an invoice for eleven thousand dollars", MediaPath: "/tmp/a.jpg",
	})
	h := &handlers{store: s}

	audio := callSearch(t, ctx, h, searchEventsInput{Kind: "audio"}).Notice
	for _, required := range []string{
		"audio_source is the capture DEVICE",
		"source_app",
		"foreground_app",
		"emitting_process",
		"Microphone audio has NO reliable source",
		"other people present",
		// The pairing clauses. Merging a chunk's two rows on a shared timestamp
		// once discarded a whole microphone transcript while the result still
		// read as complete, so the contract has to say both that a pair
		// shares an interval rather than a sound and that neither row may be
		// treated as the other's duplicate.
		"Audio rows come in PAIRS sharing one captured_at",
		"not necessarily a sound",
		"never assume one row of a pair is redundant",
		// And it must not over-correct into promising delivery of both rows.
		// Filters are per-row, so a lone row is routine — see
		// TestSearchEventsMicrophoneOnlyHitIsNotReplaced. Claiming both arrive
		// would rebuild the original defect from the other side, with an agent
		// reading one row as the whole chunk.
		"applied per ROW",
		"is NOT evidence that the chunk held one track",
	} {
		if !strings.Contains(audio, required) {
			t.Errorf("a page holding audio omits %q from its notice", required)
		}
	}

	// The saving. A screen-only page explains nothing about audio, and no
	// description carries the contract either — that is the whole point of the
	// move, and a description that quietly regained it would still pass the half
	// above.
	screen := callSearch(t, ctx, h, searchEventsInput{Kind: "screen"}).Notice
	for _, forbidden := range []string{"audio_source is the capture DEVICE", "emitting_process",
		"Audio rows come in PAIRS sharing one captured_at"} {
		if strings.Contains(screen, forbidden) {
			t.Errorf("a screen-only page still pays for %q in its notice", forbidden)
		}
	}
	for _, name := range []string{"search_events", "get_event", "list_apps", "get_transcript"} {
		if strings.Contains(findToolDescription(t, name), "audio_source is the capture DEVICE") {
			t.Errorf("%s description carries the provenance contract again; it belongs in the notice", name)
		}
	}

	// get_event renders the same three fields and is where an agent goes for the
	// complete version of a row it half-read, so it carries the contract too —
	// and only for an audio event.
	_, audioEvent, err := h.getEvent(ctx, nil, getEventInput{ID: pair[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(audioEvent.Notice, "Microphone audio has NO reliable source") {
		t.Errorf("get_event on an audio event omits the contract: %q", audioEvent.Notice)
	}
	_, screenEvent, err := h.getEvent(ctx, nil, getEventInput{ID: screenRows[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(screenEvent.Notice, "audio_source is the capture DEVICE") {
		t.Errorf("get_event on a screen event pays for the contract: %q", screenEvent.Notice)
	}

	// Every tool that can surface audio still has to name the field an agent
	// will see, so it knows there is something to read about.
	for _, name := range []string{"get_event", "list_apps", "get_transcript"} {
		description := findToolDescription(t, name)
		if !strings.Contains(description, "source_app") {
			t.Errorf("%s description never mentions source_app", name)
		}
	}
	// get_transcript states the ambiguity as a fact about the row rather than a
	// rule about what the caller may conclude: the microphone records the room,
	// and what it caught may be a person or anything else audible. search_events
	// hands that to the notice with the rest of the contract; get_event keeps its
	// own sentence because a microphone event is the whole of its answer.
	for _, name := range []string{"get_event", "get_transcript"} {
		description := findToolDescription(t, name)
		if !strings.Contains(description, "other people present") {
			t.Errorf("%s description never says what microphone audio may have caught", name)
		}
	}
}

// TestSearchEventsCollapseNeverFoldsAnAudioPair is the regression this whole
// feature was constrained to avoid, and the collapse_similar sibling of
// TestSearchEventsKeepsBothTracksOfAChunk.
//
// The pair here is the worst case on purpose: same captured_at, same app, same
// window, same display_id (audio's is always 0), and near-identical transcripts
// — which is the MAJORITY case on the live index, where all 967 audio pairs
// share those fields and their transcripts are median 0.819 similar because the
// microphone re-records what the speakers played. Keyed on those fields alone a
// collapse merges them, and collapsed_ids would preserve reachability but not
// content: the microphone's account of the room leaves the visible result.
func TestSearchEventsCollapseNeverFoldsAnAudioPair(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	const (
		systemText = "the quarterly revenue review starts on the next slide please"
		micText    = "the quarterly revenue review starts on the next slide okay"
	)
	pair := audioPair(t, ctx, s, base, systemText, micText)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Kind: "audio", CollapseSimilar: true})
	if len(out.Events) != 2 {
		t.Fatalf("collapse_similar folded an audio pair into %d row(s); both tracks must survive", len(out.Events))
	}
	bySource := map[string]EventRecord{}
	for _, rec := range out.Events {
		if len(rec.CollapsedIDs) != 0 || rec.CollapsedCount != 0 {
			t.Fatalf("an audio row carries collapse metadata: %#v", rec)
		}
		bySource[rec.AudioSource] = rec
	}
	for _, want := range []struct {
		source, text string
		id           int64
	}{
		{"system", systemText, pair[0].ID},
		{"microphone", micText, pair[1].ID},
	} {
		rec, ok := bySource[want.source]
		if !ok {
			t.Fatalf("no %s row survived the collapse: %#v", want.source, out.Events)
		}
		if rec.ID != want.id || rec.Text != want.text {
			t.Errorf("%s row = (%d, %q), want (%d, %q)", want.source, rec.ID, rec.Text, want.id, want.text)
		}
	}
}

// screenRun inserts count near-identical adjacent screen events for one app.
func screenRun(t *testing.T, ctx context.Context, s *store.Store, base time.Time, app string, count int) []store.Event {
	t.Helper()
	events := make([]store.Event, 0, count)
	for i := 0; i < count; i++ {
		events = append(events, store.Event{
			Kind: store.KindScreen, CapturedAt: base.Add(time.Duration(i) * time.Second),
			App: app, Window: app + " — main", DisplayID: 1,
			Text: fmt.Sprintf("File Edit View Window Help the deployment pipeline is green %d", i),
		})
	}
	return insertEvents(t, ctx, s, events...)
}

// TestSearchEventsCollapsesByDefault pins the default and its escape hatch
// together. The fold used to be opt-in, and an agent that did not know to ask
// for it paid for the same screen four times over; 76% of adjacent same-app
// pairs are more than 0.9 identical, so off-by-default was the wrong way round.
// Nothing becomes unreachable — collapsed_ids names every folded row — and
// expand_similar returns the page unfolded for a caller that wants each frame.
func TestSearchEventsCollapsesByDefault(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	screenRun(t, ctx, s, time.Now().UTC().Truncate(time.Second), "Ghostty", 3)
	h := &handlers{store: s}

	folded := callSearch(t, ctx, h, searchEventsInput{})
	if len(folded.Events) != 1 {
		t.Fatalf("default search returned %d events, want the run folded to 1", len(folded.Events))
	}
	if folded.Events[0].CollapsedCount != 2 {
		t.Fatalf("representative folded %d rows, want 2", folded.Events[0].CollapsedCount)
	}

	expanded := callSearch(t, ctx, h, searchEventsInput{ExpandSimilar: true})
	if len(expanded.Events) != 3 {
		t.Fatalf("expand_similar returned %d events, want all 3", len(expanded.Events))
	}
	for _, rec := range expanded.Events {
		if len(rec.CollapsedIDs) != 0 || rec.CollapsedCount != 0 {
			t.Fatalf("a record carries collapse metadata under expand_similar: %#v", rec)
		}
	}
}

// TestSearchEventsHonoursTheDeprecatedCollapseFlag: collapse_similar outlives
// its own meaning because a cached tools/list is not a hypothetical here —
// `lumi mcp` replaces its own image mid-session while the client keeps the tool
// list it has, and additionalProperties: false turns an unknown field into a
// failed call rather than an ignored argument.
func TestSearchEventsHonoursTheDeprecatedCollapseFlag(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	screenRun(t, ctx, s, time.Now().UTC().Truncate(time.Second), "Ghostty", 3)

	// Over the protocol, not through the handler: the point of keeping the field
	// is that the SDK validates arguments against the generated schema before a
	// handler ever runs, and additionalProperties: false rejects a name the
	// schema does not carry. Calling the handler directly would pass whether the
	// field were advertised or not.
	session := connect(t, ctx, s)
	res, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "search_events",
		Arguments: map[string]any{"collapse_similar": true},
	})
	if err != nil {
		t.Fatalf("collapse_similar was rejected by the generated schema: %v", err)
	}
	if res.IsError {
		t.Fatalf("collapse_similar returned a tool error: %v", res.Content)
	}
	var out searchEventsOutput
	encoded, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 {
		t.Fatalf("collapse_similar: true returned %d events, want the fold it asks for", len(out.Events))
	}
}

// TestSearchEventsCollapseCarriesEveryDroppedID: the fold is only safe because
// nothing is silently lost — get_event must still reach every dropped row.
func TestSearchEventsCollapseCarriesEveryDroppedID(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	run := screenRun(t, ctx, s, base, "Ghostty", 3)
	other := insertEvents(t, ctx, s, store.Event{
		Kind: store.KindScreen, CapturedAt: base.Add(10 * time.Second),
		App: "Comet", Window: "Comet — main", DisplayID: 1,
		Text: "a completely different page about invoices and billing",
	})
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{})
	if len(out.Events) != 2 {
		t.Fatalf("collapse returned %d records, want 2 (one per app)", len(out.Events))
	}
	byApp := map[string]EventRecord{}
	for _, rec := range out.Events {
		byApp[rec.App] = rec
	}
	rep, ok := byApp["Ghostty"]
	if !ok {
		t.Fatalf("no Ghostty representative: %#v", out.Events)
	}
	if rep.CollapsedCount != 2 || len(rep.CollapsedIDs) != 2 {
		t.Fatalf("representative folded %d rows (%v), want 2", rep.CollapsedCount, rep.CollapsedIDs)
	}
	// Browse order is captured_at DESC, so the newest row represents the run and
	// the two older ids are the ones folded into it.
	reachable := map[int64]bool{rep.ID: true}
	for _, id := range rep.CollapsedIDs {
		reachable[id] = true
	}
	for _, event := range run {
		if !reachable[event.ID] {
			t.Errorf("event %d is unreachable: neither returned nor named in collapsed_ids", event.ID)
		}
	}
	if solo, ok := byApp["Comet"]; !ok || solo.ID != other[0].ID || solo.CollapsedCount != 0 {
		t.Fatalf("a row from a different app must survive as its own record: %#v", byApp)
	}
}

// TestSearchEventsCollapsedNoticeReportsBothCounts: the collapse runs after
// LIMIT, so a notice naming one number contradicts its own payload — "capped at
// 20" beside six events, or "capped at 6" inventing a cap nothing enforced.
func TestSearchEventsCollapsedNoticeReportsBothCounts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	screenRun(t, ctx, s, time.Now().UTC().Truncate(time.Second), "Ghostty", 4)
	h := &handlers{store: s}

	out := callSearch(t, ctx, h, searchEventsInput{Limit: 4})
	if len(out.Events) != 1 {
		t.Fatalf("collapse returned %d records, want 1", len(out.Events))
	}
	if !strings.Contains(out.Notice, "4 fetched, collapsed to 1") {
		t.Fatalf("notice must report both counts honestly, got %q", out.Notice)
	}
	// The page boundary is a cursor, not advice to guess at a time window.
	if !strings.Contains(out.Notice, "cursor=next_cursor") {
		t.Fatalf("a capped notice must point at the cursor, got %q", out.Notice)
	}
	if out.NextCursor == "" {
		t.Fatal("a capped page must carry next_cursor")
	}
}

// TestSearchEventsExcerptCentersOnTheMatch is change 1 end to end: the term the
// row matched on must be in what the agent actually receives.
func TestSearchEventsExcerptCentersOnTheMatch(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	text := strings.Repeat("File Edit View Assistant History Bookmarks ", 40) + "the invoice total is 4471 dollars"
	insertEvents(t, ctx, s, store.Event{
		Kind: store.KindScreen, CapturedAt: time.Now().UTC(), App: "Comet", Text: text,
	})
	h := &handlers{store: s}

	cap := 200
	out := callSearch(t, ctx, h, searchEventsInput{Query: "invoice", MaxTextChars: &cap})
	if len(out.Events) != 1 {
		t.Fatalf("expected one hit, got %d", len(out.Events))
	}
	rec := out.Events[0]
	if !strings.Contains(strings.ToLower(rec.Text), "invoice") {
		t.Fatalf("the excerpt does not contain the term the row matched on: %q", rec.Text)
	}
	if !rec.Truncated {
		t.Fatal("truncated = false on a centred excerpt of a longer text")
	}
	if want := utf8.RuneCountInString(text); rec.TextLength != want {
		t.Fatalf("text_length = %d, want the whole text's %d", rec.TextLength, want)
	}
}

// TestSearchEventsCursorWalksATieGroup is the test the old `until=` cursor
// could not have passed. captured_at is not unique — a chunk's two audio tracks
// share one by construction, and 21% of live rows share theirs — and the old
// bound was inclusive, so the boundary group repeated on every page and a tie
// group larger than limit could not advance at all. The cursor is a keyset on
// (captured_at, id), so the walk is exact.
func TestSearchEventsCursorWalksATieGroup(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	shared := time.Now().UTC().Truncate(time.Second)
	// Five rows on one timestamp, wider than the page, plus an older row to
	// prove the walk leaves the group rather than stalling in it.
	for i := 0; i < 5; i++ {
		insertEvents(t, ctx, s, store.Event{
			Kind: store.KindScreen, CapturedAt: shared, App: "Comet",
			Text: fmt.Sprintf("invoice line item %d", i), MediaPath: "/tmp/a.jpg",
		})
	}
	insertEvents(t, ctx, s, store.Event{
		Kind: store.KindScreen, CapturedAt: shared.Add(-time.Hour), App: "Comet",
		Text: "an older page about shipping", MediaPath: "/tmp/a.jpg",
	})
	h := &handlers{store: s}

	seen := []int64{}
	cursor := ""
	for page := 0; page < 10; page++ {
		// ExpandSimilar so the walk is purely about paging: a fold mid-page would
		// reorder what a page contains without changing what it advanced past,
		// and this test is not the one that covers that.
		out := callSearch(t, ctx, h, searchEventsInput{Limit: 2, Cursor: cursor, ExpandSimilar: true})
		for _, rec := range out.Events {
			seen = append(seen, rec.ID)
		}
		if out.NextCursor == "" {
			break
		}
		if out.NextCursor == cursor {
			t.Fatal("the cursor did not advance")
		}
		cursor = out.NextCursor
	}

	want := callSearch(t, ctx, h, searchEventsInput{Limit: 500, ExpandSimilar: true})
	if len(want.Events) != 6 {
		t.Fatalf("fixture returned %d events unpaginated, want 6", len(want.Events))
	}
	if len(seen) != len(want.Events) {
		t.Fatalf("the walk saw %d ids (%v), want %d — a gap or a repeat", len(seen), seen, len(want.Events))
	}
	for i, rec := range want.Events {
		if seen[i] != rec.ID {
			t.Fatalf("the walk returned %v; position %d is event %d, want %d — "+
				"paging must not reorder or skip", seen, i, seen[i], rec.ID)
		}
	}
}

// TestSearchEventsRankedCursorPagesWithoutRepeating covers the other ordering.
// Ranked mode has no stored key to resume from, so it counts rows; the contract
// is only that a walk neither repeats nor skips while the index is still.
func TestSearchEventsRankedCursorPagesWithoutRepeating(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 4; i++ {
		insertEvents(t, ctx, s, store.Event{
			Kind: store.KindScreen, CapturedAt: base.Add(time.Duration(i) * time.Minute),
			App: "Comet", Text: fmt.Sprintf("the invoice total on page %d", i), MediaPath: "/tmp/a.jpg",
		})
	}
	h := &handlers{store: s}

	seen := map[int64]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		out := callSearch(t, ctx, h, searchEventsInput{Query: "invoice", Limit: 2, Cursor: cursor})
		for _, rec := range out.Events {
			if seen[rec.ID] {
				t.Fatalf("ranked paging returned event %d twice", rec.ID)
			}
			seen[rec.ID] = true
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	if len(seen) != 4 {
		t.Fatalf("ranked walk saw %d of 4 events", len(seen))
	}
}

// TestSearchEventsRejectsACursorFromTheOtherOrdering: browse resumes from a
// timestamp and ranked from a row count, so a cursor crossing between them
// names no place in the search it is handed to. Passing it through would return
// a narrowed range that looks exactly like a page.
func TestSearchEventsRejectsACursorFromTheOtherOrdering(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		insertEvents(t, ctx, s, store.Event{
			Kind: store.KindScreen, CapturedAt: base.Add(time.Duration(i) * time.Minute),
			App: "Comet", Text: fmt.Sprintf("the invoice total on page %d", i), MediaPath: "/tmp/a.jpg",
		})
	}
	h := &handlers{store: s}

	browse := callSearch(t, ctx, h, searchEventsInput{Limit: 1})
	if browse.NextCursor == "" {
		t.Fatal("a capped browse page must carry next_cursor")
	}
	if _, _, err := h.searchEvents(ctx, nil, searchEventsInput{Query: "invoice", Cursor: browse.NextCursor}); err == nil {
		t.Fatal("a browse cursor was accepted by a ranked search")
	}
	if _, _, err := h.searchEvents(ctx, nil, searchEventsInput{Cursor: "not-a-cursor"}); err == nil {
		t.Fatal("a malformed cursor was accepted")
	}
}

// TestSearchEventsCursorPinsARelativeWindow is the defect a review found in the
// first cut of the cursor: `since: "1h"` resolves against the clock on every
// call, so a walk that resends the same arguments walks a window sliding out
// from under it. Ranked mode pages with an OFFSET counted in the previous
// result set, so a row falling off the old end of the window makes that offset
// skip a row that still matches — silently, and with no error anywhere.
//
// The cursor therefore pins the window its first page was computed in. This
// fixture puts one row a fraction of a second inside a 1h bound so it expires
// between the two calls.
func TestSearchEventsCursorPinsARelativeWindow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC()
	rows := insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: now.Add(-time.Hour + 900*time.Millisecond),
			App: "Comet", Text: "invoice", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: now.Add(-30 * time.Minute),
			App: "Comet", Text: "invoice plus several extra words that rank this one lower",
			MediaPath: "/tmp/b.jpg"},
	)
	h := &handlers{store: s}

	first := callSearch(t, ctx, h, searchEventsInput{Query: "invoice", Since: "1h", Limit: 1})
	if len(first.Events) != 1 || first.NextCursor == "" {
		t.Fatalf("page 1: %d events, cursor %q", len(first.Events), first.NextCursor)
	}

	// The 1h bound now excludes the older row. Without the pin, the offset of 1
	// is applied to a one-row result set and returns nothing.
	time.Sleep(1200 * time.Millisecond)

	second := callSearch(t, ctx, h,
		searchEventsInput{Query: "invoice", Since: "1h", Limit: 1, Cursor: first.NextCursor})
	seen := map[int64]bool{first.Events[0].ID: true}
	for _, rec := range second.Events {
		seen[rec.ID] = true
	}
	for _, row := range rows {
		if !seen[row.ID] {
			t.Fatalf("event %d matches and was inside the window the walk started in, "+
				"but no page returned it; seen %v", row.ID, seen)
		}
	}
}

// TestSearchEventsExhaustedCursorDoesNotBlameTheFilters: a walk whose last page
// exactly fills the limit still gets a cursor, so there is always one more call
// returning nothing. That is pagination finishing. Answering it with the
// no-match notice sends an agent to widen a time range or drop an app filter
// that were working correctly the whole time.
func TestSearchEventsExhaustedCursorDoesNotBlameTheFilters(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, CapturedAt: base, App: "Comet",
			Text: "alpha one", MediaPath: "/tmp/a.jpg"},
		store.Event{Kind: store.KindScreen, CapturedAt: base.Add(-time.Minute), App: "Comet",
			Text: "beta two", MediaPath: "/tmp/b.jpg"},
	)
	h := &handlers{store: s}

	first := callSearch(t, ctx, h, searchEventsInput{Limit: 2})
	if len(first.Events) != 2 || first.NextCursor == "" {
		t.Fatalf("page 1: %d events, cursor %q", len(first.Events), first.NextCursor)
	}
	last := callSearch(t, ctx, h, searchEventsInput{Limit: 2, Cursor: first.NextCursor})
	if len(last.Events) != 0 {
		t.Fatalf("page 2 returned %d events, want none", len(last.Events))
	}
	if strings.Contains(last.Notice, "widening") || strings.Contains(last.Notice, "matched these filters") {
		t.Fatalf("an exhausted cursor blames the filters: %q", last.Notice)
	}
	if !strings.Contains(last.Notice, "end of the results") {
		t.Fatalf("an exhausted cursor must say the walk is done, got %q", last.Notice)
	}
}
