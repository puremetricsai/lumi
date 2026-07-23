package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/puremetricsai/lumi/internal/store"
)

const (
	// defaultContextChars budgets the activity context sent to Cerebras.
	// Budgeting in characters rather than tokens avoids a tokenizer
	// dependency; the ratio assumed here is a pessimistic ~3 chars/token,
	// because screen text (UI chrome, symbols, and occasional Vision errors)
	// tokenizes worse than prose. 60000 chars is therefore ~20k tokens, which leaves
	// room for the 1200-token completion reserve and the prompt scaffold
	// inside a 32k context. The floor is deliberately conservative: --model
	// and the configured cerebras_model let users point at smaller models, and
	// --max-context-chars is the escape hatch for larger ones.
	defaultContextChars = 60000
	// maxEventChars caps any single event so one full-page screen-text dump cannot
	// consume the whole budget.
	maxEventChars  = 2000
	ellipsis       = "…"
	omissionMarker = "[... %d further events omitted to fit the context budget]\n"
)

// truncateRunes limits s to max bytes without ever splitting a rune,
// appending an ellipsis when it had to cut.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	limit := max - len(ellipsis)
	if limit <= 0 {
		// No room for the marker; return whole runes only.
		return trimToRunes(s, max)
	}
	return trimToRunes(s, limit) + ellipsis
}

func trimToRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// compactScreenText strips the padding whitespace that dominates extracted
// screen text: each
// line is trimmed and blank lines are dropped. Line structure is preserved on
// purpose — collapsing all whitespace (strings.Fields) would destroy the
// layout that makes screen text readable.
func compactScreenText(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func screenEvidence(event store.Event) (string, string) {
	text := truncateRunes(compactScreenText(event.Text), maxEventChars)
	if text != "" && text == strings.TrimSpace(event.Window) {
		return "window_title_only", "[window title only; no file contents or user action captured]"
	}
	return "visible_extracted_text", text
}

// localTimestamp renders a UTC-stored instant in the user's local timezone.
// `ask` questions and answers use local wall-clock time; RFC3339 keeps the
// numeric offset so the instant stays unambiguous.
func localTimestamp(t time.Time) string {
	return t.Local().Format(time.RFC3339)
}

func eventBlock(event store.Event) string {
	text := truncateRunes(compactScreenText(event.Text), maxEventChars)
	if event.Kind == store.KindAudio {
		status := "present"
		if text == "" {
			status = "unavailable"
			text = "[no searchable transcript was produced for this audio file]"
		}
		return fmt.Sprintf("[%s] kind=audio audio_source=%q transcript_status=%s media=%q duration_ms=%d\n%s\n\n",
			localTimestamp(event.CapturedAt), event.AudioSource, status,
			event.MediaPath, event.DurationMS, text)
	}
	observation, text := screenEvidence(event)
	return fmt.Sprintf("[%s] kind=screen observation=%s app=%q window=%q text_source=%q display_id=%d media=%q\n%s\n\n",
		localTimestamp(event.CapturedAt), observation, event.App, event.Window,
		event.TextSource, event.DisplayID, event.MediaPath, text)
}

// sameScreenText reports whether adjacent selected screen events carry the
// same evidence for ask. Their screenshots remain separate files in the store;
// this only prevents a run of identical extracted text from dominating the LLM
// context and being mistaken for distinct activity.
func sameScreenText(a, b store.Event) bool {
	return a.Kind == store.KindScreen && b.Kind == store.KindScreen &&
		a.App == b.App && a.Window == b.Window && a.Text == b.Text &&
		a.TextSource == b.TextSource && a.DisplayID == b.DisplayID
}

func repeatedScreenBlock(events []store.Event) string {
	first := events[0]
	last := events[len(events)-1]
	observation, text := screenEvidence(first)
	return fmt.Sprintf("[%s .. %s] kind=screen observation=unchanged_%s captures=%d app=%q window=%q text_source=%q display_id=%d media_files=%d\n%s\n\n",
		localTimestamp(first.CapturedAt), localTimestamp(last.CapturedAt),
		observation, len(events), first.App, first.Window, first.TextSource, first.DisplayID, len(events), text)
}

// contextFor renders events as the activity context for `ask`, capping each
// event at maxEventChars and the whole string at budget.
//
// budget is counted in bytes of the returned string, including the per-event
// header lines. Events arrive ranked best-first, so admission happens in that
// order and dropping the tail discards the least relevant. The admitted events
// are then rendered oldest-first so the model can reconstruct activity without
// inventing an order from a relevance-ranked or newest-first list. At least one
// event is always emitted: handing the model an empty context is worse than
// overshooting a soft budget.
func contextFor(events []store.Event, budget int) string {
	selected := make([]store.Event, 0, len(events))
	selectedBytes := 0
	omitted := 0
	for i, event := range events {
		block := eventBlock(event)
		// The marker is part of the output, so its cost has to be reserved
		// before the block is admitted — otherwise appending it overshoots.
		marker := fmt.Sprintf(omissionMarker, len(events)-i)
		if i > 0 && selectedBytes+len(block)+len(marker) > budget {
			omitted = len(events) - i
			break
		}
		selected = append(selected, event)
		selectedBytes += len(block)
	}

	// Search still chooses which events make the cut. Sorting only after that
	// choice preserves relevance while giving the answerer a trustworthy
	// timeline. SliceStable preserves retrieval order for equal timestamps.
	sort.SliceStable(selected, func(i, j int) bool {
		return selected[i].CapturedAt.Before(selected[j].CapturedAt)
	})

	var builder strings.Builder
	builder.Grow(selectedBytes)
	for i := 0; i < len(selected); {
		end := i + 1
		for end < len(selected) && sameScreenText(selected[end-1], selected[end]) {
			end++
		}
		if end-i > 1 {
			builder.WriteString(repeatedScreenBlock(selected[i:end]))
		} else {
			builder.WriteString(eventBlock(selected[i]))
		}
		i = end
	}
	if omitted > 0 {
		builder.WriteString(fmt.Sprintf(omissionMarker, omitted))
	}
	return builder.String()
}
