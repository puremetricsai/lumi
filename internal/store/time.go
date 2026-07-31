package store

import "time"

// CapturedAtLayout renders the exact string stored in events.captured_at and
// audio_segments.captured_at.
//
// It is fixed-width on purpose. Every range filter, ORDER BY, MAX, and coverage
// query in this package compares captured_at *lexicographically*, and
// time.RFC3339Nano trims trailing zeros — so ".12Z" and ".123456789Z", which are
// .120000000 and .123456789, sort in the wrong order: '2' < '2', then 'Z'
// (0x5A) > '3' (0x33), putting the earlier instant last. Padding every fraction
// to nine digits makes byte order and chronological order the same thing.
//
// Nine digits rather than six because truncating is what breaks a *key*: a
// caller rebuilding a lookup key from a stored row must reproduce that row's
// bytes, and rows written before this constant existed carry full nanosecond
// precision. See FormatCapturedAt.
//
// The trailing Z is a literal, not Go's Z07:00 offset verb, because every value
// is converted to UTC first and a literal keeps the width constant.
const CapturedAtLayout = "2006-01-02T15:04:05.000000000Z"

// FormatCapturedAt renders t as the string stored in captured_at. It is the only
// way any caller may produce that key.
//
// It does not round or truncate. Insert stamps events.captured_at with it and
// ReplaceChunkSegments keys audio_segments with it, and ChunksMissingSegments
// joins those two columns on equality — so a lossy version turns an indexed
// "…T19:33:48.123456789Z" row into a "…T19:33:48.123456Z" key that matches no
// row, parking every newly recorded chunk on the backfill queue forever and
// reporting 0 % SegmentCoverage, with no error anywhere.
func FormatCapturedAt(t time.Time) string {
	return t.UTC().Format(CapturedAtLayout)
}

// LowerCapturedAtBound and UpperCapturedAtBound render an instant for a `>=` or
// `<=` comparison against captured_at.
//
// A single rendering is not enough, because the index holds two. For an instant
// with trailing zeros the legacy trimmed form sorts *above* the fixed-width one
// — ".12Z" against ".120000000Z", where 'Z' (0x5A) beats '0' (0x30) — so a
// fixed-width upper bound silently excludes a legacy row sitting exactly on it,
// and a legacy-shaped lower bound would exclude a new one. Taking the smallest
// rendering for a lower bound and the largest for an upper bound covers both
// without widening the range to any instant that is not already inside it.
//
// These are bounds only, and nothing may reuse them as an equality key: they
// deliberately name an instant two ways, which is right for a comparison and
// wrong for a lookup. Every equality caller here (SegmentsForChunk,
// ReplaceChunkSegments, AudioEventsAt) is handed the stored string rather than
// rebuilding one from an instant, which is what keeps the two renderings from
// mattering at all.
func LowerCapturedAtBound(t time.Time) string {
	fixed, legacy := FormatCapturedAt(t), t.UTC().Format(time.RFC3339Nano)
	if legacy < fixed {
		return legacy
	}
	return fixed
}

func UpperCapturedAtBound(t time.Time) string {
	fixed, legacy := FormatCapturedAt(t), t.UTC().Format(time.RFC3339Nano)
	if legacy > fixed {
		return legacy
	}
	return fixed
}
