package mcp

import (
	"context"
	"encoding/base64"
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
	// version is this build's version string, named in a staleness notice so the
	// skew reads as a fact about two builds rather than a vague warning.
	version string
	// binaryChanged reports whether the lumi binary on disk is no longer the one
	// this process is running. Never nil once newServer has built it; it folds an
	// absent Options hook into a constant false.
	binaryChanged func() bool
	// selfUpdating records whether this process can replace itself, which changes
	// what a staleness notice should tell the caller to do.
	selfUpdating bool
}

// stalenessNotice reports that this server process no longer matches what is
// installed, or that its database was written by a different build.
//
// It exists because both kinds of skew are otherwise *silent*, which is the
// failure mode this repository treats as the worst one. An agent holds a
// long-lived `lumi mcp` subprocess for the whole session, so an upgrade that
// replaces the bundle leaves the old image mapped and serving old code — and every migration
// is additive, so an older build reading a newer file finds every column its
// fixed SELECT names, succeeds, and returns rows missing only what the new build
// added. Nothing errors on either path. Without this the user upgrades to get a
// fix, watches the agent keep producing the old behavior, and has no way to see
// why; with it the answer says so in the same breath as the data.
//
// It is deliberately a notice and not a tool error. The results are real and
// worth having — the skew makes them possibly-incomplete, not wrong — and
// failing the call would deny the agent data it can still use. That is the same
// direction as every other notice here: state the doubt on the wire rather than
// leaving an agent to infer it from an absence.
//
// A failure to read the schema version returns no notice rather than an error,
// for the reason hasAttributedAudio does: it can only come from the database the
// results were just read out of, and losing the notice must never cost the call.
func (h *handlers) stalenessNotice(ctx context.Context) string {
	var parts []string
	if h.binaryChanged != nil && h.binaryChanged() {
		if h.selfUpdating {
			parts = append(parts, fmt.Sprintf(
				"the installed lumi binary has been upgraded since this server started (running %s); "+
					"it will replace itself once this session goes briefly idle, so results after that "+
					"may reflect newer behavior",
				h.versionLabel()))
		} else {
			parts = append(parts, fmt.Sprintf(
				"this MCP server is running an older lumi build (%s) than the one now installed, "+
					"because the agent launched it before the upgrade; "+
					"restart this session to pick up the new build",
				h.versionLabel()))
		}
	}
	if fileVersion, err := h.store.SchemaVersion(ctx); err == nil &&
		fileVersion > store.CodeSchemaVersion {
		// Additive migrations mean this cannot be detected from the rows: they
		// arrive complete as far as this build's column list is concerned.
		parts = append(parts, fmt.Sprintf(
			"this index was written by a newer Lumi (database schema %d, this build reads %d), "+
				"so events may carry fields this build does not return; "+
				"restart this session, or re-run `lumi mcp setup`, to read it with the matching build",
			fileVersion, store.CodeSchemaVersion))
	}
	return strings.Join(parts, "; ")
}

// versionLabel names this build for a notice, without ever rendering an empty
// pair of parentheses when the version was not stamped in.
func (h *handlers) versionLabel() string {
	if h.version == "" {
		return "unknown version"
	}
	return h.version
}

// withStaleness prefixes a tool's notice with any staleness warning.
//
// The skew goes first because it qualifies everything after it: a notice that
// opens by explaining an empty result would otherwise have an agent acting on
// "nothing matched" when the real answer is "this process cannot see what a
// newer build wrote".
func (h *handlers) withStaleness(ctx context.Context, notice string) string {
	stale := h.stalenessNotice(ctx)
	switch {
	case stale == "":
		return notice
	case notice == "":
		return stale
	}
	return stale + "; " + notice
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
	// CollapsedIDs names the events this record stands for when the fold ran on a
	// run of near-identical adjacent screen rows, so nothing is
	// silently lost and get_event still reaches every dropped row.
	// CollapsedCount is len(CollapsedIDs): redundant by construction, and present
	// so an agent does not have to count a list to learn its page is short.
	// Both are absent on an uncollapsed record, and always absent on an audio
	// row — audio never enters the collapse.
	CollapsedIDs   []int64 `json:"collapsed_ids,omitempty"`
	CollapsedCount int     `json:"collapsed_count,omitempty"`
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
// maxTextChars runes (zero or less means no cap) and centring that window on
// the earliest of terms to appear in it — the terms store.SearchTerms derived
// from the query, so the drop rule is never restated here. Nil terms mean a
// head cut, which is what browse mode and get_event both want. It never fills
// Metadata:
// get_event, the untruncated escape hatch, attaches that itself, so search
// results stay compact.
//
// captured_at is rendered in the machine's local zone with its offset, matching
// what `lumi search` prints, at nanosecond precision so a timestamp handed back
// as a `since` or `until` bound round-trips exactly. Storage and range
// comparison stay UTC.
func newEventRecord(event store.Event, maxTextChars int, terms []string) EventRecord {
	text, truncated, length := excerptAround(event.Text, terms, maxTextChars)
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
	Limit         int    `json:"limit,omitempty" jsonschema:"maximum rows to read from the index, counted BEFORE the fold: a page of 20 that collapses adjacent near-identical screen rows can return fewer events than this, and the notice reports both counts; defaults to 20 and is capped at 500"`
	Match         string `json:"match,omitempty" jsonschema:"\"all\" (default) requires every query term; \"any\" requires one and ranks by relevance"`
	RequireText   bool   `json:"require_text,omitempty" jsonschema:"drop events whose text or transcript is empty or only whitespace"`
	MaxTextChars  *int   `json:"max_text_chars,omitempty" jsonschema:"per-event character cap on text; defaults to 600, and 0 means no cap"`
	ExpandSimilar bool   `json:"expand_similar,omitempty" jsonschema:"return every row instead of folding a run of adjacent screen results showing the same app, window and display with near-identical text into one representative carrying the folded ids as collapsed_ids; audio rows are never folded either way"`
	// CollapseSimilar is the old name for the inverse of ExpandSimilar, kept
	// because removing it would make a cached tools/list fail rather than degrade:
	// jsonschema-go puts additionalProperties: false on every inferred schema, and
	// `lumi mcp` replaces its own process image mid-session (internal/selfexec)
	// while the client keeps the tool list it already has. Under the new default
	// collapse_similar: true asks for what already happens, so it validates and
	// changes nothing. collapse_similar: false cannot be seen at all — omitempty
	// erases it, which is why the field was inverted rather than defaulted — so it
	// is documented as the no-op it now is instead of silently meaning its
	// opposite.
	CollapseSimilar bool `json:"collapse_similar,omitempty" jsonschema:"deprecated and ignored: collapsing is now the default, so true asks for what already happens; use expand_similar to turn it off"`
	// Cursor pages this tool's RESULTS, which MCP itself says nothing about — the
	// protocol's cursor covers list methods only. It is opaque because its
	// contents are this package's business and because a raw timestamp on the wire
	// would be the one payload value not in the local zone.
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor from a previous call, to read the page after it; resend the same filters alongside it. The cursor pins the time window its first page was computed in, so since and until are ignored while paging — a different window means starting a new search without a cursor"`
}

type searchEventsOutput struct {
	Events []EventRecord `json:"events"`
	// MediaDir is the directory each kind's media_file sits in, keyed by "screen"
	// or "audio" — join the two for a path the user can open. It carries only the
	// kinds this page actually returned files for, and omits a kind whose files
	// came from more than one directory, whose records then hold whole paths.
	MediaDir map[string]string `json:"media_dir,omitempty"`
	// NextCursor resumes after the last row of this page. It is present only when
	// the page was full, and its ABSENCE is how a caller learns there is no next
	// page — the one field here that means something by not appearing, against
	// this package's rule that a doubt label must always be visible. The rule is
	// waived because every cursor protocol already works this way, and an agent
	// that reads an empty cursor as a valid one pages forever.
	NextCursor string `json:"next_cursor,omitempty"`
	// Notice explains an empty Events array, which is otherwise ambiguous:
	// nothing recorded yet and nothing matching these filters call for
	// different next moves.
	Notice string `json:"notice,omitempty"`
}

// searchCursor is the opaque page key. It carries the ordering key and nothing
// else: a cursor that also carried the filters would let a caller change them
// and the page boundary independently, and the two disagreeing is not a state
// worth being able to reach. Ranked mode has no stored key to resume from —
// bm25 is recomputed per query — so it counts rows instead.
type searchCursor struct {
	// Ranked is true for a cursor issued by a call that had a query, and is
	// checked against the current call: a browse key resumed under a query, or the
	// reverse, would silently page the wrong ordering.
	Ranked bool `json:"r,omitempty"`
	// CapturedAt is store.Event.CapturedAtRaw, the column's own bytes. Browse
	// mode only.
	CapturedAt string `json:"t,omitempty"`
	ID         int64  `json:"i,omitempty"`
	// Offset is the number of ranked rows already served. Ranked mode only.
	Offset int `json:"o,omitempty"`
	// Since and Until pin the time window the page set was computed in, as
	// RFC3339Nano, empty when the call had no bound. They are here because
	// `since: "2h"` resolves against the clock on EVERY call, so a walk that
	// resends the same arguments is walking a window that slides out from under
	// it — and a ranked Offset counts rows in a result set that just lost its
	// oldest member, which skips a row that still matches. Browse's keyset only
	// loses the far end early, but the same pin makes both finish the walk they
	// started.
	Since string `json:"s,omitempty"`
	Until string `json:"u,omitempty"`
}

// window reads the pinned bounds back. A bound that fails to parse is dropped
// rather than fatal: the cursor is this package's own writing, and a walk that
// widens by one bound beats one that cannot continue at all.
func (c searchCursor) window() (since, until *time.Time) {
	parse := func(value string) *time.Time {
		if value == "" {
			return nil
		}
		at, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil
		}
		return &at
	}
	return parse(c.Since), parse(c.Until)
}

func formatBound(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}

func encodeCursor(c searchCursor) string {
	raw, err := json.Marshal(c)
	if err != nil {
		// searchCursor is four scalars; Marshal cannot fail on it.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor rejects a cursor that does not belong to this call rather than
// passing it to the store, where a browse key under a query would quietly
// return a narrowed range that looks like a page.
func decodeCursor(encoded string, ranked bool) (searchCursor, error) {
	var c searchCursor
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err == nil {
		err = json.Unmarshal(raw, &c)
	}
	if err != nil {
		return c, errNotACursor
	}
	if c.Ranked != ranked {
		from, to := "without a query", "with one"
		if c.Ranked {
			from, to = "with a query", "without one"
		}
		return c, fmt.Errorf("this cursor came from a search %s and was passed back to one %s: "+
			"the two order results differently, so the cursor names no place in this one; "+
			"start the new search without a cursor", from, to)
	}
	// A browse cursor without its key would page from the beginning of time and
	// look like a valid page. Only a hand-forged one reaches this.
	if !ranked && c.CapturedAt == "" {
		return c, errNotACursor
	}
	return c, nil
}

var errNotACursor = errors.New(
	"cursor is not a value this tool issued; pass back next_cursor exactly as returned")

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
	// Which cursor a mode uses is not a style choice: browse orders by
	// captured_at, which every row carries and can be resumed from exactly, while
	// ranked orders by a bm25 score recomputed per query, which no row carries at
	// all.
	ranked := in.Query != ""
	var cursor searchCursor
	if in.Cursor != "" {
		if cursor, err = decodeCursor(in.Cursor, ranked); err != nil {
			return nil, empty, err
		}
		// The pinned window wins over since/until, which are still parsed above so
		// a malformed one is still an error. A caller cannot tell us whether a
		// changed bound is a deliberate narrowing or the same relative string
		// resolving a second later, so the cursor decides and the description says
		// so: a different window means starting without a cursor.
		opts.Since, opts.Until = cursor.window()
		if ranked {
			// ponytail: OFFSET, whose ceiling is that bm25 shifts as rows arrive, so
			// a boundary can drift mid-capture and a row can be served twice or
			// missed. A keyset on a recomputed float would not be better; storing the
			// whole result set would be a different tool.
			opts.Offset = cursor.Offset
		} else {
			opts.Before = &store.SearchCursor{CapturedAt: cursor.CapturedAt, ID: cursor.ID}
		}
	}
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
	// The terms are derived once per request, never per row, and never by
	// re-splitting the query here: store.SearchTerms owns the drop rule and
	// exports it precisely so this package cannot drift from it again. Browse
	// mode gets an empty slice, which is excerptAround's fall-through.
	terms := store.SearchTerms(in.Query)
	out := searchEventsOutput{Events: make([]EventRecord, 0, len(events))}
	for _, event := range events {
		out.Events = append(out.Events, newEventRecord(event, maxTextChars, terms))
	}
	// The page boundary is read BEFORE the collapse, off the last store row, so
	// a representative that folded older rows cannot push the boundary back up
	// the page.
	fetched := len(events)
	next := searchCursor{Ranked: ranked,
		Since: formatBound(opts.Since), Until: formatBound(opts.Until)}
	if fetched > 0 {
		last := events[fetched-1]
		next.CapturedAt, next.ID = last.CapturedAtRaw, last.ID
		next.Offset = cursor.Offset + fetched
	}
	if !in.ExpandSimilar {
		// ponytail: one store page in, collapsed page out — a collapsed page is
		// short. Over-fetch in a loop only if short pages prove to cost more calls
		// than they save tokens.
		out.Events = collapseSimilarScreens(out.Events)
	}
	// Hoisted last, so media_dir describes the rows actually returned.
	out.MediaDir = hoistMediaDir(out.Events)

	// Two independent notices can apply at once, so compose the non-empty parts.
	var parts []string
	// Measured on what the STORE returned, never on what survived the collapse:
	// a page of 20 that folds to 6 has still exhausted the limit.
	capped := fetched == limit
	switch {
	case len(out.Events) == 0 && in.Cursor != "":
		// A walk that ends on an exactly-full page gets one more call returning
		// nothing. That is pagination finishing, not a search that failed, and
		// telling an agent to widen its filters here sends it to repair something
		// that was never broken.
		parts = append(parts, "this is the end of the results; the previous page was the last one "+
			"with rows in it, so there is nothing further to page to")
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
		//
		// Both modes have an answer now, and they are not the same answer, so the
		// notice says which one this is. Browse resumes exactly. Ranked counts
		// rows, and a caller that will act on the boundary should know that.
		out.NextCursor = encodeCursor(next)
		continuation := "pass cursor=next_cursor with the same filters to continue"
		if ranked {
			continuation += "; ranked paging counts rows, so a result captured while you page " +
				"can shift the boundary — narrow since/until if that matters"
		}
		if collapsed := len(out.Events); collapsed < fetched {
			// Reporting only one number would contradict the payload: "capped at
			// 20" beside six events, or "capped at 6" inventing a cap nothing
			// enforced.
			parts = append(parts, fmt.Sprintf(
				"%d fetched, collapsed to %d; there may be more — %s", fetched, collapsed, continuation))
		} else {
			parts = append(parts, fmt.Sprintf(
				"results were capped at %d events; there may be more — %s", limit, continuation))
		}
	}
	// An audio hit is a 30-second window of one track, which reads poorly as
	// conversation: the machine's speech still appears in both tracks here, and
	// a turn spanning two windows arrives as two results. Point at the tool that
	// answers those, but only when it would actually have something to show.
	if h.hasAttributedAudio(ctx, events) {
		parts = append(parts, "some results are audio: get_transcript returns these as one ordered "+
			"conversation with per-turn origin labels and the machine's own speech deduplicated")
	}
	// The provenance contract goes last, because it is longer than everything
	// before it and a caller reads the front of a notice first: the operational
	// clauses would be buried behind it. It is gated on the page actually holding
	// an audio row and NOT on hasAttributedAudio, which asks whether
	// get_transcript has anything to show — an unattributed, silent or
	// not-yet-backfilled audio row has no segments and still renders every field
	// the contract explains.
	if holdsAudio(out.Events) {
		parts = append(parts, audioProvenanceContract)
	}
	out.Notice = h.withStaleness(ctx, strings.Join(parts, "; "))
	return nil, out, nil
}

// collapseSimilarScreens folds a run of adjacent screen records showing the
// same app, window and display with near-identical text into one
// representative, which is the FIRST row of the run — so ranked order and
// browse order both survive the fold. Every dropped id lands on the
// representative's CollapsedIDs, so get_event still reaches all of them.
//
// It exists because a page of screen results is largely the same screen
// repeated: over the 200 most recent screen events, 76% of adjacent same-app
// pairs are more than 0.9 identical, median similarity 0.974, at a 2-10s
// capture cadence.
//
// AUDIO IS NEVER FOLDED, under any flag value, and that is a precondition
// rather than a filter layered on top. All 967 audio pairs on the live index
// share app, window and display_id — an audio row's display_id is always 0 —
// and their two transcripts are median 0.819 similar, 87 of 140 above 0.7,
// precisely because the microphone re-records what the speakers played. Keyed
// on those fields the collapse would merge a chunk's two tracks on the majority
// of pairs: the same defect the never-merge comment in searchEvents records as
// having deleted real transcripts, arrived at from a timestamp a second time.
// collapsed_ids does not rescue that — it preserves reachability, not content,
// so the microphone's account of the room would leave the visible result.
func collapseSimilarScreens(records []EventRecord) []EventRecord {
	if len(records) < 2 {
		return records
	}
	out := make([]EventRecord, 0, len(records))
	for _, record := range records {
		if len(out) > 0 {
			if rep := &out[len(out)-1]; sameScreenRun(*rep, record) {
				rep.CollapsedIDs = append(rep.CollapsedIDs, record.ID)
				rep.CollapsedCount = len(rep.CollapsedIDs)
				continue
			}
		}
		out = append(out, record)
	}
	return out
}

// sameScreenRun is the collapse precondition and its key in one place: BOTH
// rows must be screen rows before app, window or display_id is even looked at.
//
// The comparison is against the run's representative, never the immediately
// preceding row, so a run cannot drift arbitrarily far from what it claims to
// stand for.
func sameScreenRun(rep, next EventRecord) bool {
	if rep.Kind != string(store.KindScreen) || next.Kind != string(store.KindScreen) {
		return false
	}
	if rep.App != next.App || rep.Window != next.Window || rep.DisplayID != next.DisplayID {
		return false
	}
	return nearlyIdentical(rep.Text, next.Text)
}

// nearlyIdentical compares the already-truncated excerpts as word multisets,
// as a ratio against the longer side.
//
// A bag rather than a common prefix because the menu-bar clock sits at the TOP
// of a full-display OCR read: one changed character at rune 20 drops a prefix
// score to near zero on two frames of the same screen, which is the exact pair
// the collapse exists for.
//
// ponytail: prefix/shingle similarity, not real diffing; swap in a proper
// measure if collapse quality matters more than the page it saves.
func nearlyIdentical(a, b string) bool {
	const threshold = 0.9
	left, right := strings.Fields(a), strings.Fields(b)
	if len(left) == 0 || len(right) == 0 {
		return len(left) == len(right)
	}
	counts := make(map[string]int, len(left))
	for _, word := range left {
		counts[word]++
	}
	shared := 0
	for _, word := range right {
		if counts[word] > 0 {
			counts[word]--
			shared++
		}
	}
	longer := len(left)
	if len(right) > longer {
		longer = len(right)
	}
	return float64(shared)/float64(longer) >= threshold
}

// holdsAudio reports whether a page carries a row the provenance contract
// applies to. It reads the records rather than asking the store, because the
// question is about what this response renders and nothing else.
func holdsAudio(records []EventRecord) bool {
	for _, record := range records {
		if record.Kind == string(store.KindAudio) {
			return true
		}
	}
	return false
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
	// Notice reports build or schema skew, and nothing else — this tool has no
	// empty result to explain, since a missing id is an error. It is here because
	// this is the tool an agent calls for a *complete* answer after a truncated
	// one, which is exactly where silently missing fields matter most.
	Notice string `json:"notice,omitempty"`
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
	records := []EventRecord{newEventRecord(*event, 0, nil)}
	// get_event is the only tool that returns the metadata blob.
	records[0].Metadata = decodeMetadata(event.Metadata)
	// hoistMediaDir rewrites the record in place, so it has to see the slice the
	// result is read back out of. One record cannot split a kind across two
	// directories, so the map it returns holds at most this event's own kind.
	dirs := hoistMediaDir(records)
	// An audio event renders foreground_app, source_app and attribution here the
	// same way a search hit does, and this is where an agent comes for the
	// complete version of a row it has already half-read — so the contract has to
	// reach it here too, not only through search_events.
	var notice string
	if holdsAudio(records) {
		notice = audioProvenanceContract
	}
	return nil, getEventOutput{
		Event:    records[0],
		MediaDir: dirs[records[0].Kind],
		// withStaleness rather than stalenessNotice, so skew still comes first
		// once this tool has a body to put after it.
		Notice: h.withStaleness(ctx, notice),
	}, nil
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
	out.Notice = h.withStaleness(ctx, out.Notice)
	return nil, out, nil
}
