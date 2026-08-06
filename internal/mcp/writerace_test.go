package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// blockingWriter accepts a write and then stalls, standing in for a client that
// has stopped draining its end of the pipe. released is closed once the write has
// been observed, so a test can see the reply was in progress.
type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	writes  atomic.Int64
	// stallOn is the 1-indexed write to block on. Counting writes rather than
	// racing a timer is what makes the arming deterministic: write 1 is the
	// initialize reply, write 2 is the tools/call reply this test needs stalled.
	stallOn  int64
	once     atomic.Bool
	released atomic.Bool
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	if w.writes.Add(1) == w.stallOn && w.once.CompareAndSwap(false, true) {
		close(w.started)
		<-w.release
	}
	return len(p), nil
}

func (w *blockingWriter) Close() error { return nil }

// pipeReader feeds pre-written frames to the server and then blocks, so the
// session stays open rather than ending at EOF.
type pipeReader struct {
	frames []string
	next   int
	done   chan struct{}
}

func (r *pipeReader) Read(p []byte) (int, error) {
	if r.next < len(r.frames) {
		frame := r.frames[r.next] + "\n"
		r.next++
		return copy(p, frame), nil
	}
	<-r.done
	return 0, io.EOF
}

func (r *pipeReader) Close() error { return nil }

// This is Codex's finding, reproduced. The SDK decrements in-flight when the
// handler returns, then serializes and writes the reply — jsonrpc2's
// processResult calls c.write *after* Handle returns, and ioConn.Write ends in a
// blocking rwc.Write. So a client that has stopped reading leaves the reply
// pending on a write while this updater already considers the session idle.
//
// If this test fails, the window has been closed and the guard is doing its job.
func TestUpdaterMustNotBeIdleWhileAReplyIsStillBeingWritten(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	writer := &blockingWriter{
		started: make(chan struct{}), release: make(chan struct{}),
		stallOn: 2, // 1 is the initialize reply; 2 is the tools/call reply.
	}
	reader := &pipeReader{done: make(chan struct{})}
	// Deferred cleanup runs LIFO, so the writer must be released *before*
	// session.Close() — closing the session waits on the write this test is
	// deliberately blocking, which would deadlock the test rather than fail it.
	defer close(reader.done)
	releaseWriter := func() {
		if !writer.released.Swap(true) {
			close(writer.release)
		}
	}
	defer releaseWriter()

	handshake, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "slow-client", "version": "0"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "list_apps", "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader.frames = []string{
		string(handshake),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		string(call),
	}

	updater := &selfUpdater{
		changed:       func() bool { return true },
		exec:          func() error { return nil },
		checkInterval: time.Millisecond,
		quietPeriod:   50 * time.Millisecond,
	}
	server := newServer(testStore(t), Options{Name: "lumi", Version: "test"})
	server.AddReceivingMiddleware(updater.middleware())

	// trackWrites is the guard under test, so it has to be in the path here
	// exactly as Serve installs it.
	transport := updater.trackWrites(&sdk.IOTransport{Reader: reader, Writer: writer})
	session, err := server.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Released above before this runs, so Close is never waiting on the stall.
	defer session.Close()

	select {
	case <-writer.started:
	case <-ctx.Done():
		t.Fatal("the tools/call reply was never written; the harness did not reach the case under test")
	}

	// The reply is provably mid-write right now: Write was entered and has not
	// returned. Wait past the quiet period and ask whether the updater would exec.
	time.Sleep(4 * updater.quietPeriod)
	idle := updater.idleFor(updater.quietPeriod)
	releaseWriter()
	if idle {
		t.Fatalf("updater reports the session idle while a reply is still being written: " +
			"re-execing here would drop that reply and hang the client")
	}
}

// A guard against overcorrecting: a genuinely finished exchange — reply written,
// nothing pending — must still be replaceable, or the upgrade never happens.
func TestUpdaterIsIdleOnceTheReplyHasBeenWritten(t *testing.T) {
	ctx := context.Background()
	updater := &selfUpdater{changed: func() bool { return true }, quietPeriod: time.Millisecond}
	server := newServer(testStore(t), Options{Name: "lumi", Version: "test"})
	server.AddReceivingMiddleware(updater.middleware())

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	session, err := server.Connect(ctx, serverTransport, &sdk.ServerSessionOptions{
		State: &sdk.ServerSessionState{
			InitializeParams:  &sdk.InitializeParams{ProtocolVersion: "2025-11-25"},
			InitializedParams: &sdk.InitializedParams{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	stream, err := clientTransport.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	id, err := jsonrpc.MakeID(float64(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Write(ctx, &jsonrpc.Request{
		ID: id, Method: "tools/list", Params: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatal(err)
	}

	// The client has the reply in hand, so nothing is outstanding.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if updater.idleFor(updater.quietPeriod) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("a fully delivered exchange must be considered idle, or no upgrade ever happens")
}

// The read-side guard, asserted directly on the connection wrapper rather than
// through a live session, so the window is deterministic instead of racing the
// handler goroutine.
//
// A call read off the wire is outstanding immediately, before any handler or
// middleware has run, and stays outstanding until the matching response has been
// written. This is what the handler count cannot express, and it fails if Read is
// taken out of the transport chain.
func TestAReadCallIsOutstandingBeforeAnyHandlerRuns(t *testing.T) {
	ctx := context.Background()
	updater := &selfUpdater{changed: func() bool { return true }}
	conn := &writeTrackingConn{Connection: &scriptedConn{}, updater: updater}

	id, err := jsonrpc.MakeID(float64(7))
	if err != nil {
		t.Fatal(err)
	}
	scripted := conn.Connection.(*scriptedConn)
	scripted.incoming = []jsonrpc.Message{&jsonrpc.Request{ID: id, Method: "tools/call"}}

	// A completed exchange first, so lastActivity is set and the session would
	// otherwise be replaceable. Without it idleFor is false for a reason that has
	// nothing to do with this guard — a never-used updater is never idle — and the
	// assertion below would hold even with the read tracking removed.
	updater.begin()
	updater.end()
	if !updater.idleFor(0) {
		t.Fatal("a finished exchange must be idle; the premise of this test is broken")
	}

	if _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	// No handler has run and nothing has been written, so neither existing counter
	// has moved. Only the read-side tracking stands between this request and an
	// exec that would discard it.
	if updater.inFlight != 0 {
		t.Fatalf("inFlight = %d; this test must exercise the read guard alone", updater.inFlight)
	}
	if updater.idleFor(0) {
		t.Fatal("a call that has been read off the wire but not dispatched reports the session " +
			"idle: exec'ing here discards a request the client is waiting on, and the " +
			"replacement cannot re-read it")
	}

	// The response retires it, and only then may the process be replaced.
	if err := conn.Write(ctx, &jsonrpc.Response{ID: id}); err != nil {
		t.Fatal(err)
	}
	if !updater.idleFor(0) {
		t.Fatal("an answered call must stop holding the guard, or no upgrade ever happens")
	}
}

// Synchronization at Connection.Read is too late for IOTransport: its decoder
// goroutine has already consumed the bytes. The stdin reader itself must leave a
// newly arrived frame in the pipe while claimIdle owns the replacement decision.
func TestRequestBytesAreNotConsumedDuringReplacement(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	updater := &selfUpdater{}
	updater.begin()
	updater.end()
	readReady := make(chan struct{})
	guarded := &guardedReader{
		file: reader, updater: updater,
		ready: func() { close(readReady) },
	}
	readDone := make(chan struct{})

	ran, err := updater.claimIdle(0, func() error {
		go func() {
			buf := make([]byte, 128)
			_, _ = guarded.Read(buf)
			close(readDone)
		}()
		if _, err := writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")); err != nil {
			return err
		}
		select {
		case <-readReady:
		case <-time.After(time.Second):
			t.Fatal("reader did not observe the request bytes")
		}
		select {
		case <-readDone:
			t.Fatal("request bytes were consumed after the idle decision but before replacement")
		default:
		}
		return nil // stand in for an exec failure, so the reader can resume
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("replacement callback did not run")
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("reader did not resume after the replacement attempt returned")
	}
}

// A notification must not hold the guard, in either direction.
//
// It carries no ID and receives no response, so nothing could ever retire it:
// tracking one would block every future upgrade for the life of the session, which
// is a worse failure than the one the guard fixes. Losing a notification hangs
// nobody, and the handler count still covers it once dispatched.
func TestANotificationNeverHoldsTheGuardOpen(t *testing.T) {
	ctx := context.Background()
	updater := &selfUpdater{changed: func() bool { return true }}
	conn := &writeTrackingConn{Connection: &scriptedConn{
		incoming: []jsonrpc.Message{&jsonrpc.Request{Method: "notifications/initialized"}},
	}, updater: updater}

	if _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if len(updater.outstanding) != 0 {
		t.Fatalf("a notification left %d entries outstanding; nothing will ever retire them "+
			"and every future upgrade is blocked", len(updater.outstanding))
	}
}

// A failed write must retire its call anyway. The response will not be delivered,
// and holding the ID forever would block every future upgrade over a connection
// that is already broken.
func TestAFailedWriteStillRetiresItsCall(t *testing.T) {
	ctx := context.Background()
	updater := &selfUpdater{changed: func() bool { return true }}
	scripted := &scriptedConn{writeErr: io.ErrClosedPipe}
	conn := &writeTrackingConn{Connection: scripted, updater: updater}

	id, err := jsonrpc.MakeID(float64(3))
	if err != nil {
		t.Fatal(err)
	}
	scripted.incoming = []jsonrpc.Message{&jsonrpc.Request{ID: id, Method: "tools/call"}}
	if _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, &jsonrpc.Response{ID: id}); err == nil {
		t.Fatal("expected the scripted write error")
	}
	if len(updater.outstanding) != 0 {
		t.Errorf("a failed write left %d entries outstanding, blocking every future upgrade",
			len(updater.outstanding))
	}
}

// scriptedConn is a Connection whose reads come from a slice.
type scriptedConn struct {
	incoming []jsonrpc.Message
	next     int
	writeErr error
}

func (c *scriptedConn) Read(context.Context) (jsonrpc.Message, error) {
	if c.next >= len(c.incoming) {
		return nil, io.EOF
	}
	msg := c.incoming[c.next]
	c.next++
	return msg, nil
}

func (c *scriptedConn) Write(context.Context, jsonrpc.Message) error { return c.writeErr }
func (c *scriptedConn) Close() error                                 { return nil }
func (c *scriptedConn) SessionID() string                            { return "" }
