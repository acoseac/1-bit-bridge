package dacprofile

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// lintCase is one mutation of the fixture profile and what the lint must say
// of the result.
type lintCase struct {
	name   string
	mutate func(p *Profile)
	words  string
}

// runLintCases lints the fixture profile under each mutation and requires a
// problem naming it and saying the case's words; the fixture itself must lint
// clean, so each refusal is the mutation's.
func runLintCases(t *testing.T, cases []lintCase) {
	t.Helper()
	if problems := fixture(nil, fixtureProfile("fixture.dac", "00d1")).Lint(); len(problems) > 0 {
		t.Fatalf("the fixture profile lints as %v", problems)
	}
	for _, tc := range cases {
		p := fixtureProfile("fixture.dac", "00d1")
		tc.mutate(&p)
		if problems := fixture(nil, p).Lint(); !lintSays(problems, "fixture.dac", tc.words) {
			t.Errorf("%s: the lint says %v, want a problem saying %q", tc.name, problems, tc.words)
		}
	}
}

// dropLine removes the provenance line for claim.
func dropLine(p *Profile, claim string) {
	var kept []string
	for _, l := range p.Provenance {
		if !strings.HasPrefix(l, claim+": ") {
			kept = append(kept, l)
		}
	}
	p.Provenance = kept
}

// TestEveryClaimNamesItsProvenance: every claim of the embedded database's
// profiles (its identity, which only a profile with a match makes, native,
// DoP, timing, each caveat and its iOS projection) has a line in its
// provenance naming its source and its date or version, and every shared
// reference ID says why, with its source; and the lint refuses a claim with
// no line, a line naming no date or version (or a date that is none), an
// email, a line for a claim the profile does not make or for none at all, a
// source of a kind the claim cannot have or that does not say where it is,
// and an ios line that is not the profile's own registry row.
func TestEveryClaimNamesItsProvenance(t *testing.T) {
	db := mustDefault(t)
	if problems := db.Lint(); len(problems) > 0 {
		t.Fatalf("the embedded database lints as %v", problems)
	}
	if len(db.Profiles) == 0 || len(db.SharedReferenceIDs) == 0 {
		t.Fatalf("the embedded database holds %d profiles and %d shared reference IDs", len(db.Profiles), len(db.SharedReferenceIDs))
	}
	for _, p := range db.Profiles {
		var claims []string
		if len(p.Match) > 0 {
			claims = append(claims, "identity")
		}
		if p.IOS != nil {
			claims = append(claims, "ios")
		}
		if p.Native != nil {
			claims = append(claims, "native")
		}
		if p.DoP != nil {
			claims = append(claims, "dop")
		}
		if p.Timing != nil {
			claims = append(claims, "timing")
		}
		for _, c := range p.Caveats {
			claims = append(claims, "caveat "+c.Code)
		}
		for _, c := range claims {
			found := false
			for _, l := range p.Provenance {
				found = found || strings.HasPrefix(l, c+": ")
			}
			if !found {
				t.Errorf("%s: its %s claim has no provenance line", p.ID, c)
			}
		}
	}

	runLintCases(t, []lintCase{
		{"no identity line", func(p *Profile) { dropLine(p, "identity") }, `"identity"`},
		{"no native line", func(p *Profile) { dropLine(p, "native") }, `"native"`},
		{"no dop line", func(p *Profile) { dropLine(p, "dop") }, `"dop"`},
		{"no timing line", func(p *Profile) { dropLine(p, "timing") }, `"timing"`},
		{"no caveat line", func(p *Profile) { dropLine(p, "caveat transition_clicks") }, `"caveat transition_clicks"`},
		{"a line with no date or version", func(p *Profile) { p.Provenance[0] = "identity: an owner capture" }, "date or version"},
		{"a date that is no date", func(p *Profile) { p.Provenance[0] = "identity: an owner capture, 2026-13-45" }, "date or version"},
		{"an iOS commit too short to name one", func(p *Profile) { p.Provenance[2] = "dop: iOS profile fixture.dac (iOS cd12)" }, "date or version"},
		{"an email", func(p *Profile) { p.Provenance[0] = "identity: sent by someone@example.org, 2026-11-04" }, "email"},
		{"a line for a claim the profile does not make", func(p *Profile) { p.Native = nil }, "does not make"},
		{"a line for a caveat the profile does not carry", func(p *Profile) { p.Caveats = []Caveat{} }, "does not make"},
		{"a line naming no claim", func(p *Profile) {
			p.Provenance = append(p.Provenance, "colour: owner capture, Linux, 2026-11-04")
		}, "names no claim"},
		{"a native source of another kind", func(p *Profile) { p.Native.Source.Kind = SourceIOSRegistry }, "source"},
		{"a native source of no kind", func(p *Profile) { p.Native.Source.Kind = "" }, "source"},
		{"a kernel quirk naming no tag", func(p *Profile) {
			p.Native.MaxBitRate = 0
			p.Native.Source = Source{Kind: SourceKernelQuirk, Ref: "sound/usb/quirks.c"}
		}, "kernel tag"},
		{"a kernel quirk with a rate it never states", func(p *Profile) {
			p.Native.Source = Source{Kind: SourceKernelQuirk, Ref: "sound/usb/quirks.c at v6.12"}
		}, "rate"},
		{"a capture with no date", func(p *Profile) { p.Native.Source.Ref = "owner capture, Linux" }, "date"},
		{"a capture with no rate", func(p *Profile) { p.Native.MaxBitRate = 0 }, "rate"},
		{"a DoP source that is not the iOS registry", func(p *Profile) {
			p.DoP.Source = Source{Kind: SourceCapture, Ref: "owner capture, Linux, 2026-11-04"}
		}, "DACProfile.swift"},
		{"an iOS source naming no row", func(p *Profile) { p.DoP.Source.Ref = "DACProfile.swift at iOS cd125088" }, "DACProfile.swift"},
		{"an iOS source naming no commit", func(p *Profile) { p.DoP.Source.Ref = "DACProfile.swift fixture.dac" }, "DACProfile.swift"},
	})
	runDBCases(t, []dbCase{
		{"no ios line", func(db *Database) { dropLine(&db.Profiles[0], "ios") }, `"ios"`},
		{"an identity line on a profile with no match", func(db *Database) {
			db.Profiles[0].Provenance = append(db.Profiles[0].Provenance, "identity: a placeholder, 2026-11-17")
		}, "does not make"},
		{"an ios line naming another row", func(db *Database) {
			db.Profiles[0].Provenance[1] = "ios: DACProfile.swift fixture.bravo at iOS 6a1241eb"
		}, "own registry row"},
		{"an ios line with more than its row", func(db *Database) {
			db.Profiles[0].Provenance[1] += ", the row's notes"
		}, "own registry row"},
	})

	for _, tc := range []struct {
		name  string
		why   string
		words string
	}{
		{"no why", "", "why"},
		{"a why with no source", "a reference firmware many products ship", "date or version"},
		{"a why with an email", "a reference firmware, as someone@example.org said on 2026-11-04", "email"},
	} {
		shared := []SharedID{{VendorID: "ffff", ProductID: "00e1", Why: tc.why}}
		if problems := fixture(shared).Lint(); !lintSays(problems, "ffff:00e1", tc.words) {
			t.Errorf("a shared reference ID with %s lints as %v", tc.name, problems)
		}
	}
}

// TestAHeardRateIsAHearingNeverALock: a heard DoP rate is on-device
// listening evidence (the design's sections 1.1 and 2.1), so the lint asks a
// DoP claim with a heard rate for a dop clause in evidence's form that lists
// it under heard ("heard DSD128: <source>"); a clause that says the rate was
// negotiated, confirmed on hardware or reached a DoP lock, which section 1.1
// says hears nothing back, starts with no marker and is evidence of nothing,
// and a hearing of another rate is not this one's. A claim with no heard
// rate, a refusal alone, needs none.
func TestAHeardRateIsAHearingNeverALock(t *testing.T) {
	dopLine := func(text string) func(p *Profile) {
		return func(p *Profile) {
			p.Provenance[2] = "dop: " + text + "; refused DSD256: iOS profile fixture.dac (iOS cd125088)"
		}
	}
	runLintCases(t, []lintCase{
		{"confirmed on hardware", dopLine("DSD128 hardware-confirmed"), "heard"},
		{"a DoP lock", dopLine("DSD128 reached a DoP lock"), "heard"},
		{"a negotiation", dopLine("DSD128's carrier negotiated exactly"), "heard"},
		{"a hearing of another rate", dopLine("heard DSD64: iOS profile fixture.dac"), "DSD128"},
		{"another rate heard and this one's lock, a clause each", dopLine("heard DSD64; DSD128 reached a DoP lock"), "DSD128"},
	})
	p := fixtureProfile("fixture.dac", "00d1")
	p.DoP.HeardBitRates = nil
	dopLine("DSD128 a DoP lock and no hearing")(&p)
	if problems := fixture(nil, p).Lint(); len(problems) > 0 {
		t.Errorf("a refusal with no heard rate lints as %v", problems)
	}
}

// TestARefusedRateNamesItsRefusal: a refused rate is one a device refused,
// or played as silence (section 2.1), so the lint asks a DoP claim, for each
// rate it lists as refused, for a dop clause in evidence's form that lists
// it under refused or silence at: a refusal of another rate is not its
// evidence.
func TestARefusedRateNamesItsRefusal(t *testing.T) {
	dopLine := func(text string) func(p *Profile) {
		return func(p *Profile) {
			p.Provenance[2] = "dop: heard DSD128: iOS profile fixture.dac (iOS cd125088); " + text
		}
	}
	runLintCases(t, []lintCase{
		{"a clause that names no refusal", dopLine("DSD256 not"), "refused"},
		{"a refusal of another rate", dopLine("refused DSD512: iOS profile fixture.dac"), "DSD256"},
		{"another rate refused and this one's lock, a clause each", dopLine("refused DSD512; DSD256 reached a DoP lock"), "DSD256"},
	})
	p := fixtureProfile("fixture.dac", "00d1")
	dopLine("silence at DSD256: iOS profile fixture.dac")(&p)
	if problems := fixture(nil, p).Lint(); len(problems) > 0 {
		t.Errorf("a refusal heard as silence lints as %v", problems)
	}
}

// TestNoEmbeddedDoPLineReadsAsAHearing: no embedded profile's dop lines read
// as a hearing of a rate it neither heard nor failed (the Mytek's DSD64 and
// DSD128, beside a DoP lock that hears nothing), so each such rate added to
// its heardBitRates, with no listening behind it, is refused by the lint.
func TestNoEmbeddedDoPLineReadsAsAHearing(t *testing.T) {
	db, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	tried := 0
	for _, p := range db.Profiles {
		if p.DoP == nil {
			continue
		}
		for _, r := range dsdRates {
			if slices.Contains(p.DoP.HeardBitRates, r) || slices.Contains(p.DoP.RefusedBitRates, r) {
				continue
			}
			d := *p.DoP
			d.HeardBitRates = append(slices.Clone(d.HeardBitRates), r)
			slices.Sort(d.HeardBitRates)
			q := p
			q.DoP = &d
			want := fmt.Sprintf("credits DSD%d as heard", r/44100)
			if problems := fixture(db.SharedReferenceIDs, q).Lint(); !lintSays(problems, q.ID, want) {
				t.Errorf("%s given a heard rate of %d lints as %v, want a problem saying %q", q.ID, r, problems, want)
			}
			tried++
		}
	}
	if tried == 0 {
		t.Fatal("no embedded profile holds DoP evidence without a heard rate, so nothing was checked")
	}
}

// TestADopClauseCreditsOnlyTheRatesItsFormLists: a dop line's evidence is
// written in one form, and only that form counts, since a lint cannot read
// prose (round 3 read negation, and the review of #1402 broke it from both
// sides). The line's clauses are the text after "dop: ", split on semicolons
// and trimmed. A clause that starts with a marker (heard, refused or silence
// at, in any case, a word boundary after it) is the marker, its rates
// separated by commas, then a colon or the clause's end; it credits those
// rates alone, as heard, or as refused (refused and silence at alike), and
// what follows the colon is read by nothing, so it may negate anything. A
// marker clause in any other shape is a problem that shows the form; a clause
// that starts with no marker is free text and evidence of nothing; and a rate
// both heard and refused in a profile's provenance is a contradiction, while
// refused and silence at are one result, a refusal, which two units may give
// in two ways. Each case
// says what the clauses credit by the rates the profile claims and the
// problem the lint gives, or none: every wording round 2 and round 3 tried,
// the reviewers' lists that broke round 3's reading, and the form's own.
func TestADopClauseCreditsOnlyTheRatesItsFormLists(t *testing.T) {
	const dsd64, dsd128, dsd256 = 2822400, 5644800, 11289600
	const (
		form       = "is not in the form"
		heard128   = "credits DSD128 as heard"
		heard64    = "credits DSD64 as heard"
		refused256 = "refused rate DSD256 has no dop clause"
		// heardAndRefused is a rate a profile's clauses credit as heard and
		// as refused (refused or silence at), two results for one rate.
		heardAndRefused = "both heard and refused"
	)
	for _, tc := range []struct {
		name    string
		text    string // the dop line's clauses, after "dop: "
		heard   []int
		refused []int
		want    string // what a problem says, or "" for a profile that lints clean
	}{
		// The form, which credits.
		{"heard, one rate, at the clause's end", "heard DSD128", []int{dsd128}, nil, ""},
		{"heard, its rates by commas, a colon and free text", "heard DSD64, DSD128: clean with PEQ off", []int{dsd64, dsd128}, nil, ""},
		{"a marker in capitals", "Heard DSD128", []int{dsd128}, nil, ""},
		{"refused", "refused DSD256: the unit would not negotiate its carrier", nil, []int{dsd256}, ""},
		{"silence at, in capitals", "SILENCE AT DSD256: no audio output", nil, []int{dsd256}, ""},
		{"spaces around a comma and before the colon", "heard DSD64 ,DSD128 : clean", []int{dsd64, dsd128}, nil, ""},
		{"free text may negate anything after the colon", "refused DSD256: no supported altsetting", nil, []int{dsd256}, ""},
		{"one rate under one marker twice", "heard DSD128; heard DSD128: a second unit", []int{dsd128}, nil, ""},
		{"free text beside the form proves nothing and is allowed", "refused DSD256: would not negotiate; DSD128's carrier reached a DoP lock", nil, []int{dsd256}, ""},
		// The reviewers' "rejected though true", in the form, and as written.
		{"in the form: refused, no supported altsetting", "refused DSD256: no supported 705.6 kHz altsetting", nil, []int{dsd256}, ""},
		{"in the form: heard, no clicks or dropouts", "heard DSD128: no clicks or dropouts", []int{dsd128}, nil, ""},
		{"in the form: silence at, no audio output", "silence at DSD256: no audio output", nil, []int{dsd256}, ""},
		{"as written: DSD256 refused: no supported altsetting", "DSD256 refused: no supported 705.6 kHz altsetting", nil, []int{dsd256}, refused256},
		{"as written: DSD128 heard: no clicks", "DSD128 heard: no clicks or dropouts", []int{dsd128}, nil, heard128},
		{"as written: DSD256 played as silence", "DSD256 played as silence: no audio output", nil, []int{dsd256}, refused256},
		// The reviewers' "accepted though false".
		{"heard at DSD64 but not at DSD128", "heard at DSD64 but not at DSD128", []int{dsd128}, nil, form},
		{"the unit refused DSD512, not DSD256", "the unit refused DSD512, not DSD256", nil, []int{dsd256}, refused256},
		{`DSD128 was a DoP lock, not "heard"`, `DSD128 was a DoP lock, not "heard"`, []int{dsd128}, nil, heard128},
		{"heard at DSD128, the unit did not play the DSD256 file as silence", "heard at DSD128, the unit did not play the DSD256 file as silence", []int{dsd128}, []int{dsd256}, form},
		{"DSD128 locked yes, heard no", "DSD128 locked yes, heard no", []int{dsd128}, nil, heard128},
		// A marker clause in any other shape.
		{"heard DSD64 but not at DSD128", "heard DSD64 but not at DSD128", []int{dsd64}, nil, form},
		{"heard DSD128 not", "heard DSD128 not", []int{dsd128}, nil, form},
		{"heard DSD64 and DSD128", "heard DSD64 and DSD128", []int{dsd128}, nil, form},
		{"heard at DSD128", "heard at DSD128", []int{dsd128}, nil, form},
		{"a marker with no rates", "heard", []int{dsd128}, nil, form},
		{"a marker with no rates before its colon", "refused: the unit would not negotiate", nil, []int{dsd256}, form},
		{"silence at with no rates", "silence at", nil, []int{dsd256}, form},
		{"a comma with no rate after it", "heard DSD128,", []int{dsd128}, nil, form},
		{"a rate in lower case", "heard dsd128", []int{dsd128}, nil, form},
		{"a rate past DSD512", "heard DSD1024: a rate no claim names", []int{dsd128}, nil, "is not a DSD rate"},
		// Clauses that start with no marker: free text, evidence of nothing.
		{"a marker run into its rate", "heardDSD128", []int{dsd128}, nil, heard128},
		{"refusal is no marker", "refusal of DSD256", nil, []int{dsd256}, refused256},
		{"a DoP lock", "DSD128's carrier reached a DoP lock", []int{dsd128}, nil, heard128},
		// A rate heard and refused; refused and silence at are one result.
		{"heard and silence at one rate", "heard DSD256; silence at DSD256", []int{dsd256}, nil, heardAndRefused},
		{"heard and refused at one rate", "heard DSD128; refused DSD128", []int{dsd128}, nil, heardAndRefused},
		{"refused and silence at one rate, one result", "refused DSD256; silence at DSD256", nil, []int{dsd256}, ""},
		{"two units, one refused it and one played it silent",
			"refused DSD256: unit 1 would not negotiate (iOS cd125088); silence at DSD256: unit 2 played it silent (iOS cd125088)", nil, []int{dsd256}, ""},
		// Round 3's wordings: the negations it read, which credit nothing now.
		{"round 3: not heard", "DSD128 was not heard, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 3: never heard", "DSD128 never heard, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 3: wasn't heard", "DSD128 wasn't heard, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 3: wasn’t heard, a typographic apostrophe", "DSD128 wasn’t heard, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 3: cannot be heard", "DSD128 cannot be heard, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 3: without being heard", "DSD128 played without being heard, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 3: no rate heard", "no DSD128 heard, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 3: heard: no", "DSD128 heard: no, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 3: heard nothing", "DSD128 heard nothing, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 3: not refused", "heard at DSD128, DSD256 was not refused", []int{dsd128}, []int{dsd256}, form},
		{"round 3: never refused", "heard at DSD128, DSD256 never refused", []int{dsd128}, []int{dsd256}, form},
		{"round 3: didn't refuse", "heard at DSD128, the unit didn't refuse DSD256", []int{dsd128}, []int{dsd256}, form},
		{"round 3: cannot refuse", "heard at DSD128, the unit cannot refuse DSD256", []int{dsd128}, []int{dsd256}, form},
		{"round 3: no refusal", "heard at DSD128, no refusal at DSD256", []int{dsd128}, []int{dsd256}, form},
		{"round 3: without refusal", "heard at DSD128, DSD256 played without refusal", []int{dsd128}, []int{dsd256}, form},
		{"round 3: neither refused nor silent", "heard at DSD128, DSD256 neither refused nor silent", []int{dsd128}, []int{dsd256}, form},
		{"round 3: refused: none", "heard at DSD128, DSD256 refused: none", []int{dsd128}, []int{dsd256}, form},
		{"round 3: not played as silence", "heard at DSD128, DSD256 not played as silence", []int{dsd128}, []int{dsd256}, form},
		{"round 3: did not play it as silence", "heard at DSD128, DSD256 did not play it as silence", []int{dsd128}, []int{dsd256}, form},
		{"round 3: never silent", "heard at DSD128, DSD256 never silent", []int{dsd128}, []int{dsd256}, form},
		{"round 3: wasn't silent", "heard at DSD128, DSD256 wasn't silent", []int{dsd128}, []int{dsd256}, form},
		{"round 3: without silence", "heard at DSD128, DSD256 played without silence", []int{dsd128}, []int{dsd256}, form},
		{"round 3: no silence", "heard at DSD128, no silence at DSD256", []int{dsd128}, []int{dsd256}, form},
		// Round 3's pins, a negation of another verb, which it took as evidence.
		{"round 3: which it would not negotiate", "heard at DSD128; the unit refused DSD256's 705.6 kHz carrier, which it would not negotiate", []int{dsd128}, []int{dsd256}, form},
		{"round 3: would not negotiate and was refused", "heard at DSD128, DSD256 would not negotiate and was refused", []int{dsd128}, []int{dsd256}, form},
		{"round 3: never at DSD256, which the unit refused", "heard at DSD128 with no click, never at DSD256, which the unit refused", []int{dsd128}, []int{dsd256}, form},
		{"round 3: heard no click", "heard no click at DSD128, DSD256 played as silence, not as noise", []int{dsd128}, []int{dsd256}, form},
		{"round 3: was heard, not merely locked", "DSD128 was heard, not merely locked; DSD256 refused", []int{dsd128}, []int{dsd256}, heard128},
		// Round 2's wordings.
		{"round 2: hardware-confirmed", "DSD128 hardware-confirmed, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 2: reached DoP lock", "DSD128 reached DoP lock, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 2: negotiated exactly", "DSD128's carrier negotiated exactly, DSD256 refused", []int{dsd128}, nil, heard128},
		{"round 2: heard at another rate", "heard at DSD64, DSD256 refused", []int{dsd128}, nil, form},
		{"round 2: another rate heard, this one's lock", "heard at DSD64; DSD128 reached a DoP lock; DSD256 refused", []int{dsd128}, nil, form},
		{"round 2: carrier refused on a device", "DSD256's carrier refused on a device, DSD128 a DoP lock and no hearing", nil, []int{dsd256}, refused256},
		{"round 2: DSD256 not", "heard at DSD128, DSD256 not", []int{dsd128}, []int{dsd256}, form},
		{"round 2: another rate refused", "heard at DSD128, DSD512 refused", []int{dsd128}, []int{dsd256}, form},
		{"round 2: another rate refused, this one's lock", "heard at DSD128; DSD512 refused; DSD256 reached a DoP lock", []int{dsd128}, []int{dsd256}, form},
		{"round 2: played as silence", "heard at DSD128, DSD256 played as silence", []int{dsd128}, []int{dsd256}, form},
	} {
		p := fixtureProfile("fixture.dac", "00d1")
		p.DoP.HeardBitRates, p.DoP.RefusedBitRates = tc.heard, append([]int{}, tc.refused...)
		p.Provenance[2] = "dop: " + tc.text + "; iOS profile fixture.dac (iOS cd125088)"
		problems := fixture(nil, p).Lint()
		switch {
		case tc.want == "" && len(problems) > 0:
			t.Errorf("%s: %q lints as %v, want none", tc.name, tc.text, problems)
		case tc.want != "" && !lintSays(problems, "fixture.dac", tc.want):
			t.Errorf("%s: %q lints as %v, want a problem saying %q", tc.name, tc.text, problems, tc.want)
		}
	}

	// A rate heard and refused across two dop lines of one profile.
	p := fixtureProfile("fixture.dac", "00d1")
	p.Provenance = append(p.Provenance, "dop: silence at DSD128: a second unit (iOS cd125088)")
	if problems := fixture(nil, p).Lint(); !lintSays(problems, "fixture.dac", heardAndRefused) {
		t.Errorf("heard and silence at for DSD128 on two dop lines lint as %v", problems)
	}
	// The form's problem shows the form to write.
	p = fixtureProfile("fixture.dac", "00d1")
	p.Provenance[2] = "dop: heard at DSD128: iOS profile fixture.dac (iOS cd125088); refused DSD256"
	if problems := fixture(nil, p).Lint(); !lintSays(problems, "fixture.dac", `"heard DSD64, DSD128: <source>"`) {
		t.Errorf("a clause out of the form lints as %v, which shows no form to write", problems)
	}
	if problems := fixture(nil, p).Lint(); !lintSays(problems, "fixture.dac", `write "heard DSD128: <source>"`) {
		t.Errorf("a heard rate no clause credits lints as %v, which shows no form to write", problems)
	}
}

// TestAnAnalogueControlNeedsItsProvenance: an analogue control a profile
// lists for the DoP gain check to pass over is named by its interface, name
// and index and carries a provenance line of its own, dated; the lint refuses
// one with no line, a line naming no date, an interface ALSA does not have,
// no name or a name longer than ALSA's 44 bytes, a negative index, and the
// same control twice; and every one the embedded database lists has its line.
func TestAnAnalogueControlNeedsItsProvenance(t *testing.T) {
	master := func() AnalogControl {
		return AnalogControl{ControlID: ControlID{Interface: "MIXER", Name: "Master Playback Volume", Index: 0},
			Provenance: "owner capture, Linux, 2026-11-04: an analogue stage after the DoP decoder"}
	}
	with := func(c AnalogControl) func(p *Profile) {
		return func(p *Profile) { p.DoP.AnalogVolumeControls = []AnalogControl{c} }
	}
	p := fixtureProfile("fixture.dac", "00d1")
	with(master())(&p)
	if problems := fixture(nil, p).Lint(); len(problems) > 0 {
		t.Fatalf("a profile with an analogue control and its provenance lints as %v", problems)
	}
	edit := func(f func(c *AnalogControl)) func(p *Profile) {
		c := master()
		f(&c)
		return with(c)
	}
	runLintCases(t, []lintCase{
		{"no provenance", edit(func(c *AnalogControl) { c.Provenance = "" }), "provenance"},
		{"a provenance line with no date", edit(func(c *AnalogControl) { c.Provenance = "an owner capture" }), "date or version"},
		{"an email", edit(func(c *AnalogControl) { c.Provenance = "someone@example.org, 2026-11-04" }), "email"},
		{"an interface ALSA does not have", edit(func(c *AnalogControl) { c.Interface = "MIXR" }), "interface"},
		{"an interface in another case", edit(func(c *AnalogControl) { c.Interface = "mixer" }), "interface"},
		{"no name", edit(func(c *AnalogControl) { c.Name = "" }), "name"},
		{"a name longer than ALSA's", edit(func(c *AnalogControl) { c.Name = strings.Repeat("V", 45) }), "name"},
		{"a negative index", edit(func(c *AnalogControl) { c.Index = -1 }), "index"},
		{"the same control twice", func(p *Profile) { p.DoP.AnalogVolumeControls = []AnalogControl{master(), master()} }, "twice"},
	})
	for _, q := range mustDefault(t).Profiles {
		if q.DoP == nil {
			continue
		}
		for _, c := range q.DoP.AnalogVolumeControls {
			if c.Provenance == "" {
				t.Errorf("%s lists %s/%s/%d with no provenance", q.ID, c.Interface, c.Name, c.Index)
			}
		}
	}
}

// TestAUnityAtRawMaxEntryNeedsItsEvidenceAndFirmware: a level control with
// no dB scale whose raw maximum is unity is listed with that maximum, the
// firmware it holds on, and evidence of one of two kinds, the maker's
// document by its title and section, or a measurement by its method, the
// firmware it was taken on (one the entry lists) and its dated record; the
// lint refuses an entry with neither kind, with no firmware, or with a field
// either kind needs missing.
func TestAUnityAtRawMaxEntryNeedsItsEvidenceAndFirmware(t *testing.T) {
	document := func() RawMaxControl {
		return RawMaxControl{ControlID: ControlID{Interface: "MIXER", Name: "PCM Playback Volume", Index: 0}, RawMax: 127,
			BCDDevice: []string{"0197"}, Evidence: Evidence{Kind: "document", Title: "Fixture DAC user manual", Section: "USB volume"}}
	}
	measured := func() RawMaxControl {
		c := document()
		c.BCDDevice = []string{"0197", "0198"}
		c.Evidence = Evidence{Kind: "measured", Method: "digital-capture", BCDDevice: "0198", Ref: "owner capture, Linux, 2026-11-04"}
		return c
	}
	for name, c := range map[string]RawMaxControl{"a document": document(), "a measurement": measured()} {
		p := fixtureProfile("fixture.dac", "00d1")
		p.DoP.UnityAtRawMax = []RawMaxControl{c}
		if problems := fixture(nil, p).Lint(); len(problems) > 0 {
			t.Errorf("an entry evidenced by %s lints as %v", name, problems)
		}
	}
	edit := func(base func() RawMaxControl, f func(c *RawMaxControl)) func(p *Profile) {
		return func(p *Profile) {
			c := base()
			f(&c)
			p.DoP.UnityAtRawMax = []RawMaxControl{c}
		}
	}
	runLintCases(t, []lintCase{
		{"no evidence", edit(document, func(c *RawMaxControl) { c.Evidence = Evidence{} }), "evidence"},
		{"evidence of another kind", edit(document, func(c *RawMaxControl) { c.Evidence.Kind = "hearsay" }), "evidence"},
		{"a document with no title", edit(document, func(c *RawMaxControl) { c.Evidence.Title = "" }), "title"},
		{"a document with no section", edit(document, func(c *RawMaxControl) { c.Evidence.Section = "" }), "section"},
		{"no firmware", edit(document, func(c *RawMaxControl) { c.BCDDevice = nil }), "bcdDevice"},
		{"an empty firmware list", edit(document, func(c *RawMaxControl) { c.BCDDevice = []string{} }), "bcdDevice"},
		{"a firmware that is not four hexadecimal digits", edit(document, func(c *RawMaxControl) { c.BCDDevice = []string{"1.97"} }), "bcdDevice"},
		{"a measurement naming no firmware", edit(measured, func(c *RawMaxControl) { c.Evidence.BCDDevice = "" }), "bcdDevice"},
		{"a measurement on a firmware the entry does not list", edit(measured, func(c *RawMaxControl) { c.Evidence.BCDDevice = "0199" }), "bcdDevice"},
		{"a measurement with no method", edit(measured, func(c *RawMaxControl) { c.Evidence.Method = "" }), "method"},
		{"a measurement by another method", edit(measured, func(c *RawMaxControl) { c.Evidence.Method = "it sounded fine" }), "method"},
		{"a measurement with no dated record", edit(measured, func(c *RawMaxControl) { c.Evidence.Ref = "owner capture" }), "date"},
		{"a raw maximum of zero", edit(document, func(c *RawMaxControl) { c.RawMax = 0 }), "rawMax"},
		{"an interface ALSA does not have", edit(document, func(c *RawMaxControl) { c.Interface = "" }), "interface"},
		{"the same control twice", func(p *Profile) { p.DoP.UnityAtRawMax = []RawMaxControl{document(), document()} }, "twice"},
	})
}

// TestTheCaveatCodesAreAClosedList: a caveat's code is one of the closed
// list, dop_noise_in_hidden_mode concerns DoP, every caveat carries its note,
// which never words DSD on the player as verified, decoded, bit-exact,
// bit-perfect, a lock, a seal or qualified (the design's section 1.3), and no
// caveat is carried twice.
func TestTheCaveatCodesAreAClosedList(t *testing.T) {
	if got := strings.Join(CaveatCodes(), " "); got != "dop_noise_in_hidden_mode mode_switch_caps_rates transition_clicks" {
		t.Errorf("CaveatCodes() = %s", got)
	}
	caveat := func(c Caveat) func(p *Profile) {
		return func(p *Profile) {
			p.Caveats = []Caveat{c}
			dropLine(p, "caveat transition_clicks")
			p.Provenance = append(p.Provenance, "caveat "+c.Code+": iOS profile fixture.dac (iOS cd125088)")
		}
	}
	runLintCases(t, []lintCase{
		{"a code off the list", caveat(Caveat{Code: "sounds_odd", Note: "It sounds odd."}), "code"},
		{"the hidden mode on native", caveat(Caveat{Code: CaveatDoPNoiseInHiddenMode, Transport: "native", Note: "With PEQ on it plays noise."}), "transport"},
		{"a transport the player does not have", caveat(Caveat{Code: CaveatTransitionClicks, Transport: "pcm", Note: "It clicks."}), "transport"},
		{"no note", caveat(Caveat{Code: CaveatTransitionClicks, Transport: "dop"}), "note"},
		{"a note saying verified", caveat(Caveat{Code: CaveatTransitionClicks, Note: "DoP is verified clean but it clicks."}), "verified"},
		{"a note saying lock", caveat(Caveat{Code: CaveatTransitionClicks, Note: "It clicks before DoP Lock."}), "lock"},
		{"a note saying bit-exact", caveat(Caveat{Code: CaveatTransitionClicks, Note: "It clicks, though bit-exact."}), "bit-exact"},
		{"the same caveat twice", func(p *Profile) { p.Caveats = append(p.Caveats, p.Caveats[0]) }, "twice"},
	})
	// A word that only holds one of them is not one: "clock", "unlocked" and "blocked" pass.
	p := fixtureProfile("fixture.dac", "00d1")
	p.Caveats[0].Note = "Its clock is unlocked and blocked at a DoP change."
	if problems := fixture(nil, p).Lint(); len(problems) > 0 {
		t.Errorf("a note with clock, unlocked and blocked lints as %v", problems)
	}
}

// TestTheSchemaRefusesWhatItDoesNotDescribe: Parse refuses a field the
// schema does not have (schema 1's heardMaxBitRate, an iOS key among them),
// anything after the object and another schema version, schema 1 included;
// and the lint refuses what no reading or no claim can be: an ID not in
// usbid.Hex16's form, an interface past a byte, a product string the sysfs
// reader never returns, an empty firmware list, an alternative given twice,
// a profile with no alternative and no iOS projection, no name, or an ID not
// in the dotted form, two profiles of one ID, a format ALSA's DSD does not
// have, a rate not DSD64 to DSD512, heard rates out of order or twice, a DoP
// claim that claims nothing, a timing that gives none or is past five
// seconds, a shared reference ID listed twice or not in Hex16's form; a
// profile with no match that claims what only a match reaches (native,
// timing, an analogue or raw-maximum control); a DoP source that is another
// row's, or a listening or a maker's document with no date; and an iOS name
// that is not printable ASCII, blank, not trimmed or given twice.
func TestTheSchemaRefusesWhatItDoesNotDescribe(t *testing.T) {
	valid, err := json.Marshal(iosPair())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(valid); err != nil {
		t.Fatalf("Parse refuses the iOS pair: %v", err)
	}
	for _, tc := range []struct{ name, data string }{
		{"a field the schema does not have", `{"schemaVersion": 2, "sharedReferenceIds": [], "profiles": [], "overrides": []}`},
		{"something after the object", `{"schemaVersion": 2, "sharedReferenceIds": [], "profiles": []} {}`},
		{"another schema version", `{"schemaVersion": 1, "sharedReferenceIds": [], "profiles": []}`},
		{"no schema version", `{"sharedReferenceIds": [], "profiles": []}`},
		{"schema 1's heard maximum", strings.Replace(string(valid), `"heardBitRates":`, `"heardMaxBitRate":2822400,"heardBitRates":`, 1)},
		{"a key the iOS projection does not have", strings.Replace(string(valid), `"maker":`, `"colour":"red","maker":`, 1)},
	} {
		if tc.data == string(valid) {
			t.Fatalf("%s: the mutation missed", tc.name)
		}
		if _, err := Parse([]byte(tc.data)); err == nil {
			t.Errorf("Parse takes %s", tc.name)
		}
	}
	if _, err := Parse([]byte(`{"schemaVersion": 2, "sharedReferenceIds": [], "iosGenericNames": [], "profiles": []}`)); err != nil {
		t.Errorf("Parse refuses an empty database: %v", err)
	}
	alt := func(f func(m *Match)) func(p *Profile) {
		return func(p *Profile) { f(&p.Match[0]) }
	}
	runLintCases(t, []lintCase{
		{"a vendor ID in capitals", alt(func(m *Match) { m.VendorID = "FFFF" }), "vendorId"},
		{"a vendor ID after 0x", alt(func(m *Match) { m.VendorID = "0xffff" }), "vendorId"},
		{"a product ID of three digits", alt(func(m *Match) { m.ProductID = "0d1" }), "productId"},
		{"an interface past a byte", alt(func(m *Match) { m.Interface = ptr(256) }), "interface"},
		{"a negative interface", alt(func(m *Match) { m.Interface = ptr(-1) }), "interface"},
		{"an empty product string", alt(func(m *Match) { m.Product = ptr("") }), "product"},
		{"a product string with a trailing space", alt(func(m *Match) { m.Product = ptr("Fixture DAC ") }), "product"},
		{"a product string with a line feed", alt(func(m *Match) { m.Product = ptr("Fixture\nDAC") }), "product"},
		{"a product string that is not UTF-8", alt(func(m *Match) { m.Product = ptr("Fixture \xff") }), "product"},
		{"an empty firmware list", alt(func(m *Match) { m.BCDDevice = []string{} }), "bcdDevice"},
		{"a firmware written with a dot", alt(func(m *Match) { m.BCDDevice = []string{"1.97"} }), "bcdDevice"},
		{"a firmware listed twice", alt(func(m *Match) { m.BCDDevice = []string{"0197", "0197"} }), "bcdDevice"},
		{"an alternative given twice", func(p *Profile) { p.Match = append(p.Match, p.Match[0]) }, "twice"},
		{"no alternative", func(p *Profile) { p.Match = nil }, "match"},
		{"no name", func(p *Profile) { p.Name = "" }, "name"},
		{"a name with a trailing space", func(p *Profile) { p.Name = "Fixture DAC " }, "name"},
		{"a format ALSA's DSD does not have", func(p *Profile) { p.Native.Formats = []string{"S32_LE"} }, "format"},
		{"no format", func(p *Profile) { p.Native.Formats = nil }, "format"},
		{"a format listed twice", func(p *Profile) { p.Native.Formats = []string{"DSD_U32_BE", "DSD_U32_BE"} }, "format"},
		{"a native rate that is no DSD rate", func(p *Profile) { p.Native.MaxBitRate = 2822401 }, "rate"},
		{"a DoP rate past DSD512", func(p *Profile) { p.DoP.HeardBitRates = []int{45158400} }, "rate"},
		{"a heard rate that is no DSD rate", func(p *Profile) { p.DoP.HeardBitRates = []int{176400} }, "is not a DSD rate"},
		{"heard rates out of order", func(p *Profile) { p.DoP.HeardBitRates = []int{5644800, 2822400} }, "ascending"},
		{"a heard rate listed twice", func(p *Profile) { p.DoP.HeardBitRates = []int{5644800, 5644800} }, "ascending"},
		{"a refused rate that is no DSD rate", func(p *Profile) { p.DoP.RefusedBitRates = []int{176400} }, "rate"},
		{"a refused rate listed twice", func(p *Profile) { p.DoP.RefusedBitRates = []int{11289600, 11289600} }, "twice"},
		{"a DoP claim that claims nothing", func(p *Profile) {
			p.DoP.HeardBitRates, p.DoP.RefusedBitRates = nil, []int{}
		}, "claims nothing"},
		{"a timing that gives none", func(p *Profile) { p.Timing = &Timing{} }, "timing"},
		{"a prefill past five seconds", func(p *Profile) { p.Timing.DSDPrefillMs = 5001 }, "timing"},
		{"a negative settle", func(p *Profile) { p.Timing.PCMEntryMs = -1 }, "timing"},
		{"a DoP source that is a listening with no date", func(p *Profile) {
			p.DoP.Source = Source{Kind: SourceListening, Ref: "a listening on a phone"}
		}, "names its date"},
		{"a DoP source that is a maker's document with no date", func(p *Profile) {
			p.DoP.Source = Source{Kind: SourceMaker, Ref: "the maker's specification"}
		}, "names its date"},
	})
	p := fixtureProfile("fixture.dac", "00d1")
	p.DoP.Source = Source{Kind: SourceMaker, Ref: "the maker's specification, 2026-11-01"}
	if problems := fixture(nil, p).Lint(); len(problems) > 0 {
		t.Errorf("a DoP claim sourced to a dated maker's document lints as %v", problems)
	}
	only := func(f func(p *Profile)) func(db *Database) { return func(db *Database) { f(&db.Profiles[0]) } }
	keys := func(f func(x *IOS)) func(db *Database) { return only(func(p *Profile) { f(p.IOS) }) }
	runDBCases(t, []dbCase{
		{"no match and no iOS projection", only(func(p *Profile) { p.IOS = nil; dropLine(p, "ios") }), "no match alternative"},
		{"no match and a native claim", only(func(p *Profile) {
			q := fixtureProfile("fixture.alpha", "00d1")
			p.Native = q.Native
			p.Provenance = append(p.Provenance, q.Provenance[1])
		}), "has no native claim"},
		{"no match and a timing", only(func(p *Profile) {
			p.Timing = &Timing{DSDPrefillMs: 700}
			p.Provenance = append(p.Provenance, "timing: a resync delay, 0.7 s (iOS 6a1241eb)")
		}), "has no timing"},
		{"no match and an analogue control", only(func(p *Profile) {
			p.DoP.AnalogVolumeControls = []AnalogControl{{ControlID: ControlID{Interface: "MIXER", Name: "Master", Index: 0}, Provenance: "a test, 2026-11-17"}}
		}), "has no analogue control"},
		{"no match and a raw-maximum control", only(func(p *Profile) {
			p.DoP.UnityAtRawMax = []RawMaxControl{{ControlID: ControlID{Interface: "MIXER", Name: "PCM", Index: 0}, RawMax: 127,
				BCDDevice: []string{"0101"}, Evidence: Evidence{Kind: "document", Title: "Manual", Section: "4.2"}}}
		}), "has no raw-maximum control"},
		{"a DoP source naming another row", only(func(p *Profile) {
			p.DoP.Source.Ref = "DACProfile.swift fixture.bravo at iOS 6a1241eb"
		}), "own row of the iOS registry"},
		{"another name scope", keys(func(x *IOS) { x.NameScope = "brand" }), "nameScope"},
		{"a port name holding a NUL", keys(func(x *IOS) { x.PortNames = []string{"FIXTURE\x00Alpha"} }), "not printable ASCII"},
		{"a token holding a line feed", keys(func(x *IOS) { x.NameTokens = []string{"Alpha\n"} }), "not printable ASCII"},
		{"a token holding a letter past ASCII", keys(func(x *IOS) { x.NameTokens = []string{"Alph\xc3\xa9"} }), "not printable ASCII"},
		{"an excluded token holding a carriage return", keys(func(x *IOS) { x.ExcludedTokens = []string{"Li\rte"} }), "not printable ASCII"},
		{"a legacy pattern holding a tab", keys(func(x *IOS) { x.LegacyPatterns = []string{"Al\tpha"} }), "not printable ASCII"},
		{"a generic name holding a carriage return", func(db *Database) { db.IOSGenericNames[0].Name = "Head\rphones" }, "not printable ASCII"},
		{"a blank port name", keys(func(x *IOS) { x.PortNames = []string{"  "} }), "is blank"},
		{"an empty token", keys(func(x *IOS) { x.NameTokens = []string{"Alpha", ""} }), "is blank"},
		{"a token with white space around it", keys(func(x *IOS) { x.NameTokens = []string{" Alpha"} }), "white space around it"},
		{"a generic name with white space around it", func(db *Database) { db.IOSGenericNames[0].Name = "Headphones " }, "white space around it"},
		{"a token twice, folded", keys(func(x *IOS) { x.NameTokens = []string{"Alpha", "ALPHA"} }), "listed twice"},
		{"an excluded token twice", keys(func(x *IOS) { x.ExcludedTokens = []string{"Lite", "lite"} }), "listed twice"},
		{"a port name twice, trimmed and folded", keys(func(x *IOS) { x.PortNames = []string{"FIXTURE Alpha ", "fixture alpha"} }), "listed twice"},
		{"a generic name twice, folded", func(db *Database) {
			db.IOSGenericNames = append(db.IOSGenericNames, GenericName{Name: "HEADPHONES", Why: "the same name (iOS 6a1241eb)"})
		}, "listed twice"},
		{"a generic name with no why", func(db *Database) { db.IOSGenericNames[0].Why = "" }, "says no why"},
		{"an excluded token inside the row's own token", keys(func(x *IOS) { x.ExcludedTokens = []string{"lph"} }), "inside the row's own token"},
		{"a maker with white space around it", keys(func(x *IOS) { x.Maker = "Fixture " }), "maker"},
		{"a volume topology that is no case name", keys(func(x *IOS) { x.VolumeTopology = "DAC owns gain" }), "case name"},
		{"a PCM depth iOS does not have", keys(func(x *IOS) { x.PCM.BitDepth = 20 }), "PCM ceiling"},
		{"an iOS time past five seconds", keys(func(x *IOS) { x.Timing.DSDResyncMs = ptr(5001) }), "iOS time"},
	})
	for _, tc := range []struct{ name, id string }{
		{"an ID with capitals", "Fixture.DAC"},
		{"an ID with no dot", "fixture"},
		{"an ID with a space", "fixture .dac"},
	} {
		p := fixtureProfile(tc.id, "00d1")
		if problems := fixture(nil, p).Lint(); len(problems) == 0 {
			t.Errorf("%s (%q) lints clean", tc.name, tc.id)
		}
	}
	if problems := fixture(nil, fixtureProfile("fixture.dac", "00d1"), fixtureProfile("fixture.dac", "00d2")).Lint(); !lintSays(problems, "fixture.dac", "twice") {
		t.Errorf("two profiles of one ID lint as %v", problems)
	}
	for _, tc := range []struct {
		name   string
		shared []SharedID
	}{
		{"a shared ID listed twice", append(sharedIFi(), sharedIFi()...)},
		{"a shared ID in capitals", []SharedID{{VendorID: "20B1", ProductID: "0010", Why: "XMOS's reference product ID (owner capture, Linux, 2026-11-04)"}}},
	} {
		if problems := fixture(tc.shared).Lint(); len(problems) == 0 {
			t.Errorf("%s lints clean", tc.name)
		}
	}
}

// TestEachDoPRateIsCreditedRateByRate: a DoP claim's rates and its dop
// clauses agree rate by rate, both ways: a heard rate no heard clause lists
// (DSD64 beside a clause for DSD128 alone, which a heard maximum let pass),
// a heard clause for a rate heardBitRates does not list, a refused rate no
// failure clause lists and a failure clause for a rate refusedBitRates does
// not list are each a problem naming the rate; and noise at and no frames at
// credit a failure as refused and silence at do.
func TestEachDoPRateIsCreditedRateByRate(t *testing.T) {
	dop := func(heard, refused []int, text string) func(p *Profile) {
		return func(p *Profile) {
			p.DoP.HeardBitRates, p.DoP.RefusedBitRates = heard, refused
			p.Provenance[2] = "dop: " + text
		}
	}
	runLintCases(t, []lintCase{
		{"DSD64 heard beside a clause for DSD128 alone", dop([]int{2822400, 5644800}, []int{11289600},
			"heard DSD128: a listening (iOS cd125088); refused DSD256: a refusal (iOS cd125088)"), "credits DSD64 as heard"},
		{"a heard clause for a rate the list does not hold", dop([]int{5644800}, []int{11289600},
			"heard DSD64, DSD128: a listening (iOS cd125088); refused DSD256: a refusal (iOS cd125088)"), "credits DSD64 as heard, and heardBitRates"},
		{"a refused rate no clause credits", dop([]int{5644800}, []int{11289600, 22579200},
			"heard DSD128: a listening (iOS cd125088); refused DSD256: a refusal (iOS cd125088)"), "refused rate DSD512 has no dop clause"},
		{"a failure clause for a rate the list does not hold", dop([]int{5644800}, []int{11289600},
			"heard DSD128: a listening (iOS cd125088); refused DSD256, DSD512: a refusal (iOS cd125088)"), "credits DSD512 as failed"},
	})
	for _, marker := range []string{"noise at", "no frames at"} {
		p := fixtureProfile("fixture.dac", "00d1")
		p.Provenance[2] = "dop: heard DSD128: a listening (iOS cd125088); " + marker + " DSD256: a refusal (iOS cd125088)"
		if problems := fixture(nil, p).Lint(); len(problems) > 0 {
			t.Errorf("%s DSD256 lints as %v", marker, problems)
		}
	}
}

// TestAFailureBelowAHeardRateIsKept: a rate that failed below a heard one is
// evidence the schema keeps (DSD64 refused beside DSD128 heard, where a heard
// maximum read DSD64 as heard), and lints clean; one rate heard and failed is
// a contradiction under each failure marker.
func TestAFailureBelowAHeardRateIsKept(t *testing.T) {
	p := fixtureProfile("fixture.dac", "00d1")
	p.DoP.HeardBitRates, p.DoP.RefusedBitRates = []int{5644800}, []int{2822400}
	p.Provenance[2] = "dop: heard DSD128: a listening (iOS cd125088); refused DSD64: a refusal (iOS cd125088)"
	if problems := fixture(nil, p).Lint(); len(problems) > 0 {
		t.Errorf("a refusal below a heard rate lints as %v", problems)
	}
	var cases []lintCase
	for _, marker := range []string{"refused", "silence at", "noise at", "no frames at"} {
		cases = append(cases, lintCase{"heard and " + marker + " at DSD128", func(p *Profile) {
			p.DoP.RefusedBitRates = []int{5644800, 11289600}
			p.Provenance[2] = "dop: heard DSD128: a listening (iOS cd125088); " + marker + " DSD128, DSD256: a refusal (iOS cd125088)"
		}, "DSD128 is both heard and refused"})
	}
	runLintCases(t, cases)
}

// TestAStatementIsOneOfThree: a DoP claim's statement is the maker's DoP
// figure, a DSD figure that says nothing of DoP, or no DSD at all, at most
// one of them, and its clause (stated DoP to, stated DSD to, stated no DSD)
// says the same: a clause for another statement or for none, a statement no
// clause credits, a statement clause in no form, both set, no DSD with a
// rate or beside a heard rate, and a kind off the two are each a problem.
func TestAStatementIsOneOfThree(t *testing.T) {
	stated := func(rate int, without *Statement, clause string) func(p *Profile) {
		return func(p *Profile) {
			p.DoP.StatedMaxBitRate, p.DoP.StatedWithoutDoP = rate, without
			if clause != "" {
				p.Provenance[2] += "; " + clause
			}
		}
	}
	dsd256, none := &Statement{Kind: StatementDSD, MaxBitRate: 11289600}, &Statement{Kind: StatementNone}
	for _, tc := range []struct {
		name   string
		mutate func(p *Profile)
	}{
		{"DoP stated to DSD256", stated(11289600, nil, "stated DoP to DSD256: the maker's specification (iOS 6a1241eb)")},
		{"a DSD figure to DSD256", stated(0, dsd256, "Stated DSD to DSD256: the maker's specification (iOS 6a1241eb)")},
		{"no DSD", func(p *Profile) {
			stated(0, none, "stated no DSD: the maker's specification (iOS 6a1241eb)")(p)
			p.DoP.HeardBitRates = nil
			p.Provenance[2] = strings.Replace(p.Provenance[2], "heard DSD128: iOS profile fixture.dac (iOS cd125088); ", "", 1)
		}},
	} {
		p := fixtureProfile("fixture.dac", "00d1")
		tc.mutate(&p)
		if problems := fixture(nil, p).Lint(); len(problems) > 0 {
			t.Errorf("%s lints as %v", tc.name, problems)
		}
	}
	runLintCases(t, []lintCase{
		{"a clause for another rate", stated(11289600, nil, "stated DoP to DSD128: the maker (iOS 6a1241eb)"), `"stated DoP to DSD128"`},
		{"a clause for another statement", stated(11289600, nil, "stated DSD to DSD256: the maker (iOS 6a1241eb)"), `"stated DSD to DSD256"`},
		{"a statement no clause credits", stated(0, dsd256, ""), "statement has no dop clause"},
		{"a clause and no statement", stated(0, nil, "stated no DSD: the maker (iOS 6a1241eb)"), `"stated no DSD"`},
		{"a statement clause in no form", stated(11289600, nil, "stated DoP up to DSD256: the maker (iOS 6a1241eb)"), "statement not in the form"},
		{"both set", stated(11289600, dsd256, "stated DoP to DSD256: the maker (iOS 6a1241eb)"), "at most one is set"},
		{"no DSD with a rate", stated(0, &Statement{Kind: StatementNone, MaxBitRate: 2822400}, "stated no DSD: the maker (iOS 6a1241eb)"), "statedWithoutDoP is"},
		{"no DSD beside a heard rate", stated(0, none, "stated no DSD: the maker (iOS 6a1241eb)"), "beside a heard rate"},
		{"a kind off the two", stated(0, &Statement{Kind: "dop", MaxBitRate: 11289600}, "stated DSD to DSD256: the maker (iOS 6a1241eb)"), "statedWithoutDoP is"},
	})
}

// TestAnIOSRowsClausesNameTheirSource: in a profile with an iOS projection
// every marker clause's text after its colon opens with its source's head,
// one of iOS's kinds in words and a date where it has one, a submission's
// and a field log's always, then nothing, ", " or " (", and one clause
// credits each rate and the statement: a clause with no colon, no head, a
// kind iOS does not have, a date that is none, a head run into its text, a
// submission's or a field log's head with no date, and a rate or the
// statement credited twice, are each a problem; the same clauses in a
// profile without one, where no head is read, lint clean.
func TestAnIOSRowsClausesNameTheirSource(t *testing.T) {
	const kept = "; refused DSD256: field log 2026-06-21; stated DSD to DSD256: maker (the row's note, iOS 6a1241eb)"
	heads := []struct{ name, heard, words string }{
		{"no colon", "heard DSD64", "source's head"},
		{"no head", "heard DSD64: a listening on 2026-10-04", "source's head"},
		{"a kind iOS does not have", "heard DSD64: forum post 2026-10-04", "source's head"},
		{"a date that is none", "heard DSD64: submission 2026-13-45", "source's head"},
		{"a head run into its text", "heard DSD64: submissions, 2026-10-04", "source's head"},
		{"a submission with no date", "heard DSD64: submission (one unit)", "heads a submission with no date"},
		{"a field log with no date", "heard DSD64: field log, its row's log", "heads a field log with no date"},
		{"a rate credited twice", "heard DSD64: submission 2026-10-04; heard DSD64: field log 2026-10-21", "credited by 2 dop clauses"},
	}
	var cases []dbCase
	for _, h := range heads {
		cases = append(cases, dbCase{h.name, func(db *Database) { db.Profiles[0].Provenance[0] = "dop: " + h.heard + kept }, h.words})
	}
	cases = append(cases, dbCase{"the statement credited twice", func(db *Database) {
		db.Profiles[0].Provenance[0] += "; stated DSD to DSD256: retailer (iOS 6a1241eb)"
	}, "statement is credited by 2 dop clauses"})
	runDBCases(t, cases)
	for _, h := range heads {
		p := fixtureProfile("fixture.dac", "00d1")
		p.DoP.HeardBitRates = []int{2822400}
		p.Provenance[2] = "dop: " + h.heard + "; refused DSD256: a refusal (iOS cd125088)"
		if problems := fixture(nil, p).Lint(); len(problems) > 0 {
			t.Errorf("%s, in a profile without an iOS projection, lints as %v", h.name, problems)
		}
	}
}

// TestTheIOSNameKeysLint: iOS PR 1's lint over the name keys, folded, across
// two profiles only: a token under four characters (KA3); a token inside a
// generic name or one inside a token; a token inside another profile's token
// or port name, or one of those inside it (Hugo beside Hugo 2), but never a
// profile's own token inside its own port name (qx13 in FIIO QX13 ); a port
// name two profiles give, trimmed and folded (FIIO KA17 and FIIO KA17), or
// that is a generic name; a family row with no sibling note or one saying a
// word never said of DSD, a model row with one, and a family row with a heard
// or a failed rate of its own, its other list empty; and an order given twice
// or skipped. An excluded token that holds its own row's token (JM98MAX 2
// beside JM98MAX) lints clean, keeping a sibling's longer name off the row.
func TestTheIOSNameKeysLint(t *testing.T) {
	first := func(f func(x *IOS)) func(db *Database) { return func(db *Database) { f(db.Profiles[0].IOS) } }
	second := func(f func(x *IOS)) func(db *Database) { return func(db *Database) { f(db.Profiles[1].IOS) } }
	tokens := func(a, b string) func(db *Database) {
		return func(db *Database) {
			db.Profiles[0].IOS.NameTokens, db.Profiles[1].IOS.NameTokens = []string{a}, []string{b}
		}
	}
	runDBCases(t, []dbCase{
		{"a token of three characters", first(func(x *IOS) { x.NameTokens = []string{"KA3"} }), "shorter than 4"},
		{"a token inside a generic name", first(func(x *IOS) { x.NameTokens = []string{"amanero"} }), "lie one inside the other"},
		{"a generic name inside a token", first(func(x *IOS) { x.NameTokens = []string{"Combo384 Amanero DAC"} }), "lie one inside the other"},
		{"a token inside another profile's token", tokens("Hugo", "Hugo 2"), `"Hugo" lies inside profile fixture.bravo's token "Hugo 2"`},
		{"another profile's token inside a token", tokens("Hugo 2", "Hugo"), `"Hugo" lies inside profile fixture.alpha's token "Hugo 2"`},
		{"a token inside another profile's port name", second(func(x *IOS) { x.NameTokens = []string{"Alpha"} }), "port name"},
		{"another profile's port name inside a token", second(func(x *IOS) { x.NameTokens = []string{"Bravo", "the fixture alpha DAC"} }), "port name"},
		{"a port name two profiles give", func(db *Database) {
			db.Profiles[0].IOS.PortNames, db.Profiles[1].IOS.PortNames = []string{"FIIO KA17 "}, []string{"FIIO KA17"}
		}, `both give the port name "FIIO KA17"`},
		{"a port name that is a generic name", first(func(x *IOS) { x.PortNames = []string{"headphones "} }), "is the generic name"},
		{"a family row with no sibling note", first(func(x *IOS) { x.NameScope = "family" }), "sibling note says"},
		{"a sibling note saying a word never said", first(func(x *IOS) {
			x.NameScope, x.SiblingNote = "family", ptr("A sibling locked at DSD128 (a submission, 2026-09-23).")
		}), `says "locked"`},
		{"a model row with a sibling note", first(func(x *IOS) { x.SiblingNote = ptr("a sibling's result") }), "no sibling note"},
		{"a family row with a heard rate of its own", func(db *Database) {
			pr := &db.Profiles[0]
			pr.IOS.NameScope, pr.IOS.SiblingNote = "family", ptr("Siblings under this name, not this row's own evidence: DSD64 heard over DoP on one unit (a submission, 2026-09-23).")
			if pr.DoP == nil {
				pr.DoP = &DoP{}
			}
			pr.DoP.HeardBitRates, pr.DoP.RefusedBitRates = []int{2822400}, nil
		}, "no heard or failed rate of its own"},
		{"a family row with a failed rate of its own", func(db *Database) {
			pr := &db.Profiles[0]
			pr.IOS.NameScope, pr.IOS.SiblingNote = "family", ptr("Siblings under this name, not this row's own evidence: DSD64 heard over DoP on one unit (a submission, 2026-09-23).")
			if pr.DoP == nil {
				pr.DoP = &DoP{}
			}
			pr.DoP.HeardBitRates, pr.DoP.RefusedBitRates = nil, []int{22579200}
		}, "no heard or failed rate of its own"},
		{"an order given twice", second(func(x *IOS) { x.Order = 1 }), "hold order 1"},
		{"an order skipped", second(func(x *IOS) { x.Order = 3 }), "hold order 2"},
	})
	db := iosPair()
	db.Profiles[0].IOS.PortNames, db.Profiles[0].IOS.NameTokens = []string{"FIIO QX13 "}, []string{"qx13"}
	if problems := db.Lint(); len(problems) > 0 {
		t.Errorf("a token inside its own profile's port name lints as %v", problems)
	}
	db = iosPair()
	db.Profiles[0].IOS.ExcludedTokens, db.IOSKnownSiblings = []string{"Alpha 2", "Alpha2"}, []string{"FIXTURE Alpha 2", "FIXTURE ALPHA2"}
	if problems := db.Lint(); len(problems) > 0 {
		t.Errorf("an excluded token that holds its own row's token lints as %v", problems)
	}
}

// TestNoKnownSiblingReachesAModelRow: no known sibling (iosKnownSiblings,
// iOS PR 1's list) reaches a model row through the strict matcher, by either
// of its keys, so a catalog edit that lets one in fails at the lint, before
// any export or baseline: the E30 II Lite's name given to the E30 II as a
// port name, which the matcher takes before any token and no excluded token
// rules out, and the E30 II's excluded token Lite dropped, which lets the
// Lite's names reach it by its tokens, are each refused.
func TestNoKnownSiblingReachesAModelRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(x *IOS)
		words  string
	}{
		{"a sibling's name as the model's port name", func(x *IOS) { x.PortNames = append(x.PortNames, "TOPPING E30 II Lite") },
			`"TOPPING E30 II Lite" reaches this model row through the strict matcher, by its port name`},
		{"the excluded token that keeps the sibling off dropped", func(x *IOS) { x.ExcludedTokens = nil },
			`"TOPPING E30II Lite" reaches this model row through the strict matcher, by its token`},
	} {
		db := mustDefault(t)
		i := slices.IndexFunc(db.Profiles, func(p Profile) bool { return p.ID == "topping.e30ii" })
		if i < 0 || !slices.Contains(db.IOSKnownSiblings, "TOPPING E30 II Lite") {
			t.Fatal("the embedded database holds no E30 II row, or not its Lite among the known siblings")
		}
		tc.mutate(db.Profiles[i].IOS)
		if problems := db.Lint(); !lintSays(problems, "profile topping.e30ii", tc.words) {
			t.Errorf("%s: the lint says %v, want a problem saying %q", tc.name, problems, tc.words)
		}
	}
}

// TestTheLegacyCeilingAgreesWithTheEvidence: an iOS row's legacy ceiling
// agrees with its DoP evidence as iOS PR 1 checks it: no DSD stated with a
// ceiling, a ceiling of 0 with no statement of no DSD, a heard rate above the
// ceiling and a failed rate at or below it are each a problem.
func TestTheLegacyCeilingAgreesWithTheEvidence(t *testing.T) {
	ceiling := func(rate int, f func(d *DoP, line *string)) func(db *Database) {
		return func(db *Database) {
			p := &db.Profiles[0]
			p.IOS.LegacyCeilingBitRate = rate
			if f != nil {
				f(p.DoP, &p.Provenance[0])
			}
		}
	}
	none := func(d *DoP, line *string) {
		d.HeardBitRates, d.StatedWithoutDoP = nil, &Statement{Kind: StatementNone}
		*line = "dop: refused DSD256: field log 2026-06-21; stated no DSD: maker (iOS 6a1241eb)"
	}
	runDBCases(t, []dbCase{
		{"no DSD stated with a ceiling", ceiling(5644800, none), "0 exactly where the maker states no DSD"},
		{"a ceiling of 0 with a DSD figure", ceiling(0, nil), "0 exactly where the maker states no DSD"},
		{"a heard rate above the ceiling", ceiling(2822400, func(d *DoP, line *string) {
			d.HeardBitRates = []int{2822400, 5644800}
			*line = strings.Replace(*line, "heard DSD64:", "heard DSD64, DSD128:", 1)
		}), "heard DSD128 lies above the legacy ceiling"},
		{"a failed rate at the ceiling", ceiling(11289600, nil), "failed DSD256 lies at or below the legacy ceiling"},
	})
	db := iosPair()
	ceiling(0, none)(db)
	if problems := db.Lint(); len(problems) > 0 {
		t.Errorf("no DSD stated under a ceiling of 0 lints as %v", problems)
	}
}
