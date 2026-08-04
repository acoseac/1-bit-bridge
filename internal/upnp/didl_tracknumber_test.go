package upnp

import "testing"

// TestAtoiTrackNumber pins the "N/M" tolerance for
// `upnp:originalTrackNumber`. The spec types the element xsd:int, but real
// servers emit the ID3 "5/12" form, and atoiOr's zero default silently strips
// the track number when they do.
func TestAtoiTrackNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"5", 5},
		{"5/12", 5},
		{" 5 / 12 ", 5},
		{"05/12", 5},
		{"12", 12},
		{"0/2", 0},    // explicit zero — callers guard on > 0
		{"/12", 0},    // no leading number: default
		{"", 0},       // absent
		{"abc", 0},    // garbage
		{"x/12", 0},   // garbage before the slash
		{"5/", 5},     // trailing slash, no total
		{"-3/12", -3}, /* preserved; callers guard on > 0 */
	}
	for _, tc := range cases {
		if got := atoiTrackNumber(tc.in, 0); got != tc.want {
			t.Errorf("atoiTrackNumber(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestAtoiOrStillRejectsSlash is the guard against folding atoiTrackNumber back
// into atoiOr. atoiOr's other call sites are NumberReturned / TotalMatches /
// ChildCount / sampleFrequency / bitsPerSample / nrAudioChannels, where a '/'
// is genuinely malformed and must NOT be silently truncated to its first field.
func TestAtoiOrStillRejectsSlash(t *testing.T) {
	if got := atoiOr("5/12", -1); got != -1 {
		t.Errorf("atoiOr(%q) = %d, want the -1 default — a slash is malformed for the numeric fields", "5/12", got)
	}
	if got := atoiOr("44100", 0); got != 44100 {
		t.Errorf("atoiOr(%q) = %d, want 44100", "44100", got)
	}
}
