package cerebras

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/puremetricsai/lumi/internal/config"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAnswerDelegates verifies the Cerebras wrapper targets the Cerebras
// endpoint with a Bearer token. The shared request/prompt shape is covered by
// internal/llm.
func TestAnswerDelegates(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != config.CerebrasEndpoint {
			t.Fatalf("unexpected endpoint %q", req.URL.String())
		}
		if req.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("missing authorization header")
		}
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "local context") {
			t.Fatalf("request omitted context: %s", body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
			`{"choices":[{"message":{"role":"assistant","content":"Supported answer"}}]}`)), Header: make(http.Header)}, nil
	})}
	answer, err := (Client{APIKey: "secret", HTTPClient: httpClient}).Answer(context.Background(), "What?", "local context")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Supported answer" {
		t.Fatalf("unexpected answer %q", answer)
	}
}

func TestAnswerRequiresAPIKey(t *testing.T) {
	if _, err := (Client{}).Answer(context.Background(), "question", "context"); err == nil {
		t.Fatal("expected missing key error")
	}
}
