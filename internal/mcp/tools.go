package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/puremetricsai/lumi/internal/store"
)

const (
	// defaultLimit matches `lumi search`'s default page.
	defaultLimit = 20
	// maxLimit mirrors the ceiling store.Search itself clamps to; enforcing it
	// here too keeps the "capped at 500" parameter hint honest rather than
	// relying on an unexported literal in internal/store.
	maxLimit = 500
	// defaultMaxTextChars keeps a page of results inside a reasonable share of
	// an agent's context. An agent that needs a whole document calls get_event.
	defaultMaxTextChars = 600
	// defaultAttributionLimit keeps an orientation call small; a machine that
	// has been recording for months can hold thousands of distinct windows.
	defaultAttributionLimit = 50
	maxAttributionLimit     = 500
)

// handlers binds the tool implementations to a store. databasePath is only
// used to name the file in "this index is empty" notices — it leaks nothing
// new, since every media_path a tool returns already sits under that
// directory — and is empty when Options carried none.
type handlers struct {
	store        *store.Store
	databasePath string
}

// EventRecord is one event as an MCP client sees it. It deliberately carries no
// image or audio bytes — only media_path, a string the user can open on their
// own machine. An MCP client is usually a hosted agent, so anything a tool
// returns leaves this machine.
type EventRecord struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	CapturedAt string `json:"captured_at"`
	Text       string `json:"text"`
	// Truncated and TextLength always travel together with Text, so a cut can
	// never be mistaken for the end of the content.
	Truncated   bool           `json:"truncated,omitempty"`
	TextLength  int            `json:"text_length"`
	App         string         `json:"app,omitempty"`
	Window      string         `json:"window,omitempty"`
	MediaPath   string         `json:"media_path,omitempty"`
	DurationMS  int64          `json:"duration_ms,omitempty"`
	TextSource  string         `json:"text_source,omitempty"`
	DisplayID   uint32         `json:"display_id,omitempty"`
	AudioSource string         `json:"audio_source,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// newEventRecord converts a stored event for the wire, capping Text at
// maxTextChars runes (zero or less means no cap). Metadata is included only for
// get_event, which is the untruncated escape hatch; search results stay compact.
//
// captured_at is rendered in the machine's local zone with its offset, matching
// what `lumi search` prints, at nanosecond precision so a timestamp handed back
// as a `since` or `until` bound round-trips exactly. Storage and range
// comparison stay UTC.
func newEventRecord(event store.Event, maxTextChars int, includeMetadata bool) EventRecord {
	text, truncated, length := truncateText(event.Text, maxTextChars)
	record := EventRecord{
		ID:          event.ID,
		Kind:        string(event.Kind),
		CapturedAt:  event.CapturedAt.Local().Format(time.RFC3339Nano),
		Text:        text,
		Truncated:   truncated,
		TextLength:  length,
		App:         event.App,
		Window:      event.Window,
		MediaPath:   event.MediaPath,
		DurationMS:  event.DurationMS,
		TextSource:  event.TextSource,
		DisplayID:   event.DisplayID,
		AudioSource: event.AudioSource,
	}
	if includeMetadata {
		record.Metadata = decodeMetadata(event.Metadata)
	}
	return record
}

// decodeMetadata turns the stored metadata blob into an object the tool's
// output schema can describe. Capture always writes a JSON object; anything
// else is preserved verbatim under "_raw" rather than dropped, because
// metadata is where a failed processor leaves its diagnostics.
func decodeMetadata(blob json.RawMessage) map[string]any {
	if len(blob) == 0 {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		return map[string]any{"_raw": string(blob)}
	}
	return decoded
}

type searchEventsInput struct {
	Query        string `json:"query,omitempty" jsonschema:"full-text terms to match against screen text and audio transcripts; omit to browse by time and app alone"`
	Kind         string `json:"kind,omitempty" jsonschema:"restrict to \"screen\" or \"audio\"; omit for both"`
	App          string `json:"app,omitempty" jsonschema:"exact application name, case-insensitive; call list_apps to discover real values"`
	Window       string `json:"window,omitempty" jsonschema:"case-insensitive substring of the window title"`
	Since        string `json:"since,omitempty" jsonschema:"earliest capture time: an RFC3339 timestamp, or a duration such as 2h or 45m meaning that long ago"`
	Until        string `json:"until,omitempty" jsonschema:"latest capture time, in the same forms as since"`
	Limit        int    `json:"limit,omitempty" jsonschema:"maximum events to return; defaults to 20 and is capped at 500"`
	Match        string `json:"match,omitempty" jsonschema:"\"all\" (default) requires every query term; \"any\" requires one and ranks by relevance"`
	RequireText  bool   `json:"require_text,omitempty" jsonschema:"drop events whose text or transcript is empty or only whitespace"`
	MaxTextChars *int   `json:"max_text_chars,omitempty" jsonschema:"per-event character cap on text; defaults to 600, and 0 means no cap"`
}

type searchEventsOutput struct {
	Events []EventRecord `json:"events"`
	// Notice explains an empty Events array, which is otherwise ambiguous:
	// nothing recorded yet and nothing matching these filters call for
	// different next moves.
	Notice string `json:"notice,omitempty"`
}

func (h *handlers) searchEvents(ctx context.Context, _ *sdk.CallToolRequest, in searchEventsInput) (*sdk.CallToolResult, searchEventsOutput, error) {
	var empty searchEventsOutput
	// A query that tokenizes to nothing (pure punctuation or emoji) must not
	// silently fall through to store.Search's "no MATCH clause at all"
	// behavior, which returns the most recent events with no way for an agent
	// to tell they are not real hits. Reject it here instead, at the MCP
	// boundary — internal/store stays unchanged.
	if in.Query != "" && !hasSearchableTerm(in.Query) {
		return nil, empty, fmt.Errorf(
			"query %q has no searchable terms: FTS5 tokenizes punctuation and emoji to nothing, "+
				"so this would silently match every event instead of none; "+
				"omit query entirely to browse by time and app instead", in.Query)
	}
	opts := store.SearchOptions{
		Query:       in.Query,
		App:         in.App,
		Window:      in.Window,
		Limit:       in.Limit,
		RequireText: in.RequireText,
	}
	var err error
	if opts.Kind, err = parseKind(in.Kind); err != nil {
		return nil, empty, err
	}
	if opts.Match, err = parseMatch(in.Match); err != nil {
		return nil, empty, err
	}
	if opts.Since, err = parseTimestamp("since", in.Since); err != nil {
		return nil, empty, err
	}
	if opts.Until, err = parseTimestamp("until", in.Until); err != nil {
		return nil, empty, err
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultLimit
	}
	if opts.Limit > maxLimit {
		opts.Limit = maxLimit
	}
	events, err := h.store.Search(ctx, opts)
	if err != nil {
		return nil, empty, fmt.Errorf("search the activity index: %w", err)
	}
	maxTextChars := defaultMaxTextChars
	if in.MaxTextChars != nil {
		maxTextChars = *in.MaxTextChars
	}
	out := searchEventsOutput{Events: make([]EventRecord, 0, len(events))}
	for _, event := range events {
		out.Events = append(out.Events, newEventRecord(event, maxTextChars, false))
	}
	switch {
	case len(out.Events) == 0:
		storeEmpty, err := h.storeIsEmpty(ctx)
		if err != nil {
			return nil, empty, err
		}
		if storeEmpty {
			out.Notice = h.emptyStoreNotice()
		} else {
			out.Notice = "no events matched these filters; try widening the time range, dropping the app filter, or match: \"any\""
		}
	case len(out.Events) == opts.Limit:
		// A full page is indistinguishable from "there happen to be exactly
		// this many results" unless we say so: an agent that gets exactly the
		// limit back cannot otherwise tell it saw a recency-truncated slice.
		out.Notice = fmt.Sprintf(
			"results were capped at %d events; there may be more — narrow since/until or raise limit to see them",
			opts.Limit)
	}
	return nil, out, nil
}

// emptyStoreNotice names the database file when it is known, so a typo'd
// --data-dir (which openStore happily creates as a fresh empty database)
// reads as "this file has nothing in it" rather than steering the agent to
// tell the user to start recording when they already have, elsewhere.
func (h *handlers) emptyStoreNotice() string {
	if h.databasePath == "" {
		return "this Lumi index holds no events at all yet; run `lumi record start` to begin capturing"
	}
	return fmt.Sprintf(
		"this Lumi index (%s) holds no events at all yet; run `lumi record start` to begin capturing, "+
			"or check that this is the right --data-dir/LUMI_HOME if you expected existing history",
		h.databasePath)
}

// storeIsEmpty distinguishes "nothing recorded yet" from "nothing matched". It
// reuses an unfiltered one-row Search rather than adding a count method, and
// only runs when a result set came back empty.
func (h *handlers) storeIsEmpty(ctx context.Context) (bool, error) {
	events, err := h.store.Search(ctx, store.SearchOptions{Limit: 1})
	if err != nil {
		return false, fmt.Errorf("check whether the activity index is empty: %w", err)
	}
	return len(events) == 0, nil
}

type getEventInput struct {
	ID int64 `json:"id" jsonschema:"the id of an event returned by search_events"`
}

type getEventOutput struct {
	Event EventRecord `json:"event"`
}

// getEvent returns one event with its text in full and its processor metadata
// attached. It is what makes search_events' truncation safe: an agent that sees
// truncated: true fetches the rest here.
func (h *handlers) getEvent(ctx context.Context, _ *sdk.CallToolRequest, in getEventInput) (*sdk.CallToolResult, getEventOutput, error) {
	var empty getEventOutput
	event, err := h.store.EventByID(ctx, in.ID)
	if errors.Is(err, store.ErrEventNotFound) {
		return nil, empty, fmt.Errorf("no event has id %d; use search_events to find valid ids", in.ID)
	}
	if err != nil {
		return nil, empty, fmt.Errorf("read event %d: %w", in.ID, err)
	}
	return nil, getEventOutput{Event: newEventRecord(*event, 0, true)}, nil
}

// AttributionRecord is one row of the list_apps inventory. In app mode Window
// is empty; in window mode App echoes the requested application.
type AttributionRecord struct {
	App      string `json:"app"`
	Window   string `json:"window,omitempty"`
	Events   int64  `json:"events"`
	LastSeen string `json:"last_seen"`
}

type listAppsInput struct {
	App   *string `json:"app,omitempty" jsonschema:"omit to list applications; set it to list the window titles seen for that one application, including \"\" for events with no attribution"`
	Since string  `json:"since,omitempty" jsonschema:"earliest capture time: an RFC3339 timestamp, or a duration such as 24h meaning that long ago"`
	Until string  `json:"until,omitempty" jsonschema:"latest capture time, in the same forms as since"`
	Limit int     `json:"limit,omitempty" jsonschema:"maximum rows to return; defaults to 50 and is capped at 500"`
}

type listAppsOutput struct {
	Entries []AttributionRecord `json:"entries"`
	Notice  string              `json:"notice,omitempty"`
}

// listApps reports which applications and window titles the index actually
// holds. Without it an agent guesses app filter values from the user's wording
// and silently filters everything away.
func (h *handlers) listApps(ctx context.Context, _ *sdk.CallToolRequest, in listAppsInput) (*sdk.CallToolResult, listAppsOutput, error) {
	var empty listAppsOutput
	opts := store.AttributionOptions{App: in.App, Limit: in.Limit}
	var err error
	if opts.Since, err = parseTimestamp("since", in.Since); err != nil {
		return nil, empty, err
	}
	if opts.Until, err = parseTimestamp("until", in.Until); err != nil {
		return nil, empty, err
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultAttributionLimit
	}
	if opts.Limit > maxAttributionLimit {
		opts.Limit = maxAttributionLimit
	}
	rows, err := h.store.ListAttribution(ctx, opts)
	if err != nil {
		return nil, empty, fmt.Errorf("list captured applications: %w", err)
	}
	out := listAppsOutput{Entries: make([]AttributionRecord, 0, len(rows))}
	for _, row := range rows {
		out.Entries = append(out.Entries, AttributionRecord{
			App:      row.App,
			Window:   row.Window,
			Events:   row.Events,
			LastSeen: row.LastSeen.Local().Format(time.RFC3339Nano),
		})
	}
	if len(out.Entries) == 0 {
		storeEmpty, err := h.storeIsEmpty(ctx)
		if err != nil {
			return nil, empty, err
		}
		if storeEmpty {
			out.Notice = h.emptyStoreNotice()
		} else {
			out.Notice = "no activity in this range; try widening since and until"
		}
	}
	return nil, out, nil
}
