package formatgate

import "testing"

// TestOfReadsTheExtensionWithoutRegardToCase: .wv is WavPack and .ape APE in
// any case; a correction file, a longer or a shorter extension, a folder named
// like one and a path with no extension are none.
func TestOfReadsTheExtensionWithoutRegardToCase(t *testing.T) {
	for p, want := range map[string]Format{
		"A/B/01.wv":          WavPack,
		"A/B/01.WV":          WavPack,
		"A/B/01.Wv":          WavPack,
		"01.wv":              WavPack,
		"A/.wv":              WavPack,
		"A/B/01.ape":         APE,
		"A/B/01.APE":         APE,
		"A/B/01.wvc":         None,
		"A/B/01.WVC":         None,
		"A/B/01.xwv":         None,
		"A/B/01wv":           None,
		"A/B/01.apex":        None,
		"A/B/01.flac":        None,
		"A/x.wv/01.flac":     None,
		"A/B/01.wv.flac":     None,
		"A/B/01.flac.wv":     WavPack,
		"":                   None,
		"wv":                 None,
		"A/B/01.dsf":         None,
		"x.iso/st/01.dff":    None,
		"Music/Album/01.ape": APE,
	} {
		if got := Of(p); got != want {
			t.Errorf("Of(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestParseReadsTheHeaderAsTheProtocolSays: comma-separated tokens, without
// regard to case or white space, unknown tokens ignored, several lines added
// up, and no header or no known token the zero set.
func TestParseReadsTheHeaderAsTheProtocolSays(t *testing.T) {
	for _, c := range []struct {
		values []string
		want   Set
	}{
		{nil, Set{}},
		{[]string{""}, Set{}},
		{[]string{"wavpack, ape"}, All()},
		{[]string{"WavPack,APE"}, All()},
		{[]string{" ape "}, SetOf(APE)},
		{[]string{"wavpack"}, SetOf(WavPack)},
		{[]string{"flac, opus, wavpack2"}, Set{}},
		{[]string{"opus,wavpack"}, SetOf(WavPack)},
		{[]string{"wavpack", "ape"}, All()},
		{[]string{",,wavpack,,"}, SetOf(WavPack)},
		{[]string{"wav pack"}, Set{}},
		{[]string{"wavpack;ape"}, Set{}},
	} {
		if got := Parse(c.values); got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.values, got, c.want)
		}
	}
}

// TestASetTakesWhatItDeclares: the zero set takes every path but the gated
// ones, each declaration its own format alone, and All takes everything.
func TestASetTakesWhatItDeclares(t *testing.T) {
	paths := []string{"a.flac", "a.wv", "a.ape", "a.wvc"}
	for _, c := range []struct {
		set  Set
		want []bool
	}{
		{Set{}, []bool{true, false, false, true}},
		{SetOf(WavPack), []bool{true, true, false, true}},
		{SetOf(APE), []bool{true, false, true, true}},
		{All(), []bool{true, true, true, true}},
	} {
		for i, p := range paths {
			if got := c.set.Takes(p); got != c.want[i] {
				t.Errorf("%q.Takes(%q) = %v, want %v", c.set.Key(), p, got, c.want[i])
			}
		}
	}
}

// TestKeyRoundTrips: every set's key parses back to it, and nothing else
// parses.
func TestKeyRoundTrips(t *testing.T) {
	for _, s := range []Set{{}, SetOf(WavPack), SetOf(APE), All()} {
		got, ok := ParseKey(s.Key())
		if !ok || got != s {
			t.Errorf("ParseKey(%q) = %+v, %v; want %+v", s.Key(), got, ok, s)
		}
	}
	if All().Key() != "ape,wavpack" {
		t.Errorf("All().Key() = %q", All().Key())
	}
	for _, bad := range []string{"wavpack,ape", "WAVPACK", "ape,", ",ape", "flac", "ape,ape", " ape"} {
		if s, ok := ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) = %+v, true; want refused", bad, s)
		}
	}
}

// TestEveryFormatHasItsToken: a format's token is the one Parse reads for it.
func TestEveryFormatHasItsToken(t *testing.T) {
	for _, f := range []Format{WavPack, APE} {
		if !Parse([]string{f.Token()}).Declares(f) {
			t.Errorf("%v's token %q does not declare it", f, f.Token())
		}
	}
	if None.Token() != "" || !(Set{}).Declares(None) {
		t.Error("None has a token, or a set does not declare it")
	}
}
