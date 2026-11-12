package maintenance

import (
	"testing"
	"time"
)

// sunday is a fixed instant the cases judge windows from: Sunday 12:00 UTC.
var sunday = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// TestParseWindowReadsWeeklyPeriods checks the value shape SETTINGS-C-72 gives
// the policy: one or more day-and-time periods, an empty value allowing every
// instant, and a malformed value refusing to be read as a policy.
func TestParseWindowReadsWeeklyPeriods(t *testing.T) {
	for _, value := range []string{"", "   "} {
		window, err := ParseWindow(value)
		if err != nil {
			t.Fatalf("ParseWindow(%q) = %v, want an unwritten policy", value, err)
		}
		if !window.Allows(sunday) {
			t.Fatalf("the unwritten policy refused %v", sunday)
		}
	}

	for _, value := range []string{
		"Funday 02:00-04:00",
		"Sun 02:00",
		"Sun 02:00-02:00",
		"Sun 2:00-4:00",
		"Sun 02:00-04:00,",
		"Sun 24:00-25:00",
		",Sun 02:00-04:00",
	} {
		if _, err := ParseWindow(value); err == nil {
			t.Fatalf("ParseWindow(%q) accepted a malformed value", value)
		}
	}
}

// TestWindowAllowsItsOwnPeriod checks the half-open weekly period: an opening
// instant is inside, the closing instant is outside, and the day names follow
// the UTC week.
func TestWindowAllowsItsOwnPeriod(t *testing.T) {
	window, err := ParseWindow("Sun 02:00-04:00")
	if err != nil {
		t.Fatalf("parse the window: %v", err)
	}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before the opening", time.Date(2026, 3, 1, 1, 59, 0, 0, time.UTC), false},
		{"at the opening", time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC), true},
		{"inside", time.Date(2026, 3, 1, 3, 59, 59, 0, time.UTC), true},
		{"at the closing", time.Date(2026, 3, 1, 4, 0, 0, 0, time.UTC), false},
		{"the next week's opening", time.Date(2026, 3, 8, 2, 0, 0, 0, time.UTC), true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := window.Allows(testCase.at); got != testCase.want {
				t.Fatalf("Allows(%v) = %v, want %v", testCase.at, got, testCase.want)
			}
		})
	}
}

// TestWindowReadsAPeriodAcrossMidnight checks that a period whose end precedes
// its start continues into the next day, and that its tail on the other side of
// the week's end is still inside.
func TestWindowReadsAPeriodAcrossMidnight(t *testing.T) {
	window, err := ParseWindow("Mon 22:00-02:00, Sat 23:30-00:30")
	if err != nil {
		t.Fatalf("parse the window: %v", err)
	}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"Monday before the opening", time.Date(2026, 3, 2, 21, 59, 0, 0, time.UTC), false},
		{"Monday evening", time.Date(2026, 3, 2, 23, 0, 0, 0, time.UTC), true},
		{"Tuesday after midnight", time.Date(2026, 3, 3, 1, 59, 0, 0, time.UTC), true},
		{"Tuesday at the closing", time.Date(2026, 3, 3, 2, 0, 0, 0, time.UTC), false},
		{"Saturday before midnight", time.Date(2026, 3, 7, 23, 45, 0, 0, time.UTC), true},
		{"Sunday after the week's end", time.Date(2026, 3, 8, 0, 15, 0, 0, time.UTC), true},
		{"Sunday after the tail", time.Date(2026, 3, 8, 0, 30, 0, 0, time.UTC), false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := window.Allows(testCase.at); got != testCase.want {
				t.Fatalf("Allows(%v) = %v, want %v", testCase.at, got, testCase.want)
			}
		})
	}
}

// TestNextOpeningNamesTheEarliestFutureStart checks the point the refusal
// carries: the next start strictly after the judged instant, across the week
// boundary and for a period that runs past midnight.
func TestNextOpeningNamesTheEarliestFutureStart(t *testing.T) {
	window, err := ParseWindow("Mon 22:00-02:00, Thu 10:00-12:00")
	if err != nil {
		t.Fatalf("parse the window: %v", err)
	}
	cases := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{
			"after Thursday's period",
			time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 3, 9, 22, 0, 0, 0, time.UTC),
		},
		{
			"inside the overnight period",
			time.Date(2026, 3, 3, 1, 0, 0, 0, time.UTC),
			time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC),
		},
		{
			"before Monday's period",
			time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 3, 2, 22, 0, 0, 0, time.UTC),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := window.NextOpening(testCase.at); !got.Equal(testCase.want) {
				t.Fatalf("NextOpening(%v) = %v, want %v", testCase.at, got, testCase.want)
			}
		})
	}
}
