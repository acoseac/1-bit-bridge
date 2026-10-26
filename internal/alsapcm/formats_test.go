package alsapcm

import "testing"

// TestFormatsOfNamesOnlyRealFormats pins the refusal's list of what the
// hardware offers: a refine leaves the mask's bits past the last format the
// kernel defines set, as the request had them, and those are not formats.
// The GO bar's refusal printed "formats 31, 52, 64, 65, … 255" (the B470
// PR 2a hardware gate, 2026-10-21).
func TestFormatsOfNamesOnlyRealFormats(t *testing.T) {
	var m mask
	for _, v := range []uint32{formatS16LE, formatS32LE, formatS243LE, 31, 52} {
		m.bits[v/32] |= 1 << (v % 32)
	}
	for v := uint32(53); v < maskMax; v++ {
		m.bits[v/32] |= 1 << (v % 32)
	}
	want := "S16_LE S32_LE S24_3LE (and formats 31, 52, which this backend does not pack)"
	if got := formatsOf(m); got != want {
		t.Errorf("formatsOf:\n got %q\nwant %q", got, want)
	}
}
