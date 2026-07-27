package cli

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// clockHalfWindow pads a bare clock time ("9:15 pm") by ±15 min each side —
// "around" a time is fuzzy, but wider would pull in unrelated activity.
const clockHalfWindow = 15 * time.Minute

// clockDir is the direction a clock connector implies. A directional connector
// ("after"/"before") turns a bare clock into an open-ended window rather than a
// centered band; the zero value covers "around"/"at"/"~"/none as well as the
// duration and named-window forms.
type clockDir int

const (
	dirCentered clockDir = iota // around/at/near/~/none → ±clockHalfWindow band
	dirForward                  // after/since → [clock, end of clock's day]
	dirBackward                 // before/until/by/till/til → [start of clock's day, clock]
)

// clockDirFromConnector classifies a captured connector word. Callers pass the
// empty string when no connector preceded the clock, which resolves to centered.
func clockDirFromConnector(conn string) clockDir {
	switch strings.ToLower(conn) {
	case "after", "since":
		return dirForward
	case "before", "until", "by", "till", "til":
		return dirBackward
	default: // around, at, near, ~, ""
		return dirCentered
	}
}

// clockConnector is the optional lead-in before a clock time. It is a single
// capturing group (so direction is recoverable) that folds in the "~" form; the
// inner \b…\b boundaries enforce the whitespace before the digits, so "~"/"at9pm"
// still can't glue onto a number. "about" is excluded as usually topical
// ("tell me about 9pm"), not temporal.
const clockConnector = `(?:(~|\b(?:around|at|near|after|since|before|until|by|till|til)\b)\s*)?`

var (
	// reLast matches "last hour" / "last 30 minutes" / "last 2 hours"; count defaults to 1.
	reLast = regexp.MustCompile(`(?i)\blast\s+(?:(\d+)\s*)?(hours?|mins?|minutes?)\b`)
	// reClockAMPM matches "9pm", "9:15 pm", "around 9:15pm", "after 10:15 pm". The
	// leading connector is captured so it can't survive as an FTS term and its
	// direction is known.
	reClockAMPM = regexp.MustCompile(`(?i)` + clockConnector + `\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b`)
	// reClock24 matches 24-hour times; the required colon stops bare numbers from
	// matching, and running after reClockAMPM keeps "9:15 pm" as 21:15.
	reClock24 = regexp.MustCompile(`(?i)` + clockConnector + `\b([01]?\d|2[0-3]):([0-5]\d)\b`)

	// reDayBeforeYesterday must be consulted before reYesterday everywhere:
	// \byesterday\b matches *inside* this phrase, so without its own rule the
	// question resolves to yesterday and silently loses a day.
	reDayBeforeYesterday = regexp.MustCompile(`(?i)\b(?:the\s+)?day\s+before\s+yesterday\b`)
	// reDaysAgo matches "2 days ago", "two days ago", "a day ago", "a couple
	// (of) days ago". "a few days ago" is deliberately absent: it names no
	// specific day, and no window is more honest than a guessed one. The
	// alternatives are ordered so the longer "a couple" wins over the bare
	// article, and "of" is optional because "a couple days ago" is at least as
	// common as the full form — an unmatched phrasing would leak "couple" into
	// the FTS terms.
	reDaysAgo = regexp.MustCompile(`(?i)\b(?:(\d+)|a\s+couple(?:\s+of)?|an?|one|two|three|four|five|six|seven|eight|nine|ten)\s+days?\s+ago\b`)

	reThisMorning      = regexp.MustCompile(`(?i)\bthis\s+morning\b`)
	reThisAfternoon    = regexp.MustCompile(`(?i)\bthis\s+afternoon\b`)
	reEveningOrTonight = regexp.MustCompile(`(?i)\b(?:this\s+evening|tonight)\b`)
	reLastNight        = regexp.MustCompile(`(?i)\blast\s+night\b`)
	reYesterday        = regexp.MustCompile(`(?i)\byesterday\b`)
	reToday            = regexp.MustCompile(`(?i)\btoday\b`)
)

// spelledDays maps the count words reDaysAgo accepts onto a number of days.
var spelledDays = map[string]int{
	"a": 1, "an": 1, "one": 1,
	"a couple": 2, "a couple of": 2, "two": 2,
	"three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10,
}

// daysAgoCount reads the day count out of a reDaysAgo submatch (m from
// FindStringSubmatchIndex over question). A non-positive or unrecognized count
// falls back to 1: "0 days ago" is not a day in the past, and a phrase we
// matched but cannot count is still at least a day back.
func daysAgoCount(m []int, question string) int {
	if m[2] >= 0 {
		if n, err := strconv.Atoi(question[m[2]:m[3]]); err == nil && n > 0 {
			return n
		}
		return 1
	}
	// The match ends in "day(s) ago"; everything before it is the count word.
	fields := strings.Fields(strings.ToLower(question[m[0]:m[1]]))
	if len(fields) > 2 {
		if n, ok := spelledDays[strings.Join(fields[:len(fields)-2], " ")]; ok {
			return n
		}
	}
	return 1
}

// parseTimeWindow derives a [since, until] window from a natural-time expression
// in question, resolved in now's timezone. rest is question with the match
// removed (whitespace collapsed) for FTS; when ok is false rest is unchanged.
// dir reports whether a directional clock connector ("after"/"before") produced
// an open-ended window, so callers can phrase the interpretation note; it is
// dirCentered for centered clocks, duration, and named windows. now is a
// parameter so resolution is deterministic under test.
func parseTimeWindow(question string, now time.Time) (since, until *time.Time, rest string, dir clockDir, ok bool) {
	loc := now.Location()
	dayAt := func(h int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, loc)
	}
	startOfDay := dayAt(0)
	nextDay := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)

	strip := func(spans ...int) string {
		// Remove the given [lo,hi) spans (in question order) and collapse
		// whitespace; walking left to right keeps indices valid as we go.
		var b strings.Builder
		prev := 0
		for i := 0; i < len(spans); i += 2 {
			b.WriteString(question[prev:spans[i]])
			prev = spans[i+1]
		}
		b.WriteString(question[prev:])
		return strings.Join(strings.Fields(b.String()), " ")
	}
	result := func(dir clockDir, s, u time.Time, spans ...int) (*time.Time, *time.Time, string, clockDir, bool) {
		return &s, &u, strip(spans...), dir, true
	}
	// clockInstant resolves hour:minute on the anchored day. dayOffset shifts the
	// anchor day relative to now (0 today, -1 yesterday); anchored means an
	// explicit day qualifier fixed the day, so skip the most-recent-past guess.
	// Unanchored, the clock resolves to its most recent past occurrence (today
	// if it already passed, else yesterday).
	clockInstant := func(hour, minute, dayOffset int, anchored bool) time.Time {
		c := time.Date(now.Year(), now.Month(), now.Day()+dayOffset, hour, minute, 0, 0, loc)
		if !anchored && c.After(now) {
			c = c.AddDate(0, 0, -1)
		}
		return c
	}
	// clockWindow turns a resolved clock instant into a window per direction:
	// forward spans to the end of the clock's day, backward from that day's start,
	// centered pads ±clockHalfWindow. An unanchored forward window ("since 10pm")
	// extends to now when the clock's day has already ended — otherwise evaluating
	// it after midnight would drop captures between midnight and now even though
	// they occurred since 10pm. An anchored forward window ("yesterday after 10pm")
	// stays bounded to its day.
	clockWindow := func(hour, minute, dayOffset int, anchored bool, dir clockDir) (time.Time, time.Time) {
		c := clockInstant(hour, minute, dayOffset, anchored)
		switch dir {
		case dirForward:
			end := time.Date(c.Year(), c.Month(), c.Day()+1, 0, 0, 0, 0, loc)
			if !anchored && end.Before(now) {
				end = now
			}
			return c, end
		case dirBackward:
			return time.Date(c.Year(), c.Month(), c.Day(), 0, 0, 0, 0, loc), c
		default:
			return c.Add(-clockHalfWindow), c.Add(clockHalfWindow)
		}
	}
	// dayOffsetPhrase reports a phrase naming a whole day further back than
	// yesterday ("the day before yesterday", "3 days ago"): its offset from now
	// and its span. Every caller consults it *before* the single-day words,
	// because \byesterday\b matches inside "the day before yesterday" and would
	// otherwise answer one day late.
	dayOffsetPhrase := func() (offset, lo, hi int, found bool) {
		if m := reDayBeforeYesterday.FindStringIndex(question); m != nil {
			return -2, m[0], m[1], true
		}
		if m := reDaysAgo.FindStringSubmatchIndex(question); m != nil {
			return -daysAgoCount(m, question), m[0], m[1], true
		}
		return 0, 0, 0, false
	}
	// dayQualifier reports an explicit "last night"/"yesterday"/"today"
	// accompanying a clock time: its offset from now and its span, so the clock
	// anchors to that day (not clockInstant's most-recent guess) and the day word
	// is stripped from the FTS terms rather than left to be silently dropped as a
	// stopword (or, for "night", leaked into them).
	dayQualifier := func() (offset, lo, hi int, found bool) {
		if offset, lo, hi, found := dayOffsetPhrase(); found {
			return offset, lo, hi, true
		}
		if m := reLastNight.FindStringIndex(question); m != nil {
			return -1, m[0], m[1], true
		}
		if m := reYesterday.FindStringIndex(question); m != nil {
			return -1, m[0], m[1], true
		}
		if m := reToday.FindStringIndex(question); m != nil {
			return 0, m[0], m[1], true
		}
		return 0, 0, 0, false
	}
	// resolveClock builds the window for a matched clock time at question[lo:hi],
	// anchoring to an accompanying day qualifier when present.
	resolveClock := func(hour, minute, lo, hi int, dir clockDir) (*time.Time, *time.Time, string, clockDir, bool) {
		if off, dlo, dhi, found := dayQualifier(); found {
			s, u := clockWindow(hour, minute, off, true, dir)
			if dlo < lo {
				return result(dir, s, u, dlo, dhi, lo, hi)
			}
			return result(dir, s, u, lo, hi, dlo, dhi)
		}
		s, u := clockWindow(hour, minute, 0, false, dir)
		return result(dir, s, u, lo, hi)
	}

	if m := reLast.FindStringSubmatchIndex(question); m != nil {
		n := 1
		if m[2] >= 0 {
			if parsed, err := strconv.Atoi(question[m[2]:m[3]]); err == nil && parsed > 0 {
				n = parsed
			}
		}
		unit := strings.ToLower(question[m[4]:m[5]])
		step := time.Minute
		if strings.HasPrefix(unit, "hour") {
			step = time.Hour
		}
		return result(dirCentered, now.Add(-time.Duration(n)*step), now, m[0], m[1])
	}

	if m := reClockAMPM.FindStringSubmatchIndex(question); m != nil {
		dir := dirCentered
		if m[2] >= 0 {
			dir = clockDirFromConnector(question[m[2]:m[3]])
		}
		hour, _ := strconv.Atoi(question[m[4]:m[5]])
		minute := 0
		if m[6] >= 0 {
			minute, _ = strconv.Atoi(question[m[6]:m[7]])
		}
		hour = hour % 12
		if strings.EqualFold(question[m[8]:m[9]], "pm") {
			hour += 12
		}
		return resolveClock(hour, minute, m[0], m[1], dir)
	}

	if m := reClock24.FindStringSubmatchIndex(question); m != nil {
		dir := dirCentered
		if m[2] >= 0 {
			dir = clockDirFromConnector(question[m[2]:m[3]])
		}
		hour, _ := strconv.Atoi(question[m[4]:m[5]])
		minute, _ := strconv.Atoi(question[m[6]:m[7]])
		return resolveClock(hour, minute, m[0], m[1], dir)
	}

	// A day-offset phrase with no clock is the whole of that day. It is handled
	// ahead of the named-window table both because its offset is computed (the
	// table is static) and because reYesterday there would match inside it.
	if offset, lo, hi, found := dayOffsetPhrase(); found {
		s := time.Date(now.Year(), now.Month(), now.Day()+offset, 0, 0, 0, 0, loc)
		u := time.Date(now.Year(), now.Month(), now.Day()+offset+1, 0, 0, 0, 0, loc)
		return result(dirCentered, s, u, lo, hi)
	}

	for _, named := range []struct {
		re    *regexp.Regexp
		since time.Time
		until time.Time
	}{
		{reThisMorning, dayAt(5), dayAt(12)},
		{reThisAfternoon, dayAt(12), dayAt(17)},
		{reEveningOrTonight, dayAt(17), nextDay},
		{reLastNight, dayAt(17).AddDate(0, 0, -1), startOfDay},
		{reYesterday, startOfDay.AddDate(0, 0, -1), startOfDay},
		{reToday, startOfDay, nextDay},
	} {
		if m := named.re.FindStringIndex(question); m != nil {
			return result(dirCentered, named.since, named.until, m[0], m[1])
		}
	}

	return nil, nil, question, dirCentered, false
}
