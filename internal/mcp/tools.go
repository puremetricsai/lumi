package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
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
// new, since every media file a tool names already sits under that
// directory — and is empty when Options carried none.
//
// Every handler returns a nil *sdk.CallToolResult, which is what puts its
// payload on the wire twice: the go-sdk fills Content with the serialized output
// when a handler leaves it nil, as the spec asks a tool returning
// structuredContent to do. Do not "save" those bytes by composing a summary
// there. structuredContent is optional for a client to read — Codex CLI and
// Claude Desktop, two of the three clients `lumi mcp setup` registers, render
// the content blocks and nothing else — so a summary in Content is the whole
// answer for them. A digest of counts and a time span once shipped there, and an
// agent asked to write up a meeting it had watched Lumi record reported that
// Lumi held metadata for the window but no words: the transcript was in
// structuredContent, complete, on every one of those calls.
type handlers struct {
	store        *store.Store
	databasePath string
}

// EventRecord is one event as an MCP client sees it. It deliberately carries no
// image or audio bytes — only the media file's name, which joined to the
// response's media_dir is a path the user can open on their own machine. An MCP
// client is usually a hosted agent, so anything a tool returns leaves this
// machine.
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
	Truncated  bool `json:"truncated"`
	TextLength int  `json:"text_length"`
	// App and Window are set on screen rows only. On an audio row the same two
	// columns are reported as ForegroundApp/ForegroundWindow, because there they
	// answer a question a reader of "app" does not expect them to: what the user
	// was working in while the sound recorded, never what produced it. The SQL
	// column names do not change — FTS5, Search's app filter, and ListAttribution
	// all depend on them — so the rename lives here, at the boundary an agent
	// actually reads.
	App    string `json:"app,omitempty"`
	Window string `json:"window,omitempty"`
	// ForegroundApp and ForegroundWindow are the audio-row spelling of the pair
	// above. They are never the source of the sound.
	ForegroundApp    string `json:"foreground_app,omitempty"`
	ForegroundWindow string `json:"foreground_window,omitempty"`
	// SourceApp lists the applications observed producing sound while this chunk
	// recorded, ordered by how much of the chunk each was present for. It is a
	// list because two applications can emit at once and a scalar could not say
	// so. Always absent on a microphone row.
	SourceApp []SourceAppRecord `json:"source_app,omitempty"`
	// Attribution says how source_app was earned: "emitting_process",
	// "foreground_inferred", or "unattributed". An audio row written since this
	// field existed always carries one, so an absent attribution means it was
	// never recorded — never that the sound was unattributable, which
	// "unattributed" states outright.
	Attribution string `json:"attribution,omitempty"`
	// MediaFile is the capture's file name, which joined to the response's
	// media_dir entry for this event's kind is the path the user can open. The
	// directory is identical for every event of a kind and was two thirds of
	// each repetition, so it is stated once per response instead.
	//
	// It is the stored path's base name, never a name composed from captured_at:
	// the recorder's rename to the timestamped form is best-effort, so a chunk
	// whose rename fell through keeps a name that formula would not produce. On
	// the rare page holding two directories for one kind — an index moved
	// between data dirs — that kind is absent from media_dir and its events
	// carry a whole path here rather than a name that would join to the wrong
	// directory.
	MediaFile  string `json:"media_file,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	TextSource string `json:"text_source,omitempty"`
	DisplayID  uint32 `json:"display_id,omitempty"`
	// AudioSource is the capture device this row was read from, "system" or
	// "microphone". A chunk's two rows are never merged, but a search returns
	// only the rows that matched: every filter is a per-row predicate, so one
	// row of a pair arrives alone routinely and says nothing about its
	// counterpart. Whether a pair held one sound or two is get_transcript's
	// question — see audioProvenanceContract in server.go.
	AudioSource string         `json:"audio_source,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// SourceAppRecord is one application observed producing sound during a chunk.
//
// Evidence names what earned it the place: "process" is a CoreAudio process
// object holding an output stream, "window_marker" is a window whose own title
// said it was playing audio.
//
// The store's PID is not forwarded. It is valid only for the length of that
// recording and names nothing a caller off this machine can resolve, while
// looking exactly like a durable identity to key on; BundleID and Name are the
// answer to "which application". Same reason the store's FirstOffsetMS and
// LastOffsetMS stop here.
type SourceAppRecord struct {
	BundleID string `json:"bundle_id,omitempty"`
	Name     string `json:"name,omitempty"`
	Window   string `json:"window,omitempty"`
	Evidence string `json:"evidence"`
	// Presence is how much of the chunk this application was emitting for, from
	// 0 to 1, so a notification blip and a video that filled the recording do not
	// read alike. It is the store's Samples over its Observations, sent as the
	// one ratio they were always meant to express: Observations is a chunk-level
	// count repeated identically on every entry, and the pair reads as two
	// independent measurements when it is one. Absent when nothing was observed.
	Presence float64 `json:"presence,omitempty"`
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
	record := EventRecord{
		ID:          event.ID,
		Kind:        string(event.Kind),
		CapturedAt:  localStamp(event.CapturedAt),
		Text:        text,
		Truncated:   truncated,
		TextLength:  length,
		MediaFile:   event.MediaPath,
		DurationMS:  event.DurationMS,
		TextSource:  event.TextSource,
		DisplayID:   event.DisplayID,
		AudioSource: event.AudioSource,
	}
	if event.Kind == store.KindAudio {
		record.ForegroundApp, record.ForegroundWindow = event.App, event.Window
		record.Attribution = event.AudioAttribution
		record.SourceApp = sourceAppRecords(event.SourceApps)
		return record
	}
	record.App, record.Window = event.App, event.Window
	return record
}

// sourceAppRecords decodes the stored source list for the wire. A list that will
// not decode is reported as none rather than failing the search: the audio and
// its attribution value are still worth returning, and Attribution already says
// whether a source was found at all.
func sourceAppRecords(raw string) []SourceAppRecord {
	apps, recorded, err := store.DecodeSourceApps(raw)
	if err != nil || !recorded || len(apps) == 0 {
		return nil
	}
	out := make([]SourceAppRecord, 0, len(apps))
	for _, app := range apps {
		out = append(out, SourceAppRecord{
			BundleID: app.BundleID, Name: app.Name, Window: app.Window,
			Evidence: app.Evidence, Presence: presenceOf(app),
		})
	}
	return out
}

// presenceOf reduces the store's sample counts to the fraction of the chunk an
// application was emitting for. Zero observations means nothing was ever read,
// which is not the same finding as "present for none of it" — it returns 0 so
// omitempty drops the key rather than claiming a measured absence.
//
// Rounded to two places because the input is a count of samples taken a second
// or so apart: more digits would imply a resolution the measurement does not
// have.
func presenceOf(app store.SourceApp) float64 {
	if app.Observations <= 0 {
		return 0
	}
	return math.Round(float64(app.Samples)/float64(app.Observations)*100) / 100
}

// redundantMetadataKeys are the stored metadata keys get_event does not send.
//
// It is a denylist and not an allowlist on purpose. Metadata is where a failed
// processor leaves the only account of what went wrong, and a processor added
// later will write a key nothing here knows about; an allowlist would drop that
// account silently, which is the failure this channel exists to prevent. Every
// key below is one whose meaning is already on the wire, or one that answers a
// question about Lumi's own capture rather than about what was captured.
//
// Nothing is lost by removing them: the blob is stored whole and `lumi search
// --json` exports it whole. This is what one tool sends, not what Lumi keeps.
var redundantMetadataKeys = map[string]struct{}{
	// Promoted to first-class event columns by migration 3 and already rendered
	// at the top level of this same record.
	"display_id":   {},
	"text_source":  {},
	"audio_source": {},
	// The other rendering of the fold that source_app already carries, decoded,
	// ordered by presence, and labelled with the evidence that earned it.
	"active_audio_output_processes": {},
	"audio_marker_windows":          {},
	// A second, differently-sourced rendering of the same frame, as long again
	// as the OCR text beside it — while text_length describes only that text and
	// nothing names this or says the two overlap. The Accessibility read's
	// verdict still reaches a caller as app_source/attribution_source.
	"accessibility_text": {},
	// How capture decided to keep the frame, what resolution it was, where the
	// chunk sat on the drift-free grid, and how often the samplers ran. All are
	// questions about the recorder.
	"frame_similarity":             {},
	"accessibility_trusted":        {},
	"width":                        {},
	"height":                       {},
	"grid_started_at":              {},
	"audio_source_samples":         {},
	"audio_source_sample_attempts": {},
	"foreground_samples":           {},
	"foreground_apps_seen":         {},
}

// decodeMetadata turns the stored metadata blob into an object the tool's
// output schema can describe, less the keys above. Capture always writes a JSON
// object; anything else is preserved verbatim under "_raw" rather than dropped,
// because metadata is where a failed processor leaves its diagnostics.
//
// Every *_error key survives, as do clock_anomaly and audio_attribution_reason:
// they say why text is missing or doubtful, which is the whole point of handing
// an agent this blob.
func decodeMetadata(blob json.RawMessage) map[string]any {
	if len(blob) == 0 {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		return map[string]any{"_raw": string(blob)}
	}
	for key := range decoded {
		if _, redundant := redundantMetadataKeys[key]; redundant {
			delete(decoded, key)
		}
	}
	if len(decoded) == 0 {
		return nil
	}
	return decoded
}

// hoistMediaDir replaces each record's whole media path with its file name and
// returns the directory each kind's files sit in.
//
// Every event of a kind comes from one directory, so repeating it per event was
// two thirds of each path and the largest constant cost on a page. A kind that
// somehow spans two directories — an index carried between data dirs — is left
// alone: its records keep whole paths and it gets no media_dir entry, because a
// name that joins to the wrong directory is worse than a long one.
func hoistMediaDir(records []EventRecord) map[string]string {
	dirs := make(map[string]string, 2)
	split := make(map[string]bool, 2)
	for _, record := range records {
		if record.MediaFile == "" {
			continue
		}
		dir := filepath.Dir(record.MediaFile)
		if seen, ok := dirs[record.Kind]; ok && seen != dir {
			split[record.Kind] = true
			continue
		}
		dirs[record.Kind] = dir
	}
	for kind := range split {
		delete(dirs, kind)
	}
	for i := range records {
		if records[i].MediaFile == "" || split[records[i].Kind] {
			continue
		}
		records[i].MediaFile = filepath.Base(records[i].MediaFile)
	}
	if len(dirs) == 0 {
		return nil
	}
	return dirs
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
}

type searchEventsOutput struct {
	Events []EventRecord `json:"events"`
	// MediaDir is the directory each kind's media_file sits in, keyed by "screen"
	// or "audio" — join the two for a path the user can open. It carries only the
	// kinds this page actually returned files for, and omits a kind whose files
	// came from more than one directory, whose records then hold whole paths.
	MediaDir map[string]string `json:"media_dir,omitempty"`
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
	opts.Limit = limit
	events, err := h.store.Search(ctx, opts)
	if err != nil {
		return nil, empty, fmt.Errorf("search the activity index: %w", err)
	}
	maxTextChars := defaultMaxTextChars
	if in.MaxTextChars != nil {
		maxTextChars = *in.MaxTextChars
	}

	// Both tracks of an audio chunk are returned, each as its own row. They are
	// not merged: two rows sharing a captured_at share an *interval*, not
	// necessarily a sound, and nothing available here can tell a microphone
	// re-recording of the speakers from an unrelated conversation in the room.
	// That question belongs to internal/transcript, which answers it per segment
	// against word timings and an energy envelope, and reaches an agent through
	// get_transcript. Deciding it a second time from a timestamp alone deleted
	// real transcripts.
	out := searchEventsOutput{Events: make([]EventRecord, 0, len(events))}
	for _, event := range events {
		out.Events = append(out.Events, newEventRecord(event, maxTextChars))
	}
	out.MediaDir = hoistMediaDir(out.Events)

	// Two independent notices can apply at once, so compose the non-empty parts.
	var parts []string
	capped := len(out.Events) == limit
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
		parts = append(parts, fmt.Sprintf(
			"results were capped at %d events; there may be more — narrow since/until or raise limit to see them",
			limit))
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
	// MediaDir is the directory this event's media_file sits in — join the two
	// for a path the user can open. It is a plain string rather than the map
	// search_events returns because one event has one kind.
	MediaDir string `json:"media_dir,omitempty"`
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
	records := []EventRecord{newEventRecord(*event, 0)}
	// get_event is the only tool that returns the metadata blob.
	records[0].Metadata = decodeMetadata(event.Metadata)
	// hoistMediaDir rewrites the record in place, so it has to see the slice the
	// result is read back out of. One record cannot split a kind across two
	// directories, so the map it returns holds at most this event's own kind.
	dirs := hoistMediaDir(records)
	return nil, getEventOutput{Event: records[0], MediaDir: dirs[records[0].Kind]}, nil
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
	App *string `json:"app,omitempty" jsonschema:"omit to list applications; set it to list the window titles seen for that one application, including \"\" for events with no attribution"`
	// Kind exists because an app's total sums two modalities whose relationship
	// to that app differs. Both name the focused application — a screen event's
	// text is full-display OCR that can carry other applications' windows, and
	// one focused-window snapshot is stamped onto every display's frame — but an
	// audio chunk's app is only what happened to be focused when the chunk
	// closed, which is routinely not what made the sound. Summed, the counts read
	// as one signal and an agent filtering search_events by an app gets both.
	Kind  string `json:"kind,omitempty" jsonschema:"restrict the counts to \"screen\" or \"audio\"; omit for both. Both kinds name the focused application rather than the source of the content, so this reveals how much of an app's total is each"`
	Since string `json:"since,omitempty" jsonschema:"earliest capture time: an RFC3339 timestamp, or a duration such as 24h meaning that long ago"`
	Until string `json:"until,omitempty" jsonschema:"latest capture time, in the same forms as since"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum rows to return; defaults to 50 and is capped at 500"`
}

type listAppsOutput struct {
	Entries []AttributionRecord `json:"entries"`
	Notice  string              `json:"notice,omitempty"`
}

// listApps reports which applications and window titles the index actually
// holds. Without it an agent guesses app filter values from the user's wording
// and silently filters everything away.
//
// The kind parameter is what keeps that discovery honest now that audio chunks
// carry an app: search_events applies its app filter across both kinds, so an
// app whose total is mostly audio will return sound that the application never
// produced. Reporting the split is the difference between an agent knowing that
// and inferring it from results that look legitimate.
func (h *handlers) listApps(ctx context.Context, _ *sdk.CallToolRequest, in listAppsInput) (*sdk.CallToolResult, listAppsOutput, error) {
	var empty listAppsOutput
	opts := store.AttributionOptions{App: in.App, Limit: in.Limit}
	var err error
	if opts.Kind, err = parseKind(in.Kind); err != nil {
		return nil, empty, err
	}
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
			LastSeen: localStamp(row.LastSeen),
		})
	}
	if len(out.Entries) == 0 {
		// A kind that matched nothing needs the opposite remedy from a range that
		// did, so naming only the range would send the agent to widen a window
		// that was never the cause.
		filtered := "no activity in this range; try widening since and until"
		if opts.Kind != "" {
			filtered = fmt.Sprintf(
				"no %s activity in this range; try omitting kind, or widening since and until", opts.Kind)
		}
		if out.Notice, err = h.noResultNotice(ctx, filtered); err != nil {
			return nil, empty, err
		}
	}
	return nil, out, nil
}
