package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse() *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
		`{"choices":[{"message":{"role":"assistant","content":"Supported answer"}}]}`)), Header: make(http.Header)}
}

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
		return okResponse(), nil
	})}
	answer, err := (Client{APIKey: "secret", HTTPClient: httpClient}).Answer(context.Background(), "What?", "local context")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Supported answer" {
		t.Fatalf("unexpected answer %q", answer)
	}
}

// TestAnswerNoAPIKey verifies that an empty APIKey sends no Authorization
// header, as a local llama-server expects.
func TestAnswerNoAPIKey(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "" {
			t.Fatalf("unexpected authorization header %q", got)
		}
		return okResponse(), nil
	})}
	answer, err := (Client{HTTPClient: httpClient}).Answer(context.Background(), "What?", "local context")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Supported answer" {
		t.Fatalf("unexpected answer %q", answer)
	}
}

// TestAnswerDisableThinkingRequestsNoReasoning verifies that DisableThinking
// asks the chat template to skip the reasoning pass. Reasoning tokens are drawn
// from the same completion budget as the answer, so a reasoning model that is
// left to think can exhaust the budget mid-scratchpad and return no answer at
// all.
func TestAnswerDisableThinkingRequestsNoReasoning(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		var decoded struct {
			ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if enabled, ok := decoded.ChatTemplateKwargs["enable_thinking"]; !ok || enabled != false {
			t.Fatalf("expected enable_thinking=false, got %v: %s", decoded.ChatTemplateKwargs, body)
		}
		return okResponse(), nil
	})}
	if _, err := (Client{DisableThinking: true, HTTPClient: httpClient}).Answer(context.Background(), "What?", "local context"); err != nil {
		t.Fatal(err)
	}
}

// TestAnswerOmitsChatTemplateKwargsByDefault keeps the llama.cpp-specific knob
// off the wire for hosted backends that would reject an unknown field.
func TestAnswerOmitsChatTemplateKwargsByDefault(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		if strings.Contains(string(body), "chat_template_kwargs") {
			t.Fatalf("default request must not send chat_template_kwargs: %s", body)
		}
		return okResponse(), nil
	})}
	if _, err := (Client{HTTPClient: httpClient}).Answer(context.Background(), "What?", "local context"); err != nil {
		t.Fatal(err)
	}
}

// TestAnswerReasoningOnlyResponseReportsCause covers a reasoning model that
// spends its whole completion budget thinking and returns an empty content
// field. The error must name that cause instead of the generic "no answer",
// which gives the user nothing to act on.
func TestAnswerReasoningOnlyResponseReportsCause(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
			`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","reasoning_content":"Let me think about this..."}}]}`)), Header: make(http.Header)}, nil
	})}
	_, err := (Client{HTTPClient: httpClient}).Answer(context.Background(), "What?", "local context")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"reasoning", "length"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q omits %q", err, want)
		}
	}
}

// TestAnswerReasoningWithoutTruncationOmitsBudgetClaim covers reasoning with a
// non-truncated finish reason: the model stopped on its own without ever
// opening an answer channel. Only finish_reason="length" evidences budget
// exhaustion, so this case must report the missing answer without blaming the
// token budget.
func TestAnswerReasoningWithoutTruncationOmitsBudgetClaim(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
			`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"","reasoning_content":"Thought it through."}}]}`)), Header: make(http.Header)}, nil
	})}
	_, err := (Client{HTTPClient: httpClient}).Answer(context.Background(), "What?", "local context")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The reasoning signal is the actionable part and must survive.
	if !strings.Contains(err.Error(), "reasoning") {
		t.Fatalf("error %q drops the reasoning signal", err)
	}
	for _, unwanted := range []string{"budget", "1200"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Fatalf("error %q claims budget exhaustion without a truncated finish reason", err)
		}
	}
}
