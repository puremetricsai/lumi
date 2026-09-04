package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"
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
// Idleness here means no call outstanding, no handler running, and no frame being
// written — see idleLocked, which reads all three, because a request is in flight
// from the moment it leaves the pipe until its reply has been written and neither
// end of that is where the handler runs. That is the correctness guarantee; this
// duration is the margin on top of it, covering the gap between two requests of
// one exchange rather than the gap inside a single request. A client mid-conversation
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
	// outstanding holds the IDs of calls read off the wire whose response has not
	// finished being written.
	//
	// This is the other end of a request's life from inFlight, and neither half
	// sees it: jsonrpc2 reads a request and appends it to an unbounded
	// handlerQueue before any handler starts (acceptRequest in
	// internal/jsonrpc2/conn.go), so a call the client is already waiting on
	// increments nothing until handleAsync reaches it. Replacing the image there
	// discards a request that was read out of the pipe and can never be recovered
	// — the new image cannot re-read what this one consumed — and the client hangs
	// exactly as it does when a reply is exec'd away.
	//
	// Keyed by ID rather than counted, because the retire signal has to be the
	// matching response: a count would need reads and dispatches to correspond
	// one-to-one, and they do not (a response to a server-initiated call also
	// arrives through Read, and a request rejected during shutdown never reaches
	// the handler at all). Every call gets a response — processResult guarantees
	// it for IsCall, on the error paths too — so an ID retires itself and cannot
	// leak.
	//
	// Notifications are deliberately absent. They carry no ID and receive no
	// response, so nothing here could ever retire one, and a permanently
	// outstanding entry would block every future upgrade — a worse failure than
	// the one being fixed. Losing a notification also hangs nobody: no client is
	// blocked on a reply to it. The handler-side count still covers them once they
	// are dispatched.
	outstanding map[any]struct{}
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

// received records a call read off the wire, before any handler has seen it.
func (u *selfUpdater) received(id any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.outstanding == nil {
		u.outstanding = make(map[any]struct{}, 1)
	}
	u.outstanding[id] = struct{}{}
	u.lastActivity = time.Now()
}

// answered retires a call once its response has been written.
func (u *selfUpdater) answered(id any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.outstanding, id)
	u.lastActivity = time.Now()
}

// idleFor reports whether nothing has been in flight for at least d.
func (u *selfUpdater) idleFor(d time.Duration) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.idleLocked(d)
}

// idleLocked is the idleness rule itself, stated once. Both readers of it —
// idleFor and claimIdle — must agree exactly: the whole purpose of claimIdle is to
// act on this decision without releasing the lock, so a second copy of the
// predicate that drifted from this one would silently license the replacement the
// guard exists to withhold.
//
// All three counters are consulted, because each covers a different stretch of a
// request's life and any one alone reports a session idle while the client is
// still waiting: outstanding spans read-to-response, inFlight covers the handler
// and the write, and lastActivity supplies the quiet margin.
func (u *selfUpdater) idleLocked(d time.Duration) bool {
	return u.inFlight == 0 && len(u.outstanding) == 0 &&
		!u.lastActivity.IsZero() && time.Since(u.lastActivity) >= d
}

// claimIdle runs replace if the session is idle, with the lock held across both,
// and reports whether it ran.
//
// Holding u.mu for the duration is the point, not an implementation detail. Every
// path that admits new work — received, begin, and end — takes the same lock, so
// while replace runs no request can be recorded behind the decision that was just
// made on its absence. Checking idleness and then exec'ing outside the lock is the
// defect: the check passes, a request lands in the interval, and the replacement
// discards it while the client waits for a reply that no longer has a process to
// come from.
//
// Blocking those paths for the length of an execve is safe because a successful
// execve never returns — there is no caller left to unblock, and the new image
// re-reads nothing this one consumed. On failure the lock is released normally and
// the server carries on, which is why the guard cannot wedge future upgrades: it
// is scoped to one attempt rather than latched until an upgrade succeeds.
func (u *selfUpdater) claimIdle(quiet time.Duration, replace func() error) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.idleLocked(quiet) {
		return false, nil
	}
	return true, replace()
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

// trackWrites wraps a transport so a request counts as in-flight work from the
// moment it is read off the wire until its response has finished being written.
//
// This is the acknowledgement the handler count cannot give, at both ends. The
// write is the last thing that happens for a request and the part that blocks on
// the client, so a process that waits for it cannot exec away from a reply it has
// already committed to sending. The read is the *first* thing that happens, and
// waiting for it is what closes the window the handler count leaves open ahead of
// dispatch — see selfUpdater.outstanding. Intercepting both on one connection is
// what makes the guard symmetric: a request is covered for its whole life rather
// than for the middle of it.
func (u *selfUpdater) trackWrites(t sdk.Transport) sdk.Transport {
	return &writeTrackingTransport{delegate: t, updater: u}
}

// stdioTransport synchronizes at the byte-consumption boundary. Wrapping only
// Connection.Read is too late: IOTransport's decoder goroutine may already have
// drained a complete frame from stdin before Connection.Read returns it.
func (u *selfUpdater) stdioTransport() sdk.Transport {
	return u.trackWrites(&sdk.IOTransport{
		Reader: &guardedReader{file: os.Stdin, updater: u},
		Writer: nopWriteCloser{Writer: os.Stdout},
	})
}

// guardedReader leaves bytes in the inherited pipe until they can be consumed
// under the same lock as claimIdle. Polling happens outside the lock because an
// idle stdin normally blocks indefinitely.
type guardedReader struct {
	file    *os.File
	updater *selfUpdater
	ready   func() // test hook: called after poll reports bytes, before mu is acquired
}

func (r *guardedReader) Read(p []byte) (int, error) {
	for {
		_, err := unix.Poll([]unix.PollFd{{Fd: int32(r.file.Fd()), Events: unix.POLLIN}}, -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if r.ready != nil {
			r.ready()
		}

		r.updater.mu.Lock()
		n, readErr := r.file.Read(p)
		if n > 0 {
			// Make a read visible to claimIdle before the SDK decoder has turned
			// these bytes into a request and writeTrackingConn records its ID.
			r.updater.lastActivity = time.Now()
		}
		r.updater.mu.Unlock()
		return n, readErr
	}
}

func (r *guardedReader) Close() error { return nil }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

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

// writeTrackingConn embeds the real connection so Close and SessionID —
// including any method the SDK's Connection interface gains later — keep their
// behavior untouched. Only Read and Write are intercepted.
type writeTrackingConn struct {
	sdk.Connection
	updater *selfUpdater
}

// Read marks an incoming call outstanding the instant it leaves the pipe.
//
// The tracking goes after the read returns, not around it: Read blocks waiting
// for the client, which is the session's whole idle time, so counting the wait as
// in-flight work would mean the process could never be replaced at all.
func (c *writeTrackingConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err != nil {
		return msg, err
	}
	// Only a call with an ID is tracked. A notification has none and gets no
	// response, so nothing would ever retire it; a response arriving here belongs
	// to a call this server made, and is not work a client is waiting on.
	if request, ok := msg.(*jsonrpc.Request); ok && request.ID.IsValid() {
		c.updater.received(request.ID.Raw())
	}
	return msg, nil
}

func (c *writeTrackingConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	c.updater.begin()
	defer c.updater.end()
	err := c.Connection.Write(ctx, msg)
	// Retired after the write completes, so the gap between a handler returning
	// and its reply reaching the client is never mistaken for idleness. Retired
	// even when the write failed: the response will not be sent, and holding the
	// ID forever would block every future upgrade over a connection that is
	// already broken.
	if response, ok := msg.(*jsonrpc.Response); ok && response.ID.IsValid() {
		c.updater.answered(response.ID.Raw())
	}
	return err
}

// restartNeeded folds the two reasons this process should replace itself into
// the one predicate the watcher polls: the binary was upgraded, or the database
// it opened was replaced underneath it. Both are invisible from the rows, and
// re-exec is the same fix for both — a fresh image re-opens whatever is at the
// path with whatever key is now stored.
func restartNeeded(opts Options) func() bool {
	return func() bool {
		if opts.BinaryChanged != nil && opts.BinaryChanged() {
			return true
		}
		return opts.DatabaseReplaced != nil && opts.DatabaseReplaced()
	}
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
		if !u.changed() {
			continue
		}
		// The idleness check and the exec have to be one atomic decision. Checking
		// and then exec'ing without the lock re-opens the race this guard exists to
		// close from the other side: a request read in the interval increments
		// nothing the already-made decision can see, and the replacement carries it
		// away. claimIdle keeps the lock held across both.
		replaced, err := u.claimIdle(quiet, func() error {
			if err := stashSessionState(state()); err != nil {
				return err
			}
			logger.Info("the lumi binary or its index changed on disk; replacing this process in place")
			// On success this does not return: same pid, same fds 0/1/2, newer code.
			return u.exec()
		})
		if !replaced {
			continue
		}
		if err != nil {
			// Either the handshake could not be preserved — a replacement coming up
			// cold would reject every subsequent request, which is strictly worse
			// than serving the old build — or execve itself failed, leaving this
			// image intact and still serving. Both keep running, and both drop the
			// stashed state: keeping it would hand a stale handshake to some
			// unrelated child this process later spawns.
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
