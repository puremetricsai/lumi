package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/puremetricsai/lumi/internal/store"
)

// defaultTranscriptTextChars caps a turn's text unless the caller says otherwise.
// It is generous compared with search_events' per-event cap because a transcript
// is read for continuity: a turn cut mid-sentence costs more than a search hit
// cut mid-paragraph.
const defaultTranscriptTextChars = 600

// defaultTranscriptWindow is how far back an unbounded request reaches. A
// transcript with no window is almost always "what was just said", and defaulting
// to the whole index would return a thousand unrelated turns.
const defaultTranscriptWindow = time.Hour

type getTranscriptInput struct {
	Since string `json:"since,omitempty" jsonschema:"earliest capture time: an RFC3339 timestamp, or a duration such as 2h meaning that long ago; defaults to 1h ago"`
	Until string `json:"until,omitempty" jsonschema:"latest capture time, in the same forms as since; defaults to now"`
	// Origin is a plain string rather than an enum so an unrecognized value can
	// be answered with a sentence explaining the vocabulary, which is more use to
	// an agent mid-task than a schema rejection.
	Origin        string   `json:"origin,omitempty" jsonschema:"restrict to one origin: \"internal\" for sound this machine produced, \"external\" for sound its microphone picked up from the room, or \"unknown\"; omit for all"`
	MinConfidence *float64 `json:"min_confidence,omitempty" jsonschema:"drop turns whose attribution confidence is below this, from 0 to 1; defaults to 0 so every turn is returned and its confidence speaks for itself"`
	MaxTurns      int      `json:"max_turns,omitempty" jsonschema:"maximum turns to return; defaults to 100 and is capped at 1000"`
	MaxTextChars  *int     `json:"max_text_chars,omitempty" jsonschema:"per-turn character cap on text; defaults to 600, and 0 means no cap"`
}

// TranscriptTurnRecord is one turn of an assembled transcript.
//
// Confidence and OrderConfidence carry no omitempty, for the same reason
// EventRecord's Truncated does: a turn's trustworthiness must be present on every
// row. If a doubtful label arrived as a missing key, an agent would have to infer
// the doubt from an absence, which is exactly the inference it cannot make.
type TranscriptTurnRecord struct {
	Origin     string `json:"origin"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	TextLength int    `json:"text_length"`
	// StartedAt and EndedAt are absent for turns recovered from stored text
	// alone, which preserves order but not absolute time.
	StartedAt       string  `json:"started_at,omitempty"`
	EndedAt         string  `json:"ended_at,omitempty"`
	Confidence      float64 `json:"confidence"`
	OrderConfidence string  `json:"order_confidence"`
	Overlaps        bool    `json:"overlaps,omitempty"`
	EventIDs        []int64 `json:"event_ids"`
}

type getTranscriptOutput struct {
	Turns  []TranscriptTurnRecord `json:"turns"`
	Notice string                 `json:"notice,omitempty"`
}

// parseOrigin maps the tool's origin parameter onto a stored origin value.
//
// The error spells out the vocabulary rather than listing valid strings, because
// the names are only obvious once you know that Lumi records two audio tracks and
// that "internal" is about which machine produced the sound rather than who was
// speaking.
func parseOrigin(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "":
		return "", nil
	case store.OriginInternal:
		return store.OriginInternal, nil
	case store.OriginExternal:
		return store.OriginExternal, nil
	case store.OriginUnknown:
		return store.OriginUnknown, nil
	}
	return "", fmt.Errorf(`origin must be "internal" (sound this machine produced, such as the far side `+
		`of a call, a video, or a notification), "external" (sound the microphone picked up from the room, `+
		`usually the user), or "unknown" (machine audio was playing but produced no transcript, so the `+
		`origin could not be determined); got %q`, value)
}

// getTranscript assembles one ordered, attributed transcript for a time range.
func (h *handlers) getTranscript(ctx context.Context, _ *sdk.CallToolRequest, in getTranscriptInput) (*sdk.CallToolResult, getTranscriptOutput, error) {
	var empty getTranscriptOutput

	since, err := parseTimestamp("since", in.Since)
	if err != nil {
		return nil, empty, err
	}
	until, err := parseTimestamp("until", in.Until)
	if err != nil {
		return nil, empty, err
	}
	origin, err := parseOrigin(in.Origin)
	if err != nil {
		return nil, empty, err
	}
	minConfidence := 0.0
	if in.MinConfidence != nil {
		if *in.MinConfidence < 0 || *in.MinConfidence > 1 {
			return nil, empty, fmt.Errorf("min_confidence must be between 0 and 1; got %v", *in.MinConfidence)
		}
		minConfidence = *in.MinConfidence
	}

	opts := store.TranscriptOptions{
		Since: time.Now().Add(-defaultTranscriptWindow), Until: time.Now(),
		Origin: origin, MinConfidence: minConfidence,
		// The store clamps this itself, so the number the schema documents is the
		// number enforced.
		MaxTurns: store.ClampTranscriptTurns(in.MaxTurns),
	}
	if since != nil {
		opts.Since = *since
	}
	if until != nil {
		opts.Until = *until
	}
	if opts.Until.Before(opts.Since) {
		return nil, empty, fmt.Errorf("until (%s) is before since (%s)",
			opts.Until.Format(time.RFC3339), opts.Since.Format(time.RFC3339))
	}

	result, err := h.store.Transcript(ctx, opts)
	if err != nil {
		return nil, empty, fmt.Errorf("assemble transcript: %w", err)
	}

	maxChars := defaultTranscriptTextChars
	if in.MaxTextChars != nil {
		maxChars = *in.MaxTextChars
	}
	out := getTranscriptOutput{Turns: make([]TranscriptTurnRecord, 0, len(result.Turns))}
	for _, turn := range result.Turns {
		text, truncated, length := truncateText(turn.Text, maxChars)
		record := TranscriptTurnRecord{
			Origin: turn.Origin, Text: text, Truncated: truncated, TextLength: length,
			Confidence: turn.Confidence, OrderConfidence: turn.OrderConfidence,
			Overlaps: turn.Overlaps, EventIDs: turn.EventIDs,
		}
		if turn.StartedAt != nil {
			record.StartedAt = turn.StartedAt.Local().Format(time.RFC3339Nano)
		}
		if turn.EndedAt != nil {
			record.EndedAt = turn.EndedAt.Local().Format(time.RFC3339Nano)
		}
		out.Turns = append(out.Turns, record)
	}

	notice, err := h.transcriptNotice(ctx, opts, result)
	if err != nil {
		return nil, empty, err
	}
	out.Notice = notice
	return nil, out, nil
}

// transcriptNotice explains what the turns above cannot: that the window holds
// audio nobody has attributed yet, that the page was capped, or that an empty
// result means an empty index rather than a quiet hour.
func (h *handlers) transcriptNotice(ctx context.Context, opts store.TranscriptOptions, result store.TranscriptResult) (string, error) {
	var parts []string

	if len(result.Turns) == 0 {
		// Why the transcript is empty decides what the agent should do next, and
		// the three reasons need three different actions: attribute the audio,
		// relax the filters, or look elsewhere. Reporting "not attributed" for a
		// range that is fully attributed sends it to run a backfill that cannot
		// change anything.
		filtered := "no attributed audio in this range"
		switch {
		case result.Chunks > 0 && result.AttributedChunks < result.Chunks:
			filtered = fmt.Sprintf("this range holds %d audio chunks and %d of them have not been "+
				"attributed yet; run `lumi transcript backfill` to attribute them",
				result.Chunks, result.Chunks-result.AttributedChunks)
		case result.Chunks > 0 && filtersNarrowed(opts):
			filtered = fmt.Sprintf("all %d audio chunks in this range are attributed, but no turn "+
				"matched the filters; drop origin or lower min_confidence to see what is there",
				result.Chunks)
		case result.Chunks > 0:
			filtered = fmt.Sprintf("all %d audio chunks in this range are attributed but hold no "+
				"speech — the machine played nothing and the microphone heard nobody", result.Chunks)
		}
		notice, err := h.noResultNotice(ctx, filtered)
		if err != nil {
			return "", err
		}
		return notice, nil
	}

	if result.Truncated {
		parts = append(parts, fmt.Sprintf(
			"this range holds more audio than one call returns, so the transcript stops at %s "+
				"rather than at until; request since=%s to continue from there",
			result.CoveredUntil.Local().Format(time.RFC3339),
			result.CoveredUntil.UTC().Format(time.RFC3339)))
	}
	if result.Capped {
		parts = append(parts, fmt.Sprintf(
			"results were capped at %d turns; narrow the range or raise max_turns", len(result.Turns)))
	}
	// A transcript with holes is worse than a short one, because nothing in the
	// turns themselves reveals the gap.
	if missing := result.Chunks - result.AttributedChunks; missing > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d of %d audio chunks in this range are not attributed yet, so this transcript has gaps; "+
				"run `lumi transcript backfill` to fill them", missing, result.Chunks))
	}
	return strings.Join(parts, "; "), nil
}

// filtersNarrowed reports whether the request could have excluded turns of its
// own accord, so an empty result can name the filter rather than blaming the
// index for it.
func filtersNarrowed(opts store.TranscriptOptions) bool {
	return opts.Origin != "" || opts.MinConfidence > 0
}
