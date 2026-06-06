package upnp

import "testing"

func TestNextStartingIndex(t *testing.T) {
	cases := []struct {
		name                     string
		current, returned, total int
		wantNext                 int
		wantMore                 bool
	}{
		{"empty page is EOF", 0, 0, 999, 0, false},
		{"first page, more to fetch", 0, 200, 500, 200, true},
		{"middle page, more to fetch", 200, 200, 500, 400, true},
		{"last page reaches total", 400, 100, 500, 0, false},
		{"single page exact (2Go root: 4 of 4)", 0, 4, 4, 0, false},
		{"unknown total keeps going", 0, 50, 0, 50, true},
		{"unknown total, second page", 50, 50, 0, 100, true},
		{"overshoot terminates", 450, 100, 500, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, more := NextStartingIndex(tc.current, tc.returned, tc.total)
			if next != tc.wantNext || more != tc.wantMore {
				t.Fatalf("NextStartingIndex(%d,%d,%d) = (%d,%v); want (%d,%v)",
					tc.current, tc.returned, tc.total, next, more, tc.wantNext, tc.wantMore)
			}
		})
	}
}
