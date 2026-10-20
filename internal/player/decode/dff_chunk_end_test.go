package decode

import (
	"math"
	"testing"
)

// TestDFFChunkEndBoundsBySubtraction pins that a chunk is bounded by
// subtraction from what remains, so no input wraps past the checks. body =
// math.MaxInt64 cannot come from Open today (positions in a form are even
// and math.MaxInt64 is odd), so it is tested here: math.MaxInt64-body-1 was
// -1 there, which as a uint64 admitted any size, and body+size+pad then
// wrapped negative and passed the form's bound.
func TestDFFChunkEndBoundsBySubtraction(t *testing.T) {
	const top = math.MaxInt64
	cases := []struct {
		name        string
		body, limit int64
		size        uint64
		end         int64  // when accepted
		reason      Reason // when refused
	}{
		{"even body", 16, 100, 4, 20, ""},
		{"odd body counts its pad", 16, 100, 5, 22, ""},
		{"ends at the limit", 16, 100, 84, 100, ""},
		{"odd body whose pad ends at the limit", 16, 100, 83, 100, ""},
		{"odd body whose pad passes the limit", 16, 100, 85, 0, ReasonTruncated},
		{"past the limit", 16, 100, 86, 0, ReasonTruncated},
		{"empty at the last position", top, top, 0, top, ""},
		{"one byte at the last position", top, top, 1, 0, ReasonUnsupportedLayout},
		{"two bytes at the last position", top, top, 2, 0, ReasonUnsupportedLayout},
		{"largest size at the last position", top, top, math.MaxUint64, 0, ReasonUnsupportedLayout},
		{"one byte before the end, one byte long", top - 1, top, 1, 0, ReasonUnsupportedLayout},
		{"even size ending one byte before the end", 0, top, top - 1, top - 1, ""},
		{"odd size whose pad passes the end", 0, top, top, 0, ReasonUnsupportedLayout},
		{"largest size", 0, top, math.MaxUint64, 0, ReasonUnsupportedLayout},
	}
	for _, c := range cases {
		end, err := dffChunkEnd("TEST", c.body, c.limit, c.size)
		if c.reason == "" {
			if err != nil || end != c.end {
				t.Errorf("%s: end %d err %v, want %d", c.name, end, err, c.end)
			}
			continue
		}
		r, ok := AsRefusal(err)
		if !ok || r.Reason != c.reason {
			t.Errorf("%s: end %d err %v, want refusal %s", c.name, end, err, c.reason)
		}
	}
}
