package season

import (
	"testing"
	"time"
)

// The window's edges, each a nanosecond either side, and instants given in
// another location, which the window reads in UTC.
func TestTheWindowIsTheFirstOfDecemberToTheFifthOfJanuaryInUTC(t *testing.T) {
	plus14 := time.FixedZone("UTC+14", 14*60*60)
	minus10 := time.FixedZone("UTC-10", -10*60*60)
	cases := []struct {
		at   time.Time
		want bool
		why  string
	}{
		{time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), false, "30 November 23:59:59"},
		{time.Date(2026, 11, 30, 23, 59, 59, 999999999, time.UTC), false, "the last instant of 30 November"},
		{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), true, "1 December 00:00:00, the window opens"},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), true, "31 December 23:59:59"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), true, "1 January 00:00:00"},
		{time.Date(2027, 1, 5, 23, 59, 59, 0, time.UTC), true, "5 January 23:59:59"},
		{time.Date(2027, 1, 5, 23, 59, 59, 999999999, time.UTC), true, "the last instant served"},
		{time.Date(2027, 1, 6, 0, 0, 0, 0, time.UTC), false, "6 January 00:00:00, the window has closed"},
		{time.Date(2026, 12, 1, 5, 0, 0, 0, plus14), false, "1 December 05:00 at UTC+14 is 30 November 15:00 UTC"},
		{time.Date(2026, 11, 30, 14, 0, 0, 0, minus10), true, "30 November 14:00 at UTC-10 is 1 December 00:00 UTC"},
		{time.Date(2027, 1, 5, 20, 0, 0, 0, minus10), false, "5 January 20:00 at UTC-10 is 6 January 06:00 UTC"},
		{time.Date(2027, 1, 6, 9, 0, 0, 0, plus14), true, "6 January 09:00 at UTC+14 is 5 January 19:00 UTC"},
		{time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC), false, "February"},
		{time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), false, "midsummer"},
	}
	for _, c := range cases {
		if got := InWindow(c.at); got != c.want {
			t.Errorf("InWindow(%s) = %v, want %v: %s", c.at.Format(time.RFC3339Nano), got, c.want, c.why)
		}
	}
}

// The season is named by the December that opens it, so New Year's Eve and
// New Year's Day are one season, and the next season begins as the window
// closes, ahead of its own opening.
func TestTheSeasonsYearIsTheDecemberThatOpensIt(t *testing.T) {
	plus14 := time.FixedZone("UTC+14", 14*60*60)
	minus10 := time.FixedZone("UTC-10", -10*60*60)
	cases := []struct {
		at   time.Time
		want int
		why  string
	}{
		{time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), 2026, "a cache built in November is the coming season's"},
		{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), 2026, "the window opens"},
		{time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC), 2026, "New Year's Eve"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 2026, "New Year's Day is the same season"},
		{time.Date(2027, 1, 5, 23, 59, 59, 999999999, time.UTC), 2026, "the last instant served"},
		{time.Date(2027, 1, 6, 0, 0, 0, 0, time.UTC), 2027, "the next season begins as the window closes"},
		{time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC), 2027, "midsummer is the coming December's"},
		{time.Date(2027, 1, 1, 5, 0, 0, 0, plus14), 2026, "1 January 05:00 at UTC+14 is 31 December UTC"},
		{time.Date(2026, 12, 31, 20, 0, 0, 0, minus10), 2026, "31 December 20:00 at UTC-10 is 1 January UTC"},
		{time.Date(2027, 1, 5, 20, 0, 0, 0, minus10), 2027, "5 January 20:00 at UTC-10 is 6 January UTC"},
	}
	for _, c := range cases {
		if got := Year(c.at); got != c.want {
			t.Errorf("Year(%s) = %d, want %d: %s", c.at.Format(time.RFC3339Nano), got, c.want, c.why)
		}
	}
}

// Every hour of a window names one season, and the season changes nowhere
// inside it: the year a regeneration seeds the order with never moves while
// the mix is served.
func TestOneWindowIsOneSeason(t *testing.T) {
	for _, opens := range []int{2025, 2026, 2027, 2028} {
		start := time.Date(opens, 12, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(opens+1, 1, 6, 0, 0, 0, 0, time.UTC)
		for at := start; at.Before(end); at = at.Add(time.Hour) {
			if !InWindow(at) {
				t.Fatalf("%s is inside the %d window and InWindow says no", at.Format(time.RFC3339), opens)
			}
			if got := Year(at); got != opens {
				t.Fatalf("Year(%s) = %d inside the window the %d December opens", at.Format(time.RFC3339), got, opens)
			}
		}
		if InWindow(end) || InWindow(start.Add(-time.Nanosecond)) {
			t.Fatalf("the %d window reaches past its edges", opens)
		}
	}
}
