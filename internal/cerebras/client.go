package cerebras

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
)

type Client struct {
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

type chatRequest struct {
	Model               string    `json:"model"`
	Messages            []message `json:"messages"`
	MaxCompletionTokens int       `json:"max_completion_tokens"`
	Temperature         float64   `json:"temperature"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

func (c Client) Answer(ctx context.Context, question, activityContext string) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", errors.New("CEREBRAS_API_KEY is not set")
	}
	model := c.Model
	if model == "" {
		model = config.DefaultCerebrasModel
	}
	request := chatRequest{
		Model: model,
		Messages: []message{
			{Role: "system", Content: "You answer questions using the supplied local work-activity records. Be concise, distinguish Accessibility or Vision screen text from system or microphone audio transcripts, cite event timestamps, and say when the records do not support a claim."},
			{Role: "user", Content: "Work-activity records:\n\n" + activityContext + "\n\nQuestion: " + question},
		},
		MaxCompletionTokens: 1200,
		Temperature:         0.2,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode Cerebras request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.CerebrasEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create Cerebras request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "lumi/0.1")
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call Cerebras: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("read Cerebras response: %w", err)
	}
	var decoded chatResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return "", fmt.Errorf("decode Cerebras response (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(responseBody))
		if decoded.Error != nil && decoded.Error.Message != "" {
			message = decoded.Error.Message
		}
		return "", fmt.Errorf("Cerebras returned HTTP %d: %s", resp.StatusCode, message)
	}
	if len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return "", errors.New("Cerebras response contained no answer")
	}
	return strings.TrimSpace(decoded.Choices[0].Message.Content), nil
}
