package manifest

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The store half of the DSD renditions (migration v43): the two gain
// columns and their wire shape, the `pcm` / `optimized-dsd` families
// everywhere a prefix is read, the `tracks.compression` accelerator,
// and the EligibilityOpts-gated SQL mirrors. The Go ⇄ SQL lockstep for
// the eligibility rule itself lives in internal/admin (the one package
// importing both manifest and transcode); this file pins the store's
// own contracts.

func ptrF(v float64) *float64 { return &v }

// seedDSDTrack upserts a DSD row with every column the render gates and
// the candidate query read: the v25 format facts, the v43 compression
// accelerator, and the duration / channels the sweeper projects.
func seedDSDTrack(t *testing.T, s *Store, path, codec string, rate float64, compression string, durationSec float64, channels int) {
	t.Helper()
	isDSD, bits := true, 1
	tr := &Track{
		Path: path, Size: 1_000_000, ModTime: time.Unix(1_700_000_000, 0),
		Codec: codec, IsDSD: &isDSD, SampleRate: &rate, BitsPerSample: &bits,
		Compression: compression,
	}
	if durationSec > 0 {
		d := durationSec
		tr.Duration = &d
	}
	if channels > 0 {
		c := channels
		tr.Channels = &c
	}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("UpsertTrack(%q): %v", path, err)
	}
}

func compressionColumn(t *testing.T, s *Store, path string) string {
	t.Helper()
	var c string
	if err := s.db.QueryRow(`SELECT COALESCE(compression, '') FROM tracks WHERE path = ?`, path).Scan(&c); err != nil {
		t.Fatalf("read compression for %q: %v", path, err)
	}
	return c
}

// TestMigrationV43AddsColumnsIdempotently — the four columns exist after
// a fresh open, the upserts stamp `compression`, and a RE-RUN of v43
// (columns present, version rewound) both skips the duplicate ALTERs and
// re-derives `compression` from tags_json — the backfill runs on every
// attempt, outside the column-exists guard (the v30 lesson).
func TestMigrationV43AddsColumnsIdempotently(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	for _, c := range []struct{ table, col string }{
		{"track_variants", "applied_gain_db"},
		{"track_variants", "true_peak_dbtp"},
		{"upscale_batches", "kind"},
		{"tracks", "compression"},
	} {
		exists, err := atlasColumnExists(s.db, c.table, c.col)
		if err != nil {
			t.Fatalf("inspect %s.%s: %v", c.table, c.col, err)
		}
		if !exists {
			t.Errorf("v43 column %s.%s missing after a fresh open", c.table, c.col)
		}
	}

	seedDSDTrack(t, s, "A/01.dff", "DFF", 2822400, "DST", 0, 0)
	seedDSDTrack(t, s, "A/02.dsf", "DSF", 2822400, "", 0, 0)
	if got := compressionColumn(t, s, "A/01.dff"); got != "DST" {
		t.Fatalf("upsert stamped compression=%q, want DST", got)
	}
	if got := compressionColumn(t, s, "A/02.dsf"); got != "" {
		t.Fatalf("upsert stamped compression=%q on an uncompressed row, want empty", got)
	}

	// The state a failed backfill leaves behind: columns present, no
	// row written, version still at 42. The restart is the retry.
	if _, err := s.db.ExecContext(ctx, `UPDATE tracks SET compression = NULL`); err != nil {
		t.Fatalf("clear compression: %v", err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := s.db.ExecContext(ctx, `PRAGMA user_version = 42`); err != nil {
			t.Fatalf("rewind user_version: %v", err)
		}
		if err := s.migrate(); err != nil {
			t.Fatalf("re-run %d of migrate: %v", run, err)
		}
		if v, want := readUserVersion(t, s.db), migrations[len(migrations)-1].version; v != want {
			t.Errorf("re-run %d: user_version = %d, want %d", run, v, want)
		}
		if got := compressionColumn(t, s, "A/01.dff"); got != "DST" {
			t.Errorf("re-run %d: compression=%q, want DST re-derived from tags_json", run, got)
		}
		if got := compressionColumn(t, s, "A/02.dsf"); got != "" {
			t.Errorf("re-run %d: compression=%q on a row whose tags carry none, want empty", run, got)
		}
	}
}

// TestUpsertVariantRoundTripsGainColumns — the gain facts survive every
// one of the five VariantRow readers, a PCM row reads back nil on both,
// and a re-upsert without them CLEARS them (the DO UPDATE arm).
func TestUpsertVariantRoundTripsGainColumns(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	const src = "Music/A/1.dsf"
	upsertParent(t, s, src)

	base := VariantRow{
		SourcePath: src, SidecarPath: "/tmp/x.flac", Format: "flac",
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}
	dsd := base
	dsd.VariantID, dsd.SampleRate, dsd.BitsPerSample, dsd.SizeBytes = "pcm-v1-176400-24", 176400, 24, 10
	dsd.AppliedGainDB, dsd.TruePeakDBTP = ptrF(4.8), ptrF(-5.8)
	pcm := base
	pcm.VariantID, pcm.SampleRate, pcm.BitsPerSample, pcm.SizeBytes = "optimized-v2-44100-16", 44100, 16, 5
	for _, v := range []VariantRow{dsd, pcm} {
		if err := s.UpsertVariant(ctx, v); err != nil {
			t.Fatalf("UpsertVariant(%s): %v", v.VariantID, err)
		}
	}

	check := func(reader string, v *VariantRow, wantGain, wantPeak *float64) {
		t.Helper()
		if v == nil {
			t.Fatalf("%s: no row", reader)
		}
		switch {
		case (v.AppliedGainDB == nil) != (wantGain == nil):
			t.Errorf("%s/%s: AppliedGainDB nil-ness = %v, want %v", reader, v.VariantID, v.AppliedGainDB == nil, wantGain == nil)
		case wantGain != nil && *v.AppliedGainDB != *wantGain:
			t.Errorf("%s/%s: AppliedGainDB = %v, want %v", reader, v.VariantID, *v.AppliedGainDB, *wantGain)
		}
		switch {
		case (v.TruePeakDBTP == nil) != (wantPeak == nil):
			t.Errorf("%s/%s: TruePeakDBTP nil-ness = %v, want %v", reader, v.VariantID, v.TruePeakDBTP == nil, wantPeak == nil)
		case wantPeak != nil && *v.TruePeakDBTP != *wantPeak:
			t.Errorf("%s/%s: TruePeakDBTP = %v, want %v", reader, v.VariantID, *v.TruePeakDBTP, *wantPeak)
		}
	}
	byID := func(reader string, rows []VariantRow, err error) map[string]*VariantRow {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", reader, err)
		}
		out := map[string]*VariantRow{}
		for i := range rows {
			out[rows[i].VariantID] = &rows[i]
		}
		return out
	}

	g, err := s.GetVariant(ctx, src, dsd.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	check("GetVariant", g, ptrF(4.8), ptrF(-5.8))
	g, err = s.GetVariant(ctx, src, pcm.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	check("GetVariant", g, nil, nil)

	l, err := s.lookupVariantByLowerCase(ctx, "music/a/1.dsf", dsd.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	check("lookupVariantByLowerCase", l, ptrF(4.8), ptrF(-5.8))

	allRows, err := s.AllVariants(ctx)
	all := byID("AllVariants", allRows, err)
	check("AllVariants", all[dsd.VariantID], ptrF(4.8), ptrF(-5.8))
	check("AllVariants", all[pcm.VariantID], nil, nil)

	preRows, err := s.ListVariantsByPathPrefix(ctx, "Music")
	pre := byID("ListVariantsByPathPrefix", preRows, err)
	check("ListVariantsByPathPrefix", pre[dsd.VariantID], ptrF(4.8), ptrF(-5.8))
	check("ListVariantsByPathPrefix", pre[pcm.VariantID], nil, nil)

	fpRows, err := s.ListVariantsForPath(ctx, src)
	fp := byID("ListVariantsForPath", fpRows, err)
	check("ListVariantsForPath", fp[dsd.VariantID], ptrF(4.8), ptrF(-5.8))
	check("ListVariantsForPath", fp[pcm.VariantID], nil, nil)

	// A re-render whose result carries no gain facts must not keep the
	// previous ones: the columns follow the upsert, both directions.
	cleared := dsd
	cleared.AppliedGainDB, cleared.TruePeakDBTP = nil, nil
	if err := s.UpsertVariant(ctx, cleared); err != nil {
		t.Fatal(err)
	}
	g, err = s.GetVariant(ctx, src, dsd.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	check("GetVariant after clearing upsert", g, nil, nil)
}

// TestListTracksVariantsCarryAppliedGainDBOnlyWhenSet — the wire shape
// of `appliedGainDB`: a DSD rendition clamped to 0 dB ships the key with
// `0` (a pointer to zero is not omitted), a PCM variant ships NO key at
// all, and the pcm family carries its own label.
func TestListTracksVariantsCarryAppliedGainDBOnlyWhenSet(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	upsertParent(t, s, "Music/A/1.dsf")
	upsertParent(t, s, "Music/A/2.flac")

	if err := s.UpsertVariant(ctx, VariantRow{
		SourcePath: "Music/A/1.dsf", VariantID: "pcm-v1-176400-24",
		SidecarPath: "/tmp/p.flac", Format: "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 10,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
		AppliedGainDB: ptrF(0), TruePeakDBTP: ptrF(-0.4),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVariant(ctx, VariantRow{
		SourcePath: "Music/A/2.flac", VariantID: "optimized-v2-44100-16",
		SidecarPath: "/tmp/o.flac", Format: "flac", SampleRate: 44100, BitsPerSample: 16, SizeBytes: 10,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	tracks, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatalf("ListTracks: %v", err)
	}
	wire := map[string][]map[string]any{}
	structs := map[string][]Variant{}
	for _, tr := range tracks {
		raw, err := json.Marshal(tr.Variants)
		if err != nil {
			t.Fatal(err)
		}
		var got []map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		wire[tr.Path] = got
		structs[tr.Path] = tr.Variants
	}

	dsd := wire["Music/A/1.dsf"]
	if len(dsd) != 1 {
		t.Fatalf("DSD track: %d variants on the wire, want 1", len(dsd))
	}
	if v, ok := dsd[0]["appliedGainDB"]; !ok {
		t.Errorf("DSD rendition clamped to 0 dB lost its appliedGainDB key: %v", dsd[0])
	} else if f, isNum := v.(float64); !isNum || f != 0 {
		t.Errorf("appliedGainDB = %v (%T), want 0", v, v)
	}
	if got := dsd[0]["label"]; got != "PCM FLAC 24/176.4" {
		t.Errorf("pcm label on the wire = %v, want %q", got, "PCM FLAC 24/176.4")
	}
	if sv := structs["Music/A/1.dsf"]; len(sv) != 1 || sv[0].AppliedGainDB == nil || *sv[0].AppliedGainDB != 0 {
		t.Errorf("Variant.AppliedGainDB on the struct = %+v, want a pointer to 0", sv)
	}

	pcm := wire["Music/A/2.flac"]
	if len(pcm) != 1 {
		t.Fatalf("PCM track: %d variants on the wire, want 1", len(pcm))
	}
	if v, ok := pcm[0]["appliedGainDB"]; ok {
		t.Errorf("PCM variant carries appliedGainDB=%v; the key must be ABSENT (omitempty on a nil pointer)", v)
	}
	if sv := structs["Music/A/2.flac"]; len(sv) != 1 || sv[0].AppliedGainDB != nil {
		t.Errorf("Variant.AppliedGainDB on a PCM variant = %+v, want nil", sv)
	}
}

// TestVariantKindBuckets_PCMAndOptimizedDSD — the two kind-bucket
// helpers file the faithful tier under "pcm" (pre-seeded, so an empty
// table still carries the key — TestVariantStatsByKind_PreseedsEmptyTable
// covers that) and the compact DSD tier under "optimize", because its
// prefix IS the optimize prefix.
func TestVariantKindBuckets_PCMAndOptimizedDSD(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedKindVariants(t, s, []VariantRow{
		{SourcePath: "A/01.dsf", VariantID: "pcm-v1-176400-24", SidecarPath: "/tmp/a1.flac", Format: "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 700, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
		{SourcePath: "A/01.dsf", VariantID: "optimized-dsd-v1-44100-16", SidecarPath: "/tmp/a2.flac", Format: "flac", SampleRate: 44100, BitsPerSample: 16, SizeBytes: 50, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
		{SourcePath: "B/01.flac", VariantID: "optimized-v2-44100-16", SidecarPath: "/tmp/b1.flac", Format: "flac", SampleRate: 44100, BitsPerSample: 16, SizeBytes: 400, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
	})

	counts, err := s.CountVariantsByKind(context.Background())
	if err != nil {
		t.Fatalf("CountVariantsByKind: %v", err)
	}
	if counts["pcm"] != 700 || counts["optimize"] != 450 || counts["upscale"] != 0 {
		t.Errorf("CountVariantsByKind = %v, want pcm=700 optimize=450 upscale=0", counts)
	}
	if _, ok := counts["unknown"]; ok {
		t.Errorf("a DSD family landed in the unknown bucket: %v", counts)
	}

	stats, err := s.VariantStatsByKind(context.Background())
	if err != nil {
		t.Fatalf("VariantStatsByKind: %v", err)
	}
	if got := stats["pcm"]; got.Files != 1 || got.Bytes != 700 {
		t.Errorf("pcm = %+v, want {Files:1 Bytes:700}", got)
	}
	if got := stats["optimize"]; got.Files != 2 || got.Bytes != 450 {
		t.Errorf("optimize = %+v, want {Files:2 Bytes:450} (the optimized-dsd row counts here)", got)
	}
	if _, ok := stats["unknown"]; ok {
		t.Errorf("a DSD family landed in the unknown bucket: %v", stats)
	}
}

// dsdEligibilityFixture is one folder of the cases the opts-gated
// mirrors must agree on across all four helpers: an eligible DSF, a DST
// DFF (needs the dst decoder), an SACD virtual row (never), an
// off-family rate (never — but it CARRIES a pcm- variant, so it is
// covered for the pcm tier in every state), a PCM optimize-eligible
// FLAC and a FLAC at the CarPlay floor.
func dsdEligibilityFixture(t *testing.T, s *Store) []string {
	t.Helper()
	seedDSDTrack(t, s, "D/01.dsf", "DSF", 2822400, "", 0, 0)
	seedDSDTrack(t, s, "D/02.dff", "DFF", 2822400, "DST", 0, 0)
	seedDSDTrack(t, s, "D/Album.iso/st/01.dff", "DFF", 2822400, "", 0, 0)
	seedDSDTrack(t, s, "D/03.dsf", "DSF", 3000000, "", 0, 0)
	seedFormatTrack(t, s, "D/04.flac", "FLAC", 96000, 24, false)
	seedFormatTrack(t, s, "D/05.flac", "FLAC", 44100, 16, false)
	if err := s.UpsertVariant(context.Background(), VariantRow{
		SourcePath: "D/03.dsf", VariantID: "pcm-v1-176400-24",
		SidecarPath: "/tmp/p.flac", Format: "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 1,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return []string{"D/01.dsf", "D/02.dff", "D/Album.iso/st/01.dff", "D/03.dsf", "D/04.flac", "D/05.flac"}
}

// TestEligibility_DSDRenderOptsAcrossHelpers — all four eligibility
// helpers agree, in every capability state, on the optimize and pcm
// denominators of the same fixture: PCM-only counts exactly what it
// counted before v43, DSD adds the DSF, DST adds the DFF, the SACD
// virtual and off-family rows never count, and an existing `pcm-`
// variant is coverage regardless of caps.
func TestEligibility_DSDRenderOptsAcrossHelpers(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	paths := dsdEligibilityFixture(t, s)

	cases := []struct {
		name             string
		opts             EligibilityOpts
		wantOpt, wantPCM int
	}{
		{"pcm-only", EligibilityOpts{}, 1, 1},
		{"dsd", EligibilityOpts{DSDRender: true}, 2, 2},
		{"dsd+dst", EligibilityOpts{DSDRender: true, DST: true}, 3, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			folders, err := s.EligibleCountsForFolders(ctx, []string{"D"}, 192000, 24, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := folders["D"]; got.Optimize != c.wantOpt || got.PCM != c.wantPCM {
				t.Errorf("EligibleCountsForFolders = %+v, want optimize=%d pcm=%d", got, c.wantOpt, c.wantPCM)
			}
			for _, prefix := range []string{"D", ""} {
				rollup, err := s.EligibleRollupByPrefix(ctx, prefix, 192000, 24, c.opts)
				if err != nil {
					t.Fatal(err)
				}
				if rollup.Optimize != c.wantOpt || rollup.PCM != c.wantPCM {
					t.Errorf("EligibleRollupByPrefix(%q) = %+v, want optimize=%d pcm=%d", prefix, rollup, c.wantOpt, c.wantPCM)
				}
			}
			byPaths, err := s.EligibleCountsForPaths(ctx, paths, 192000, 24, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if byPaths.Optimize != c.wantOpt || byPaths.PCM != c.wantPCM {
				t.Errorf("EligibleCountsForPaths = %+v, want optimize=%d pcm=%d", byPaths, c.wantOpt, c.wantPCM)
			}
			kinds, err := s.AllEligibleKinds(ctx, 192000, 24, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			opt, pcm := 0, 0
			for _, k := range kinds {
				if k.Optimize {
					opt++
				}
				if k.PCM {
					pcm++
				}
			}
			if opt != c.wantOpt || pcm != c.wantPCM {
				t.Errorf("AllEligibleKinds: optimize=%d pcm=%d, want %d/%d", opt, pcm, c.wantOpt, c.wantPCM)
			}
			if kinds["D/Album.iso/st/01.dff"].PCM || kinds["D/Album.iso/st/01.dff"].Optimize {
				t.Errorf("SACD virtual row counted as eligible: %+v", kinds["D/Album.iso/st/01.dff"])
			}
			if kinds["D/01.dsf"].Upscale {
				t.Errorf("a DSD row counted for the upscale tier")
			}
			if !kinds["D/03.dsf"].PCM {
				t.Errorf("a row carrying a pcm- variant must read as covered for the pcm tier in every state")
			}
		})
	}
}

// TestListAutoOptimizeCandidates_DSDArm — the sweeper's candidate query
// admits DSD sources only under the opts that say so, carries the
// render-job facts on the row, counts in lockstep with the listing, and
// reads an `optimized-dsd-` variant as coverage (it is an `optimized-%`)
// while a `pcm-` variant is not.
func TestListAutoOptimizeCandidates_DSDArm(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedDSDTrack(t, s, "D/01.dsf", "DSF", 2822400, "", 300.5, 2)
	seedDSDTrack(t, s, "D/02.dff", "DFF", 2822400, "DST", 0, 0)
	seedOptimizeTrack(t, s, "P/01.flac", 96000, 24, "FLAC", false)

	list := func(opts EligibilityOpts) map[string]AutoOptimizeCandidate {
		t.Helper()
		cands, err := s.ListAutoOptimizeCandidates(ctx, 100, opts)
		if err != nil {
			t.Fatalf("ListAutoOptimizeCandidates(%+v): %v", opts, err)
		}
		n, err := s.CountAutoOptimizeCandidates(ctx, opts)
		if err != nil {
			t.Fatalf("CountAutoOptimizeCandidates(%+v): %v", opts, err)
		}
		if n != len(cands) {
			t.Errorf("count=%d but the listing returned %d rows under %+v", n, len(cands), opts)
		}
		out := map[string]AutoOptimizeCandidate{}
		for _, c := range cands {
			out[c.Path] = c
		}
		return out
	}
	keys := func(m map[string]AutoOptimizeCandidate) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	equal := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	if got := keys(list(EligibilityOpts{})); !equal(got, []string{"P/01.flac"}) {
		t.Errorf("pcm-only candidates = %v, want [P/01.flac]", got)
	}
	on := list(EligibilityOpts{DSDRender: true})
	if got := keys(on); !equal(got, []string{"D/01.dsf", "P/01.flac"}) {
		t.Errorf("dsd candidates = %v, want [D/01.dsf P/01.flac]", got)
	}
	if c := on["D/01.dsf"]; !c.IsDSD || c.Compression != "" || c.DurationSec != 300.5 || c.Channels != 2 ||
		c.SampleRate != 2822400 || c.BitsPerSample != 1 || c.Codec != "DSF" {
		t.Errorf("DSD candidate row = %+v, want IsDSD, duration 300.5, 2 channels, 2822400/1 DSF", c)
	}
	if c := on["P/01.flac"]; c.IsDSD || c.DurationSec != 0 || c.Channels != 0 {
		t.Errorf("PCM candidate row carries DSD-only facts: %+v", c)
	}
	dst := list(EligibilityOpts{DSDRender: true, DST: true})
	if got := keys(dst); !equal(got, []string{"D/01.dsf", "D/02.dff", "P/01.flac"}) {
		t.Errorf("dsd+dst candidates = %v, want all three", got)
	}
	if c := dst["D/02.dff"]; c.Compression != "DST" {
		t.Errorf("DST candidate Compression = %q, want DST", c.Compression)
	}

	// A fresh compact rendition is coverage (its prefix is the optimize
	// prefix); a faithful one is a different tier and is not.
	m1, s1 := trackRowMTimeAndSize(t, s, "D/01.dsf")
	seedOptimizeVariant(t, s, "D/01.dsf", "optimized-dsd-v1-44100-16", m1, s1)
	m2, s2 := trackRowMTimeAndSize(t, s, "D/02.dff")
	seedOptimizeVariant(t, s, "D/02.dff", "pcm-v1-176400-24", m2, s2)
	if got := keys(list(EligibilityOpts{DSDRender: true, DST: true})); !equal(got, []string{"D/02.dff", "P/01.flac"}) {
		t.Errorf("after the renditions landed: %v, want [D/02.dff P/01.flac]", got)
	}
}

// TestTrackProjectionCarriesCompression — both projection readers
// surface the v43 accelerator, so a DSD-render gate can refuse DST from
// projected columns alone.
func TestTrackProjectionCarriesCompression(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedDSDTrack(t, s, "D/01.dff", "DFF", 2822400, "DST", 0, 0)
	seedDSDTrack(t, s, "D/02.dsf", "DSF", 2822400, "", 0, 0)

	under, err := s.ListTrackProjectionsUnderPrefix(ctx, "D", VariantKindPrefixOptimized)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]TrackProjection{}
	for _, p := range under {
		byPath[p.Path] = p
	}
	if p := byPath["D/01.dff"]; p.Compression != "DST" || !p.IsDSD {
		t.Errorf("D/01.dff projection = %+v, want Compression DST + IsDSD", p)
	}
	if p := byPath["D/02.dsf"]; p.Compression != "" {
		t.Errorf("D/02.dsf projection Compression = %q, want empty", p.Compression)
	}
	forPaths, err := s.TrackProjectionsForPaths(ctx, []string{"D/01.dff"}, VariantKindPrefixPCM)
	if err != nil {
		t.Fatal(err)
	}
	if len(forPaths) != 1 || forPaths[0].Compression != "DST" {
		t.Errorf("TrackProjectionsForPaths = %+v, want one DST row", forPaths)
	}
}

// TestInsertUpscaleBatchRoundTripsKind — the v43 `kind` column follows
// the row through insert and list; a pre-v43-shaped row (no kind) reads
// back "" so the readers' sentinel derivation still applies to it.
func TestInsertUpscaleBatchRoundTripsKind(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	pcm := UpscaleBatchRow{ID: uuid.New(), Path: "DSD", TargetRate: 176400, TargetBits: 24, Kind: "pcm",
		Status: "pending", CreatedAt: 2, UpdatedAt: 2}
	legacy := UpscaleBatchRow{ID: uuid.New(), Path: "", TargetRate: 0, TargetBits: 16,
		Status: "pending", CreatedAt: 1, UpdatedAt: 1}
	for _, row := range []UpscaleBatchRow{pcm, legacy} {
		if err := s.InsertUpscaleBatch(ctx, row); err != nil {
			t.Fatalf("InsertUpscaleBatch: %v", err)
		}
	}
	rows, err := s.ListUpscaleBatches(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[uuid.UUID]string{}
	for _, r := range rows {
		kinds[r.ID] = r.Kind
	}
	if kinds[pcm.ID] != "pcm" {
		t.Errorf("pcm batch kind = %q, want pcm", kinds[pcm.ID])
	}
	if got, ok := kinds[legacy.ID]; !ok || got != "" {
		t.Errorf("legacy batch kind = %q (present %v), want the empty pre-v43 shape", got, ok)
	}
}
