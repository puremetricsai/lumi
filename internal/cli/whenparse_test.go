package cli

import (
	"testing"
	"time"
)

// pdt is a fixed offset zone used so window math is deterministic regardless of
// the machine's timezone.
var pdt = time.FixedZone("PDT", -7*3600)

func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, pdt)
}

func TestParseTimeWindow(t *testing.T) {
	// A fixed "now": 2026-07-22 (Wed) 22:30 local.
	now := at(2026, 7, 22, 22, 30)

	for _, tc := range []struct {
		name      string
		question  string
		now       time.Time
		wantSince time.Time
		wantUntil time.Time
		wantRest  string
		wantDir   clockDir
	}{
		{
			name:      "clock pm with minutes, earlier today",
			question:  "what was captured around 9:15 pm",
			now:       now,
			wantSince: at(2026, 7, 22, 21, 0),
			wantUntil: at(2026, 7, 22, 21, 30),
			wantRest:  "what was captured",
		},
		{
			name:      "clock pm rolls to yesterday when still future today",
			question:  "9:15 pm",
			now:       at(2026, 7, 22, 10, 0), // 10am: 21:15 has not happened yet today
			wantSince: at(2026, 7, 21, 21, 0),
			wantUntil: at(2026, 7, 21, 21, 30),
			wantRest:  "",
		},
		{
			name:      "clock anchored to yesterday overrides most-recent heuristic",
			question:  "yesterday at 9:15 pm",
			now:       now, // 22:30: 21:15 already passed today, so the bare-clock heuristic would wrongly pick today
			wantSince: at(2026, 7, 21, 21, 0),
			wantUntil: at(2026, 7, 21, 21, 30),
			wantRest:  "",
		},
		{
			name:      "clock anchored to today overrides most-recent heuristic",
			question:  "what happened today at 11pm",
			now:       now, // 22:30: 23:00 is future, so the bare-clock heuristic would wrongly roll to yesterday
			wantSince: at(2026, 7, 22, 22, 45),
			wantUntil: at(2026, 7, 22, 23, 15),
			wantRest:  "what happened",
		},
		{
			name:      "bare hour pm",
			question:  "tell me about 9pm",
			now:       now,
			wantSince: at(2026, 7, 22, 20, 45),
			wantUntil: at(2026, 7, 22, 21, 15),
			wantRest:  "tell me about",
		},
		{
			name:      "24-hour clock",
			question:  "screens at 21:15",
			now:       now,
			wantSince: at(2026, 7, 22, 21, 0),
			wantUntil: at(2026, 7, 22, 21, 30),
			wantRest:  "screens",
		},
		{
			name:      "yesterday",
			question:  "what did I work on yesterday",
			now:       now,
			wantSince: at(2026, 7, 21, 0, 0),
			wantUntil: at(2026, 7, 22, 0, 0),
			wantRest:  "what did I work on",
		},
		{
			name:      "today",
			question:  "summarize today",
			now:       now,
			wantSince: at(2026, 7, 22, 0, 0),
			wantUntil: at(2026, 7, 23, 0, 0),
			wantRest:  "summarize",
		},
		{
			name:      "this morning",
			question:  "meetings this morning",
			now:       now,
			wantSince: at(2026, 7, 22, 5, 0),
			wantUntil: at(2026, 7, 22, 12, 0),
			wantRest:  "meetings",
		},
		{
			name:      "this afternoon",
			question:  "what happened this afternoon",
			now:       now,
			wantSince: at(2026, 7, 22, 12, 0),
			wantUntil: at(2026, 7, 22, 17, 0),
			wantRest:  "what happened",
		},
		{
			name:      "tonight",
			question:  "anything tonight",
			now:       now,
			wantSince: at(2026, 7, 22, 17, 0),
			wantUntil: at(2026, 7, 23, 0, 0),
			wantRest:  "anything",
		},
		{
			name:      "last hour",
			question:  "what did I see last hour",
			now:       now,
			wantSince: at(2026, 7, 22, 21, 30),
			wantUntil: now,
			wantRest:  "what did I see",
		},
		{
			name:      "last N minutes",
			question:  "last 30 minutes please",
			now:       now,
			wantSince: at(2026, 7, 22, 22, 0),
			wantUntil: now,
			wantRest:  "please",
		},
		{
			name:      "last N hours",
			question:  "recap the last 2 hours",
			now:       now,
			wantSince: at(2026, 7, 22, 20, 30),
			wantUntil: now,
			wantRest:  "recap the",
		},
		{
			name:      "after clock is forward to end of day",
			question:  "what was said after 10:15 pm",
			now:       now,
			wantSince: at(2026, 7, 22, 22, 15),
			wantUntil: at(2026, 7, 23, 0, 0),
			wantRest:  "what was said",
			wantDir:   dirForward,
		},
		{
			name:      "since clock is forward",
			question:  "emails since 3pm",
			now:       now,
			wantSince: at(2026, 7, 22, 15, 0),
			wantUntil: at(2026, 7, 23, 0, 0),
			wantRest:  "emails",
			wantDir:   dirForward,
		},
		{
			// After midnight, an unanchored "since 10pm" anchors to yesterday; the
			// window must still reach now, not stop at yesterday's midnight, or
			// captures between midnight and now are wrongly excluded.
			name:      "unanchored forward after midnight extends to now",
			question:  "what happened since 10pm",
			now:       at(2026, 7, 23, 0, 30), // 12:30 AM
			wantSince: at(2026, 7, 22, 22, 0),
			wantUntil: at(2026, 7, 23, 0, 30),
			wantRest:  "what happened",
			wantDir:   dirForward,
		},
		{
			name:      "before clock is backward from start of day",
			question:  "anything before 9 am",
			now:       now,
			wantSince: at(2026, 7, 22, 0, 0),
			wantUntil: at(2026, 7, 22, 9, 0),
			wantRest:  "anything",
			wantDir:   dirBackward,
		},
		{
			name:      "until clock is backward",
			question:  "work until 5pm",
			now:       now,
			wantSince: at(2026, 7, 22, 0, 0),
			wantUntil: at(2026, 7, 22, 17, 0),
			wantRest:  "work",
			wantDir:   dirBackward,
		},
		{
			name:      "last night standalone",
			question:  "what happened last night",
			now:       now,
			wantSince: at(2026, 7, 21, 17, 0),
			wantUntil: at(2026, 7, 22, 0, 0),
			wantRest:  "what happened",
		},
		{
			name:      "last night anchors an after clock to yesterday",
			question:  "what was said last night after 10:15 pm",
			now:       now,
			wantSince: at(2026, 7, 21, 22, 15),
			wantUntil: at(2026, 7, 22, 0, 0),
			wantRest:  "what was said",
			wantDir:   dirForward,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			since, until, rest, dir, ok := parseTimeWindow(tc.question, tc.now)
			if !ok {
				t.Fatalf("expected a match for %q", tc.question)
			}
			if since == nil || !since.Equal(tc.wantSince) {
				t.Fatalf("since = %v, want %v", since, tc.wantSince)
			}
			if until == nil || !until.Equal(tc.wantUntil) {
				t.Fatalf("until = %v, want %v", until, tc.wantUntil)
			}
			if rest != tc.wantRest {
				t.Fatalf("rest = %q, want %q", rest, tc.wantRest)
			}
			if dir != tc.wantDir {
				t.Fatalf("dir = %d, want %d", dir, tc.wantDir)
			}
		})
	}
}

func TestParseTimeWindowNoMatch(t *testing.T) {
	now := at(2026, 7, 22, 22, 30)
	for _, question := range []string{
		"what did I read about postgres",
		"summarize my work",
		"", // empty
	} {
		if _, _, _, _, ok := parseTimeWindow(question, now); ok {
			t.Fatalf("did not expect a time match in %q", question)
		}
	}
}
