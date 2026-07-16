package manifest

import (
	"context"
	"database/sql"
	"testing"
)

// seedFormatTrack upserts a track with the given geometry. rate<=0 /
// bits<=0 leave the pointer nil (unknown geometry).
func seedFormatTrack(t *testing.T, s *Store, path, codec string, rate float64, bits int, isDSD bool) {
	t.Helper()
	tr := &Track{Path: path, Size: 1_000_000, Codec: codec, IsDSD: &isDSD}
	if rate > 0 {
		tr.SampleRate = &rate
	}
	if bits > 0 {
		tr.BitsPerSample = &bits
	}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("UpsertTrack %q: %v", path, err)
	}
}

func seedVariantFor(t *testing.T, s *Store, sourcePath, variantID string) {
	t.Helper()
	if err := s.UpsertVariant(context.Background(), VariantRow{
		SourcePath: sourcePath, VariantID: variantID,
		SidecarPath: "/tmp/" + variantID + ".flac", Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1000,
		SourceMTimeNS: 1, SourceSize: 1_000_000, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertVariant %q: %v", sourcePath, err)
	}
}

// TestUpsertTrackStampsFormatColumns pins the v25 write-path contract:
// both upsert paths stamp the format-fact columns from the Track's own
// fields, with nil pointers landing as SQL NULL (unknown preserved).
func TestUpsertTrackStampsFormatColumns(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	seedFormatTrack(t, s, "A/hi.flac", "FLAC", 96000, 24, false)
	var rate, bits, isDSD sql.NullInt64
	var codec sql.NullString
	if err := s.db.QueryRow(
		`SELECT sample_rate, bits_per_sample, is_dsd, codec FROM tracks WHERE path = ?`,
		"A/hi.flac").Scan(&rate, &bits, &isDSD, &codec); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rate.Int64 != 96000 || bits.Int64 != 24 || isDSD.Int64 != 0 || codec.String != "FLAC" {
		t.Errorf("stamped columns = (%v, %v, %v, %v), want (96000, 24, 0, FLAC)",
			rate.Int64, bits.Int64, isDSD.Int64, codec.String)
	}

	// Unknown geometry → NULLs, not zeros: the eligibility SQL's
	// COALESCE(...,0) arms rely on NULL meaning "unknown".
	if err := s.UpsertTrack(context.Background(), &Track{Path: "A/mystery.bin", Size: 5}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	if err := s.db.QueryRow(
		`SELECT sample_rate, bits_per_sample, is_dsd, codec FROM tracks WHERE path = ?`,
		"A/mystery.bin").Scan(&rate, &bits, &isDSD, &codec); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rate.Valid || bits.Valid || isDSD.Valid || codec.Valid {
		t.Errorf("unknown-geometry row stamped (%v, %v, %v, %v), want all NULL",
			rate, bits, isDSD, codec)
	}

	// Batch path mirrors the single path.
	r2 := 44100.0
	b2 := 16
	dsd2 := false
	if err := s.UpsertTrackBatch(context.Background(), []*Track{{
		Path: "B/cd.flac", Size: 7, Codec: "FLAC",
		SampleRate: &r2, BitsPerSample: &b2, IsDSD: &dsd2,
	}}); err != nil {
		t.Fatalf("UpsertTrackBatch: %v", err)
	}
	if err := s.db.QueryRow(
		`SELECT sample_rate, bits_per_sample FROM tracks WHERE path = ?`,
		"B/cd.flac").Scan(&rate, &bits); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rate.Int64 != 44100 || bits.Int64 != 16 {
		t.Errorf("batch-stamped = (%d, %d), want (44100, 16)", rate.Int64, bits.Int64)
	}
}

// TestBackfillFormatColumns pins the migration backfill: it recomputes
// the four columns from tags_json and touches NOTHING else — in
// particular enriched_at (the enricher's queue driver) and indexed_at
// (the iOS delta-sync clock) must be byte-identical before/after.
func TestBackfillFormatColumns(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	seedFormatTrack(t, s, "A/hi.flac", "FLAC", 96000, 24, false)
	seedFormatTrack(t, s, "A/dsd.dsf", "DSF", 2822400, 1, true)

	type clocks struct{ enriched, indexed int64 }
	readClocks := func() map[string]clocks {
		t.Helper()
		rows, err := s.db.Query(`SELECT path, enriched_at, indexed_at FROM tracks`)
		if err != nil {
			t.Fatalf("clocks: %v", err)
		}
		defer rows.Close()
		out := map[string]clocks{}
		for rows.Next() {
			var p string
			var c clocks
			if err := rows.Scan(&p, &c.enriched, &c.indexed); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[p] = c
		}
		return out
	}
	before := readClocks()

	// Simulate a pre-v25 DB: null the columns, then backfill.
	if _, err := s.db.Exec(`UPDATE tracks SET sample_rate=NULL, bits_per_sample=NULL, is_dsd=NULL, codec=NULL`); err != nil {
		t.Fatalf("null columns: %v", err)
	}
	if err := backfillFormatColumns(s.db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var rate, bits, isDSD sql.NullInt64
	var codec sql.NullString
	if err := s.db.QueryRow(
		`SELECT sample_rate, bits_per_sample, is_dsd, codec FROM tracks WHERE path = ?`,
		"A/dsd.dsf").Scan(&rate, &bits, &isDSD, &codec); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rate.Int64 != 2822400 || bits.Int64 != 1 || isDSD.Int64 != 1 || codec.String != "DSF" {
		t.Errorf("backfilled = (%d, %d, %d, %q), want (2822400, 1, 1, DSF)",
			rate.Int64, bits.Int64, isDSD.Int64, codec.String)
	}
	if got := readClocks(); len(got) != len(before) {
		t.Fatalf("row count changed: %d → %d", len(before), len(got))
	} else {
		for p, b := range before {
			if got[p] != b {
				t.Errorf("clocks moved for %q: before=%+v after=%+v — backfill must not touch enriched_at/indexed_at", p, b, got[p])
			}
		}
	}
}

// TestEligibleCountsForFolders pins the eligible-denominator semantics
// across the format matrix (target 192 kHz / 24-bit for the upscale
// arm; the optimize arm is target-independent).
func TestEligibleCountsForFolders(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	// AtFloor: CD-quality FLAC — nothing for optimize (at the CarPlay
	// floor), below the upscale target so upscale-eligible.
	seedFormatTrack(t, s, "AtFloor/01.flac", "FLAC", 44100, 16, false)
	seedFormatTrack(t, s, "AtFloor/02.flac", "FLAC", 48000, 16, false)
	// HiRes: above the floor; one covered per kind.
	seedFormatTrack(t, s, "HiRes/01.flac", "FLAC", 96000, 24, false)
	seedFormatTrack(t, s, "HiRes/02.flac", "FLAC", 96000, 24, false)
	seedVariantFor(t, s, "HiRes/01.flac", "optimized-v1-44100-16")
	seedVariantFor(t, s, "HiRes/02.flac", "upscaled-v2-192000-24")
	// DSD: excluded from both kinds.
	seedFormatTrack(t, s, "DSD/01.dsf", "DSF", 2822400, 1, true)
	// Lossy: not optimize-eligible (PCM allowlist); IS upscale-eligible
	// today (Submit has no codec gate — parity, see upscaleEligibleSQL).
	seedFormatTrack(t, s, "Lossy/01.mp3", "MP3", 44100, 16, false)
	// Unknown geometry: excluded from both.
	seedFormatTrack(t, s, "Unknown/01.flac", "", 0, 0, false)
	// Legacy codec-empty row WITH geometry: the extension fallback
	// makes it optimize-eligible.
	seedFormatTrack(t, s, "ExtFallback/01.flac", "", 96000, 24, false)
	// Done: at the upscale target but covered — stays in the
	// denominator via the covered arm (a hot target change must not
	// push a covered bar above 100%).
	seedFormatTrack(t, s, "Done/01.flac", "FLAC", 192000, 24, false)
	seedVariantFor(t, s, "Done/01.flac", "upscaled-v2-192000-24")

	paths := []string{"AtFloor", "HiRes", "DSD", "Lossy", "Unknown", "ExtFallback", "Done"}
	got, err := s.EligibleCountsForFolders(context.Background(), paths, 192000, 24)
	if err != nil {
		t.Fatalf("EligibleCountsForFolders: %v", err)
	}
	want := map[string]EligibleCounts{
		"AtFloor":     {Upscale: 2, Optimize: 0},
		"HiRes":       {Upscale: 2, Optimize: 2},
		"DSD":         {Upscale: 0, Optimize: 0},
		"Lossy":       {Upscale: 1, Optimize: 0},
		"Unknown":     {Upscale: 0, Optimize: 0},
		"ExtFallback": {Upscale: 1, Optimize: 1},
		"Done":        {Upscale: 1, Optimize: 1},
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s = %+v, want %+v", p, got[p], w)
		}
	}
}

// TestEligibleCountsForFolders_bindingOrder pins the (rate, bits,
// rate, bits) bind order of upscaleEligibleSQL: a track whose rate and
// bits values would SWAP into a different verdict distinguishes the
// correct order from a transposed one. Track (rate=100, bits=200)
// against target (rate=200, bits=100): correctly bound, bits 200 >
// targetBits 100 → NOT eligible; a (rate, rate, bits, bits)
// transposition would call it eligible.
func TestEligibleCountsForFolders_bindingOrder(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedFormatTrack(t, s, "Bind/01.flac", "FLAC", 100, 200, false)

	got, err := s.EligibleCountsForFolders(context.Background(), []string{"Bind"}, 200, 100)
	if err != nil {
		t.Fatalf("EligibleCountsForFolders: %v", err)
	}
	if got["Bind"].Upscale != 0 {
		t.Errorf("Upscale = %d, want 0 — binding order regressed (rate/bits transposed?)", got["Bind"].Upscale)
	}
}

// TestEligibleRollupByPrefix pins the whole-subtree twin, including
// the empty-prefix (whole library) branch.
func TestEligibleRollupByPrefix(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	seedFormatTrack(t, s, "AtFloor/01.flac", "FLAC", 44100, 16, false)
	seedFormatTrack(t, s, "HiRes/01.flac", "FLAC", 96000, 24, false)
	seedFormatTrack(t, s, "DSD/01.dsf", "DSF", 2822400, 1, true)
	seedVariantFor(t, s, "HiRes/01.flac", "optimized-v1-44100-16")

	all, err := s.EligibleRollupByPrefix(context.Background(), "", 192000, 24)
	if err != nil {
		t.Fatalf("EligibleRollupByPrefix(all): %v", err)
	}
	if all.Upscale != 2 || all.Optimize != 1 {
		t.Errorf("whole library = %+v, want {Upscale:2 Optimize:1}", all)
	}
	one, err := s.EligibleRollupByPrefix(context.Background(), "HiRes", 192000, 24)
	if err != nil {
		t.Fatalf("EligibleRollupByPrefix(HiRes): %v", err)
	}
	if one.Upscale != 1 || one.Optimize != 1 {
		t.Errorf("HiRes = %+v, want {Upscale:1 Optimize:1}", one)
	}
}
