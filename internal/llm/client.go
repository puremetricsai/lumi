// Package llm holds the shared OpenAI-compatible chat-completions client used by
// every inference backend (Cerebras, llama.cpp). The system prompt and request
// parameters live here so they stay single-sourced across backends.
package llm

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
)

// SystemPrompt is the answer-shaping instruction sent with every question. It is
// a project invariant shared by all backends; changing it changes every
// provider's behavior.
const SystemPrompt = "You answer questions using the supplied local work-activity records. Write in natural second person, addressing the user directly (for example, \"Earlier you were watching a video about…\"); do not narrate the record-keeping itself, and never open with phrases like \"Based on the screen records\" or name the focused application as the source of what was on screen. Answer directly and summarize substantive activity instead of restating records. For a broad overview, group related records by activity and evidence source, mention every represented source (including saved media with unavailable transcripts), and return at most five concise bullets with useful time ranges. Do not organize one bullet per record, use a table, enumerate captures, or quote long transcripts unless asked. Records are chronological, oldest to newest. Each record's timestamp is in the user's local timezone (the numeric offset is shown); interpret times the user mentions as local wall-clock time and report times in that same local timezone, never in UTC. Each screen record's app and window name only the focused window at capture time; with text_source=vision the extracted text is OCR of the whole display and routinely includes content from other applications (a browser, a video, a document), so identify what the user was doing from the visible text itself and never attribute that content to the focused app. For observation=window_title_only, say only that the named window title was captured; never call the file viewed, opened, reviewed, modified, or edited. Other screen records establish only their visible extracted text, not an unstated user action. For audio content, name only the audio_source on a record with transcript_status=present. A simultaneous transcript_status=unavailable record means that source's media was saved without a searchable transcript; never merge it with a nearby transcript. Transcripts may contain recognition errors. The supplied records may be limited by time, filters, result count, or context budget, so never claim they are the entire history; describe absence only as not visible in the supplied records."

// maxCompletionTokens bounds the reply. Reasoning models bill their scratchpad
// against this same budget, which is why DisableThinking exists.
const maxCompletionTokens = 1200

// finishReasonTruncated is the OpenAI-protocol finish reason for a reply cut off
// at the token limit. It is the only evidence that the budget was exhausted.
const finishReasonTruncated = "length"

// Client speaks the OpenAI chat-completions protocol against Endpoint. APIKey is
// optional: when empty (as with a local llama-server) no Authorization header is
// sent. A nil HTTPClient uses a 90s-timeout default.
type Client struct {
	Endpoint string
	APIKey   string
	Model    string
	// DisableThinking asks the server's chat template to skip the reasoning
	// pass. A reasoning model spends maxCompletionTokens on its scratchpad
	// before it writes any content, so on a long activity context it can hit the
	// budget mid-thought and return an empty answer. SystemPrompt already
	// prescribes the answer's shape, so the reasoning pass buys nothing here.
	// Only backends known to honor the flag should set it.
	DisableThinking bool
	HTTPClient      *http.Client
}

type chatRequest struct {
	Model               string    `json:"model"`
	Messages            []message `json:"messages"`
	MaxCompletionTokens int       `json:"max_completion_tokens"`
	Temperature         float64   `json:"temperature"`
	// ChatTemplateKwargs is a llama.cpp extension; it stays off the wire for
	// hosted backends that would reject an unknown field.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Reasoning carries a reasoning model's scratchpad on responses. It is never
	// an answer: when the reply was truncated it holds partial thinking, so it
	// is used only to explain a missing answer, never returned as one.
	Reasoning string `json:"reasoning_content,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		FinishReason string  `json:"finish_reason"`
		Message      message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// Answer sends the question and activity context and returns the model's reply.
func (c Client) Answer(ctx context.Context, question, activityContext string) (string, error) {
	request := chatRequest{
		Model: c.Model,
		Messages: []message{
			{Role: "system", Content: SystemPrompt},
			{Role: "user", Content: "Work-activity records:\n\n" + activityContext + "\n\nQuestion: " + question},
		},
		MaxCompletionTokens: maxCompletionTokens,
		Temperature:         0.2,
	}
	if c.DisableThinking {
		request.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	if strings.TrimSpace(c.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "lumi/0.1")
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call inference endpoint: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("read inference response: %w", err)
	}
	var decoded chatResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return "", fmt.Errorf("decode inference response (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(responseBody))
		if decoded.Error != nil && decoded.Error.Message != "" {
			msg = decoded.Error.Message
		}
		return "", fmt.Errorf("inference endpoint returned HTTP %d: %s", resp.StatusCode, msg)
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("inference response contained no answer")
	}
	choice := decoded.Choices[0]
	if answer := strings.TrimSpace(choice.Message.Content); answer != "" {
		return answer, nil
	}
	// A reasoning model that never reached an answer is the common cause here,
	// and the generic error gave the user nothing to act on. Name the cause --
	// but only the cause the response actually evidences. Budget exhaustion is
	// established by a truncated finish reason, not by the mere presence of
	// reasoning: a model can also stop on its own without ever opening an
	// answer channel, which is a template or model fault the budget won't fix.
	if strings.TrimSpace(choice.Message.Reasoning) != "" {
		if choice.FinishReason == finishReasonTruncated {
			return "", fmt.Errorf(
				"inference response contained only the model's reasoning and no answer (finish_reason=%q): the model spent its whole %d-token budget thinking; use a non-reasoning model for `lumi ask`",
				choice.FinishReason, maxCompletionTokens)
		}
		return "", fmt.Errorf(
			"inference response contained only the model's reasoning and no answer (finish_reason=%q): the model stopped without emitting a reply; its chat template may not separate reasoning from the answer",
			choice.FinishReason)
	}
	return "", fmt.Errorf("inference response contained no answer (finish_reason=%q)", choice.FinishReason)
}
