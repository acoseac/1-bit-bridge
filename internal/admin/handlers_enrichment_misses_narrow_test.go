package admin

import (
	"context"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestNarrowMissesMarksSamplesCutByTheLimit is the F4 pin.
//
// `Truncated` is documented as naming "the facets whose sample hit the cap",
// and the renderer keys its "Showing the first N. The count above is exact"
// note off exactly that. narrowMisses carried the field over verbatim from
// the cached full snapshot, so `?limit=1` against a 3-miss facet returned one
// sample row, a count of 3, and no truncation marker — a short list reading
// as the complete set.
func TestNarrowMissesMarksSamplesCutByTheLimit(t *testing.T) {
	full, err := computeEnrichmentMisses(context.Background(),
		&fakeMissLister{refs: missFixture()}, "")
	if err != nil {
		t.Fatalf("computeEnrichmentMisses: %v", err)
	}
	// Sanity: the fixture must have a facet the limit can actually cut.
	if full.Facets[manifest.MissFacetRelease] < 2 {
		t.Fatalf("fixture has %d release misses; need >=2 to exercise the limit",
			full.Facets[manifest.MissFacetRelease])
	}
	if len(full.Truncated) != 0 {
		t.Fatalf("fixture unexpectedly hit the walk cap: %v", full.Truncated)
	}

	got := narrowMisses(full, "", 1)

	if !slices.Contains(got.Truncated, manifest.MissFacetRelease) {
		t.Errorf("Truncated = %v; want it to name %q — the sample was cut to 1 "+
			"of %d and the renderer has no way to say so",
			got.Truncated, manifest.MissFacetRelease, full.Facets[manifest.MissFacetRelease])
	}
	// Every named facet must genuinely be short, and every short facet named.
	for f, n := range got.Facets {
		short := len(got.Sample[f]) < n
		named := slices.Contains(got.Truncated, f)
		if short != named {
			t.Errorf("facet %q: sample %d of %d, truncated-marker=%v — the marker "+
				"must exactly track whether the sample covers the count",
				f, len(got.Sample[f]), n, named)
		}
	}
}

// TestNarrowMissesDropsUnselectedFacetCounts — with ?facet= the response must
// not advertise counts for facets whose sample it withheld. The renderer
// decides what to draw from Facets and then reads Sample, so a retained count
// paints a section with no rows in it.
func TestNarrowMissesDropsUnselectedFacetCounts(t *testing.T) {
	full, err := computeEnrichmentMisses(context.Background(),
		&fakeMissLister{refs: missFixture()}, "")
	if err != nil {
		t.Fatalf("computeEnrichmentMisses: %v", err)
	}
	if len(full.Facets) < 2 {
		t.Fatalf("fixture has %d facets; need >=2", len(full.Facets))
	}

	got := narrowMisses(full, manifest.MissFacetRelease, enrichmentMissSampleCap)

	if len(got.Facets) != 1 {
		t.Errorf("Facets carries %d entries (%v); want only the requested facet — "+
			"a count without a sample renders as an empty section",
			len(got.Facets), got.Facets)
	}
	if got.Facets[manifest.MissFacetRelease] != full.Facets[manifest.MissFacetRelease] {
		t.Errorf("requested facet's count changed: %d -> %d",
			full.Facets[manifest.MissFacetRelease], got.Facets[manifest.MissFacetRelease])
	}
	// Library-level totals still describe the library, not the view.
	if got.Scanned != full.Scanned || got.Missing != full.Missing {
		t.Errorf("narrowing changed Scanned/Missing: %d/%d -> %d/%d",
			full.Scanned, full.Missing, got.Scanned, got.Missing)
	}
}

// TestNarrowMissesDoesNotMutateTheCachedSnapshot — `in` is the shared cached
// value behind libMetaMisses; appending to its Truncated slice or writing to
// its Facets map would corrupt what the next caller reads.
func TestNarrowMissesDoesNotMutateTheCachedSnapshot(t *testing.T) {
	full, err := computeEnrichmentMisses(context.Background(),
		&fakeMissLister{refs: missFixture()}, "")
	if err != nil {
		t.Fatalf("computeEnrichmentMisses: %v", err)
	}
	beforeFacets := len(full.Facets)
	beforeTruncated := slices.Clone(full.Truncated)

	_ = narrowMisses(full, manifest.MissFacetRelease, 1)

	if len(full.Facets) != beforeFacets {
		t.Errorf("cached Facets mutated: %d -> %d entries", beforeFacets, len(full.Facets))
	}
	if !slices.Equal(full.Truncated, beforeTruncated) {
		t.Errorf("cached Truncated mutated: %v -> %v", beforeTruncated, full.Truncated)
	}
}
