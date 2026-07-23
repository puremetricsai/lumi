package cerebras

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/llm"
)

// Client answers questions with Cerebras's hosted inference. It is a thin
// wrapper over the shared OpenAI-compatible llm.Client, pinned to the Cerebras
// endpoint and requiring an API key.
type Client struct {
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// Answer sends the question and activity context to Cerebras. Unlike a local
// backend, Cerebras requires an API key.
func (c Client) Answer(ctx context.Context, question, activityContext string) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", errors.New("Cerebras API key is not set; run `lumi configure`")
	}
	model := c.Model
	if model == "" {
		model = config.DefaultCerebrasModel
	}
	return llm.Client{
		Endpoint:   config.CerebrasEndpoint,
		APIKey:     c.APIKey,
		Model:      model,
		HTTPClient: c.HTTPClient,
	}.Answer(ctx, question, activityContext)
}
