package cli

import (
	"fmt"
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
	// and LUMI_CEREBRAS_MODEL let users point at smaller models, and
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

// contextFor renders events as the activity context for `ask`, capping each
// event at maxEventChars and the whole string at budget.
//
// budget is counted in bytes of the returned string, including the per-event
// header lines. Events arrive ranked best-first, so dropping the tail discards
// the least relevant. At least one event is always emitted: handing the model
// an empty context is worse than overshooting a soft budget.
func contextFor(events []store.Event, budget int) string {
	var builder strings.Builder
	for i, event := range events {
		block := fmt.Sprintf("[%s] kind=%s app=%q window=%q text_source=%q display_id=%d audio_source=%q media=%q\n%s\n\n",
			event.CapturedAt.Format(time.RFC3339), event.Kind, event.App, event.Window,
			event.TextSource, event.DisplayID, event.AudioSource, event.MediaPath,
			truncateRunes(compactScreenText(event.Text), maxEventChars))
		// The marker is part of the output, so its cost has to be reserved
		// before the block is admitted — otherwise appending it overshoots.
		marker := fmt.Sprintf(omissionMarker, len(events)-i)
		if i > 0 && builder.Len()+len(block)+len(marker) > budget {
			builder.WriteString(marker)
			break
		}
		builder.WriteString(block)
	}
	return builder.String()
}
