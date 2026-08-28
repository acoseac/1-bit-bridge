package enrich

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// stubAcousticLookup answers every path with a fixed verdict.
type stubAcousticLookup struct{ hits int }

func (s *stubAcousticLookup) LookupPath(string) (AcousticMatch, bool) {
	s.hits++
	return AcousticMatch{ArtistMBID: "11111111-1111-4111-8111-111111111111", ArtistName: "X"}, true
}

// TestAcousticActiveGatesBothSites is the reason the gate is a method
// rather than a check inside the lookup.
//
// acousticSkipReason keys the bounded skip reason off the SAME question,
// so gating only applyAcousticFallback would leave every unmatched track
// on a fingerprint-disabled bridge reporting "no fingerprint match" in
// the admin's enrichment breakdown — a reason for work that never ran,
// and one an operator would reasonably act on.
func TestAcousticActiveGatesBothSites(t *testing.T) {
	on := false
	e := &Enricher{}
	e.WithAcousticFallback(&stubAcousticLookup{}).
		WithAcousticEnabled(func() bool { return on })

	if e.acousticActive() {
		t.Fatal("gate reports active while the predicate says off")
	}
	// Site 2: the skip reason must fall back exactly as on a bridge with
	// no lookup wired at all.
	if got := acousticSkipReason(e, acousticNoVerdict, "no_mb_match"); got != "no_mb_match" {
		t.Errorf("skip reason while disabled = %q, want the fallback %q — a disabled "+
			"feature must not claim it looked and found nothing", got, "no_mb_match")
	}

	on = true
	if !e.acousticActive() {
		t.Fatal("gate did not follow the predicate on")
	}
	if got := acousticSkipReason(e, acousticNoVerdict, "no_mb_match"); got == "no_mb_match" {
		t.Error("with the feature on, a no-verdict must report the fingerprint-specific reason")
	}
}

// TestAcousticLookupNotConsultedWhileDisabled — site 1. The lookup is
// wired unconditionally now, so "wired" can no longer stand in for "on".
func TestAcousticLookupNotConsultedWhileDisabled(t *testing.T) {
	stub := &stubAcousticLookup{}
	e := &Enricher{}
	e.WithAcousticFallback(stub).WithAcousticEnabled(func() bool { return false })

	tr := trackForAcousticGate()
	if _, out := e.applyAcousticFallback(t.Context(), &tr); out != acousticNoVerdict {
		t.Errorf("outcome = %v, want acousticNoVerdict while disabled", out)
	}
	if stub.hits != 0 {
		t.Errorf("lookup consulted %d times while disabled — the gate has to sit in front "+
			"of it, not inside it", stub.hits)
	}
}

// TestNilAcousticPredicateMeansWiredEqualsEnabled — every caller other
// than cmd/bridge leaves the predicate nil and must be unaffected.
func TestNilAcousticPredicateMeansWiredEqualsEnabled(t *testing.T) {
	e := &Enricher{}
	if e.acousticActive() {
		t.Error("no lookup wired must read as inactive")
	}
	e.WithAcousticFallback(&stubAcousticLookup{})
	if !e.acousticActive() {
		t.Error("a wired lookup with no predicate must read as active (the pre-change behaviour)")
	}
}

// trackForAcousticGate is the minimal track the fallback needs: it is
// gated out before any field but Path is read.
func trackForAcousticGate() manifest.Track {
	return manifest.Track{Path: "Artist/Album/01.flac"}
}
