// Package llamacpp is the local llama.cpp inference backend for `lumi ask`. It
// talks to a llama-server over its OpenAI-compatible HTTP API and can launch
// llama-server on demand, leaving it running so the model stays warm.
package llamacpp

import (
	"context"
	"net/http"
	"strings"

	"github.com/puremetricsai/lumi/internal/llm"
)

// Client answers questions against a local llama-server. No API key is required.
type Client struct {
	BaseURL    string
	Model      string
	HTTPClient *http.Client
}

// Answer sends the question and activity context to llama-server's
// OpenAI-compatible chat-completions endpoint.
func (c Client) Answer(ctx context.Context, question, activityContext string) (string, error) {
	return llm.Client{
		Endpoint: strings.TrimRight(c.BaseURL, "/") + "/v1/chat/completions",
		Model:    c.Model,
		// llama-server bills reasoning tokens against the same completion budget
		// as the answer, so a thinking model on a long activity context can
		// exhaust the budget mid-scratchpad and return no answer at all.
		DisableThinking: true,
		HTTPClient:      c.HTTPClient,
	}.Answer(ctx, question, activityContext)
}
