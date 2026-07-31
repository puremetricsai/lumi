package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/puremetricsai/lumi/internal/store"
)

// Options carries what the server advertises during the MCP handshake, and
// nothing else. Keeping Lumi's identity here — rather than as constants in this
// package — is what lets internal/cli own the version string while this package
// stays a plain function of a store.
type Options struct {
	Name    string
	Version string
	// DatabasePath is the resolved database file the store was opened from. It
	// is surfaced only in "this index is empty" tool notices, so a mistyped
	// --data-dir (which openStore happily creates as a fresh empty database)
	// reads as "wrong/empty file" rather than "you never recorded anything" —
	// it leaks nothing new, since every media_path a tool returns already sits
	// under that same directory. Optional; an empty value falls back to the
	// generic notice.
	DatabasePath string
}

// Serve runs the MCP server on stdin/stdout until the client closes the
// connection or ctx is cancelled.
//
// The stdio transport owns os.Stdout: every byte written there must be a
// JSON-RPC frame. A stray print, a default slog handler, or a cobra usage dump
// corrupts the stream, and the failure reaches the user as "the agent cannot
// see Lumi" with no error text anywhere. Diagnostics go to stderr.
func Serve(ctx context.Context, s *store.Store, opts Options) error {
	return newServer(s, opts).Run(ctx, &sdk.StdioTransport{})
}

// audioProvenanceContract is the part of search_events' description that keeps
// an audio row's three provenance fields from being read as one.
//
// It is stated here, in the tool text, because that text is loaded into an
// agent's context before any row is fetched — so it is the real contract, and a
// field an agent misreads once it has the data is a field the description
// failed. The three fields answer different questions and routinely disagree:
// the ordinary case is a video playing in a background browser while the user
// works somewhere else.
//
// The microphone sentence is the load-bearing one. Lumi cannot tell who is
// speaking in the room and does not try, so the honest output is an ambiguity
// stated outright rather than a guess. Raw audio is deleted on the retention
// schedule while any summary built from it survives, which makes a fabricated
// speaker permanent.
//
// The pairing sentences are there for the same reason and were learned the same
// way. search_events used to merge each chunk's two rows into one on the
// strength of a shared captured_at, which asserted they held the same sound
// when all they shared was a 30-second interval; a whole microphone transcript
// could be dropped and the result still read as finished.
//
// They say what this tool does *not* do rather than what a caller will receive,
// which is the only version that stays true. Every filter here is a predicate on
// a single row — an FTS MATCH, require_text, the LIMIT — so one row of a pair
// arrives alone routinely, and over a window of silent system tracks
// require_text returns nothing else. Promising that "both are returned" would
// re-create the original defect from the other side: an agent holding one row
// would read it as the whole chunk. The honest contract is that the pair is
// never merged, that a lone row proves nothing about its counterpart, and that
// get_transcript is what reads a chunk whole.
const audioProvenanceContract = "Audio results keep three different things apart. " +
	"audio_source is the capture DEVICE the row was read from — \"system\" is the machine's own " +
	"output, \"microphone\" is the room — and is never a claim about who or what made the sound. " +
	"source_app names the applications observed producing sound while the chunk recorded, and is the " +
	"only field that names a source at all. foreground_app is what the user had focused at the time " +
	"and is NOT the source of anything. attribution says how source_app was earned: " +
	"\"emitting_process\" means CoreAudio reported those processes holding an audio output stream — " +
	"trust source_app only here; \"foreground_inferred\" means CoreAudio named no process — usually " +
	"because it found none, sometimes because it could not be read — but an on-screen window's own " +
	"title said it was playing audio; \"unattributed\" means nothing named a " +
	"source. Microphone audio has NO reliable source — it may be the user, other people present, a " +
	"TV, another machine, or ambient playback. Microphone rows are always \"unattributed\" and carry " +
	"no source_app. Never attribute microphone content to a person or to an application, and never " +
	"present it as something the user said or did. " +
	"Audio rows come in PAIRS sharing one captured_at — one per capture device — and this tool never " +
	"merges them. Whether both rows of a pair reach you depends on your query and filters, which are " +
	"applied per ROW: a query matching only one track's transcript, require_text, or a limit falling " +
	"between the two all return one row of a pair. So a single audio row is NOT evidence that the " +
	"chunk held one track, and must never be read as the whole chunk. A pair also shares a 30-second " +
	"interval, not necessarily a sound: the microphone re-records whatever the speakers play, so the " +
	"two rows may transcribe the same speech twice, or hold two entirely different conversations. " +
	"Nothing in a search result tells you which, so never assume one row of a pair is redundant. " +
	"get_transcript answers both questions — it reads each chunk whole and returns the conversation " +
	"once, in order, with the machine's own speech deduplicated."

// newServer builds the server and registers the tools. It is separate from
// Serve so tests can drive it over an in-memory transport.
func newServer(s *store.Store, opts Options) *sdk.Server {
	if opts.Name == "" {
		opts.Name = "lumi"
	}
	server := sdk.NewServer(&sdk.Implementation{Name: opts.Name, Version: opts.Version}, nil)
	h := &handlers{store: s, databasePath: opts.DatabasePath}

	// AddTool infers each tool's input and output schema from the Go types and
	// validates arguments before the handler runs, so the descriptions below are
	// the only thing an agent has to go on. They say what the tool is for, not
	// just what it does.
	sdk.AddTool(server, &sdk.Tool{
		Name: "search_events",
		Description: "Search the user's captured screen text and audio transcripts. " +
			"Every parameter is optional; with none it returns the most recent activity. " +
			"Results are ranked by relevance when query is set, and newest-first otherwise — " +
			"with a query, result[0] is the best match, not necessarily the most recent. " +
			"Text is capped per event — when a result says truncated, call get_event for the full text. " +
			"Returns text and metadata only: screenshots and audio never leave the user's machine, " +
			"and media_path is a local path the user can open themselves. " +
			audioProvenanceContract,
	}, h.searchEvents)

	sdk.AddTool(server, &sdk.Tool{
		Name: "get_event",
		Description: "Fetch one captured event by id, with its text in full (never truncated) " +
			"and its processor metadata. Use this after search_events reports truncated text. " +
			"An audio event carries foreground_app (what the user had focused), source_app (what was " +
			"observed producing the sound), and attribution (how that was earned) as three separate " +
			"fields; see search_events. Microphone content is never attributed to a person or an app.",
	}, h.getEvent)

	sdk.AddTool(server, &sdk.Tool{
		Name: "list_apps",
		Description: "List the applications the user's activity was captured from, most active first, " +
			"or — with app set — the window titles seen for one application. " +
			"Call this before filtering by app so the filter values are real ones rather than guesses. " +
			"Every app named here is what the user was focused on, never what produced the content: " +
			"screen text is full-display OCR that can include other applications' windows, and an audio " +
			"chunk's app is whatever was focused while it recorded, not the source of the sound. " +
			"On an audio event that same value is returned as foreground_app, and the sound's actual " +
			"source — where one could be determined — is the event's source_app, which is never counted here. " +
			"Counts span both kinds unless kind narrows them; check the split before filtering " +
			"search_events by an app, since that filter spans both too.",
	}, h.listApps)

	sdk.AddTool(server, &sdk.Tool{
		Name: "get_transcript",
		Description: "Read captured audio as one ordered conversation instead of separate search hits. " +
			"Lumi records two audio tracks, so this labels every turn by where the sound came from: " +
			"\"internal\" is sound this machine produced — the far side of a call, a video, music, " +
			"a notification — and is NOT necessarily a person; \"external\" is sound the microphone " +
			"picked up from the room, usually the user; \"unknown\" means machine audio was playing " +
			"but produced no transcript, so the origin could not be determined. " +
			"Because the machine's audio also bleeds into the microphone, its words are deduplicated here — " +
			"a phrase the machine played appears once, not twice as in search_events. " +
			"Turns are chronological, never ranked by relevance. " +
			"Every turn carries confidence and order_confidence: order_confidence \"exact\" means the " +
			"position was measured, \"sequence\" means the order is reliable but absolute times are not " +
			"known, and \"approximate\" means the position was inferred. " +
			"Turns may overlap in time when both parties spoke at once; those are marked overlaps. " +
			"A transcript that stops short — because the range held more audio than one call returns, " +
			"or because max_turns capped it — says so in its notice and returns resume_from: pass that " +
			"as since to read the next page. Continue from resume_from and not from the last turn's " +
			"time, which would repeat turns you already have. " +
			"Use event_ids with get_event to read either track's raw, undeduplicated transcript. " +
			"A turn's origin names provenance relative to this machine and is a different axis from an " +
			"event's source_app. No turn is attributed to a person: \"external\" audio came from the " +
			"room's microphone and may be the user, other people present, a TV, another machine, or " +
			"ambient playback. Never present it as something the user said or did.",
	}, h.getTranscript)

	return server
}
