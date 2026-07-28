package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/puremetricsai/lumi/internal/store"
)

// TestServeWritesOnlyJSONRPCFramesToStdout is the regression test for the
// invariant that a stdio server lives or dies by: os.Stdout carries protocol
// frames and nothing else. A single stray line of human-readable output
// desynchronizes the stream, and the client's only symptom is that Lumi
// vanished. It swaps the real stdin/stdout for pipes, so it also catches
// anything the SDK itself might print.
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

	go func() {
		for _, frame := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-agent","version":"0"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_events","arguments":{"query":"roadmap"}}}`,
		} {
			fmt.Fprintln(stdinWriter, frame)
		}
		// Closing stdin ends the session, which returns Serve.
		stdinWriter.Close()
	}()

	if err := Serve(ctx, s, Options{Name: "lumi", Version: "test"}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	stdoutWriter.Close()
	os.Stdout = originalOut

	captured, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(captured))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var frames int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var frame struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatalf("stdout carried a non-JSON line %q: %v", line, err)
		}
		if frame.JSONRPC != "2.0" {
			t.Fatalf("stdout carried a non-JSON-RPC object: %s", line)
		}
		frames++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if frames < 2 {
		t.Fatalf("expected at least the initialize and tools/call responses, got %d frames", frames)
	}
}
