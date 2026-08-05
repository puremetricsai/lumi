package mcp

import (
	"context"
	"encoding/json"
	"io"
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
