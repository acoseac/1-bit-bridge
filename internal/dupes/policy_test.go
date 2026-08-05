package dupes

import (
	"sort"
	"strings"
	"testing"
)

func planFor(t *testing.T, members []Row, mode FilterMode) []string {
	t.Helper()
	g := Group{Key: KeyFor(members[0]), Tier: classify(members), Members: members}
	return PlanSuppression(g, Policy{Mode: mode})
}

func TestPlanSuppression_OffAndInconclusiveSuppressNothing(t *testing.T) {
	same := []Row{
		flacRow("A/B/x.flac", 44100, 16, 200, 1000),
		flacRow("C/B/x.flac", 44100, 16, 200, 900),
	}
	if got := planFor(t, same, FilterOff); len(got) != 0 {
		t.Fatalf("FilterOff must suppress nothing, got %v", got)
	}
	inconclusive := []Row{
		flacRow("A/B/x.flac", 44100, 16, 200, 1000),
		flacRow("C/B/x.flac", 44100, 16, 250, 900), // duration disagreement
	}
	if tier := classify(inconclusive); tier != TierInconclusive {
		t.Fatalf("fixture should be inconclusive, got %s", tier)
	}
	if got := planFor(t, inconclusive, FilterHighestQuality); len(got) != 0 {
		t.Fatalf("inconclusive must never be suppressed, got %v", got)
	}
}

func TestPlanSuppression_SameFormatServesExactlyOneWinner(t *testing.T) {
	members := []Row{
		flacRow("A/B/x.flac", 44100, 16, 263.73, 26_634_341),
		flacRow("C/B/x.flac", 44100, 16, 263.73, 26_817_127), // larger → wins
		flacRow("D/B/x.flac", 44100, 16, 263.73, 20_000_000),
	}
	for _, mode := range []FilterMode{FilterSameFormat, FilterHighestQuality} {
		got := planFor(t, members, mode)
		if len(got) != 2 {
			t.Fatalf("mode %s: got %v, want 2 suppressed", mode, got)
		}
		for _, p := range got {
			if p == "C/B/x.flac" {
				t.Fatalf("mode %s: the largest same-format copy must be the served winner", mode)
			}
		}
	}
}

func TestPlanSuppression_DifferentFormatOnlyUnderHighestQuality(t *testing.T) {
	members := []Row{
		flacRow("A/Voyage/01.flac", 96000, 24, 2483, 1_400_000_000),
		flacRow("B/Voyage/01.flac", 48000, 24, 2483, 700_000_000),
	}
	if tier := classify(members); tier != TierDifferentFormat {
		t.Fatalf("fixture should be different-format, got %s", tier)
	}
	if got := planFor(t, members, FilterSameFormat); len(got) != 0 {
		t.Fatalf("same-format mode must serve all cross-format members, got %v", got)
	}
	got := planFor(t, members, FilterHighestQuality)
	if len(got) != 1 || got[0] != "B/Voyage/01.flac" {
		t.Fatalf("highest-quality must suppress the 48k copy only, got %v", got)
	}
}

// TestPlanSuppression_DSDAndPCMNeverCrossSuppressed pins the product
// decision (2026-08-05): a DSD and a PCM edition of the same album are
// different masters and BOTH are served, regardless of policy. Quality
// ranking applies within each domain only.
func TestPlanSuppression_DSDAndPCMNeverCrossSuppressed(t *testing.T) {
	dsd := func(path string, size int64) Row {
		r := flacRow(path, 2822400, 1, 200, size)
		r.Codec = "DSF"
		r.IsDSD = true
		return r
	}
	// One DSD + one PCM: nothing suppressed under ANY mode.
	pair := []Row{
		flacRow("A/B/x.flac", 96000, 24, 200, 5000),
		dsd("C/B/x.dsf", 9000),
	}
	for _, mode := range []FilterMode{FilterOff, FilterSameFormat, FilterHighestQuality} {
		if got := planFor(t, pair, mode); len(got) != 0 {
			t.Fatalf("mode %s: DSD↔PCM pair must serve both, got %v", mode, got)
		}
	}
	// Two PCM + one DSD: the PCM domain collapses to its winner, the
	// lone DSD member is untouched.
	trio := []Row{
		flacRow("A/B/x.flac", 96000, 24, 200, 5000),
		flacRow("B/B/x.flac", 48000, 24, 200, 3000),
		dsd("C/B/x.dsf", 9000),
	}
	got := planFor(t, trio, FilterHighestQuality)
	if len(got) != 1 || got[0] != "B/B/x.flac" {
		t.Fatalf("only the losing PCM copy may be suppressed, got %v", got)
	}
	// Two DSD (DSD128 beats DSD64) + one PCM: within-DSD collapse only.
	dsdTrio := []Row{
		dsd("A/B/x.dsf", 9000), // DSD64
		func() Row { r := dsd("B/B/x.dsf", 18000); r.SampleRate = 5644800; return r }(), // DSD128 → wins
		flacRow("C/B/x.flac", 96000, 24, 200, 5000),
	}
	got = planFor(t, dsdTrio, FilterHighestQuality)
	if len(got) != 1 || got[0] != "A/B/x.dsf" {
		t.Fatalf("only the DSD64 copy may be suppressed, got %v", got)
	}
}

func TestPlanSuppression_SelfNestedKeepsShallowestPerTwinClass(t *testing.T) {
	members := []Row{
		flacRow("Chicago/CD 01/x.flac", 44100, 16, 200, 1000),
		flacRow("Chicago/CD 01/CD 01/x.flac", 44100, 16, 200, 1000),
		flacRow("Chicago/CD 01/CD 01/CD 01/x.flac", 44100, 16, 200, 1000),
		// A same-key member OUTSIDE the twin class: conservatively left
		// alone (self-nested suppression is nest-twins only).
		flacRow("Elsewhere/Chicago Christmas/x.flac", 44100, 16, 200, 1200),
	}
	g := Group{Key: KeyFor(members[0]), Tier: TierSelfNested, Members: members}
	got := PlanSuppression(g, Policy{Mode: FilterHighestQuality})
	sort.Strings(got)
	want := []string{
		"Chicago/CD 01/CD 01/CD 01/x.flac",
		"Chicago/CD 01/CD 01/x.flac",
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("nest suppression = %v, want %v", got, want)
	}
}

func TestOutranks_TotalOrderPins(t *testing.T) {
	flac := flacRow("a/b/x.flac", 44100, 16, 200, 100)
	mp3 := flacRow("a/b/y.mp3", 44100, 16, 200, 100)
	mp3.Codec = "MP3"
	if !outranks(flac, mp3) || outranks(mp3, flac) {
		t.Fatal("lossless must outrank lossy")
	}
	hiBits := flacRow("a/b/x.flac", 48000, 24, 200, 100)
	hiRate := flacRow("a/b/y.flac", 192000, 16, 200, 100)
	if !outranks(hiBits, hiRate) {
		t.Fatal("bit depth outranks sample rate (24/48 beats 16/192)")
	}
	big := flacRow("a/b/x.flac", 44100, 16, 200, 200)
	small := flacRow("a/b/y.flac", 44100, 16, 200, 100)
	if !outranks(big, small) {
		t.Fatal("same geometry: larger file wins")
	}
	shallow := flacRow("a/x.flac", 44100, 16, 200, 100)
	deep := flacRow("a/b/c/x.flac", 44100, 16, 200, 100)
	if !outranks(shallow, deep) {
		t.Fatal("tie: shallower path wins")
	}
	// Total order: for any two distinct paths, exactly one direction.
	a := flacRow("a/b/x.flac", 44100, 16, 200, 100)
	b := flacRow("a/b/y.flac", 44100, 16, 200, 100)
	if outranks(a, b) == outranks(b, a) {
		t.Fatal("outranks must be a strict total order on distinct rows")
	}
}

func TestKeyID_StableAndInjectiveOnBoundaries(t *testing.T) {
	k := Key{AlbumID: "a|b|2020", Disc: 1, Track: 2, NormTitle: "song"}
	if k.ID() != k.ID() {
		t.Fatal("ID must be deterministic")
	}
	if len(k.ID()) != 64 || strings.ToLower(k.ID()) != k.ID() {
		t.Fatalf("ID must be lowercase hex sha256, got %q", k.ID())
	}
	// Field-boundary forgery: moving a character across the AlbumID /
	// NormTitle boundary must change the hash (length-prefixed writes).
	k1 := Key{AlbumID: "ab", NormTitle: "c"}
	k2 := Key{AlbumID: "a", NormTitle: "bc"}
	if k1.ID() == k2.ID() {
		t.Fatal("length prefixes must make field boundaries injective")
	}
}
