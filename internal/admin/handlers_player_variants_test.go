package admin

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedVariantAlbum stages one album of hi-res FLAC plus a DSD track, so
// the fixture covers both a track that CAN gain variants and one that
// never can.
func seedVariantAlbum(t *testing.T, st *manifest.Store) {
	t.Helper()
	rate, bits, no := 96000.0, 24, false
	dsdRate, dsdBits, yes := 2822400.0, 1, true
	for _, tr := range []*manifest.Track{
		{Path: "Hi/Album/01.flac", Title: "One", Album: "Album", AlbumArtist: "Artist",
			Artist: "Artist", Codec: "FLAC", Size: 1000, ModTime: time.Unix(7, 0),
			SampleRate: &rate, BitsPerSample: &bits, IsDSD: &no},
		{Path: "Hi/Album/02.flac", Title: "Two", Album: "Album", AlbumArtist: "Artist",
			Artist: "Artist", Codec: "FLAC", Size: 2000, ModTime: time.Unix(7, 0),
			SampleRate: &rate, BitsPerSample: &bits, IsDSD: &no},
		{Path: "Hi/Album/03.dsf", Title: "Three", Album: "Album", AlbumArtist: "Artist",
			Artist: "Artist", Codec: "DSF", Size: 3000, ModTime: time.Unix(7, 0),
			SampleRate: &dsdRate, BitsPerSample: &dsdBits, IsDSD: &yes},
	} {
		if err := st.UpsertTrack(t.Context(), tr); err != nil {
			t.Fatal(err)
		}
	}
}

func albumDetailBody(t *testing.T, srv *Server, title string) map[string]any {
	t.Helper()
	id := albumIDByTitle(t, srv, title)
	w, body := playerGet(t, srv, "/api/player/albums/"+id)
	if w.Code != 200 {
		t.Fatalf("album detail: status %d body %s", w.Code, w.Body.String())
	}
	return body
}

func trackByTitle(t *testing.T, body map[string]any, title string) map[string]any {
	t.Helper()
	tracks, _ := body["tracks"].([]any)
	for _, raw := range tracks {
		tr, _ := raw.(map[string]any)
		if tr["title"] == title {
			return tr
		}
	}
	t.Fatalf("track %q not in detail: %v", title, tracks)
	return nil
}

// TestAlbumDetailReportsVariantPresence: a track's cached sidecars ride
// the detail response whether or not the source needs one for playback.
// The pre-existing `play.variantId` names a substitute for an
// unplayable source; these describe what EXISTS, which for a
// universally-playable FLAC is otherwise invisible.
func TestAlbumDetailReportsVariantPresence(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	seedVariantAlbum(t, st)
	if err := st.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: "Hi/Album/01.flac", VariantID: "optimized-v2-48000-16",
		SidecarPath: "x.flac", Format: "FLAC", SampleRate: 48000, BitsPerSample: 16,
		SizeBytes: 400, SourceMTimeNS: time.Unix(7, 0).UnixNano(), SourceSize: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	body := albumDetailBody(t, srv, "Album")
	one := trackByTitle(t, body, "One")
	vars, _ := one["variants"].([]any)
	if len(vars) != 1 {
		t.Fatalf("track One variants = %v, want one entry", one["variants"])
	}
	v, _ := vars[0].(map[string]any)
	if v["kind"] != "optimize" {
		t.Errorf("kind = %v, want \"optimize\" (the endpoint vocabulary, not the id prefix)", v["kind"])
	}
	if v["fresh"] != true {
		t.Errorf("fresh = %v, want true — stamped source facts match the track row", v["fresh"])
	}
	if v["sizeBytes"] != float64(400) {
		t.Errorf("sizeBytes = %v, want 400", v["sizeBytes"])
	}
	// The source is a plain FLAC, so playback needs no substitute. The
	// variant is still reported.
	if one["play"].(map[string]any)["variantId"] != nil {
		t.Errorf("play.variantId set for a universally-playable source: %v", one["play"])
	}

	if two := trackByTitle(t, body, "Two"); two["variants"] != nil {
		t.Errorf("track Two has no variants but reported %v", two["variants"])
	}
}

// TestAlbumDetailMarksStaleVariants: a sidecar whose stamped source
// facts no longer match the library's record is reported as NOT fresh.
// Showing it as simply "present" would promise a copy the serve path
// answers 410 for.
func TestAlbumDetailMarksStaleVariants(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	seedVariantAlbum(t, st)
	if err := st.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: "Hi/Album/01.flac", VariantID: "upscaled-v2-192000-24",
		SidecarPath: "x.flac", Format: "FLAC", SampleRate: 192000, BitsPerSample: 24,
		SizeBytes: 9000,
		// Source has since been re-encoded: neither stamp matches.
		SourceMTimeNS: time.Unix(1, 0).UnixNano(), SourceSize: 55,
	}); err != nil {
		t.Fatal(err)
	}
	one := trackByTitle(t, albumDetailBody(t, srv, "Album"), "One")
	vars, _ := one["variants"].([]any)
	if len(vars) != 1 {
		t.Fatalf("variants = %v, want one entry", one["variants"])
	}
	if v, _ := vars[0].(map[string]any); v["fresh"] != false {
		t.Errorf("fresh = %v, want false for a variant whose source moved on", v["fresh"])
	}
}

// TestAlbumDetailReportsVariantSkipReason: a DSD track can never gain
// either kind, and says so with the same vocabulary the browse rows
// use. Without it the UI would show an actionable-looking gap for work
// that is impossible.
func TestAlbumDetailReportsVariantSkipReason(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedVariantAlbum(t, srv.deps.Manifest)
	body := albumDetailBody(t, srv, "Album")

	if got := trackByTitle(t, body, "Three")["variantSkip"]; got != "dsd_bitstream" {
		t.Errorf("DSD track variantSkip = %v, want \"dsd_bitstream\"", got)
	}
	if got := trackByTitle(t, body, "One")["variantSkip"]; got != nil {
		t.Errorf("hi-res FLAC variantSkip = %v, want absent", got)
	}
}

// TestAlbumDetailVariantSummaryUsesAnEligibleDenominator is the
// coverage-bar contract. The album has three tracks, one of them DSD —
// so "1 of 2", not "1 of 3", and the DSD track is reported as exempt
// rather than as outstanding work.
func TestAlbumDetailVariantSummaryUsesAnEligibleDenominator(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	seedVariantAlbum(t, st)
	if err := st.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: "Hi/Album/01.flac", VariantID: "optimized-v2-48000-16",
		SidecarPath: "x.flac", Format: "FLAC", SampleRate: 48000, BitsPerSample: 16,
		SizeBytes: 400, SourceMTimeNS: time.Unix(7, 0).UnixNano(), SourceSize: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	body := albumDetailBody(t, srv, "Album")
	raw, ok := body["variants"].(map[string]any)
	if !ok {
		t.Fatalf("no variants summary in detail: %v", body["variants"])
	}
	var sum playerVariantSummaryDTO
	blob, _ := json.Marshal(raw)
	if err := json.Unmarshal(blob, &sum); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if sum.Optimize.Covered != 1 {
		t.Errorf("optimize covered = %d, want 1", sum.Optimize.Covered)
	}
	if sum.Optimize.Eligible != 2 {
		t.Errorf("optimize eligible = %d, want 2 (the DSD track can never be optimized)",
			sum.Optimize.Eligible)
	}
	if sum.Optimize.Exempt != 1 {
		t.Errorf("optimize exempt = %d, want 1", sum.Optimize.Exempt)
	}
	if sum.SourceBytes != 6000 {
		t.Errorf("sourceBytes = %d, want 6000", sum.SourceBytes)
	}
	if sum.VariantBytes != 400 {
		t.Errorf("variantBytes = %d, want 400", sum.VariantBytes)
	}
}

// TestAlbumDetailVariantSummarySeparatesOffFromNoSox: the two reasons a
// bridge cannot generate variants right now are different problems with
// different fixes, and a UI that collapses them tells the operator
// "unavailable" when the answer is "install sox".
func TestAlbumDetailVariantSummarySeparatesOffFromNoSox(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	seedVariantAlbum(t, srv.deps.Manifest)
	// A working toolchain with the feature switched OFF is the
	// discriminating case, and the only one that can catch a collapse:
	// with no sox probe wired, both fields read false whether or not
	// the code conflates them.
	srv.deps.UpscalePrecheck = func() error { return nil }

	body := albumDetailBody(t, srv, "Album")
	sum, _ := body["variants"].(map[string]any)
	if sum == nil {
		t.Fatal("no variants summary on a bridge with the feature off")
	}
	if sum["enabled"] != false {
		t.Errorf("enabled = %v, want false (upscale not configured in the fixture)", sum["enabled"])
	}
	if sum["soxAvailable"] != true {
		t.Errorf("soxAvailable = %v, want true — sox is present, the FEATURE is off, "+
			"and an operator needs to be told which one to change", sum["soxAvailable"])
	}

	// Flipping the config moves `enabled` and leaves `soxAvailable`
	// exactly where it was.
	cfg.Upscale.Enabled = true
	srv.deps.CfgHolder.Store(cfg)
	sum, _ = albumDetailBody(t, srv, "Album")["variants"].(map[string]any)
	if sum["enabled"] != true {
		t.Errorf("enabled = %v after enabling upscale, want true", sum["enabled"])
	}
	if sum["soxAvailable"] != true {
		t.Errorf("soxAvailable = %v, want true — the two are independent", sum["soxAvailable"])
	}

	// And a missing toolchain with the feature ON is the mirror image.
	srv.deps.UpscalePrecheck = func() error { return errNoSoxForTest }
	sum, _ = albumDetailBody(t, srv, "Album")["variants"].(map[string]any)
	if sum["enabled"] != true || sum["soxAvailable"] != false {
		t.Errorf("enabled=%v soxAvailable=%v, want true/false", sum["enabled"], sum["soxAvailable"])
	}
}

var errNoSoxForTest = errors.New("sox not found")

// TestAlbumDetailCountsStaleVariantsAsCoveredAndSaysSo pins a
// deliberately awkward pair of facts.
//
// A stale sidecar stays in COVERED, because that is what the batch
// walks do: Submit skips any track that already has a variant of the
// kind, freshness unread. Reporting it as missing would show an
// enabled Generate button that enqueues nothing.
//
// So the staleness has to be said separately, or a full bar quietly
// hides a copy that will never be served.
func TestAlbumDetailCountsStaleVariantsAsCoveredAndSaysSo(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	seedVariantAlbum(t, st)
	fresh := time.Unix(7, 0).UnixNano()
	for _, v := range []manifest.VariantRow{
		{SourcePath: "Hi/Album/01.flac", VariantID: "optimized-v2-48000-16",
			SidecarPath: "a.flac", Format: "FLAC", SampleRate: 48000, BitsPerSample: 16,
			SizeBytes: 400, SourceMTimeNS: fresh, SourceSize: 1000},
		// Same kind, source has since moved on.
		{SourcePath: "Hi/Album/02.flac", VariantID: "optimized-v2-48000-16",
			SidecarPath: "b.flac", Format: "FLAC", SampleRate: 48000, BitsPerSample: 16,
			SizeBytes: 400, SourceMTimeNS: 1, SourceSize: 99},
	} {
		if err := st.UpsertVariant(t.Context(), v); err != nil {
			t.Fatal(err)
		}
	}

	var sum playerVariantSummaryDTO
	blob, _ := json.Marshal(albumDetailBody(t, srv, "Album")["variants"])
	if err := json.Unmarshal(blob, &sum); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if sum.Optimize.Covered != 2 {
		t.Errorf("covered = %d, want 2 — a stale sidecar is still what Submit will skip",
			sum.Optimize.Covered)
	}
	if sum.Optimize.Stale != 1 {
		t.Errorf("stale = %d, want 1", sum.Optimize.Stale)
	}
}

// TestAlbumDetailStaleCountIsPerTrackNotPerRow: a track carrying a
// superseded sidecar BESIDE a current one is served correctly, so it is
// neither double-counted as covered nor flagged as stale. Counting rows
// would push the numerator past the denominator and send an operator
// hunting for a problem that is already solved.
//
// BOTH orderings are exercised, and that is the point of the table.
// VariantsForPaths returns rows ordered by variant_id, so a single
// fixture pins the tally only for whichever of fresh/stale that
// ordering happens to put last — a "last row wins" tally passed the
// first version of this test outright. The property is that ANY fresh
// row makes the track fresh, which must hold either way round.
func TestAlbumDetailStaleCountIsPerTrackNotPerRow(t *testing.T) {
	fresh := time.Unix(7, 0).UnixNano()
	// ids are returned in ascending order, so which of the two sorts
	// last is chosen here rather than left to chance.
	for _, tc := range []struct {
		name             string
		freshID, staleID string
	}{
		{"stale sorts last", "optimized-v1-44100-16", "optimized-v2-48000-16"},
		{"fresh sorts last", "optimized-v2-48000-16", "optimized-v1-44100-16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			st := srv.deps.Manifest
			seedVariantAlbum(t, st)
			for _, v := range []manifest.VariantRow{
				{SourcePath: "Hi/Album/01.flac", VariantID: tc.freshID,
					SidecarPath: "new.flac", Format: "FLAC", SampleRate: 48000, BitsPerSample: 16,
					SizeBytes: 400, SourceMTimeNS: fresh, SourceSize: 1000},
				{SourcePath: "Hi/Album/01.flac", VariantID: tc.staleID,
					SidecarPath: "old.flac", Format: "FLAC", SampleRate: 44100, BitsPerSample: 16,
					SizeBytes: 300, SourceMTimeNS: 1, SourceSize: 99},
			} {
				if err := st.UpsertVariant(t.Context(), v); err != nil {
					t.Fatal(err)
				}
			}

			var sum playerVariantSummaryDTO
			blob, _ := json.Marshal(albumDetailBody(t, srv, "Album")["variants"])
			if err := json.Unmarshal(blob, &sum); err != nil {
				t.Fatalf("decode summary: %v", err)
			}
			if sum.Optimize.Covered != 1 {
				t.Errorf("covered = %d, want 1 — two sidecars on one track is one covered track",
					sum.Optimize.Covered)
			}
			if sum.Optimize.Stale != 0 {
				t.Errorf("stale = %d, want 0 — a current copy sits beside the superseded one",
					sum.Optimize.Stale)
			}
		})
	}
}
