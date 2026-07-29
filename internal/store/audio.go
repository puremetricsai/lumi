package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// AudioTrack is one row of a collapsed audio chunk, including the survivor. The
// JSON tags make `lumi search --collapse-audio --json` render tracks in the same
// snake_case as every other exported field; the MCP boundary maps through its
// own AudioTrackRecord and is unaffected.
type AudioTrack struct {
	ID          int64  `json:"id"`
	AudioSource string `json:"audio_source"` // "system" or "microphone"
	// TextLength is the transcript's rune count after TrimSpace, so 0
	// unambiguously means this track recorded silence. This is a different
	// question from EventRecord.TextLength, which pairs with Truncated and
	// counts the untrimmed text.
	TextLength int    `json:"text_length"`
	MediaPath  string `json:"media_path"`
}

// AudioChunk is what one survivor was collapsed from.
type AudioChunk struct {
	// Origin is "system", "microphone", "both", or "silent" — which tracks
	// carried speech, not which one won. "both" is speaker bleed. Nothing in
	// the data distinguishes a remote speaker from media playback; both are
	// "system".
	Origin string
	Tracks []AudioTrack // every row in the group, survivor first
}

// Audio origin values reported by CollapseAudioTracks.
const (
	AudioOriginSystem     = "system"
	AudioOriginMicrophone = "microphone"
	AudioOriginBoth       = "both"
	AudioOriginSilent     = "silent"
)

// CollapseAudioTracks returns the surviving events in input order, plus each
// survivor's provenance keyed by its id.
//
// The microphone re-records whatever the speakers play, so when both tracks of
// a 30-second chunk carry speech they transcribe the same utterance twice. This
// collapses each such pair to one row — the system track, which is the cleaner
// original — and records in the returned AudioChunk what was merged away and
// which tracks actually carried speech.
//
// Grouping is on the shared captured_at string alone. The two rows of a chunk
// are written from one Audio.Record call with one timestamp, so their stored
// captured_at is byte-identical; their ids are not necessarily adjacent (a
// screen row can be inserted between them), so id arithmetic must never be used
// to pair them. KindScreen events pass through untouched and get no chunk.
func CollapseAudioTracks(events []Event) (kept []Event, chunks map[int64]AudioChunk) {
	kept = make([]Event, 0, len(events))
	chunks = make(map[int64]AudioChunk)

	// groupIndex maps a chunk's captured_at key to its position in kept, so the
	// survivor keeps the earliest rank any member of its group earned.
	groupIndex := make(map[string]int)
	// members collects every track of a group, in input order, by the same key.
	members := make(map[string][]Event)
	// order remembers the first-seen key for each kept slot so we can rebuild
	// provenance after every member has been seen.
	keyAt := make(map[int]string)

	for _, event := range events {
		if event.Kind != KindAudio {
			kept = append(kept, event)
			continue
		}
		key := event.CapturedAt.UTC().Format(time.RFC3339Nano)
		if slot, seen := groupIndex[key]; seen {
			members[key] = append(members[key], event)
			// Promote the group's survivor into the slot if this member beats
			// the one currently there.
			if audioLess(kept[slot], event) {
				kept[slot] = event
			}
			continue
		}
		slot := len(kept)
		kept = append(kept, event)
		groupIndex[key] = slot
		keyAt[slot] = key
		members[key] = []Event{event}
	}

	for slot, key := range keyAt {
		group := members[key]
		survivor := kept[slot]
		chunks[survivor.ID] = buildChunk(survivor, group)
	}
	return kept, chunks
}

// audioLess reports whether a should lose to b within a chunk. The winner is the
// lexicographic maximum of (hasText, isSystem, runeLen(text), -id):
//
//  1. hasText — a non-blank transcript always beats a blank one. This sits above
//     isSystem on purpose: a silent row is not a duplicate of anything, so
//     letting it win would delete a transcript rather than deduplicate one.
//  2. isSystem — among rows that both have text, the system track wins
//     unconditionally, because it is the original the microphone re-recorded and
//     the measurably cleaner transcript.
//  3. runeLen(text), then lowest id — deterministic tie-breaks only reachable
//     between two same-source rows, so they never override isSystem.
func audioLess(a, b Event) bool {
	aHas, bHas := hasText(a.Text), hasText(b.Text)
	if aHas != bHas {
		return !aHas // a loses if it has no text and b does
	}
	aSys, bSys := isSystem(a), isSystem(b)
	if aSys != bSys {
		return !aSys // a loses if it is not system and b is
	}
	aLen, bLen := utf8.RuneCountInString(a.Text), utf8.RuneCountInString(b.Text)
	if aLen != bLen {
		return aLen < bLen
	}
	// Lower id wins, so a with the higher id loses.
	return a.ID > b.ID
}

func buildChunk(survivor Event, group []Event) AudioChunk {
	tracks := make([]AudioTrack, 0, len(group))
	tracks = append(tracks, trackOf(survivor))
	for _, member := range group {
		if member.ID == survivor.ID {
			continue
		}
		tracks = append(tracks, trackOf(member))
	}
	return AudioChunk{Origin: OriginOf(tracks), Tracks: tracks}
}

// OriginOf reports the chunk origin implied by a set of tracks — which sources
// carried speech (TextLength > 0), independent of which track won a collapse.
// It is how the MCP boundary derives a truthful origin from AudioTracksAt, which
// returns the whole chunk even when a search matched only one of its tracks.
func OriginOf(tracks []AudioTrack) string {
	var systemText, micText bool
	for _, tr := range tracks {
		if tr.TextLength == 0 {
			continue
		}
		if tr.AudioSource == "system" {
			systemText = true
		} else if tr.AudioSource == "microphone" {
			micText = true
		}
	}
	return origin(systemText, micText)
}

func trackOf(event Event) AudioTrack {
	return AudioTrack{
		ID:          event.ID,
		AudioSource: event.AudioSource,
		TextLength:  utf8.RuneCountInString(strings.TrimSpace(event.Text)),
		MediaPath:   event.MediaPath,
	}
}

func origin(systemText, micText bool) string {
	switch {
	case systemText && micText:
		return AudioOriginBoth
	case systemText:
		return AudioOriginSystem
	case micText:
		return AudioOriginMicrophone
	default:
		return AudioOriginSilent
	}
}

func hasText(text string) bool { return strings.TrimSpace(text) != "" }

func isSystem(event Event) bool { return event.AudioSource == "system" }

// AudioTracksAt returns every audio row at each of the given capture times,
// keyed by the RFC3339Nano timestamp, regardless of what a search matched.
//
// It exists so provenance can describe the whole chunk even when a search
// matched only one of its tracks: an FTS query landing in only the microphone
// transcript returns that row alone, but the chunk's origin is still "both". The
// collapse decides among matched rows; this reports the whole chunk.
func (s *Store) AudioTracksAt(ctx context.Context, times []string) (map[string][]AudioTrack, error) {
	result := make(map[string][]AudioTrack)
	if len(times) == 0 {
		return result, nil
	}
	for start := 0; start < len(times); start += deleteBatchSize {
		end := start + deleteBatchSize
		if end > len(times) {
			end = len(times)
		}
		if err := s.scanAudioTracksBatch(ctx, times[start:end], result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// scanAudioTracksBatch reads one bounded IN (…) batch into result. Splitting it
// out keeps rows.Close scoped to the batch rather than deferred across the whole
// loop.
func (s *Store) scanAudioTracksBatch(ctx context.Context, batch []string, result map[string][]AudioTrack) error {
	placeholders := make([]string, len(batch))
	args := make([]any, len(batch))
	for i, t := range batch {
		placeholders[i] = "?"
		args[i] = t
	}
	query := `SELECT captured_at, id, audio_source, text, media_path FROM events
WHERE kind = 'audio' AND captured_at IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("select audio tracks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var capturedAt, text string
		var track AudioTrack
		if err := rows.Scan(&capturedAt, &track.ID, &track.AudioSource, &text, &track.MediaPath); err != nil {
			return fmt.Errorf("scan audio track: %w", err)
		}
		track.TextLength = utf8.RuneCountInString(strings.TrimSpace(text))
		result[capturedAt] = append(result[capturedAt], track)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate audio tracks: %w", err)
	}
	return nil
}
