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
	MinConfidence *float64 `json:"min_confidence,omitempty" jsonschema:"drop turns below this confidence, from 0 to 1; defaults to 0 so every turn is returned and its confidence speaks for itself. A turn's confidence combines the recognizer's own score with penalties for how uncertain its attribution is, and the largest of those penalties apply to microphone turns alone, so this threshold does not sort turns by quality alone: measured on a live index, internal turns scored 0.682-0.983 and external turns 0.331-0.592, so 0.6 removed every external turn and no internal one. Whatever it removes is reported back in confidence_filtered and in the notice. Prefer reading each turn's confidence over filtering on it"`
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
	Turns []TranscriptTurnRecord `json:"turns"`
	// ResumeFrom is what to pass as since to continue past a transcript that
	// stopped short, and is absent when there is nothing left to read. It is
	// given as a value rather than left to the notice's prose because an agent
	// paging through a long range should not have to parse a sentence to do it.
	//
	// Rendered by localStamp like every other timestamp here. It used to be the
	// one exception, in UTC, which read as a different instant beside a turn's
	// started_at to anything comparing the two as strings.
	ResumeFrom string `json:"resume_from,omitempty"`
	// ConfidenceFiltered counts the turns min_confidence removed, keyed by origin,
	// and is absent when it removed nothing. It is a value for the same reason
	// ResumeFrom is: the notice explains why the removal matters, but an agent
	// deciding whether it is holding a whole conversation should not have to parse
	// a sentence to find out.
	ConfidenceFiltered map[string]int `json:"confidence_filtered,omitempty"`
	Notice             string         `json:"notice,omitempty"`
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
	// The vocabulary belongs to the column, so the store names it; the error it
	// composes already spells the meanings out, which is what an agent mid-task
	// needs from an unrecognized value rather than a schema rejection.
	origin, err := store.ParseOrigin(in.Origin)
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
	out := getTranscriptOutput{
		Turns:              make([]TranscriptTurnRecord, 0, len(result.Turns)),
		ConfidenceFiltered: result.ConfidenceFiltered,
	}
	if !result.ResumeFrom.IsZero() {
		out.ResumeFrom = localStamp(result.ResumeFrom)
	}
	for _, turn := range result.Turns {
		text, truncated, length := truncateText(turn.Text, maxChars)
		record := TranscriptTurnRecord{
			Origin: turn.Origin, Text: text, Truncated: truncated, TextLength: length,
			Confidence: turn.Confidence, OrderConfidence: turn.OrderConfidence,
			Overlaps: turn.Overlaps, EventIDs: turn.EventIDs,
		}
		if turn.StartedAt != nil {
			record.StartedAt = localStamp(*turn.StartedAt)
		}
		if turn.EndedAt != nil {
			record.EndedAt = localStamp(*turn.EndedAt)
		}
		out.Turns = append(out.Turns, record)
	}

	notice, err := h.transcriptNotice(ctx, opts, result)
	if err != nil {
		return nil, empty, err
	}
	out.Notice = notice
	return summary(turnsDigest(out.Turns), out.Notice), out, nil
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
		missing := result.MissingChunks()
		// An exact count of what a filter deleted outranks every explanation below,
		// and has to be stated even when the range also has holes. Blaming an empty
		// transcript on unattributed audio when a threshold emptied it sends the
		// agent to run a backfill for turns that were found, transcribed, and then
		// filtered out — the one recommendation guaranteed not to help.
		if removed := result.ConfidenceRemovals(); removed != "" {
			filtered = fmt.Sprintf("min_confidence %.2f removed %s, leaving nothing to return; "+
				"lower or drop min_confidence to see them", opts.MinConfidence, removed)
			// The gap keeps its own guidance rather than being reduced to a count.
			// "Why is this empty" and "what can I do about the rest" are separate
			// questions, and answering the first must not consume the second.
			if gap := coverageGap(result, fmt.Sprintf(
				"%d of %d audio chunks in this range are also not attributed yet",
				missing, result.Chunks)); gap != "" {
				filtered += ". " + gap
			}
			notice, err := h.noResultNotice(ctx, filtered)
			if err != nil {
				return "", err
			}
			return notice, nil
		}
		switch {
		case result.Chunks > 0 && missing > 0 && result.RecoverableChunks() == 0:
			// Every hole here is one no backfill can fill, so naming the command
			// would be an instruction to watch nothing happen.
			filtered = fmt.Sprintf("this range holds %d audio chunks and none could be transcribed, "+
				"so there is nothing to attribute; their audio is still on disk",
				result.Chunks)
		case result.Chunks > 0 && missing > 0:
			filtered = fmt.Sprintf("this range holds %d audio chunks and %d of them have not been "+
				"attributed yet; run `lumi transcript backfill` to attribute them",
				result.Chunks, missing)
		case result.Chunks > 0 && opts.Origin != "":
			// Only origin is named. Reaching here means the removal report above
			// found nothing, so min_confidence provably took no turn — advising the
			// agent to lower it points at a control that is holding nothing back,
			// and competes for attention with the one that is.
			filtered = fmt.Sprintf("all %d audio chunks in this range are attributed, but no turn "+
				"has origin %q; drop origin to see what is there", result.Chunks, opts.Origin)
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

	// Both notices point at ResumeFrom rather than CoveredUntil. The two differ by
	// design: coverage ends inclusively at the last chunk the turns reach, and the
	// segment read is inclusive too, so resuming at that value would serve the
	// same chunk's turns again on every page.
	resume := ""
	if !result.ResumeFrom.IsZero() {
		resume = localStamp(result.ResumeFrom)
	}
	if result.Truncated {
		parts = append(parts, fmt.Sprintf(
			"this range holds more audio than one call returns, so the transcript stops at %s "+
				"rather than at until; request since=%s to continue from there",
			result.CoveredUntil.Local().Format(time.RFC3339), resume))
	}
	if result.Capped {
		parts = append(parts, fmt.Sprintf(
			"results were capped at %d turns; request since=%s to continue from there, "+
				"or raise max_turns", len(result.Turns), resume))
	}
	// The removal a non-empty transcript cannot reveal by itself. Every branch
	// above this point in the empty case explains why nothing came back; there was
	// nothing to explain a transcript that came back looking whole with one side of
	// the conversation deleted out of it.
	if removed := result.ConfidenceRemovals(); removed != "" {
		parts = append(parts, fmt.Sprintf(
			"min_confidence %.2f removed %s; a turn's confidence combines the recognizer's "+
				"score with penalties for how uncertain its attribution is, and the largest of "+
				"those apply to microphone turns alone — so a threshold sorts by origin as much "+
				"as by quality, and this one may have removed a whole side of the conversation "+
				"rather than its least reliable turns",
			opts.MinConfidence, removed))
	}
	// A transcript with holes is worse than a short one, because nothing in the
	// turns themselves reveals the gap.
	if gap := coverageGap(result, fmt.Sprintf(
		"%d of %d audio chunks in this range are not attributed yet, so this transcript has gaps",
		result.MissingChunks(), result.Chunks)); gap != "" {
		parts = append(parts, gap)
	}
	return strings.Join(parts, "; "), nil
}

// coverageGap completes a sentence about unattributed chunks with what can be
// done about them, and is empty when the range is fully attributed.
//
// Chunks whose recognition failed stay unattributed permanently, so the backfill
// is named only for the ones it can actually move: recommending it for the rest
// is an instruction to run a command and watch nothing change, which is worse
// than reporting an unfixable hole. Both the empty and the non-empty notice need
// that distinction, and it is written once so the two cannot come to disagree
// about which holes are worth acting on.
func coverageGap(result store.TranscriptResult, sentence string) string {
	if result.MissingChunks() <= 0 {
		return ""
	}
	if result.FailedChunks > 0 {
		sentence += fmt.Sprintf("; %d of them could not be transcribed and no backfill can recover them",
			result.FailedChunks)
	}
	if result.RecoverableChunks() > 0 {
		sentence += "; run `lumi transcript backfill` to fill the rest"
	}
	return sentence
}
