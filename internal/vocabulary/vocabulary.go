// Package vocabulary reads the user-maintained term list that biases Apple's
// on-device speech recognition. It owns the file format, the cache, and the
// cap so callers read the rules rather than restating them, and it depends on
// nothing native so every rule here is testable without permissions.
package vocabulary

import (
	"strings"
)

// MaxTerms caps how many phrases are handed to contextual biasing. Biasing is
// a budget, not a dictionary: an oversized list dilutes every term and makes
// the recognizer likelier to snap acoustically similar audio onto an injected
// phrase. 100 matches Apple's guidance for the legacy
// SFSpeechRecognitionRequest.contextualStrings. The value is provisional —
// revisit it once `lumi transcribe` can measure where a longer list hurts.
const MaxTerms = 100

// Parse applies the file format: one term per line, `#` comments and blank
// lines dropped, surrounding whitespace trimmed, exact duplicates collapsed to
// their first occurrence. File order is priority order, so the cap keeps the
// earliest terms and dropped counts only what the cap discarded — duplicates
// are not drops.
func Parse(data []byte) (terms []string, dropped int) {
	seen := make(map[string]struct{})
	// Splitting beats bufio.Scanner here: the whole file is already in memory,
	// so scanning buys no streaming, while its 64KB token limit would silently
	// discard every term after an over-long line — and Parse has no way to
	// report that.
	for _, line := range strings.Split(string(data), "\n") {
		term := strings.TrimSpace(line)
		if term == "" || strings.HasPrefix(term, "#") {
			continue
		}
		if _, duplicate := seen[term]; duplicate {
			continue
		}
		seen[term] = struct{}{}
		if len(terms) >= MaxTerms {
			dropped++
			continue
		}
		terms = append(terms, term)
	}
	return terms, dropped
}
