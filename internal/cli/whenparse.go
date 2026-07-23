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

var (
	// reLast matches "last hour" / "last 30 minutes" / "last 2 hours"; count defaults to 1.
	reLast = regexp.MustCompile(`(?i)\blast\s+(?:(\d+)\s*)?(hours?|mins?|minutes?)\b`)
	// reClockAMPM matches "9pm", "9:15 pm", "around 9:15pm". A leading connector
	// (around/at/near/~) is consumed so it can't survive as an FTS term; "about" is
	// excluded as usually topical ("tell me about 9pm"), not temporal.
	reClockAMPM = regexp.MustCompile(`(?i)(?:\b(?:around|at|near)\s+|~\s*)?\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b`)
	// reClock24 matches 24-hour times; the required colon stops bare numbers from
	// matching, and running after reClockAMPM keeps "9:15 pm" as 21:15.
	reClock24 = regexp.MustCompile(`(?i)(?:\b(?:around|at|near)\s+|~\s*)?\b([01]?\d|2[0-3]):([0-5]\d)\b`)

	reThisMorning      = regexp.MustCompile(`(?i)\bthis\s+morning\b`)
	reThisAfternoon    = regexp.MustCompile(`(?i)\bthis\s+afternoon\b`)
	reEveningOrTonight = regexp.MustCompile(`(?i)\b(?:this\s+evening|tonight)\b`)
	reYesterday        = regexp.MustCompile(`(?i)\byesterday\b`)
	reToday            = regexp.MustCompile(`(?i)\btoday\b`)
)

// parseTimeWindow derives a [since, until] window from a natural-time expression
// in question, resolved in now's timezone. rest is question with the match
// removed (whitespace collapsed) for FTS; when ok is false rest is unchanged.
// now is a parameter so resolution is deterministic under test.
func parseTimeWindow(question string, now time.Time) (since, until *time.Time, rest string, ok bool) {
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
	result := func(s, u time.Time, spans ...int) (*time.Time, *time.Time, string, bool) {
		return &s, &u, strip(spans...), true
	}
	// clockWindow centers ±clockHalfWindow on hour:minute. dayOffset shifts the
	// anchor day relative to now (0 today, -1 yesterday); anchored means an
	// explicit day qualifier fixed the day, so skip the most-recent-past guess.
	// Unanchored, the clock resolves to its most recent past occurrence (today
	// if it already passed, else yesterday).
	clockWindow := func(hour, minute, dayOffset int, anchored bool) (time.Time, time.Time) {
		c := time.Date(now.Year(), now.Month(), now.Day()+dayOffset, hour, minute, 0, 0, loc)
		if !anchored && c.After(now) {
			c = c.AddDate(0, 0, -1)
		}
		return c.Add(-clockHalfWindow), c.Add(clockHalfWindow)
	}
	// dayQualifier reports an explicit "yesterday"/"today" accompanying a clock
	// time: its offset from now and its span, so the clock anchors to that day
	// (not clockWindow's most-recent guess) and the day word is stripped from
	// the FTS terms rather than left to be silently dropped as a stopword.
	dayQualifier := func() (offset, lo, hi int, found bool) {
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
	resolveClock := func(hour, minute, lo, hi int) (*time.Time, *time.Time, string, bool) {
		if off, dlo, dhi, found := dayQualifier(); found {
			s, u := clockWindow(hour, minute, off, true)
			if dlo < lo {
				return result(s, u, dlo, dhi, lo, hi)
			}
			return result(s, u, lo, hi, dlo, dhi)
		}
		s, u := clockWindow(hour, minute, 0, false)
		return result(s, u, lo, hi)
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
		return result(now.Add(-time.Duration(n)*step), now, m[0], m[1])
	}

	if m := reClockAMPM.FindStringSubmatchIndex(question); m != nil {
		hour, _ := strconv.Atoi(question[m[2]:m[3]])
		minute := 0
		if m[4] >= 0 {
			minute, _ = strconv.Atoi(question[m[4]:m[5]])
		}
		hour = hour % 12
		if strings.EqualFold(question[m[6]:m[7]], "pm") {
			hour += 12
		}
		return resolveClock(hour, minute, m[0], m[1])
	}

	if m := reClock24.FindStringSubmatchIndex(question); m != nil {
		hour, _ := strconv.Atoi(question[m[2]:m[3]])
		minute, _ := strconv.Atoi(question[m[4]:m[5]])
		return resolveClock(hour, minute, m[0], m[1])
	}

	for _, named := range []struct {
		re    *regexp.Regexp
		since time.Time
		until time.Time
	}{
		{reThisMorning, dayAt(5), dayAt(12)},
		{reThisAfternoon, dayAt(12), dayAt(17)},
		{reEveningOrTonight, dayAt(17), nextDay},
		{reYesterday, startOfDay.AddDate(0, 0, -1), startOfDay},
		{reToday, startOfDay, nextDay},
	} {
		if m := named.re.FindStringIndex(question); m != nil {
			return result(named.since, named.until, m[0], m[1])
		}
	}

	return nil, nil, question, false
}
