package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/puremetricsai/lumi/internal/store"
)

const (
	// defaultLimit matches `lumi search`'s default page.
	defaultLimit = 20
	// defaultMaxTextChars keeps a page of results inside a reasonable share of
	// an agent's context. An agent that needs a whole document calls get_event.
	defaultMaxTextChars = 600
)

// handlers binds the tool implementations to a store. It holds nothing else:
// this package has no configuration and no state of its own.
type handlers struct {
	store *store.Store
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
	if len(out.Events) == 0 {
		storeEmpty, err := h.storeIsEmpty(ctx)
		if err != nil {
			return nil, empty, err
		}
		if storeEmpty {
			out.Notice = "this Lumi index holds no events at all yet; run `lumi record start` to begin capturing"
		} else {
			out.Notice = "no events matched these filters; try widening the time range, dropping the app filter, or match: \"any\""
		}
	}
	return nil, out, nil
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
