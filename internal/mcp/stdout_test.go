package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// TestServeWritesOnlyJSONRPCFramesToStdout is the regression test for the
// invariant that a stdio server lives or dies by: os.Stdout carries protocol
// frames and nothing else. A single stray line of human-readable output
// desynchronizes the stream, and the client's only symptom is that Lumi
// vanished. It swaps the real stdin/stdout for pipes, so it also catches
// anything the SDK itself might print.
//
// It models a real client's shutdown sequence: hold stdin open until every
// reply it is waiting on has actually been read from stdout, and only then
// close stdin to end the session. A client that fires a request and closes
// its input before reading the reply is not something any real MCP client
// does, and testing that shape here would just be testing an artificial race
// against the SDK's own shutdown handling — not Lumi's stdout invariant.
//
// It must not run in parallel with any other test: it replaces process-wide
// state.
func TestServeWritesOnlyJSONRPCFramesToStdout(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	insertEvents(t, ctx, s,
		store.Event{Kind: store.KindScreen, Text: "quarterly roadmap review", App: "Safari", MediaPath: "/tmp/a.jpg"},
	)

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalIn, originalOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinReader, stdoutWriter
	t.Cleanup(func() { os.Stdin, os.Stdout = originalIn, originalOut })

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(ctx, s, Options{Name: "lumi", Version: "test"})
	}()

	for _, request := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-agent","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_events","arguments":{"query":"roadmap"}}}`,
	} {
		fmt.Fprintln(stdinWriter, request)
	}

	// Read stdout concurrently with Serve, validating every line as it
	// arrives, so this goroutine can tell us the moment the tools/call
	// response (id 2) has actually been read — the earliest point a real
	// client could legitimately end the session.
	type frame struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
	}
	frames := make(chan frame)
	scanErr := make(chan error, 1)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(stdoutReader)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var f frame
			if err := json.Unmarshal(line, &f); err != nil {
				scanErr <- fmt.Errorf("stdout carried a non-JSON line %q: %w", line, err)
				return
			}
			if f.JSONRPC != "2.0" {
				scanErr <- fmt.Errorf("stdout carried a non-JSON-RPC object: %s", line)
				return
			}
			frames <- f
		}
		if err := scanner.Err(); err != nil {
			scanErr <- err
		}
	}()

	const readTimeout = 5 * time.Second
	timeout := time.After(readTimeout)
	var seen int
	sawToolCallResponse := false
	for !sawToolCallResponse {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stdout closed before the tools/call response arrived")
			}
			seen++
			if string(f.ID) == "2" {
				sawToolCallResponse = true
			}
		case err := <-scanErr:
			t.Fatal(err)
		case <-timeout:
			t.Fatal("timed out waiting for the tools/call response on stdout")
		}
	}

	// The client has its answer, so — exactly like a real MCP client — end
	// the session by closing its write end now, not before.
	stdinWriter.Close()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(readTimeout):
		t.Fatal("Serve did not return after stdin closed")
	}
	stdoutWriter.Close()
	os.Stdout = originalOut

	// Drain and validate anything written after the tools/call response
	// (there should be none, but the scanner goroutine is still checking).
	for range frames {
		seen++
	}
	select {
	case err := <-scanErr:
		if err != nil {
			t.Fatal(err)
		}
	default:
	}

	if seen < 2 {
		t.Fatalf("expected at least the initialize and tools/call responses, got %d frames", seen)
	}
}
