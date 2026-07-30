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
			"and media_path is a local path the user can open themselves.",
	}, h.searchEvents)

	sdk.AddTool(server, &sdk.Tool{
		Name: "get_event",
		Description: "Fetch one captured event by id, with its text in full (never truncated) " +
			"and its processor metadata. Use this after search_events reports truncated text.",
	}, h.getEvent)

	sdk.AddTool(server, &sdk.Tool{
		Name: "list_apps",
		Description: "List the applications the user's activity was captured from, most active first, " +
			"or — with app set — the window titles seen for one application. " +
			"Call this before filtering by app so the filter values are real ones rather than guesses.",
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
			"A range holding more audio than one call can return says so in its notice and names the " +
			"timestamp to resume from, so a long window is read in passes rather than silently cut short. " +
			"Use event_ids with get_event to read either track's raw, undeduplicated transcript.",
	}, h.getTranscript)

	return server
}
