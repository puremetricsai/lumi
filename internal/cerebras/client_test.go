package cerebras

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnswer(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("missing authorization header")
		}
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "local context") {
			t.Fatalf("request omitted context: %s", body)
		}
		for _, instruction := range []string{
			"at most five concise bullets",
			"mention every represented source",
			"Records are chronological",
			"observation=window_title_only",
			"never call the file viewed",
			"transcript_status=present",
			"never merge it with a nearby transcript",
			"not visible in the supplied records",
			"second person",
			"name only the focused window",
			"never attribute that content to the focused app",
			"never open with phrases like",
			"local timezone",
			"never in UTC",
		} {
			if !strings.Contains(string(body), instruction) {
				t.Fatalf("request omitted %q guidance: %s", instruction, body)
			}
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
