package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessionStateEnv carries the MCP handshake across a re-exec.
//
// The 2025-11-25 protocol this SDK speaks gates every method on having seen
// `initialize`: a fresh process that inherited a live connection would reject
// the client's next request with "method is invalid during session
// initialization" (go-sdk's ServerSession.handle). The replacement therefore has
// to be told the handshake already happened, and an environment variable is the
// only channel that survives execve alongside the file descriptors.
//
// It holds the client's own advertised identity and capabilities — no captured
// data, and nothing from the index.
const sessionStateEnv = "LUMI_MCP_SESSION_STATE"

// reexecQuietPeriod is how long a session must be idle before the process will
// replace itself.
//
// Idleness here means no handler running *and* no frame being written — see
// selfUpdater.inFlight, which counts both, because a handler returning is not the
// end of a request. That is the correctness guarantee; this duration is the
// margin on top of it, covering the gap between two requests of one exchange
// rather than the gap inside a single request. A client mid-conversation
// typically follows one tool call with another, and replacing the process between
// them is safe but pointless churn.
const reexecQuietPeriod = 3 * time.Second

// reexecCheckInterval is how often the binary is re-stamped. An upgrade is not
// urgent — it only has to be picked up before the user's next question — and a
// stat of one file at this rate is free.
const reexecCheckInterval = 5 * time.Second

// selfUpdater replaces the process when its binary changes on disk.
//
// Both function fields are injected rather than called directly, because both
// are things this package does not do: `internal/mcp` reads the store and
// nothing else of the machine, and the no-filesystem-access rule in its
// CLAUDE.md is kept true by construction rather than by test. internal/cli
// supplies the real implementations from internal/selfexec.
type selfUpdater struct {
	// changed reports whether the watched binary has been replaced.
	changed func() bool
	// exec replaces the process image. It returns only on failure.
	exec func() error
	// checkInterval and quietPeriod override the constants above. They exist so
	// tests can drive the timing directly instead of sleeping through it; zero
	// means use the constant.
	checkInterval time.Duration
	quietPeriod   time.Duration

	mu sync.Mutex
	// inFlight counts work that must finish before the process may be replaced:
	// handlers still running, and replies still being written to the transport.
	//
	// Both are counted because a handler returning is *not* the end of a request.
	// jsonrpc2 serializes and writes the reply after Handle returns
	// (processResult calls c.write), and the SDK's ioConn.Write ends in a blocking
	// write to stdout — so a client that has stopped draining its pipe leaves the
	// reply pending with every handler already finished. Counting only handlers
	// reported such a session idle, and exec'ing there takes the reply with it and
	// hangs the client on a response nobody will ever send.
	inFlight int
	// lastActivity is when in-flight work most recently started or finished.
	lastActivity time.Time
}

// begin records the start of in-flight work.
func (u *selfUpdater) begin() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.inFlight++
	u.lastActivity = time.Now()
}

// end records the completion of in-flight work.
func (u *selfUpdater) end() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.inFlight--
	u.lastActivity = time.Now()
}

// idleFor reports whether nothing has been in flight for at least d.
func (u *selfUpdater) idleFor(d time.Duration) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.inFlight == 0 && !u.lastActivity.IsZero() && time.Since(u.lastActivity) >= d
}

// middleware counts running handlers.
//
// It is necessary but not sufficient on its own: it closes the window where a
// handler is still computing, while trackWrites closes the window after it
// returns. Both are needed, and neither subsumes the other — a handler can run
// with nothing queued to write, and a reply can be mid-write with no handler
// left running.
func (u *selfUpdater) middleware() sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			u.begin()
			defer u.end()
			return next(ctx, method, req)
		}
	}
}

// trackWrites wraps a transport so every frame written to the client counts as
// in-flight work for as long as the write is in progress.
//
// This is the acknowledgement the handler count cannot give. The write is the
// last thing that happens for a request and it is the part that blocks on the
// client, so a process that waits for it to finish cannot exec away from a reply
// it has already committed to sending.
func (u *selfUpdater) trackWrites(t sdk.Transport) sdk.Transport {
	return &writeTrackingTransport{delegate: t, updater: u}
}

type writeTrackingTransport struct {
	delegate sdk.Transport
	updater  *selfUpdater
}

func (t *writeTrackingTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := t.delegate.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &writeTrackingConn{Connection: conn, updater: t.updater}, nil
}

// writeTrackingConn embeds the real connection so Read, Close and SessionID —
// including any method the SDK's Connection interface gains later — keep their
// behavior untouched. Only Write is intercepted.
type writeTrackingConn struct {
	sdk.Connection
	updater *selfUpdater
}

func (c *writeTrackingConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	c.updater.begin()
	defer c.updater.end()
	return c.Connection.Write(ctx, msg)
}

// watch replaces the process once the binary has changed and the session has
// gone quiet. It returns when ctx is done, or does not return at all.
func (u *selfUpdater) watch(ctx context.Context, state func() *sdk.ServerSessionState, logger *slog.Logger) {
	interval, quiet := u.checkInterval, u.quietPeriod
	if interval <= 0 {
		interval = reexecCheckInterval
	}
	if quiet <= 0 {
		quiet = reexecQuietPeriod
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !u.changed() || !u.idleFor(quiet) {
			continue
		}
		if err := stashSessionState(state()); err != nil {
			// Without the handshake the replacement would reject every
			// subsequent request, which is strictly worse than serving the old
			// build. Keep running.
			logger.Error("not replacing this process: could not preserve the MCP session", "error", err)
			continue
		}
		logger.Info("lumi binary changed on disk; replacing this process in place")
		// On success this does not return: same pid, same fds 0/1/2, newer code.
		if err := u.exec(); err != nil {
			// execve failed, so this image is still intact and still serving.
			// Drop the stashed state again — keeping it would hand a stale
			// handshake to some unrelated child this process later spawns.
			os.Unsetenv(sessionStateEnv)
			logger.Error("could not replace this process; continuing on the current build", "error", err)
		}
	}
}

// stashSessionState puts the live handshake where the replacement will find it.
func stashSessionState(state *sdk.ServerSessionState) error {
	if state == nil || state.InitializeParams == nil {
		return fmt.Errorf("no completed handshake to carry across")
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode session state: %w", err)
	}
	if err := os.Setenv(sessionStateEnv, string(encoded)); err != nil {
		return fmt.Errorf("set %s: %w", sessionStateEnv, err)
	}
	return nil
}

// restoreSessionState reads a handshake stashed by the process this one
// replaced, and clears it.
//
// A nil result is the ordinary case: a server the client launched itself has no
// handshake yet and will do one. The variable is unset either way so it cannot
// outlive this process into an unrelated child.
//
// Unparseable state is discarded rather than fatal. It can only mean a version
// skew in the SDK's own state type across the upgrade that just happened, and
// the recoverable reading of that is "this is a fresh session": the client is
// mid-session and will not re-handshake, so its next request fails and it
// reports the server as broken — visible, and fixed by restarting the session.
// Refusing to start would produce the same outcome with less information.
func restoreSessionState(logger *slog.Logger) *sdk.ServerSessionState {
	encoded, ok := os.LookupEnv(sessionStateEnv)
	os.Unsetenv(sessionStateEnv)
	if !ok || encoded == "" {
		return nil
	}
	var state sdk.ServerSessionState
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		logger.Error("ignoring unreadable session state from the previous build", "error", err)
		return nil
	}
	if state.InitializeParams == nil {
		return nil
	}
	logger.Info("resumed an MCP session across a binary upgrade")
	return &state
}
