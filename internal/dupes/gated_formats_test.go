package dupes

// Suppression and the client format gate (backlog B489). dupe_suppressed is
// one flag for every client, while a WavPack or Monkey's Audio row reaches
// only a client that declares its format, so a gated copy never wins over a
// copy every client is served, and gated copies of two formats are never
// suppressed against each other.

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
)

// planViews are the four views a client can declare: nothing (every app before
// B489), WavPack alone, Monkey's Audio alone, and both.
var planViews = []formatgate.Set{
	{}, formatgate.SetOf(formatgate.WavPack), formatgate.SetOf(formatgate.APE), formatgate.All(),
}

// codecRow is flacRow with another codec.
func codecRow(path, codec string, rate, bits int, size int64) Row {
	r := flacRow(path, rate, bits, 200, size)
	r.Codec = codec
	return r
}

// dsdRow is a DSD copy: a DSF, or a WavPack DSD file.
func dsdRow(path, codec string, rate int, size int64) Row {
	r := codecRow(path, codec, rate, 1, size)
	r.IsDSD = true
	return r
}

// servedTo is what a client with view sees of a group once plan is applied:
// the members it is served, sorted.
func servedTo(view formatgate.Set, members []Row, suppressed []string) []string {
	var out []string
	for _, m := range members {
		if view.Takes(m.Path) && !slices.Contains(suppressed, m.Path) {
			out = append(out, m.Path)
		}
	}
	slices.Sort(out)
	return out
}

func TestAGatedCopyNeverWinsOverACopyEveryClientIsServed(t *testing.T) {
	cases := []struct {
		name    string
		members []Row
		want    []string // suppressed under FilterHighestQuality
	}{
		{
			// The case the rule exists for: the WavPack copy has the better
			// geometry, and without the gate's term it wins and an app that
			// declares nothing is left no copy of the track.
			name: "a hi-res WavPack beside a CD FLAC",
			members: []Row{
				codecRow("A/x.flac", "FLAC", 44100, 16, 30_000_000),
				codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
			},
			want: []string{"B/x.wv"},
		},
		{
			name: "a hi-res Monkey's Audio copy beside a CD WAV",
			members: []Row{
				codecRow("A/x.wav", "WAV", 44100, 16, 50_000_000),
				codecRow("B/x.APE", "APE", 96000, 24, 80_000_000),
			},
			want: []string{"B/x.APE"},
		},
		{
			// DSD is its own domain, and within it the same rule holds: a
			// DSF is served to every client, a WavPack DSD file is not.
			name: "a DSD128 WavPack file beside a DSD64 DSF",
			members: []Row{
				dsdRow("A/x.dsf", "DSF", 2822400, 100),
				dsdRow("B/x.wv", "WAVPACK-DSD", 5644800, 200),
			},
			want: []string{"B/x.wv"},
		},
		{
			name: "both gated formats beside a FLAC",
			members: []Row{
				codecRow("A/x.flac", "FLAC", 44100, 16, 30_000_000),
				codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
				codecRow("C/x.ape", "APE", 192000, 24, 120_000_000),
			},
			want: []string{"B/x.wv", "C/x.ape"},
		},
		{
			// No copy every client is served: one unit per gated format, so
			// a .wv and an .ape are never suppressed against each other, and
			// a client that declares one of them alone keeps its copy.
			name: "a WavPack and a Monkey's Audio copy alone",
			members: []Row{
				codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
				codecRow("C/x.ape", "APE", 44100, 16, 40_000_000),
			},
			want: nil,
		},
		{
			// Within one gated format the order is the one it always was.
			name: "two WavPack copies and a Monkey's Audio one",
			members: []Row{
				codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
				codecRow("D/x.wv", "WAVPACK", 44100, 16, 30_000_000),
				codecRow("C/x.ape", "APE", 44100, 16, 40_000_000),
			},
			want: []string{"D/x.wv"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if tier := classify(c.members); tier != TierDifferentFormat {
				t.Fatalf("fixture should be different-format, got %s", tier)
			}
			got := planFor(t, c.members, FilterHighestQuality)
			if !slices.Equal(got, c.want) {
				t.Fatalf("suppressed %v, want %v", got, c.want)
			}
			// The property itself, in every view: a client served any copy
			// of the track before the plan is served one after it.
			for _, view := range planViews {
				if len(servedTo(view, c.members, nil)) > 0 && len(servedTo(view, c.members, got)) == 0 {
					t.Errorf("view %q: served a copy before suppression and none after (suppressed %v)",
						view.Key(), got)
				}
			}
			// Under same-format a different-format group is served whole,
			// gated copies included, as it always was.
			if got := planFor(t, c.members, FilterSameFormat); len(got) != 0 {
				t.Errorf("same-format suppressed %v, want nothing", got)
			}
		})
	}
}

// TestAGatedCopyIsSuppressedAgainstItsOwnFormatAsAnyCopyIs: two WavPack
// copies of one geometry are a same-format group and keep one winner, the
// larger, exactly as two FLAC copies do.
func TestAGatedCopyIsSuppressedAgainstItsOwnFormatAsAnyCopyIs(t *testing.T) {
	members := []Row{
		codecRow("A/x.wv", "WAVPACK", 44100, 16, 30_000_000),
		codecRow("B/x.wv", "WAVPACK", 44100, 16, 31_000_000),
	}
	if tier := classify(members); tier != TierSameFormat {
		t.Fatalf("fixture should be same-format, got %s", tier)
	}
	for _, mode := range []FilterMode{FilterSameFormat, FilterHighestQuality} {
		if got := planFor(t, members, mode); !slices.Equal(got, []string{"A/x.wv"}) {
			t.Errorf("mode %s: suppressed %v, want the smaller copy", mode, got)
		}
	}
}

// TestALossyHybridWavPackCopyIsLossy: WAVPACK-LOSSY is in the ranking's lossy
// set (the manifest side's lockstep test holds the set to IsLossyCodec), so a
// lossless WavPack copy outranks it within the format.
func TestALossyHybridWavPackCopyIsLossy(t *testing.T) {
	lossless := codecRow("A/x.wv", "WAVPACK", 44100, 16, 10)
	lossy := codecRow("B/x.wv", "WAVPACK-LOSSY", 44100, 16, 20)
	if !outranks(lossless, lossy) || outranks(lossy, lossless) {
		t.Fatal("a lossless WavPack copy must outrank a lossy hybrid one, whatever their sizes")
	}
	// And the gate's term comes before it: an MP3 beside a lossless WavPack
	// copy is the one every client is served.
	mp3 := codecRow("C/x.mp3", "MP3", 44100, 16, 5)
	if !outranks(mp3, lossless) {
		t.Fatal("a copy every client is served must outrank a gated one, lossless or not")
	}
}

// gone is r whose file the last scan did not find (Row.Missing).
func gone(r Row) Row {
	r.Missing = true
	return r
}

// foundTo is what a client with view can play of a group once plan is
// applied: the members it is served whose file the last scan found, sorted.
func foundTo(view formatgate.Set, members []Row, suppressed []string) []string {
	var out []string
	for _, m := range members {
		if view.Takes(m.Path) && !m.Missing && !slices.Contains(suppressed, m.Path) {
			out = append(out, m.Path)
		}
	}
	slices.Sort(out)
	return out
}

// TestTheGateAndTheMissingTermHoldBesideEachOther: B756's term, a copy whose
// file the last scan did not find never wins its unit, and B489's, a .wv or
// an .ape never wins over a copy an app that declares neither can play, hold
// together (round 3 of backlog B489, the merge of B756). A copy whose file is
// gone is no copy any client can play, so Missing comes first: a found .wv
// takes a gone .flac's place, while a found .flac still beats every .wv and
// .ape. And a domain with no found copy every client is served stays one
// unit per gated format, so a .wv and an .ape are not suppressed against
// each other because a gone .flac sits beside them, while a unit whose every
// copy is gone is suppressed whenever another unit of its domain holds a
// found copy (round 4): a client is never served a dead row beside the copy
// it can play. A domain whose every copy is gone keeps each unit's winner. In
// every view, a client that could play a copy of the track before the plan
// can play one after it, and is served no gone copy beside it (viewProblems).
func TestTheGateAndTheMissingTermHoldBesideEachOther(t *testing.T) {
	flac := codecRow("A/x.flac", "FLAC", 44100, 16, 30_000_000)
	flac2 := codecRow("D/x.flac", "FLAC", 44100, 16, 20_000_000)
	wv := codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000)
	wv2 := codecRow("E/x.wv", "WAVPACK", 44100, 16, 40_000_000)
	ape := codecRow("C/x.ape", "APE", 192000, 24, 120_000_000)
	cases := []struct {
		name    string
		members []Row
		want    []string // suppressed under FilterHighestQuality
	}{
		{"a found FLAC beside a found hi-res WavPack", []Row{flac, wv}, []string{wv.Path}},
		{"a gone FLAC beside a found WavPack", []Row{gone(flac), wv}, []string{flac.Path}},
		{"a found FLAC beside a gone WavPack", []Row{flac, gone(wv)}, []string{wv.Path}},
		{"a gone FLAC beside a gone WavPack", []Row{gone(flac), gone(wv)}, []string{wv.Path}},
		{"a gone FLAC beside a found WavPack and a found APE", []Row{gone(flac), wv, ape}, []string{flac.Path}},
		{"a gone FLAC and a gone WavPack beside a found APE", []Row{gone(flac), gone(wv), ape}, []string{flac.Path, wv.Path}},
		{"a gone FLAC beside a found WavPack and a gone APE", []Row{gone(flac), wv, gone(ape)}, []string{flac.Path, ape.Path}},
		{"two gone FLACs beside a found WavPack and a found APE", []Row{gone(flac), gone(flac2), wv, ape}, []string{flac.Path, flac2.Path}},
		{"a gone and a found WavPack beside a found APE and a gone FLAC", []Row{gone(flac), gone(wv), wv2, ape}, []string{flac.Path, wv.Path}},
		{"a gone WavPack beside a found APE", []Row{gone(wv), ape}, []string{wv.Path}},
		{"every copy gone, of three formats", []Row{gone(flac), gone(wv), gone(ape)}, nil},
		{"a gone FLAC, a found FLAC and a WavPack", []Row{gone(flac), flac2, wv}, []string{flac.Path, wv.Path}},
		{"a gone FLAC beside two found WavPack copies", []Row{gone(flac), wv, wv2}, []string{flac.Path, wv2.Path}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planFor(t, c.members, FilterHighestQuality)
			if !slices.Equal(got, c.want) {
				t.Errorf("suppressed %v, want %v", got, c.want)
			}
			for _, view := range planViews {
				if len(foundTo(view, c.members, nil)) > 0 && len(foundTo(view, c.members, got)) == 0 {
					t.Errorf("view %q: could play a copy before suppression and none after (suppressed %v)",
						view.Key(), got)
				}
			}
			for _, p := range viewProblems(c.members, got) {
				t.Error(p)
			}
		})
	}
	// The order itself: Missing, then the gate.
	if !outranks(wv, gone(flac)) || outranks(gone(flac), wv) {
		t.Error("a found WavPack copy must outrank a FLAC whose file is gone")
	}
	if !outranks(flac, wv) || outranks(wv, flac) {
		t.Error("a found FLAC must outrank a found WavPack copy")
	}
	if !outranks(gone(flac), gone(wv)) {
		t.Error("between two gone copies the gate still decides")
	}
}

// viewProblems is the view check every plan of planDomainWinners passes, in
// each view and each DSD or PCM domain (round 4 of backlog B489): while a
// found copy of the track is in the view in the domain, the view is served no
// gone copy there, and at least one found copy. A domain is judged alone, as
// suppression never crosses one: a gone DSD edition beside found PCM copies
// is still served.
func viewProblems(members []Row, suppressed []string) []string {
	var out []string
	for _, view := range planViews {
		for _, dsd := range []bool{false, true} {
			found, servedFound := false, false
			var servedGone []string
			for _, m := range members {
				if m.IsDSD != dsd || !view.Takes(m.Path) {
					continue
				}
				served := !slices.Contains(suppressed, m.Path)
				switch {
				case !m.Missing:
					found = true
					servedFound = servedFound || served
				case served:
					servedGone = append(servedGone, m.Path)
				}
			}
			if !found {
				continue
			}
			if len(servedGone) > 0 {
				out = append(out, fmt.Sprintf("view %q, %s: served the gone %v beside a found copy (suppressed %v)",
					view.Key(), domainName(dsd), servedGone, suppressed))
			}
			if !servedFound {
				out = append(out, fmt.Sprintf("view %q, %s: held a found copy and is served none (suppressed %v)",
					view.Key(), domainName(dsd), suppressed))
			}
		}
	}
	return out
}

// domainName names a domain in a failure.
func domainName(dsd bool) string {
	if dsd {
		return "DSD"
	}
	return "PCM"
}

// describeMembers names each member, a gone one marked so.
func describeMembers(members []Row) string {
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = m.Path
		if m.Missing {
			names[i] = "gone " + m.Path
		}
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// planRoutes are the tiers and modes that reach planDomainWinners.
var planRoutes = []struct {
	tier Tier
	mode FilterMode
}{
	{TierSameFormat, FilterSameFormat}, {TierSameFormat, FilterHighestQuality},
	{TierIdenticalAudio, FilterSameFormat}, {TierIdenticalAudio, FilterHighestQuality},
	{TierDifferentFormat, FilterHighestQuality},
}

// eachPlannedGroup plans every group of two members or more drawn from a pool
// of copies of one track, in path order (two FLACs, an MP3, two WavPack and
// two Monkey's Audio copies, a DSF and a WavPack DSD file), each member's
// file found or gone, under every route to planDomainWinners, and hands fn
// each group and its plan.
func eachPlannedGroup(fn func(members []Row, tier Tier, mode FilterMode, suppressed []string)) {
	pool := []Row{
		codecRow("A/x.flac", "FLAC", 44100, 16, 30_000_000),
		codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
		codecRow("C/x.ape", "APE", 192000, 24, 120_000_000),
		codecRow("D/x.flac", "FLAC", 44100, 16, 20_000_000),
		codecRow("E/x.wv", "WAVPACK", 44100, 16, 40_000_000),
		codecRow("F/x.ape", "APE", 44100, 16, 35_000_000),
		dsdRow("G/x.dsf", "DSF", 2822400, 100),
		dsdRow("H/x.wv", "WAVPACK-DSD", 5644800, 200),
		codecRow("I/x.mp3", "MP3", 44100, 16, 8_000_000),
	}
	for in := 0; in < 1<<len(pool); in++ {
		var picked []Row
		for i, r := range pool {
			if in&(1<<i) != 0 {
				picked = append(picked, r)
			}
		}
		if len(picked) < 2 {
			continue
		}
		for goneMask := 0; goneMask < 1<<len(picked); goneMask++ {
			members := make([]Row, len(picked))
			for i, r := range picked {
				if goneMask&(1<<i) != 0 {
					r = gone(r)
				}
				members[i] = r
			}
			for _, route := range planRoutes {
				g := Group{Key: KeyFor(members[0]), Tier: route.tier, Members: members}
				fn(members, route.tier, route.mode, PlanSuppression(g, Policy{Mode: route.mode}))
			}
		}
	}
}

// TestNoViewIsServedAGoneCopyBesideAFoundOne: over every group of the pool
// and every tier and mode that reach planDomainWinners, no client is served a
// gone copy while a found copy of the track is in its view in that domain,
// and each view keeps a found copy per domain where it held one (round 4 of
// backlog B489). Round 3 left a gone FLAC beside a found .wv and a found .ape
// a unit of its own, served beside them to every view that declares either.
func TestNoViewIsServedAGoneCopyBesideAFoundOne(t *testing.T) {
	plans, failed := 0, 0
	eachPlannedGroup(func(members []Row, tier Tier, mode FilterMode, suppressed []string) {
		plans++
		problems := viewProblems(members, suppressed)
		if len(problems) == 0 {
			return
		}
		failed++
		if failed <= 10 {
			t.Errorf("%s under %s, %s: %s", tier, mode, describeMembers(members), strings.Join(problems, "; "))
		}
	})
	if plans == 0 {
		t.Fatal("no plan was checked")
	}
	if failed > 0 {
		t.Errorf("%d of %d plans failed the view check", failed, plans)
	}
}

// TestAFoundPCMCopyNeverSuppressesAGoneDSDEdition: a unit whose every copy is
// gone is suppressed beside a found copy of its own DSD or PCM domain alone
// (round 4 of backlog B489). DSD and PCM are never cross-suppressed, so a
// gone DSD edition, the one member of its domain, is served beside found PCM
// copies as any lone domain's member is, until the deletion pass reaps it.
func TestAFoundPCMCopyNeverSuppressesAGoneDSDEdition(t *testing.T) {
	flac := codecRow("A/x.flac", "FLAC", 44100, 16, 30_000_000)
	wv := codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000)
	ape := codecRow("C/x.ape", "APE", 192000, 24, 120_000_000)
	dsf := dsdRow("G/x.dsf", "DSF", 2822400, 100)
	wvDSD := dsdRow("H/x.wv", "WAVPACK-DSD", 5644800, 200)
	cases := []struct {
		name    string
		members []Row
	}{
		{"a gone DSF beside a found FLAC", []Row{flac, gone(dsf)}},
		{"a gone DSF beside a found WavPack and a found APE", []Row{wv, ape, gone(dsf)}},
		{"a gone WavPack DSD file beside a found FLAC", []Row{flac, gone(wvDSD)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, mode := range []FilterMode{FilterOff, FilterSameFormat, FilterHighestQuality} {
				if got := planFor(t, c.members, mode); len(got) != 0 {
					t.Errorf("mode %s: suppressed %v, want nothing", mode, got)
				}
			}
		})
	}
}

// TestTheGoneUnitRuleChangesNoOtherTierOrMode: the rule lives in
// planDomainWinners alone, so what reaches no domain election suppresses what
// it did (round 4 of backlog B489): FilterOff, an inconclusive or
// different-audio group, a different-format group under same-format, and a
// self-nested group's members outside every twin class.
func TestTheGoneUnitRuleChangesNoOtherTierOrMode(t *testing.T) {
	members := []Row{
		gone(codecRow("A/x.flac", "FLAC", 44100, 16, 30_000_000)),
		codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
		codecRow("C/x.ape", "APE", 192000, 24, 120_000_000),
	}
	for _, c := range []struct {
		tier Tier
		mode FilterMode
	}{
		{TierDifferentFormat, FilterOff}, {TierSameFormat, FilterOff},
		{TierInconclusive, FilterSameFormat}, {TierInconclusive, FilterHighestQuality},
		{TierDifferentAudio, FilterSameFormat}, {TierDifferentAudio, FilterHighestQuality},
		{TierDifferentFormat, FilterSameFormat},
	} {
		g := Group{Key: KeyFor(members[0]), Tier: c.tier, Members: members}
		if got := PlanSuppression(g, Policy{Mode: c.mode}); len(got) != 0 {
			t.Errorf("%s under %s: suppressed %v, want nothing", c.tier, c.mode, got)
		}
	}
	// A self-nested group suppresses its deeper twin alone: a gone copy
	// outside every twin class stays as it was, beside found copies of other
	// formats.
	nested := []Row{
		codecRow("Chicago/CD 01/CD 01/x.wv", "WAVPACK", 44100, 16, 40_000_000),
		codecRow("Chicago/CD 01/x.wv", "WAVPACK", 44100, 16, 40_000_000),
		gone(codecRow("Elsewhere/x.flac", "FLAC", 44100, 16, 30_000_000)),
		codecRow("Other/x.ape", "APE", 44100, 16, 35_000_000),
	}
	if tier := classify(nested); tier != TierSelfNested {
		t.Fatalf("fixture should be self-nested, got %s", tier)
	}
	for _, mode := range []FilterMode{FilterSameFormat, FilterHighestQuality} {
		if got := planFor(t, nested, mode); !slices.Equal(got, []string{"Chicago/CD 01/CD 01/x.wv"}) {
			t.Errorf("mode %s: suppressed %v, want the deeper twin alone", mode, got)
		}
	}
}
