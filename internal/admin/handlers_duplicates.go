// Admin Duplicates page endpoints: the persisted stamping-pass summary
// (tiles), the paged group listing (evidence + serving badges), and the
// re-evaluate trigger. Loopback/console surface only — none of this is
// the /v1 wire.
package admin

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// pageDuplicates renders the Library → Duplicates page shell; all data
// is client-fetched (the pageJobs shape).
func (s *Server) pageDuplicates(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "duplicates", nil)
}

// duplicatesSummaryResponse: GET /api/duplicates/summary. Reads the
// scan_state document the stamping pass persisted — zero-cost tiles that
// are exactly consistent with the stamps, no TTL/singleflight machinery
// (the summary only changes when a pass runs, and the page refetches on
// the scan-complete SSE edge).
type duplicatesSummaryResponse struct {
	// Policy is the LIVE resolved duplicates.filter; StampedPolicy is
	// the one the last pass ran under — they diverge only in the window
	// between a settings PATCH and the nudged restamp finishing, and the
	// page uses the pair to render a "re-evaluating…" hint.
	Policy        string              `json:"policy"`
	StampedPolicy string              `json:"stampedPolicy,omitempty"`
	StampedAt     *time.Time          `json:"stampedAt,omitempty"`
	Stamped       bool                `json:"stamped"`
	Scanned       int                 `json:"scanned"`
	Groups        int                 `json:"groups"`
	Suppressed    int                 `json:"suppressed"`
	Served        int                 `json:"served"`
	MD5Known      int                 `json:"md5Known"`
	MD5Total      int                 `json:"md5Total"`
	Tiers         []duplicatesTierRow `json:"tiers"`
}

type duplicatesTierRow struct {
	Tier           string `json:"tier"`
	Groups         int    `json:"groups"`
	RedundantFiles int    `json:"redundantFiles"`
	// NonLargestBytes is "bytes in the non-largest copies" — the
	// deliberate vocabulary from the CLI report: never "wasted" or
	// "reclaimable", because the different-format tier's copies are
	// different masters.
	NonLargestBytes int64 `json:"bytesInNonLargestCopies"`
	Suppressed      int   `json:"suppressed"`
}

func (s *Server) apiDuplicatesSummary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), snapshotDBTimeout)
	defer cancel()
	sum, err := s.deps.Manifest.LoadDupeSummary(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := duplicatesSummaryResponse{
		Policy: resolvedDuplicatesFilter(s.deps.CfgHolder.Load()),
	}
	if sum != nil {
		resp.Stamped = true
		st := sum.StampedAt
		resp.StampedAt = &st
		resp.StampedPolicy = sum.Policy
		resp.Scanned, resp.Groups = sum.Scanned, sum.Groups
		resp.Suppressed, resp.Served = sum.Suppressed, sum.Served
		resp.MD5Known, resp.MD5Total = sum.MD5Known, sum.MD5Total
		for _, t := range sum.Tiers {
			resp.Tiers = append(resp.Tiers, duplicatesTierRow(t))
		}
	}
	// no-store: a disk-cache hit after a restamp would resurrect stale
	// tiles (the house rule from the enrichment-misses endpoint).
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

// duplicatesGroupsResponse: GET /api/duplicates/groups?tier=&cursor=&limit=
// — a live paged read over the stamped columns. Admin wire DTOs, never
// the store row structs (wire-type discipline).
type duplicatesGroupsResponse struct {
	Groups     []duplicatesGroupRow `json:"groups"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

type duplicatesGroupRow struct {
	GroupID string                `json:"groupID"`
	Tier    string                `json:"tier"`
	Members []duplicatesMemberRow `json:"members"`
}

type duplicatesMemberRow struct {
	Path          string  `json:"path"`
	Suppressed    bool    `json:"suppressed"`
	Codec         string  `json:"codec,omitempty"`
	SampleRate    int     `json:"sampleRate,omitempty"`
	BitsPerSample int     `json:"bitsPerSample,omitempty"`
	IsDSD         bool    `json:"isDSD,omitempty"`
	SizeBytes     int64   `json:"sizeBytes"`
	DurationSec   float64 `json:"durationSec,omitempty"`
	Title         string  `json:"title,omitempty"`
	Album         string  `json:"album,omitempty"`
	AlbumArtist   string  `json:"albumArtist,omitempty"`
}

// validDupeTierParam bounds the tier filter to the stamped vocabulary —
// an unknown tier is a 400, not a silently-empty listing.
func validDupeTierParam(tier string) bool {
	switch tier {
	case "", "self-nested", "same-format", "identical-audio", "different-audio",
		"different-format", "inconclusive":
		return true
	}
	return false
}

func (s *Server) apiDuplicatesGroups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tier := q.Get("tier")
	if !validDupeTierParam(tier) {
		writeError(w, http.StatusBadRequest, "validate",
			"tier must be one of self-nested, same-format, different-format, inconclusive")
		return
	}
	// Bad/absent limit silently falls to the store's default (the
	// browse handlers' convention).
	limit := 0
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), snapshotDBTimeout)
	defer cancel()
	groups, next, err := s.deps.Manifest.ListDupeGroupsPage(ctx, tier, q.Get("cursor"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := duplicatesGroupsResponse{Groups: []duplicatesGroupRow{}, NextCursor: next}
	for _, g := range groups {
		row := duplicatesGroupRow{GroupID: g.GroupID, Tier: g.Tier}
		for _, m := range g.Members {
			row.Members = append(row.Members, duplicatesMemberRow{
				Path: m.Path, Suppressed: m.Suppressed, Codec: m.Codec,
				SampleRate: m.SampleRate, BitsPerSample: m.BitsPerSample,
				IsDSD: m.IsDSD, SizeBytes: m.SizeBytes, DurationSec: m.DurationSec,
				Title: m.Title, Album: m.Album, AlbumArtist: m.AlbumArtist,
			})
		}
		resp.Groups = append(resp.Groups, row)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

// apiDuplicatesSweep: POST /api/duplicates/sweep — the "Re-evaluate now"
// button. apiFingerprintSweep's shape: 202 = nudged (coalescing send),
// 503 = sweeper not wired. No extra rate guard — the nudge coalesces and
// the pass is a sub-second DB-only walk.
func (s *Server) apiDuplicatesSweep(w http.ResponseWriter, _ *http.Request) {
	trigger := s.deps.TriggerDuplicatesPass
	if trigger == nil {
		writeError(w, http.StatusServiceUnavailable, "duplicates_unavailable",
			"the duplicates stamping sweeper is not wired on this bridge")
		return
	}
	trigger()
	writeJSON(w, http.StatusAccepted, map[string]bool{"triggered": true})
}
