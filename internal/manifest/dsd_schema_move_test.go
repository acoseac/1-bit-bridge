package manifest

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// The DSD v1 → v2 move (the album-level gain): what re-renders, what stays,
// and what a phone is handed first.

// TestAutoOptimizeCandidatesMoveDSDToTheCurrentSchema: a DSD source is
// covered only by a fresh row of the CURRENT DSD schema, so a track whose
// only compact rendition is v1 is a candidate again — nothing else would
// move it, since a phone never asks for a family it already holds. A PCM
// source keeps the version-agnostic rule.
func TestAutoOptimizeCandidatesMoveDSDToTheCurrentSchema(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedDSDTrack(t, s, "D/old.dsf", "DSF", 2822400, "", 300, 2)
	seedDSDTrack(t, s, "D/new.dsf", "DSF", 2822400, "", 300, 2)
	seedOptimizeTrack(t, s, "P/old.flac", 96000, 24, "FLAC", false)
	for path, id := range map[string]string{
		"D/old.dsf":  "optimized-dsd-v1-44100-16",
		"D/new.dsf":  "optimized-dsd-" + DSDRenditionSchemaVersion + "-44100-16",
		"P/old.flac": "optimized-v1-48000-16",
	} {
		m, sz := trackRowMTimeAndSize(t, s, path)
		seedOptimizeVariant(t, s, path, id, m, sz)
	}
	opts := EligibilityOpts{DSDRender: true}
	cands, err := s.ListAutoOptimizeCandidates(ctx, 100, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Path != "D/old.dsf" {
		t.Fatalf("candidates %+v, want only the DSD track whose rendition is v1", cands)
	}
	if cands[0].StaleVariantID != "optimized-dsd-v1-44100-16" {
		t.Errorf("StaleVariantID %q: a v1 → v2 move is a regeneration", cands[0].StaleVariantID)
	}
	if n, err := s.CountAutoOptimizeCandidates(ctx, opts); err != nil || n != 1 {
		t.Errorf("count %d (err %v), want 1 — the card and the sweep share the predicate", n, err)
	}
}

// TestListSupersededPCMRenditions: the faithful tier is re-rendered only
// where a `pcm-` rendition already exists and none is fresh on the current
// schema — never for a track that had none (a 5 GB-an-hour tier nobody
// asked for), and not at all while DSD renditions are off.
func TestListSupersededPCMRenditions(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	current := "pcm-" + DSDRenditionSchemaVersion + "-176400-24"
	fixtures := map[string][]string{
		"D/v1.dsf":      {"pcm-v1-176400-24"},
		"D/current.dsf": {current},
		"D/both.dsf":    {"pcm-v1-176400-24", current},
		"D/none.dsf":    nil,
		"D/drift.dsf":   {"pcm-v1-176400-24", current + "|stale"},
	}
	for path, ids := range fixtures {
		seedDSDTrack(t, s, path, "DSF", 2822400, "", 300, 2)
		m, sz := trackRowMTimeAndSize(t, s, path)
		for _, id := range ids {
			facts := m
			if strings.HasSuffix(id, "|stale") {
				id, facts = strings.TrimSuffix(id, "|stale"), m-1 // rendered from an older copy of the file
			}
			mustUpsertVariant(t, s, VariantRow{SourcePath: path, VariantID: id, SampleRate: 176400, BitsPerSample: 24,
				SourceMTimeNS: facts, SourceSize: sz, AppliedGainDB: fpk(4), SoxSettings: `{"rateFlag":"-v"}`})
		}
	}
	paths := func(cs []AutoOptimizeCandidate) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.Path
		}
		sort.Strings(out)
		return out
	}
	got, err := s.ListSupersededPCMRenditions(ctx, 100, EligibilityOpts{DSDRender: true})
	if err != nil {
		t.Fatal(err)
	}
	if p := paths(got); strings.Join(p, ",") != "D/drift.dsf,D/v1.dsf" {
		t.Errorf("superseded %v, want the v1-only track and the one whose current rendition is stale", p)
	}
	if off, _ := s.ListSupersededPCMRenditions(ctx, 100, EligibilityOpts{}); len(off) != 0 {
		t.Errorf("with DSD renditions off nothing is re-rendered, got %v", paths(off))
	}
	if one, _ := s.ListSupersededPCMRenditions(ctx, 1, EligibilityOpts{DSDRender: true}); len(one) != 1 {
		t.Errorf("limit 1 returned %d rows", len(one))
	}
	if zero, err := s.ListSupersededPCMRenditions(ctx, 0, EligibilityOpts{DSDRender: true}); zero != nil || err != nil {
		t.Errorf("limit 0 must return nothing, got %v / %v", zero, err)
	}
}

// TestVariantsListTheNewestRenditionFirst: iOS resolves a family by id
// prefix and takes the FIRST match, and after the v1 → v2 move a track
// holds both rows. The manifest lists the newest first, so every shipped
// app version streams and downloads the current rendition, while a phone
// holding the v1 file still finds its id — and its own gain — further down.
// Equal creation times fall back to the id, so the order is deterministic.
func TestVariantsListTheNewestRenditionFirst(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedDSDTrack(t, s, "D/a.dsf", "DSF", 2822400, "", 300, 2)
	seedDSDTrack(t, s, "D/tie.dsf", "DSF", 2822400, "", 300, 2)
	m, sz := trackRowMTimeAndSize(t, s, "D/a.dsf")
	for _, v := range []VariantRow{
		{VariantID: "optimized-dsd-v1-44100-16", SampleRate: 44100, BitsPerSample: 16, AppliedGainDB: fpk(5.5), CreatedAt: 100},
		{VariantID: "pcm-v1-176400-24", SampleRate: 176400, BitsPerSample: 24, AppliedGainDB: fpk(5.4), CreatedAt: 150},
		{VariantID: "optimized-dsd-v2-44100-16", SampleRate: 44100, BitsPerSample: 16, AppliedGainDB: fpk(2.0), CreatedAt: 200},
	} {
		v.SourcePath, v.SourceMTimeNS, v.SourceSize, v.SoxSettings = "D/a.dsf", m, sz, "{}"
		mustUpsertVariant(t, s, v)
	}
	mt, st := trackRowMTimeAndSize(t, s, "D/tie.dsf")
	for _, id := range []string{"optimized-dsd-v1-44100-16", "optimized-dsd-v2-44100-16"} {
		mustUpsertVariant(t, s, VariantRow{SourcePath: "D/tie.dsf", VariantID: id, SampleRate: 44100, BitsPerSample: 16,
			SourceMTimeNS: mt, SourceSize: st, AppliedGainDB: fpk(3), SoxSettings: "{}", CreatedAt: 300})
	}

	tracks, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	order, gains := manifestVariantOrder(tracks)
	if got := strings.Join(order["D/a.dsf"], ","); got != "optimized-dsd-v2-44100-16,pcm-v1-176400-24,optimized-dsd-v1-44100-16" {
		t.Errorf("order %s, want newest first", got)
	}
	firstCompact := firstWithPrefix(order["D/a.dsf"], VariantKindPrefixOptimizedDSD+"-")
	if firstCompact != "optimized-dsd-v2-44100-16" || gains["optimized-dsd-v1-44100-16@D/a.dsf"] != 5.5 {
		t.Errorf("a prefix-first client picks %q; the v1 row must keep its own gain (got %v)", firstCompact, gains)
	}
	if got := strings.Join(order["D/tie.dsf"], ","); got != "optimized-dsd-v2-44100-16,optimized-dsd-v1-44100-16" {
		t.Errorf("tie order %s, want the id to break it (v2 first)", got)
	}
}

// manifestVariantOrder is each track's variant ids in manifest order, and
// every variant's gain keyed "<id>@<path>".
func manifestVariantOrder(tracks []Track) (map[string][]string, map[string]float64) {
	order := map[string][]string{}
	gains := map[string]float64{}
	for _, tr := range tracks {
		for _, v := range tr.Variants {
			order[tr.Path] = append(order[tr.Path], v.ID)
			if v.AppliedGainDB != nil {
				gains[v.ID+"@"+tr.Path] = *v.AppliedGainDB
			}
		}
	}
	return order, gains
}

// firstWithPrefix is what a prefix-first client (iOS) resolves a family to.
func firstWithPrefix(ids []string, prefix string) string {
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			return id
		}
	}
	return ""
}
