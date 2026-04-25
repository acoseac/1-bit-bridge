package config

import "testing"

func TestParseQuietHoursAccepts(t *testing.T) {
	cases := map[string][2]int{
		"00:00-06:00": {0, 360},
		"23:00-06:00": {1380, 360}, // wraps midnight
		"01:30-02:45": {90, 165},
		"12:00-12:00": {720, 720}, // degenerate, parses fine
	}
	for in, want := range cases {
		s, e, err := ParseQuietHours(in)
		if err != nil {
			t.Errorf("ParseQuietHours(%q) err: %v", in, err)
			continue
		}
		if s != want[0] || e != want[1] {
			t.Errorf("ParseQuietHours(%q) = (%d, %d), want %v", in, s, e, want)
		}
	}
}

func TestParseQuietHoursRejects(t *testing.T) {
	bad := []string{
		"",               // empty (caller should not have invoked)
		"00:00",          // missing end
		"00:00-",         // missing end half
		"24:00-01:00",    // hour out of range
		"00:60-01:00",    // minute out of range
		"abc-def",        // garbage
		"00:00-01:00:00", // too many colons in second half
	}
	for _, in := range bad {
		if _, _, err := ParseQuietHours(in); err == nil {
			t.Errorf("ParseQuietHours(%q) should error", in)
		}
	}
}

func TestIsInQuietHours(t *testing.T) {
	cases := []struct {
		name           string
		start, end, at int
		want           bool
	}{
		{"morning window inside", 0, 360, 120, true},
		{"morning window outside", 0, 360, 720, false},
		{"morning boundary start inclusive", 0, 360, 0, true},
		{"morning boundary end inclusive", 0, 360, 360, true},
		{"midnight wrap inside before midnight", 1380, 360, 1410, true}, // 23:30
		{"midnight wrap inside after midnight", 1380, 360, 60, true},    // 01:00
		{"midnight wrap outside", 1380, 360, 720, false},                // 12:00
		{"midnight wrap boundary start", 1380, 360, 1380, true},         // 23:00
		{"midnight wrap boundary end", 1380, 360, 360, true},            // 06:00
		{"degenerate zero-length never inside", 600, 600, 600, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsInQuietHours(c.start, c.end, c.at); got != c.want {
				t.Errorf("IsInQuietHours(%d, %d, %d) = %v, want %v",
					c.start, c.end, c.at, got, c.want)
			}
		})
	}
}
