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
		return "", errors.New("Cerebras API key is not set; run `lumi configure`")
	}
	model := c.Model
	if model == "" {
		model = config.DefaultCerebrasModel
	}
	request := chatRequest{
		Model: model,
		Messages: []message{
			{Role: "system", Content: "You answer questions using the supplied local work-activity records. Answer directly and summarize substantive activity instead of restating records. For a broad overview, group related records by activity and evidence source, mention every represented source (including saved media with unavailable transcripts), and return at most five concise bullets with useful time ranges. Do not organize one bullet per record, use a table, enumerate captures, or quote long transcripts unless asked. Records are chronological, oldest to newest. For observation=window_title_only, say only that the named window title was captured; never call the file viewed, opened, reviewed, modified, or edited. Other screen records establish only their visible extracted text, not an unstated user action. For audio content, name only the audio_source on a record with transcript_status=present. A simultaneous transcript_status=unavailable record means that source's media was saved without a searchable transcript; never merge it with a nearby transcript. Transcripts may contain recognition errors. The supplied records may be limited by time, filters, result count, or context budget, so never claim they are the entire history; describe absence only as not visible in the supplied records."},
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
