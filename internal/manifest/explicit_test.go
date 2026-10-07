package manifest

import "testing"

// TestExplicitVerdict is the truth table shared with the iOS
// ExplicitContent.verdict. A row is one ExplicitSignals value. Any signal
// that says explicit wins. Title is the raw track title only.
func TestExplicitVerdict(t *testing.T) {
	rtng := func(n uint64) *uint64 {
		v := n
		return &v
	}
	cases := []struct {
		name string
		in   ExplicitSignals
		want bool
	}{
		{"rtng_1", ExplicitSignals{Rtng: rtng(1)}, true},
		{"rtng_4", ExplicitSignals{Rtng: rtng(4)}, true},
		{"rtng_2", ExplicitSignals{Rtng: rtng(2)}, false},
		{"rtng_0", ExplicitSignals{Rtng: rtng(0)}, false},
		{"rtng_absent", ExplicitSignals{}, false},
		{"advisory_1", ExplicitSignals{ItunesAdvisory: "1"}, true},
		{"advisory_4", ExplicitSignals{ItunesAdvisory: "4"}, true},
		{"advisory_true", ExplicitSignals{ItunesAdvisory: "true"}, true},
		{"advisory_YES", ExplicitSignals{ItunesAdvisory: "YES"}, true},
		{"advisory_Explicit", ExplicitSignals{ItunesAdvisory: "Explicit"}, true},
		{"advisory_e", ExplicitSignals{ItunesAdvisory: "e"}, true},
		{"advisory_2", ExplicitSignals{ItunesAdvisory: "2"}, false},
		{"advisory_0", ExplicitSignals{ItunesAdvisory: "0"}, false},
		{"advisory_false", ExplicitSignals{ItunesAdvisory: "false"}, false},
		{"advisory_no", ExplicitSignals{ItunesAdvisory: "no"}, false},
		{"advisory_clean", ExplicitSignals{ItunesAdvisory: "clean"}, false},
		{"advisory_empty", ExplicitSignals{ItunesAdvisory: ""}, false},
		{"advisory_01", ExplicitSignals{ItunesAdvisory: "01"}, false},
		{"explicit_field_1", ExplicitSignals{Explicit: "1"}, true},
		{"rtng2_plus_advisory_1", ExplicitSignals{Rtng: rtng(2), ItunesAdvisory: "1"}, true},
		{"rtng0_plus_advisory_1", ExplicitSignals{Rtng: rtng(0), ItunesAdvisory: "1"}, true},
		{"rtng1_plus_advisory_2", ExplicitSignals{Rtng: rtng(1), ItunesAdvisory: "2"}, true},
		{"advisory_2_plus_explicit_field_1", ExplicitSignals{ItunesAdvisory: "2", Explicit: "1"}, true},
		{"title_[E]", ExplicitSignals{Title: "Live [E] Cut"}, true},
		{"title_[Explicit]", ExplicitSignals{Title: "Song [Explicit]"}, true},
		{"title_[Explicit_Version]", ExplicitSignals{Title: "Song [Explicit Version]"}, true},
		{"title_(Explicit)", ExplicitSignals{Title: "Song (Explicit)"}, true},
		{"title_(Explicit_Version)", ExplicitSignals{Title: "Song (Explicit Version)"}, true},
		{"title_mid_[E]", ExplicitSignals{Title: "A [E] B"}, true},
		{"title_case", ExplicitSignals{Title: "song [eXPLICIT]"}, true},
		{"title_paren_trailing_space", ExplicitSignals{Title: "Song (Explicit) "}, true},
		{"title_paren_mid", ExplicitSignals{Title: "Song (Explicit) Live"}, false},
		{"title_[Clean]", ExplicitSignals{Title: "Song [Clean]"}, false},
		{"title_(Clean)", ExplicitSignals{Title: "Song (Clean)"}, false},
		{"title_[Clean_Version]", ExplicitSignals{Title: "Song [Clean Version]"}, false},
		{"title_(Clean_Version)", ExplicitSignals{Title: "Song (Clean Version)"}, false},
		{"title_bare_word", ExplicitSignals{Title: "The Explicit Song"}, false},
		{"title_bare_e", ExplicitSignals{Title: "e"}, false},
		{"rtng2_does_not_cancel_a_title", ExplicitSignals{Rtng: rtng(2), Title: "Song [E]"}, true},
		{"clean_advisory_does_not_cancel_a_title", ExplicitSignals{ItunesAdvisory: "2", Title: "Song [E]"}, true},
		// A UPnP row and a row from a bridge that predates the field carry
		// no advisory. The title marker is the whole verdict.
		{"upnp_and_old_bridge_title", ExplicitSignals{Title: "Song [Explicit]"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExplicitVerdict(tc.in); got != tc.want {
				t.Fatalf("ExplicitVerdict = %v, want %v", got, tc.want)
			}
		})
	}
}
