package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// staleHandlers builds handlers reporting a binary that has been replaced.
func staleHandlers(t *testing.T, s *store.Store, selfUpdating bool) *handlers {
	t.Helper()
	return &handlers{
		store:         s,
		version:       "v0.1.0",
		binaryChanged: func() bool { return true },
		selfUpdating:  selfUpdating,
	}
}

func screenEvent(at time.Time, text string) store.Event {
	return store.Event{
		Kind: store.KindScreen, CapturedAt: at, Text: text,
		App: "Zed", Window: "notes.md", MediaPath: "/tmp/screens/a.png",
	}
}

// A replaced binary is the failure this whole mechanism exists for: the process
// keeps serving old code and nothing anywhere says so. The notice is the only
// channel that can report it, so it has to reach a normal, non-empty result.
func TestSearchReportsAnUpgradedBinaryOnASuccessfulResult(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s, screenEvent(time.Now(), "quarterly planning notes"))

	h := staleHandlers(t, s, false)
	out := callSearch(t, ctx, h, searchEventsInput{})

	if len(out.Events) == 0 {
		t.Fatal("expected the search to return the inserted event")
	}
	if !strings.Contains(out.Notice, "older lumi build") {
		t.Errorf("notice does not report the stale build: %q", out.Notice)
	}
	if !strings.Contains(out.Notice, "v0.1.0") {
		t.Errorf("notice does not name the running version: %q", out.Notice)
	}
}

// A build that can replace itself must not tell the user to restart the
// session: the fix is automatic, and the advice would be wrong.
func TestStalenessNoticeDistinguishesASelfUpdatingBuild(t *testing.T) {
	ctx := context.Background()
	notice := staleHandlers(t, testStore(t), true).stalenessNotice(ctx)

	if !strings.Contains(notice, "replace itself") {
		t.Errorf("self-updating notice does not say the process will replace itself: %q", notice)
	}
	if strings.Contains(notice, "restart this session") {
		t.Errorf("self-updating notice should not ask for a session restart: %q", notice)
	}
}

// An unchanged binary must stay silent. A notice on every call would be noise an
// agent learns to ignore, which would cost the warning its meaning when it
// matters.
func TestNoStalenessNoticeWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s, screenEvent(time.Now(), "quarterly planning notes"))

	h := &handlers{store: s, version: "v0.1.0", binaryChanged: func() bool { return false }}
	if notice := h.stalenessNotice(ctx); notice != "" {
		t.Errorf("expected no notice for an unchanged binary, got %q", notice)
	}
	if out := callSearch(t, ctx, h, searchEventsInput{}); out.Notice != "" {
		t.Errorf("expected no notice on a clean result, got %q", out.Notice)
	}
}

// A database written by a newer Lumi is the silent half of the problem: every
// migration is additive, so this build's fixed column list still resolves and
// the rows come back looking complete.
func TestStalenessNoticeReportsANewerDatabaseSchema(t *testing.T) {
	ctx := context.Background()
	s, path := testStoreWithPath(t)
	bumpSchemaVersion(t, ctx, path, store.CodeSchemaVersion+1)

	notice := (&handlers{store: s, version: "v0.1.0"}).stalenessNotice(ctx)
	if !strings.Contains(notice, "newer Lumi") {
		t.Errorf("notice does not report the newer schema: %q", notice)
	}
}

// The database's own schema version must not be reported as skew. This is the
// ordinary case on every call, so a false positive here would fire constantly.
func TestNoStalenessNoticeForACurrentDatabaseSchema(t *testing.T) {
	ctx := context.Background()
	notice := (&handlers{store: testStore(t), version: "v0.1.0"}).stalenessNotice(ctx)
	if notice != "" {
		t.Errorf("expected no notice for a current database, got %q", notice)
	}
}

// The skew has to qualify the notice it precedes: an agent that reads "no events
// matched" first will act on the filters when the real answer is that this
// process cannot see what a newer build wrote.
func TestStalenessPrefixesAnExistingNotice(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	h := staleHandlers(t, s, false)

	out := callSearch(t, ctx, h, searchEventsInput{})
	if !strings.Contains(out.Notice, "holds no events at all yet") {
		t.Fatalf("expected the empty-index notice to survive: %q", out.Notice)
	}
	stale := strings.Index(out.Notice, "older lumi build")
	empty := strings.Index(out.Notice, "holds no events at all yet")
	if stale == -1 || stale > empty {
		t.Errorf("staleness should come first in %q", out.Notice)
	}
}

// get_event is where an agent goes for the complete version of a truncated
// result, so undetectable missing fields matter more there than anywhere.
func TestGetEventReportsStaleness(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	events := insertEvents(t, ctx, s, screenEvent(time.Now(), "quarterly planning notes"))

	_, out, err := staleHandlers(t, s, false).getEvent(ctx, nil, getEventInput{ID: events[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Notice, "older lumi build") {
		t.Errorf("get_event does not report staleness: %q", out.Notice)
	}
}

func TestListAppsAndTranscriptReportStaleness(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s, screenEvent(time.Now(), "quarterly planning notes"))
	h := staleHandlers(t, s, false)

	_, apps, err := h.listApps(ctx, nil, listAppsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(apps.Notice, "older lumi build") {
		t.Errorf("list_apps does not report staleness: %q", apps.Notice)
	}

	_, transcript, err := h.getTranscript(ctx, nil, getTranscriptInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcript.Notice, "older lumi build") {
		t.Errorf("get_transcript does not report staleness: %q", transcript.Notice)
	}
}

// A nil hook is the configuration every existing caller and test uses, and it
// must not panic: newServer folds an absent Options hook into a constant false,
// but a handlers value built directly leaves it nil.
func TestStalenessNoticeToleratesAbsentHooks(t *testing.T) {
	ctx := context.Background()
	if notice := (&handlers{store: testStore(t)}).stalenessNotice(ctx); notice != "" {
		t.Errorf("expected no notice with no hooks, got %q", notice)
	}
}
