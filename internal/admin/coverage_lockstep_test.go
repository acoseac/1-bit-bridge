package admin

// The lockstep pin for the freshness term of every coverage read (backlog
// B100). A rendition covers its track only while it is FRESH: the version
// of its source it recorded is the one the track row records. The SQL
// asks that through one predicate (manifest's renditionFreshSQL and its
// EXISTS form) at every site: the projection a batch walks, the browse
// rows' per-track marks, the folder and subtree numerators, the album
// presence the player folds, and the covered arm of every
// covered-or-eligible denominator. The Go side asks it in the player's
// per-track chips (describeVariants, the row's own stamp). This table
// seeds every eligibility case of the sibling truth tables with every
// shape of rendition a track can hold, and fails wherever a site's answer
// differs from the Go one, or a denominator from "fresh, or eligible by
// the Go gate".

import (
	"context"
	"fmt"
	"path"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// lockstepKind is one rendition family: its id prefix, the id a render
// writes now, and an older schema's id of the same family (which still
// counts: the kind test is version-agnostic).
type lockstepKind struct {
	name, prefix, current, older string
}

var lockstepKinds = []lockstepKind{
	{variantKindUpscale, manifest.VariantKindPrefixUpscaled, "upscaled-v2-192000-24", "upscaled-v1-192000-24"},
	{variantKindOptimize, manifest.VariantKindPrefixOptimized, "optimized-v2-48000-16", "optimized-v1-44100-16"},
	{variantKindPCM, manifest.VariantKindPrefixPCM, "pcm-v2-176400-24", "pcm-v1-176400-24"},
}

// A rendition's recorded version, against the row's: what a render after
// the row's scan records, and three stale ones (a retag moved the mtime,
// the size, or both).
type stamp int

const (
	stampFresh stamp = iota
	stampStale
	stampStaleMTime
	stampStaleSize
)

func (s stamp) of(mtimeNS, size int64) (int64, int64) {
	switch s {
	case stampStale:
		return mtimeNS - int64(time.Minute), size - 8
	case stampStaleMTime:
		return mtimeNS - int64(time.Minute), size
	case stampStaleSize:
		return mtimeNS, size - 8
	}
	return mtimeNS, size
}

// renditionState is what a track holds of one kind: each entry an id
// (current or older) and its stamp.
type renditionState struct {
	name string
	of   func(kind string) []heldRendition
}

type heldRendition struct {
	older bool
	stamp stamp
}

func sameForEveryKind(h ...heldRendition) func(string) []heldRendition {
	return func(string) []heldRendition { return h }
}

var renditionStates = []renditionState{
	{"none", sameForEveryKind()},
	{"fresh", sameForEveryKind(heldRendition{stamp: stampFresh})},
	{"stale", sameForEveryKind(heldRendition{stamp: stampStale})},
	{"stale-mtime", sameForEveryKind(heldRendition{stamp: stampStaleMTime})},
	{"stale-size", sameForEveryKind(heldRendition{stamp: stampStaleSize})},
	{"fresh-beside-stale", sameForEveryKind(heldRendition{older: true, stamp: stampStale}, heldRendition{stamp: stampFresh})},
	{"two-stale", sameForEveryKind(heldRendition{older: true, stamp: stampStale}, heldRendition{stamp: stampStaleSize})},
	{"older-schema-fresh", sameForEveryKind(heldRendition{older: true, stamp: stampFresh})},
	// Kind scoping: a fresh rendition of one family is no coverage for
	// another.
	{"fresh-carplay-stale-hires", func(kind string) []heldRendition {
		switch kind {
		case variantKindOptimize:
			return []heldRendition{{stamp: stampFresh}}
		case variantKindUpscale:
			return []heldRendition{{stamp: stampStale}}
		}
		return nil
	}},
}

// freshnessCase is one seeded track: an eligibility case in a state.
type freshnessCase struct {
	elig   eligibilityCase
	state  string
	folder string // top-level folder holding only this track
	path   string
	// What the Go side says, per kind: a fresh rendition (describeVariants'
	// Fresh), any rendition, and the bytes of every rendition.
	fresh, held map[string]bool
	bytes       map[string]int64
}

func (c freshnessCase) label(kind string) string {
	return fmt.Sprintf("%s/%s/%s", c.elig.name, c.state, kind)
}

// seedFreshnessMatrix seeds one track per eligibility case and state, each
// in its own top-level folder with a subfolder (so the root listing and a
// folder listing both give it a row of its own), and the renditions the
// state names, stamped against the row.
func seedFreshnessMatrix(t *testing.T, st *manifest.Store) []freshnessCase {
	t.Helper()
	ctx := context.Background()
	var out []freshnessCase
	n := 0
	for _, e := range eligibilityMatrix {
		for _, s := range renditionStates {
			folder := fmt.Sprintf("F%03d", n)
			c := freshnessCase{elig: e, state: s.name, folder: folder, path: e.trackPath(folder + "/S")}
			size, mtime := int64(1_000_000+n), time.Unix(1_700_000_000+int64(n), 0)
			isDSD := e.isDSD
			tr := &manifest.Track{Path: c.path, Size: size, ModTime: mtime, Codec: e.codec, IsDSD: &isDSD, Compression: e.compression}
			if e.rate > 0 {
				r := e.rate
				tr.SampleRate = &r
			}
			if e.bits > 0 {
				b := e.bits
				tr.BitsPerSample = &b
			}
			if err := st.UpsertTrack(ctx, tr); err != nil {
				t.Fatalf("UpsertTrack %s: %v", c.path, err)
			}
			for _, f := range []string{folder, folder + "/S"} {
				if err := st.UpsertFolder(ctx, &manifest.Folder{Path: f}); err != nil {
					t.Fatal(err)
				}
			}
			for _, k := range lockstepKinds {
				for i, h := range s.of(k.name) {
					id := k.current
					if h.older {
						id = k.older
					}
					mt, sz := h.stamp.of(mtime.UnixNano(), size)
					if err := st.UpsertVariant(ctx, manifest.VariantRow{
						SourcePath: c.path, VariantID: id, SidecarPath: "/variants/" + c.path + "." + id,
						Format: "flac", SampleRate: 48000, BitsPerSample: 16, SizeBytes: int64(100 + 10*i),
						SourceMTimeNS: mt, SourceSize: sz, SoxSettings: "{}", CreatedAt: 1,
					}); err != nil {
						t.Fatalf("UpsertVariant %s %s: %v", c.path, id, err)
					}
				}
			}
			out = append(out, c)
			n++
		}
	}
	return out
}

// goFreshness fills each case's Go-side answers from the rows the store
// holds, through the player's own freshness decision against the row.
func goFreshness(t *testing.T, st *manifest.Store, cases []freshnessCase) {
	t.Helper()
	ctx := context.Background()
	paths := casePaths(cases)
	rows, err := st.CatalogTrackRowsForPaths(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]manifest.CatalogTrackRow, len(rows))
	for _, r := range rows {
		byPath[r.Path] = r
	}
	variants, err := st.VariantsForPaths(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	for i := range cases {
		c := &cases[i]
		row, ok := byPath[c.path]
		if !ok {
			t.Fatalf("no catalog row for %s", c.path)
		}
		c.fresh, c.held, c.bytes = map[string]bool{}, map[string]bool{}, map[string]int64{}
		for _, v := range describeVariants(variants[c.path], row) {
			c.held[v.Kind] = true
			c.fresh[v.Kind] = c.fresh[v.Kind] || v.Fresh
			c.bytes[v.Kind] += v.SizeBytes
		}
	}
}

func casePaths(cases []freshnessCase) []string {
	out := make([]string, len(cases))
	for i, c := range cases {
		out[i] = c.path
	}
	return out
}

// TestEveryCoverageReadAgreesWithTheRowFreshnessOfItsRenditions — see the
// file comment. The matrix is vacuous unless the states it seeds give both
// answers for every kind, which the first check below asserts.
func TestEveryCoverageReadAgreesWithTheRowFreshnessOfItsRenditions(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	cases := seedFreshnessMatrix(t, st)
	goFreshness(t, st, cases)
	for _, k := range lockstepKinds {
		var fresh, staleOnly int
		for _, c := range cases {
			switch {
			case c.fresh[k.name]:
				fresh++
			case c.held[k.name]:
				staleOnly++
			}
		}
		if fresh == 0 || staleOnly == 0 {
			t.Fatalf("%s: %d fresh and %d stale-only tracks; the matrix has stopped exercising the term", k.name, fresh, staleOnly)
		}
	}
	checkProjectionFreshness(t, st, cases)
	checkBrowseFreshness(t, st, cases)
	checkRollupFreshness(t, st, cases)
	checkPresenceFreshness(t, st, cases)
	checkDenominatorFreshness(t, st, cases)
}

// checkProjectionFreshness: the projection the batch walks reads a fresh
// rendition of the kind as HasVariant, and nothing else, in both scopes.
func checkProjectionFreshness(t *testing.T, st *manifest.Store, cases []freshnessCase) {
	t.Helper()
	ctx := context.Background()
	for _, k := range lockstepKinds {
		byPaths, err := st.TrackProjectionsForPaths(ctx, casePaths(cases), k.prefix)
		if err != nil {
			t.Fatal(err)
		}
		whole, err := st.ListTrackProjectionsUnderPrefix(ctx, "", k.prefix)
		if err != nil {
			t.Fatal(err)
		}
		for scope, ps := range map[string][]manifest.TrackProjection{"paths": byPaths, "prefix": whole} {
			has := map[string]bool{}
			for _, p := range ps {
				has[p.Path] = p.HasVariant
			}
			for _, c := range cases {
				if has[c.path] != c.fresh[k.name] {
					t.Errorf("%s: the %s projection's HasVariant=%v, the row's freshness=%v — a batch would %s",
						c.label(k.name), scope, has[c.path], c.fresh[k.name], batchVerdict(has[c.path]))
				}
			}
		}
	}
}

func batchVerdict(has bool) string {
	if has {
		return "skip a stale rendition as covered"
	}
	return "render a fresh rendition again"
}

// checkBrowseFreshness: the per-track marks and the folder rows' counts
// (the root listing and a folder listing) count a fresh rendition, and the
// folder sizes every rendition.
func checkBrowseFreshness(t *testing.T, st *manifest.Store, cases []freshnessCase) {
	t.Helper()
	ctx := context.Background()
	root, err := st.ListChildFolders(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	byFolder := map[string]manifest.ChildFolderRollup{}
	for _, f := range root {
		byFolder[f.Path] = f
	}
	for _, c := range cases {
		marks, err := st.ListChildTracks(ctx, path.Dir(c.path))
		if err != nil {
			t.Fatal(err)
		}
		var mark *manifest.ChildTrack
		for i := range marks {
			if marks[i].Path == c.path {
				mark = &marks[i]
			}
		}
		if mark == nil {
			t.Fatalf("%s: not listed under %s", c.path, path.Dir(c.path))
		}
		sub, err := st.ListChildFolders(ctx, c.folder)
		if err != nil || len(sub) != 1 {
			t.Fatalf("%s: listing %s gave %+v, %v", c.path, c.folder, sub, err)
		}
		reads := []struct {
			site string
			roll manifest.FolderRollup
		}{{"root listing", byFolder[c.folder].FolderRollup}, {"folder listing", sub[0].FolderRollup}}
		for _, r := range reads {
			checkFolderRollup(t, c, r.site, r.roll)
		}
		if mark.IsUpscaled != c.fresh[variantKindUpscale] || mark.IsOptimized != c.fresh[variantKindOptimize] {
			t.Errorf("%s: the track's marks (hi-res %v, CarPlay %v), the row's freshness (%v, %v)",
				c.label("marks"), mark.IsUpscaled, mark.IsOptimized, c.fresh[variantKindUpscale], c.fresh[variantKindOptimize])
		}
	}
}

func checkFolderRollup(t *testing.T, c freshnessCase, site string, r manifest.FolderRollup) {
	t.Helper()
	for _, k := range []struct {
		kind  string
		count int
		bytes int64
	}{
		{variantKindUpscale, r.UpscaledTrackCount, r.UpscaledSizeBytes},
		{variantKindOptimize, r.OptimizedTrackCount, r.OptimizedSizeBytes},
	} {
		if (k.count == 1) != c.fresh[k.kind] || k.count > 1 {
			t.Errorf("%s: the %s counts %d covered, the row's freshness=%v", c.label(k.kind), site, k.count, c.fresh[k.kind])
		}
		if k.bytes != c.bytes[k.kind] {
			t.Errorf("%s: the %s sums %d bytes, want every rendition's %d", c.label(k.kind), site, k.bytes, c.bytes[k.kind])
		}
	}
}

// checkRollupFreshness: RollupByPrefix over each case's folder counts a
// fresh rendition as covered and a track holding only stale ones as stale,
// and the whole-library form sums to the same.
func checkRollupFreshness(t *testing.T, st *manifest.Store, cases []freshnessCase) {
	t.Helper()
	ctx := context.Background()
	var want manifest.FolderRollup
	for _, c := range cases {
		r, err := st.RollupByPrefix(ctx, c.folder)
		if err != nil {
			t.Fatal(err)
		}
		checkFolderRollup(t, c, "subtree rollup", r)
		staleUp, staleOpt := boolInt(c.held[variantKindUpscale] && !c.fresh[variantKindUpscale]),
			boolInt(c.held[variantKindOptimize] && !c.fresh[variantKindOptimize])
		if r.UpscaledStaleCount != staleUp || r.OptimizedStaleCount != staleOpt {
			t.Errorf("%s: the subtree rollup counts stale (hi-res %d, CarPlay %d), want (%d, %d)",
				c.label("stale"), r.UpscaledStaleCount, r.OptimizedStaleCount, staleUp, staleOpt)
		}
		want.UpscaledTrackCount += r.UpscaledTrackCount
		want.OptimizedTrackCount += r.OptimizedTrackCount
		want.UpscaledStaleCount += r.UpscaledStaleCount
		want.OptimizedStaleCount += r.OptimizedStaleCount
		want.UpscaledSizeBytes += r.UpscaledSizeBytes
		want.OptimizedSizeBytes += r.OptimizedSizeBytes
	}
	whole, err := st.RollupByPrefix(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	whole.TrackCount, whole.TotalSizeBytes = 0, 0
	if whole != want {
		t.Errorf("the whole-library rollup = %+v, want the folders' sum %+v", whole, want)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// checkPresenceFreshness: the album presence reads agree on present and
// fresh, and foldPresence turns them into covered (fresh) and stale (held,
// none fresh).
func checkPresenceFreshness(t *testing.T, st *manifest.Store, cases []freshnessCase) {
	t.Helper()
	ctx := context.Background()
	scoped, err := st.VariantPresenceForPaths(ctx, casePaths(cases))
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.AllVariantPresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for scope, m := range map[string]map[string]manifest.VariantPresence{"scoped": scoped, "whole-library": all} {
		for _, c := range cases {
			p := m[c.path]
			for _, k := range []struct {
				kind           string
				present, fresh bool
			}{
				{variantKindUpscale, p.Upscaled, p.UpscaledFresh},
				{variantKindOptimize, p.Optimized, p.OptimizedFresh},
			} {
				if k.present != c.held[k.kind] || k.fresh != c.fresh[k.kind] {
					t.Errorf("%s: the %s presence reads present=%v fresh=%v, the rows say %v / %v",
						c.label(k.kind), scope, k.present, k.fresh, c.held[k.kind], c.fresh[k.kind])
				}
				var cov playerVariantCoverageDTO
				foldPresence(&cov, k.present, k.fresh)
				if cov.Covered != boolInt(c.fresh[k.kind]) || cov.Stale != boolInt(c.held[k.kind] && !c.fresh[k.kind]) {
					t.Errorf("%s: folded to covered %d stale %d", c.label(k.kind), cov.Covered, cov.Stale)
				}
			}
		}
	}
}

// checkDenominatorFreshness: in every capability state, each
// covered-or-eligible denominator holds a track exactly when it has a
// fresh rendition of the kind or the Go gate would give it one.
func checkDenominatorFreshness(t *testing.T, st *manifest.Store, cases []freshnessCase) {
	t.Helper()
	ctx := context.Background()
	const targetRate, targetBits = 192000, 24
	folders := make([]string, len(cases))
	for i, c := range cases {
		folders[i] = c.folder
	}
	for _, cs := range dsdRenderStates {
		perFolder, err := st.EligibleCountsForFolders(ctx, folders, targetRate, targetBits, cs.opts)
		if err != nil {
			t.Fatal(err)
		}
		kinds, err := st.AllEligibleKinds(ctx, targetRate, targetBits, cs.opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			e := c.elig
			want := map[string]bool{
				variantKindUpscale:  c.fresh[variantKindUpscale] || upscaleSubmitGate(targetRate, targetBits)(e.codec, e.rate, e.bits),
				variantKindOptimize: c.fresh[variantKindOptimize] || transcode.OptimizeEligibleFor(c.path, e.codec, int(e.rate), e.bits, e.isDSD, e.compression, cs.caps),
				variantKindPCM:      c.fresh[variantKindPCM] || transcode.PCMRenderEligible(c.path, e.codec, e.isDSD, int(e.rate), e.compression, cs.caps),
			}
			rollup, err := st.EligibleRollupByPrefix(ctx, c.folder, targetRate, targetBits, cs.opts)
			if err != nil {
				t.Fatal(err)
			}
			byPaths, err := st.EligibleCountsForPaths(ctx, []string{c.path}, targetRate, targetBits, cs.opts)
			if err != nil {
				t.Fatal(err)
			}
			k := kinds[c.path]
			reads := []struct {
				site string
				up   bool
				opt  bool
				pcm  bool
			}{
				{"folder counts", perFolder[c.folder].Upscale == 1, perFolder[c.folder].Optimize == 1, perFolder[c.folder].PCM == 1},
				{"subtree rollup", rollup.Upscale == 1, rollup.Optimize == 1, rollup.PCM == 1},
				{"path counts", byPaths.Upscale == 1, byPaths.Optimize == 1, byPaths.PCM == 1},
				{"per-track kinds", k.Upscale, k.Optimize, k.PCM},
			}
			for _, r := range reads {
				for kind, got := range map[string]bool{variantKindUpscale: r.up, variantKindOptimize: r.opt, variantKindPCM: r.pcm} {
					if got != want[kind] {
						t.Errorf("%s [%s]: the %s denominator holds it=%v, want fresh-or-eligible=%v (fresh %v)",
							c.label(kind), cs.name, r.site, got, want[kind], c.fresh[kind])
					}
				}
			}
		}
	}
}

// upscaleSubmitGate is the literal re-statement of Coordinator.Submit's
// candidate gate that TestEligibilitySQLAgreesWithUpscaleSubmitGate pins
// upscaleEligibleSQL against.
func upscaleSubmitGate(targetRate, targetBits int) func(codec string, rate float64, bits int) bool {
	return func(codec string, rate float64, bits int) bool {
		r, b := int(rate), bits
		if r <= 0 || b <= 0 {
			return false
		}
		if manifest.IsLossyCodec(codec) {
			return false
		}
		if r > targetRate || b > targetBits {
			return false
		}
		return r != targetRate || b != targetBits
	}
}
