package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stashed state has to be readable by the process that inherits it, and it is
// the only thing standing between a replacement and rejecting every request the
// client sends it.
func TestSessionStateRoundTripsThroughTheEnvironment(t *testing.T) {
	t.Setenv(sessionStateEnv, "")
	state := &sdk.ServerSessionState{
		InitializeParams: &sdk.InitializeParams{
			ProtocolVersion: "2025-11-25",
			ClientInfo:      &sdk.Implementation{Name: "test-agent", Version: "1"},
		},
		InitializedParams: &sdk.InitializedParams{},
	}
	if err := stashSessionState(state); err != nil {
		t.Fatal(err)
	}

	restored := restoreSessionState(quietLogger())
	if restored == nil || restored.InitializeParams == nil {
		t.Fatal("expected the handshake to survive")
	}
	if got := restored.InitializeParams.ClientInfo.Name; got != "test-agent" {
		t.Errorf("client identity did not survive: got %q", got)
	}
	// The SDK gates every method on having seen notifications/initialized, so a
	// restored session missing this rejects the client's next request.
	if restored.InitializedParams == nil {
		t.Error("restored state must record that initialization completed")
	}
}

// The variable must not outlive the process that consumed it: a stale handshake
// inherited by some later child would be applied to a session it never belonged
// to.
func TestRestoringSessionStateClearsTheEnvironment(t *testing.T) {
	t.Setenv(sessionStateEnv, `{"initializeParams":{"protocolVersion":"2025-11-25"}}`)
	restoreSessionState(quietLogger())
	if _, ok := os.LookupEnv(sessionStateEnv); ok {
		t.Errorf("%s should be unset after being read", sessionStateEnv)
	}
}

// A fresh server — the ordinary launch — has no stashed state and must connect
// as a new session rather than treating the absence as an error.
func TestNoSessionStateMeansAFreshSession(t *testing.T) {
	t.Setenv(sessionStateEnv, "")
	if state := restoreSessionState(quietLogger()); state != nil {
		t.Errorf("expected no state for a fresh launch, got %+v", state)
	}
}

// Unreadable state can only mean the SDK's own state type changed across the
// upgrade. Discarding it leaves a visible failure the user can fix by restarting;
// failing to start would produce the same outcome with less information.
func TestUnreadableSessionStateIsDiscarded(t *testing.T) {
	t.Setenv(sessionStateEnv, "{not json")
	if state := restoreSessionState(quietLogger()); state != nil {
		t.Errorf("expected unreadable state to be discarded, got %+v", state)
	}
}

// A handshake that never completed must not be stashed: handing a replacement a
// nil InitializeParams would restore a session the SDK still considers
// uninitialized, with no way to tell that from a fresh launch.
func TestIncompleteHandshakeIsNotStashed(t *testing.T) {
	t.Setenv(sessionStateEnv, "")
	if err := stashSessionState(nil); err == nil {
		t.Error("expected stashing nil state to fail")
	}
	if err := stashSessionState(&sdk.ServerSessionState{}); err == nil {
		t.Error("expected stashing a state with no handshake to fail")
	}
	if value := os.Getenv(sessionStateEnv); value != "" {
		t.Errorf("nothing should have been stashed, got %q", value)
	}
}

// This is the property the whole mechanism rests on. Nothing in the SDK reports
// "the response for request N is on the wire" — receiving middleware returns
// before the result is serialized — so a replacement that fired while a request
// was in flight would exec away with the reply and leave the client waiting
// forever for a response nobody will send. Idleness is the proxy, and it must
// exclude a request that is still running.
func TestUpdaterWillNotReplaceWhileARequestIsInFlight(t *testing.T) {
	u := &selfUpdater{changed: func() bool { return true }, exec: nil}
	u.begin()
	if u.idleFor(0) {
		t.Error("a session with a request in flight must never be considered idle")
	}
	u.end()
	if !u.idleFor(0) {
		t.Error("a session with nothing in flight should be idle")
	}
}

// A session that has only just gone quiet is still mid-conversation as far as
// the client is concerned; the quiet period is what keeps the replacement out of
// a pipelined exchange.
func TestUpdaterWaitsForTheQuietPeriod(t *testing.T) {
	u := &selfUpdater{changed: func() bool { return true }}
	u.begin()
	u.end()
	if u.idleFor(time.Hour) {
		t.Error("a just-finished request must not satisfy a one-hour quiet period")
	}
}

// A server that has never handled a request has no activity to measure. It must
// not be treated as idle-forever and replaced before the client's handshake has
// even been stashed.
func TestUpdaterIsNotIdleBeforeAnyActivity(t *testing.T) {
	u := &selfUpdater{changed: func() bool { return true }}
	if u.idleFor(0) {
		t.Error("a server with no activity yet must not be considered idle")
	}
}

// The middleware is what tells the updater a request is running, so it has to
// wrap the real call and still return its result untouched.
func TestMiddlewareCountsRequestsAndPreservesTheResult(t *testing.T) {
	u := &selfUpdater{changed: func() bool { return false }}
	var duringCall bool
	handler := u.middleware()(func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		duringCall = !u.idleFor(0)
		return &sdk.CallToolResult{}, nil
	})

	result, err := handler(context.Background(), "tools/call", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Error("middleware dropped the handler's result")
	}
	if !duringCall {
		t.Error("the handler did not observe itself as in flight")
	}
	if !u.idleFor(0) {
		t.Error("the request should have been released after the handler returned")
	}
}

// An exec that fails leaves this image running and serving, so the stashed
// handshake has to be dropped again — otherwise it would be inherited by some
// unrelated child this process later spawns and applied to a foreign session.
func TestFailedExecClearsTheStashedState(t *testing.T) {
	t.Setenv(sessionStateEnv, "")
	var once sync.Once
	stop := make(chan struct{})
	u := &selfUpdater{
		changed: func() bool { return true },
		exec: func() error {
			once.Do(func() { close(stop) })
			return os.ErrPermission
		},
		checkInterval: time.Millisecond,
		quietPeriod:   time.Millisecond,
	}
	u.begin()
	u.end()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go u.watch(ctx, func() *sdk.ServerSessionState {
		return &sdk.ServerSessionState{
			InitializeParams:  &sdk.InitializeParams{ProtocolVersion: "2025-11-25"},
			InitializedParams: &sdk.InitializedParams{},
		}
	}, quietLogger())

	select {
	case <-stop:
	case <-ctx.Done():
		t.Fatal("watch never attempted the replacement")
	}
	// The unset races the goroutine's return, so allow it to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := os.LookupEnv(sessionStateEnv); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%s should have been cleared after a failed exec", sessionStateEnv)
}

// An unchanged binary must never trigger a replacement, however long the session
// sits idle. Restarting the process without a new build to run would drop
// in-flight work for nothing.
func TestUpdaterDoesNotReplaceAnUnchangedBinary(t *testing.T) {
	execCalled := make(chan struct{}, 1)
	u := &selfUpdater{
		changed:       func() bool { return false },
		exec:          func() error { execCalled <- struct{}{}; return nil },
		checkInterval: time.Millisecond,
		quietPeriod:   time.Millisecond,
	}
	u.begin()
	u.end()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go u.watch(ctx, func() *sdk.ServerSessionState { return nil }, quietLogger())

	select {
	case <-execCalled:
		t.Error("an unchanged binary must not be replaced")
	case <-ctx.Done():
	}
}

// A real session must survive the restore path end to end: this is the case that
// fails as "method is invalid during session initialization" if the state is not
// carried across, and it fails on the client's *next* request rather than at
// startup.
func TestARestoredSessionServesToolsWithoutAHandshake(t *testing.T) {
	ctx := context.Background()
	encoded, err := json.Marshal(&sdk.ServerSessionState{
		InitializeParams: &sdk.InitializeParams{
			ProtocolVersion: "2025-11-25",
			ClientInfo:      &sdk.Implementation{Name: "test-agent", Version: "1"},
		},
		InitializedParams: &sdk.InitializedParams{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionStateEnv, string(encoded))

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	serverSession, err := newServer(testStore(t), Options{Name: "lumi", Version: "test"}).
		Connect(ctx, serverTransport, &sdk.ServerSessionOptions{State: restoreSessionState(quietLogger())})
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	// Raw JSON-RPC, deliberately not sdk.Client: a real client mid-session has
	// already handshaked and will never send initialize again, which is exactly
	// the condition being tested. sdk.Client would re-handshake and hide it.
	stream, err := clientTransport.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	// MakeID takes the default JSON unmarshaling of an ID, so float64 not int.
	id, err := jsonrpc.MakeID(float64(1))
	if err != nil {
		t.Fatal(err)
	}
	call := &jsonrpc.Request{ID: id, Method: "tools/list", Params: json.RawMessage(`{}`)}
	if err := stream.Write(ctx, call); err != nil {
		t.Fatal(err)
	}
	message, err := stream.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	response, ok := message.(*jsonrpc.Response)
	if !ok {
		t.Fatalf("expected a response, got %T", message)
	}
	if response.Error != nil {
		t.Fatalf("a restored session rejected tools/list: %v", response.Error)
	}
	if !bytes.Contains(response.Result, []byte(`"tools"`)) {
		t.Errorf("expected a tool list, got %s", response.Result)
	}
}

// The idleness check and the replacement must be one atomic decision.
//
// A check that releases the lock before exec'ing re-opens the race from the other
// side: the watcher decides the session is idle, a request arrives in the interval
// before execve, and the replacement carries it away. state() is called inside
// that exact window, which makes it a precise stand-in for "a request landed here"
// without having to win a real race.
func TestReplacementIsAtomicWithTheIdlenessCheck(t *testing.T) {
	t.Setenv(sessionStateEnv, "")
	execed := make(chan struct{})
	var once sync.Once
	u := &selfUpdater{
		changed:       func() bool { return true },
		checkInterval: time.Millisecond,
		quietPeriod:   time.Millisecond,
	}
	u.exec = func() error {
		once.Do(func() { close(execed) })
		return nil
	}
	u.begin()
	u.end()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Records whether a request could be admitted after the idleness decision but
	// before the process was replaced. With the lock held across both, this
	// goroutine cannot proceed until exec has returned.
	// Guarded by its own Once: a failed stash leaves watch ticking, so state() can
	// be called more than once and an unguarded close would panic.
	admitted := make(chan struct{})
	var probe sync.Once
	go u.watch(ctx, func() *sdk.ServerSessionState {
		probe.Do(func() {
			go func() {
				u.begin()
				close(admitted)
			}()
		})
		// Give that goroutine every chance to take the lock if it can.
		time.Sleep(50 * time.Millisecond)
		select {
		case <-admitted:
			t.Error("a request was admitted between the idleness check and the replacement: " +
				"exec'ing here discards it and hangs the client on a reply that will never come")
		default:
		}
		return &sdk.ServerSessionState{
			InitializeParams:  &sdk.InitializeParams{ProtocolVersion: "2025-11-25"},
			InitializedParams: &sdk.InitializedParams{},
		}
	}, quietLogger())

	select {
	case <-execed:
	case <-ctx.Done():
		t.Fatal("watch never attempted the replacement")
	}
}
