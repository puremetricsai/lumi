package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/puremetricsai/lumi/internal/store"
)

const (
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
	// never be mistaken for the end of the content. Neither carries omitempty:
	// an absent truncated would drop out of the generated output schema's
	// required list while text_length stayed in it, leaving an agent to infer
	// from a missing key the one thing this pair exists to state outright.
	Truncated   bool   `json:"truncated"`
	TextLength  int    `json:"text_length"`
	App         string `json:"app,omitempty"`
	Window      string `json:"window,omitempty"`
	MediaPath   string `json:"media_path,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	TextSource  string `json:"text_source,omitempty"`
	DisplayID   uint32 `json:"display_id,omitempty"`
	AudioSource string `json:"audio_source,omitempty"`
	// AudioOrigin says which tracks of this 30-second audio chunk carried
	// speech — "system", "microphone", "both", or "silent" — independent of
	// which track survived a collapse. "both" is speaker bleed; nothing
	// distinguishes a remote speaker from local media playback, so both read as
	// "system". Set only on collapsed audio events.
	AudioOrigin string `json:"audio_origin,omitempty"`
	// AudioTracks lists every row of a collapsed chunk (survivor first). It is
	// emitted only when the chunk held more than one row, since a lone track
	// would just restate this record's own id/audio_source/media_path and
	// audio_origin already carries the answer. The dropped track's text is not
	// inlined: text_length says whether it held speech and its id reaches the
	// full transcript through get_event.
	AudioTracks []AudioTrackRecord `json:"audio_tracks,omitempty"`
	Metadata    map[string]any     `json:"metadata,omitempty"`
}

// AudioTrackRecord is one track of a collapsed audio chunk as a client sees it.
// Like EventRecord it carries media_path as a string the user can open, never
// bytes — it is the only way to reach a dropped track's WAV at all.
type AudioTrackRecord struct {
	ID          int64  `json:"id"`
	AudioSource string `json:"audio_source"`
	TextLength  int    `json:"text_length"`
	MediaPath   string `json:"media_path,omitempty"`
}

// newEventRecord converts a stored event for the wire, capping Text at
// maxTextChars runes (zero or less means no cap). It never fills Metadata:
// get_event, the untruncated escape hatch, attaches that itself, so search
// results stay compact.
//
// captured_at is rendered in the machine's local zone with its offset, matching
// what `lumi search` prints, at nanosecond precision so a timestamp handed back
// as a `since` or `until` bound round-trips exactly. Storage and range
// comparison stay UTC.
func newEventRecord(event store.Event, maxTextChars int) EventRecord {
	text, truncated, length := truncateText(event.Text, maxTextChars)
	return EventRecord{
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
	Query  string `json:"query,omitempty" jsonschema:"full-text terms to match against screen text and audio transcripts; omit to browse by time and app alone"`
	Kind   string `json:"kind,omitempty" jsonschema:"restrict to \"screen\" or \"audio\"; omit for both"`
	App    string `json:"app,omitempty" jsonschema:"exact application name, case-insensitive; call list_apps to discover real values"`
	Window string `json:"window,omitempty" jsonschema:"case-insensitive substring of the window title"`
	Since  string `json:"since,omitempty" jsonschema:"earliest capture time: an RFC3339 timestamp, or a duration such as 2h or 45m meaning that long ago"`
	Until  string `json:"until,omitempty" jsonschema:"latest capture time, in the same forms as since"`
	// The numbers in limit's description are store.DefaultSearchLimit and
	// store.MaxSearchLimit; a struct tag cannot interpolate them, so
	// TestSearchLimitDescriptionMatchesStoreBounds fails if they drift apart.
	Limit        int    `json:"limit,omitempty" jsonschema:"maximum events to return; defaults to 20 and is capped at 500"`
	Match        string `json:"match,omitempty" jsonschema:"\"all\" (default) requires every query term; \"any\" requires one and ranks by relevance"`
	RequireText  bool   `json:"require_text,omitempty" jsonschema:"drop events whose text or transcript is empty or only whitespace"`
	MaxTextChars *int   `json:"max_text_chars,omitempty" jsonschema:"per-event character cap on text; defaults to 600, and 0 means no cap"`
	// CollapseAudioTracks is a pointer so an omitted value means true: each
	// 30-second chunk is recorded twice (system output and microphone), and by
	// default the duplicate is merged into one result carrying audio_origin and
	// audio_tracks. false returns both rows unmerged.
	CollapseAudioTracks *bool `json:"collapse_audio_tracks,omitempty" jsonschema:"collapse the microphone/system duplicate of each audio chunk into one result; defaults to true, and false returns both tracks unmerged"`
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
	if in.Query != "" && !store.HasSearchableTerms(in.Query) {
		return nil, empty, fmt.Errorf(
			"query %q has no searchable terms: FTS5 tokenizes punctuation and emoji to nothing, "+
				"so this would silently match every event instead of none; "+
				"omit query entirely to browse by time and app instead", in.Query)
	}
	opts := store.SearchOptions{
		Query:       in.Query,
		App:         in.App,
		Window:      in.Window,
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
	limit := clampLimit(in.Limit, store.DefaultSearchLimit, store.MaxSearchLimit)
	collapse := in.CollapseAudioTracks == nil || *in.CollapseAudioTracks
	// Over-fetch when collapsing so a page stays full: a chunk yields at most
	// two rows, so fetching 2*limit (bounded by the store ceiling) guarantees at
	// least limit survivors. With collapse off the two coincide.
	fetchLimit := limit
	if collapse {
		fetchLimit = min(2*limit, store.MaxSearchLimit)
	}
	opts.Limit = fetchLimit
	rawEvents, err := h.store.Search(ctx, opts)
	if err != nil {
		return nil, empty, fmt.Errorf("search the activity index: %w", err)
	}
	maxTextChars := defaultMaxTextChars
	if in.MaxTextChars != nil {
		maxTextChars = *in.MaxTextChars
	}

	events := rawEvents
	var chunks map[int64]store.AudioChunk
	if collapse {
		events, chunks = store.CollapseAudioTracks(rawEvents)
	}
	if len(events) > limit {
		events = events[:limit]
	}

	out := searchEventsOutput{Events: make([]EventRecord, 0, len(events))}
	for _, event := range events {
		out.Events = append(out.Events, newEventRecord(event, maxTextChars))
	}

	var collapsed int
	if collapse {
		// Provenance describes the whole chunk, not the matched set: a query
		// that hit only the microphone track of a bleed pair must still report
		// audio_origin "both". AudioTracksAt re-reads every track at each
		// survivor's timestamp, regardless of what the search matched.
		if err = h.annotateAudio(ctx, out.Events, chunks); err != nil {
			return nil, empty, err
		}
		for _, event := range events {
			if event.Kind == store.KindAudio {
				collapsed += len(chunks[event.ID].Tracks) - 1
			}
		}
	}

	// Two independent notices can apply at once, so compose the non-empty parts.
	var parts []string
	capped := len(out.Events) == limit || len(rawEvents) == fetchLimit
	switch {
	case len(out.Events) == 0:
		notice, err := h.noResultNotice(ctx,
			"no events matched these filters; try widening the time range, dropping the app filter, or match: \"any\"")
		if err != nil {
			return nil, empty, err
		}
		parts = append(parts, notice)
	case capped:
		// A full page is indistinguishable from "there happen to be exactly
		// this many results" unless we say so: an agent that gets exactly the
		// limit back cannot otherwise tell it saw a recency-truncated slice.
		// Over-fetching means a short page can still be capped, so the raw fetch
		// hitting fetchLimit is a second signal there is more.
		parts = append(parts, fmt.Sprintf(
			"results were capped at %d events; there may be more — narrow since/until or raise limit to see them",
			limit))
	}
	if collapsed > 0 {
		parts = append(parts, fmt.Sprintf(
			"collapsed %d duplicate audio events: the microphone and system tracks record the same "+
				"30-second chunk; each result lists its merged tracks in audio_tracks and which carried "+
				"speech in audio_origin, and collapse_audio_tracks: false returns them unmerged",
			collapsed))
	}
	// An audio hit is a 30-second window of one track, which reads poorly as
	// conversation: the machine's speech still appears in both tracks here, and
	// a turn spanning two windows arrives as two results. Point at the tool that
	// answers those, but only when it would actually have something to show.
	if h.hasAttributedAudio(ctx, events) {
		parts = append(parts, "some results are audio: get_transcript returns these as one ordered "+
			"conversation with per-turn origin labels and the machine's own speech deduplicated")
	}
	out.Notice = strings.Join(parts, "; ")
	return nil, out, nil
}

// hasAttributedAudio reports whether any returned audio event's chunk holds
// attributed speech, so the transcript hint is never offered for a range where
// get_transcript would come back empty.
//
// It asks for speech rather than for coverage: a chunk of pure silence is
// attributed — that is what makes the backfill queue drain — but has nothing to
// show, and pointing an agent at an empty transcript costs it a round trip to
// learn nothing.
//
// It reads the store events rather than the records rendered from them: the
// timestamps are already time.Time there, and recovering them by re-parsing this
// package's own output would make the hint quietly depend on how EventRecord
// happens to format a time.
//
// A failure to answer costs the hint, never the search, so there is no error to
// return: the results are assembled and correct, and it can only come from the
// database they were just read out of.
func (h *handlers) hasAttributedAudio(ctx context.Context, events []store.Event) bool {
	var earliest, latest time.Time
	for _, event := range events {
		if event.Kind != store.KindAudio {
			continue
		}
		if earliest.IsZero() || event.CapturedAt.Before(earliest) {
			earliest = event.CapturedAt
		}
		if event.CapturedAt.After(latest) {
			latest = event.CapturedAt
		}
	}
	if earliest.IsZero() {
		return false
	}
	attributed, err := h.store.HasSpeechSegments(ctx, earliest, latest)
	return err == nil && attributed
}

// annotateAudio fills audio_origin and audio_tracks on the collapsed audio
// records. Provenance is read fresh from the store (AudioTracksAt) so it
// describes every track of the chunk even when the search matched only one, and
// audio_tracks is emitted only for chunks that held more than one row.
func (h *handlers) annotateAudio(ctx context.Context, records []EventRecord, chunks map[int64]store.AudioChunk) error {
	times := make([]string, 0, len(records))
	seen := make(map[string]bool)
	recordTime := make(map[int]string, len(records))
	for i := range records {
		if records[i].Kind != string(store.KindAudio) {
			continue
		}
		// CapturedAt is rendered local; re-parse to the same UTC RFC3339Nano key
		// AudioTracksAt and storage use.
		parsed, err := time.Parse(time.RFC3339Nano, records[i].CapturedAt)
		if err != nil {
			return fmt.Errorf("parse audio timestamp %q: %w", records[i].CapturedAt, err)
		}
		key := parsed.UTC().Format(time.RFC3339Nano)
		recordTime[i] = key
		if !seen[key] {
			seen[key] = true
			times = append(times, key)
		}
	}
	if len(times) == 0 {
		return nil
	}
	tracksAt, err := h.store.AudioTracksAt(ctx, times)
	if err != nil {
		return fmt.Errorf("read audio track provenance: %w", err)
	}
	for i := range records {
		key, ok := recordTime[i]
		if !ok {
			continue
		}
		tracks := tracksAt[key]
		records[i].AudioOrigin = store.OriginOf(tracks)
		if len(tracks) > 1 {
			records[i].AudioTracks = audioTrackRecords(records[i].ID, tracks)
		}
	}
	return nil
}

// audioTrackRecords converts store tracks for the wire with the survivor first.
func audioTrackRecords(survivorID int64, tracks []store.AudioTrack) []AudioTrackRecord {
	out := make([]AudioTrackRecord, 0, len(tracks))
	var rest []AudioTrackRecord
	for _, tr := range tracks {
		rec := AudioTrackRecord{ID: tr.ID, AudioSource: tr.AudioSource, TextLength: tr.TextLength, MediaPath: tr.MediaPath}
		if tr.ID == survivorID {
			out = append(out, rec)
		} else {
			rest = append(rest, rec)
		}
	}
	return append(out, rest...)
}

// noResultNotice explains an empty result set, which every tool must do and
// none can do from its own result alone: "nothing recorded yet" and "nothing
// matched these filters" call for different next moves. whenFiltered is the
// caller's wording for the second case; the first is shared, since the remedy
// does not depend on which tool asked.
//
// The database file is named when it is known, so a typo'd --data-dir (which
// openStore happily creates as a fresh empty database) reads as "this file has
// nothing in it" rather than steering the agent to tell the user to start
// recording when they already have, elsewhere.
func (h *handlers) noResultNotice(ctx context.Context, whenFiltered string) (string, error) {
	hasEvents, err := h.store.HasEvents(ctx)
	if err != nil {
		return "", fmt.Errorf("check whether the activity index is empty: %w", err)
	}
	switch {
	case hasEvents:
		return whenFiltered, nil
	case h.databasePath == "":
		return "this Lumi index holds no events at all yet; run `lumi record start` to begin capturing", nil
	}
	return fmt.Sprintf(
		"this Lumi index (%s) holds no events at all yet; run `lumi record start` to begin capturing, "+
			"or check that this is the right --data-dir/LUMI_HOME if you expected existing history",
		h.databasePath), nil
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
	record := newEventRecord(*event, 0)
	// get_event is the only tool that returns the metadata blob.
	record.Metadata = decodeMetadata(event.Metadata)
	return nil, getEventOutput{Event: record}, nil
}

// AttributionRecord is one row of the list_apps inventory. In app mode Window
// is empty; in window mode App echoes the requested application.
//
// It shadows store.Attribution rather than reusing it because LastSeen has to
// cross the wire as a preformatted local-zone string, matching every other
// timestamp a tool returns; store.Attribution keeps a time.Time.
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
	opts.Limit = clampLimit(in.Limit, defaultAttributionLimit, maxAttributionLimit)
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
		if out.Notice, err = h.noResultNotice(ctx,
			"no activity in this range; try widening since and until"); err != nil {
			return nil, empty, err
		}
	}
	return nil, out, nil
}
