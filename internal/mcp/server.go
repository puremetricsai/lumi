package mcp

import (
	"context"
	"io"
	"os"
	"time"

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
}

// Serve runs the MCP server on stdin/stdout until the client closes the
// connection or ctx is cancelled.
//
// The stdio transport owns os.Stdout: every byte written there must be a
// JSON-RPC frame. A stray print, a default slog handler, or a cobra usage dump
// corrupts the stream, and the failure reaches the user as "the agent cannot
// see Lumi" with no error text anywhere. Diagnostics go to stderr.
func Serve(ctx context.Context, s *store.Store, opts Options) error {
	// sdk.StdioTransport reads os.Stdin directly, and the SDK's connection
	// treats a read EOF as an immediate signal to reject any response still
	// being written — even for a call it already accepted. A client (or a
	// non-interactive caller, such as this project's own smoke test) that
	// writes its last request and closes stdin without waiting to read the
	// reply can therefore lose that reply outright: the handler finishes and
	// tries to write, but the connection has already decided it is shutting
	// down. stdinEOFGrace wraps stdin so the SDK only learns about EOF after a
	// short delay, giving an in-flight handler (a local SQLite query, in
	// practice sub-millisecond to a few milliseconds) room to finish and flush
	// before the connection tears down. It costs nothing on the common path —
	// a still-open stdin never sees the delay — and only applies once, to the
	// read that actually observes end of stream.
	transport := &sdk.IOTransport{
		Reader: io.NopCloser(stdinEOFGrace{os.Stdin}),
		Writer: nopCloseWriter{os.Stdout},
	}
	return newServer(s, opts).Run(ctx, transport)
}

// stdinEOFGraceDelay bounds how long Serve waits after stdin reports EOF
// before passing that EOF along. It is not a promise — a handler slower than
// this can still lose its response — but it comfortably covers the local
// SQLite reads and JSON encoding this server's tools do.
const stdinEOFGraceDelay = 200 * time.Millisecond

// stdinEOFGrace delays reporting an EOF (or any other read error, since a
// closed pipe on this end reads the same way) from the wrapped reader, so a
// handler that is still writing its response is not raced by the SDK's
// shutdown-on-EOF behavior.
type stdinEOFGrace struct{ r io.Reader }

func (g stdinEOFGrace) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	if err != nil {
		time.Sleep(stdinEOFGraceDelay)
	}
	return n, err
}

// nopCloseWriter adapts os.Stdout to io.WriteCloser without ever actually
// closing the process's real stdout, mirroring sdk.StdioTransport's own
// nopCloserWriter (which is unexported, hence this local copy).
type nopCloseWriter struct{ io.Writer }

func (nopCloseWriter) Close() error { return nil }

// newServer builds the server and registers the tools. It is separate from
// Serve so tests can drive it over an in-memory transport.
func newServer(s *store.Store, opts Options) *sdk.Server {
	if opts.Name == "" {
		opts.Name = "lumi"
	}
	server := sdk.NewServer(&sdk.Implementation{Name: opts.Name, Version: opts.Version}, nil)
	h := &handlers{store: s}

	// AddTool infers each tool's input and output schema from the Go types and
	// validates arguments before the handler runs, so the descriptions below are
	// the only thing an agent has to go on. They say what the tool is for, not
	// just what it does.
	sdk.AddTool(server, &sdk.Tool{
		Name: "search_events",
		Description: "Search the user's captured screen text and audio transcripts. " +
			"Every parameter is optional; with none it returns the most recent activity. " +
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

	return server
}
