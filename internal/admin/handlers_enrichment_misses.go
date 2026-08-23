// GET /api/enrichment/misses — WHICH tracks came back short, and of what.
//
// The dashboard card has always been able to say "5,435 tracks are missing
// a cover, artist or release ID". It could never say which tracks, or which
// of the three each one lacked. That gap is why a matching bug survived four
// consecutive enrichment PRs: every surface reported one aggregate number,
// so a regression and a genuinely-unmatchable library looked identical.
//
// COST DISCIPLINE: this walks StreamTrackMetaRefsUnderPrefix, a json_extract
// subtree scan (the AtlasMetaBreakdownCounts cost class). It is click-driven
// only, always behind the libMetaCache TTL + singleflight, and must NEVER be
// wired to an SSE tick.
package admin

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// enrichmentMissSampleCap bounds the per-facet path list a single response
// carries. The counts are exact; only the sample is capped. A production
// library can have thousands of misses per facet and the point of the
// endpoint is to show an operator what they look like, not to stream the
// whole set through a loopback JSON body — `bridge enrichment misses`
// exists for the full enumeration.
const enrichmentMissSampleCap = 200

// enrichmentMissRow is one short track. Wire DTO — admin-local by
// design, never manifest.TrackMetaRef itself (wire-type discipline: the
// domain type is a SQL projection and must stay free to change shape).
type enrichmentMissRow struct {
	Path   string   `json:"path"`
	Facets []string `json:"facets"`
}

// enrichmentMissesResponse is the wire shape for GET /api/enrichment/misses.
type enrichmentMissesResponse struct {
	Path    string `json:"path"`
	Scanned int    `json:"scanned"`
	Missing int    `json:"missing"`
	// Facets maps each facet name to the EXACT number of tracks short of
	// it. A track short of two facets counts in both, so these sum to more
	// than Missing.
	Facets map[string]int `json:"facets"`
	// Sample is the capped per-facet path list. Populated for every facet
	// on the unfiltered call; the handler narrows it when ?facet= is given.
	Sample map[string][]enrichmentMissRow `json:"sample"`
	// Truncated names the facets whose sample hit the cap.
	Truncated []string `json:"truncated,omitempty"`
	// SkipReasons is the enricher's process-lifetime tally of WHY it gave
	// up, keyed by bounded reason. Absent when no enricher is wired (the
	// CLI path) or when nothing has been skipped since start.
	SkipReasons map[string]int64 `json:"skipReasons,omitempty"`
}

// apiEnrichmentMisses handles GET /api/enrichment/misses.
//
// Query parameters: `path` scopes to a subtree ("" = whole library),
// `facet` narrows to one of artwork|artist|release, `limit` caps the
// returned sample (still bounded by enrichmentMissSampleCap).
//
// `facet` and `limit` are deliberately NOT part of the cache key — they
// filter a cached full snapshot, so switching facets in the UI doesn't
// re-walk the library once per facet.
func (s *Server) apiEnrichmentMisses(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manifest store not wired")
		return
	}
	normalised, ok := normaliseBrowsePath(safeQuery(r).Get("path"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path",
			"path contains traversal segments or is otherwise invalid")
		return
	}
	facet := r.URL.Query().Get("facet")
	if facet != "" && !validMissFacet(facet) {
		writeError(w, http.StatusBadRequest, "bad-facet",
			"facet must be one of artwork, artist, release")
		return
	}
	limit := enrichmentMissSampleCap
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "bad-limit", "limit must be a positive integer")
			return
		}
		limit = min(n, enrichmentMissSampleCap)
	}

	resp, err := s.libMetaMisses.fetch(r, normalised,
		func(ctx context.Context) (enrichmentMissesResponse, error) {
			return computeEnrichmentMisses(ctx, s.deps.Manifest, normalised)
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "misses-failed", err.Error())
		return
	}

	// Narrow the CACHED snapshot — never re-walk per facet/limit.
	resp = narrowMisses(resp, facet, limit)
	if s.deps.EnrichSkipReasons != nil {
		if rs := s.deps.EnrichSkipReasons(); len(rs) > 0 {
			resp.SkipReasons = rs
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func validMissFacet(f string) bool {
	switch f {
	case manifest.MissFacetArtwork, manifest.MissFacetArtist, manifest.MissFacetRelease:
		return true
	}
	return false
}

// missTrackLister is the store surface computeEnrichmentMisses needs. An
// interface so the CLI can share the walk without importing the admin
// server, and so tests can drive it without a live Store.
type missTrackLister interface {
	StreamTrackMetaRefsUnderPrefix(ctx context.Context, prefix string, fn func(manifest.TrackMetaRef) error) error
}

// computeEnrichmentMisses walks the subtree once and tallies every facet.
// Counts are exact; per-facet samples stop at enrichmentMissSampleCap.
func computeEnrichmentMisses(ctx context.Context, store missTrackLister, prefix string) (enrichmentMissesResponse, error) {
	resp := enrichmentMissesResponse{
		Path:   prefix,
		Facets: map[string]int{},
		Sample: map[string][]enrichmentMissRow{},
	}
	truncated := map[string]bool{}
	err := store.StreamTrackMetaRefsUnderPrefix(ctx, prefix, func(ref manifest.TrackMetaRef) error {
		resp.Scanned++
		facets := ref.MissFacets()
		if len(facets) == 0 {
			return nil
		}
		resp.Missing++
		row := enrichmentMissRow{Path: ref.Path, Facets: facets}
		for _, f := range facets {
			resp.Facets[f]++
			if len(resp.Sample[f]) < enrichmentMissSampleCap {
				resp.Sample[f] = append(resp.Sample[f], row)
			} else {
				truncated[f] = true
			}
		}
		return nil
	})
	if err != nil {
		return enrichmentMissesResponse{}, err
	}
	for f := range truncated {
		resp.Truncated = append(resp.Truncated, f)
	}
	sort.Strings(resp.Truncated)
	return resp, nil
}

// narrowMisses filters a cached full snapshot down to one facet and/or a
// smaller sample limit.
//
// Scanned / Missing are left intact — they describe the library, and
// narrowing the VIEW must not make the problem look smaller than it is.
// Two things DO have to be recomputed, or the response contradicts its
// own documented contract:
//
//   - Truncated must name every facet whose sample no longer covers its
//     count, whether it was cut by the walk's own cap or by this call's
//     smaller limit. It was carried over verbatim until 2026-08-06, so
//     `?limit=5` against a 50-miss facet returned five rows, a count of
//     50, and no truncation marker — and the renderer keys its "showing
//     the first N" note off exactly that marker, so a short list read as
//     the complete set.
//   - Facets must drop the facets the caller filtered out. Reporting a
//     count for a facet whose sample isn't in the response leaves the
//     renderer (which decides what to draw from Facets, then reads
//     Sample) painting an empty section.
//
// Every map and slice is rebuilt rather than shared: `in` is the CACHED
// snapshot, so appending to its Truncated or writing to its Facets would
// mutate what the next caller reads.
func narrowMisses(in enrichmentMissesResponse, facet string, limit int) enrichmentMissesResponse {
	out := in
	out.Sample = make(map[string][]enrichmentMissRow, len(in.Sample))
	out.Facets = make(map[string]int, len(in.Facets))
	out.Truncated = nil
	for f, n := range in.Facets {
		if facet != "" && f != facet {
			continue
		}
		out.Facets[f] = n
	}
	for f, rows := range in.Sample {
		if facet != "" && f != facet {
			continue
		}
		if len(rows) > limit {
			rows = rows[:limit]
		}
		out.Sample[f] = rows
		if len(rows) < in.Facets[f] {
			out.Truncated = append(out.Truncated, f)
		}
	}
	sort.Strings(out.Truncated)
	return out
}
