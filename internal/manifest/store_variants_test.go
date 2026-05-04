package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestVariantCRUDRoundTrip pins the basic insert/get/list/delete
// shape on the new track_variants table. Underpins every higher-
// level test below.
func TestVariantCRUDRoundTrip(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	// First need a parent track row — track_variants has a FK on it.
	upsertParent(t, s, "Music/Album/01.flac")

	row := VariantRow{
		SourcePath:    "Music/Album/01.flac",
		VariantID:     "upscaled-v1-176400-24",
		SidecarPath:   "/tmp/x/abc-upscaled-v1-176400-24.flac",
		Format:        "flac",
		SampleRate:    176400,
		BitsPerSample: 24,
		SizeBytes:     12345678,
		SourceMTimeNS: time.Now().UnixNano(),
		SourceSize:    7654321,
		SoxSettings:   `{"resampler":"sox"}`,
		CreatedAt:     time.Now().UnixNano(),
	}
	if err := s.UpsertVariant(row); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	got, err := s.GetVariant(row.SourcePath, row.VariantID)
	if err != nil {
		t.Fatalf("GetVariant: %v", err)
	}
	if got == nil {
		t.Fatal("GetVariant: row not found")
	}
	if got.SidecarPath != row.SidecarPath {
		t.Errorf("SidecarPath: got %q want %q", got.SidecarPath, row.SidecarPath)
	}
	if got.SampleRate != row.SampleRate {
		t.Errorf("SampleRate: got %d want %d", got.SampleRate, row.SampleRate)
	}

	all, err := s.AllVariants()
	if err != nil {
		t.Fatalf("AllVariants: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("AllVariants: got %d rows, want 1", len(all))
	}

	if err := s.DeleteVariant(row.SourcePath, row.VariantID); err != nil {
		t.Fatalf("DeleteVariant: %v", err)
	}
	got2, _ := s.GetVariant(row.SourcePath, row.VariantID)
	if got2 != nil {
		t.Fatal("DeleteVariant: row still present after delete")
	}
}

// TestUpsertVariantReplacesExisting proves the ON CONFLICT clause
// of UpsertVariant updates instead of inserting a duplicate. Test
// for `bridge upscale --force` re-converting an existing variant.
func TestUpsertVariantReplacesExisting(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")

	first := VariantRow{
		SourcePath: "Music/A/1.flac", VariantID: "upscaled-v1-176400-24",
		SidecarPath: "/tmp/old.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24,
		SizeBytes: 100, SourceMTimeNS: 1, SourceSize: 1,
		SoxSettings: "{}", CreatedAt: 1,
	}
	if err := s.UpsertVariant(first); err != nil {
		t.Fatal(err)
	}

	second := first
	second.SidecarPath = "/tmp/new.flac"
	second.SizeBytes = 200
	second.CreatedAt = 99
	if err := s.UpsertVariant(second); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetVariant(first.SourcePath, first.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SidecarPath != "/tmp/new.flac" || got.SizeBytes != 200 || got.CreatedAt != 99 {
		t.Errorf("UpsertVariant did not replace: got %+v", got)
	}

	all, _ := s.AllVariants()
	if len(all) != 1 {
		t.Errorf("AllVariants: expected 1 row after replace, got %d", len(all))
	}
}

// TestListTracksSplicesVariants is the load-bearing integration:
// ListTracks must return Track.Variants populated from the
// `json_group_array` LEFT JOIN. Without this, the manifest serves
// the bare track and iOS never sees the variant.
func TestListTracksSplicesVariants(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")
	upsertParent(t, s, "Music/A/2.flac")

	// Two variants on track 1, none on track 2 — covers the
	// non-empty + empty branches of the aggregation.
	for _, vid := range []string{"upscaled-v1-176400-24", "upscaled-v1-352800-32"} {
		if err := s.UpsertVariant(VariantRow{
			SourcePath: "Music/A/1.flac", VariantID: vid,
			SidecarPath: "/tmp/" + vid + ".flac", Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: 10,
			SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	tracks, err := s.ListTracks(nil)
	if err != nil {
		t.Fatalf("ListTracks: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("ListTracks: got %d tracks, want 2", len(tracks))
	}

	byPath := make(map[string]Track, len(tracks))
	for _, tr := range tracks {
		byPath[tr.Path] = tr
	}

	t1 := byPath["Music/A/1.flac"]
	if len(t1.Variants) != 2 {
		t.Errorf("Music/A/1.flac variants: got %d, want 2", len(t1.Variants))
	}
	for _, v := range t1.Variants {
		if v.Format != "flac" {
			t.Errorf("variant.Format = %q, want flac", v.Format)
		}
		if v.Label == "" {
			t.Errorf("variant.Label empty (humanLabelForVariant should have populated it)")
		}
	}

	t2 := byPath["Music/A/2.flac"]
	if len(t2.Variants) != 0 {
		t.Errorf("Music/A/2.flac (no variants) got %d entries (want 0)", len(t2.Variants))
	}
}

// TestDeleteTrackRemovesSidecarFiles is the regression test for
// the orphan-sidecar trap (CASCADE deletes the DB row but not the
// `.flac` on disk). Pre-fix: 100MB+ files would leak per deleted
// source, and `--gc` couldn't find them by querying the DB
// (because the row was already gone).
func TestDeleteTrackRemovesSidecarFiles(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	tempDir := t.TempDir()
	upsertParent(t, s, "Music/A/1.flac")

	// Create two real sidecar files — proves DeleteTrack handles
	// multi-variant tracks correctly.
	side1 := filepath.Join(tempDir, "abc-upscaled-v1-176400-24.flac")
	side2 := filepath.Join(tempDir, "abc-upscaled-v1-352800-32.flac")
	for _, p := range []string{side1, side2} {
		if err := os.WriteFile(p, []byte("not really a flac"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for vid, path := range map[string]string{
		"upscaled-v1-176400-24": side1,
		"upscaled-v1-352800-32": side2,
	} {
		if err := s.UpsertVariant(VariantRow{
			SourcePath: "Music/A/1.flac", VariantID: vid,
			SidecarPath: path, Format: "flac",
			SampleRate: 176400, BitsPerSample: 24,
			SizeBytes: 17, SourceMTimeNS: 1, SourceSize: 1,
			SoxSettings: "{}", CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.DeleteTrack("Music/A/1.flac"); err != nil {
		t.Fatalf("DeleteTrack: %v", err)
	}

	for _, p := range []string{side1, side2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("sidecar %q should have been removed by DeleteTrack (err: %v)", p, err)
		}
	}

	// CASCADE should have removed the variant rows too.
	all, _ := s.AllVariants()
	if len(all) != 0 {
		t.Errorf("variants should have been cascaded; got %d rows", len(all))
	}
}

// TestDeleteTrackToleratesMissingSidecar — best-effort cleanup
// must not block the parent DELETE when a sidecar is already
// gone (manual rm, OS-side cleanup, double-delete races). The
// scanner needs the row gone regardless of disk state.
func TestDeleteTrackToleratesMissingSidecar(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")

	// Insert variant pointing at a path that doesn't exist on disk.
	if err := s.UpsertVariant(VariantRow{
		SourcePath: "Music/A/1.flac", VariantID: "upscaled-v1-176400-24",
		SidecarPath: "/does/not/exist/abc.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 1,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteTrack("Music/A/1.flac"); err != nil {
		t.Fatalf("DeleteTrack should swallow missing-sidecar errors: %v", err)
	}

	got, _ := s.GetTrack("Music/A/1.flac")
	if got != nil {
		t.Error("DeleteTrack: row still present despite cleanup tolerance")
	}
}

// TestVariantsHumanLabel pins the iOS-facing label format — any
// drift here will look wrong in the picker UI.
func TestVariantsHumanLabel(t *testing.T) {
	cases := []struct {
		v    Variant
		want string
	}{
		{Variant{Format: "flac", BitsPerSample: 24, SampleRate: 176400}, "Upscaled FLAC 24/176.4"},
		{Variant{Format: "flac", BitsPerSample: 24, SampleRate: 192000}, "Upscaled FLAC 24/192"},
		{Variant{Format: "flac", BitsPerSample: 32, SampleRate: 352800}, "Upscaled FLAC 32/352.8"},
	}
	for _, c := range cases {
		got := humanLabelForVariant(c.v)
		if got != c.want {
			t.Errorf("humanLabelForVariant(%+v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// TestUpsertVariantBumpsParentIndexedAt: when a variant is written for
// a track, the parent row's `indexed_at` MUST advance to the timestamp
// that `Store.now()` returns at the call site. Without this, an iOS
// client running an incremental sync after submitting an upscale
// request never sees the new variant — the delta-filtered manifest
// (`WHERE indexed_at > since`) skips the parent row entirely.
//
// Uses an injected clock so the assertion is deterministic. No
// time.Sleep — eliminates CI flakiness on slow runners.
func TestUpsertVariantBumpsParentIndexedAt(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")
	// Inject a stepping clock AFTER UpsertTrack so the parent's
	// `indexed_at` (set via direct `time.Now()`, outside this PR's
	// scope) doesn't overlap with our injected timestamps. Baseline
	// is parent's `indexed_at + 1h` so each subsequent step is
	// unambiguously past it.
	var step int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&step); err != nil {
		t.Fatalf("read parent indexed_at for clock seed: %v", err)
	}
	step += (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time {
		step += 1_000_000_000 // +1 sec per call; arbitrary but >> wall-clock jitter
		return time.Unix(0, step)
	}
	// Capture the parent's `indexed_at` from before UpsertVariant
	// runs (set by UpsertTrack above). The test asserts UpsertVariant
	// strictly advances it past this value AND lands at exactly the
	// timestamp s.now() returned at the call site.
	var beforeIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&beforeIndexedAt); err != nil {
		t.Fatalf("read parent indexed_at: %v", err)
	}

	// Next s.now() will return step+1s. UpsertVariant should write
	// exactly that value into tracks.indexed_at for the parent row.
	expectedIndexedAt := step + 1_000_000_000
	if err := s.UpsertVariant(VariantRow{
		SourcePath: "Music/A/1.flac", VariantID: "upscaled-v1-176400-24",
		SidecarPath: "/tmp/x.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 100,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	var afterIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&afterIndexedAt); err != nil {
		t.Fatalf("read parent indexed_at after UpsertVariant: %v", err)
	}

	if afterIndexedAt != expectedIndexedAt {
		t.Errorf("parent indexed_at = %d, want %d (before=%d, %d ns advance)",
			afterIndexedAt, expectedIndexedAt, beforeIndexedAt,
			afterIndexedAt-beforeIndexedAt)
	}
	if afterIndexedAt <= beforeIndexedAt {
		t.Errorf("parent indexed_at did not advance: before=%d after=%d",
			beforeIndexedAt, afterIndexedAt)
	}
}

// TestUpsertVariantDeltaManifestSurfacesNewVariant is the regression-
// locking test for the user-visible bug. Mimics the exact iOS flow:
//
//  1. Track inserted at T0 (UpsertTrack sets `indexed_at = T0`).
//  2. iOS finishes its initial sync with `share.lastScanFinishedAt = T0`.
//  3. User submits an upscale; bridge produces variant at T1 > T0.
//  4. iOS triggers a delta-sync with `since = T0`.
//  5. Bridge runs `ListTracks(since: &T0)` — `WHERE indexed_at > T0`.
//
// Without the bump in UpsertVariant, the parent row's `indexed_at`
// is still T0, so it doesn't pass the `> T0` filter; the delta-sync
// returns zero rows and iOS never sees the variant. WITH the bump,
// the row's `indexed_at` advances to T1, the row is returned, AND
// its `Variants` slice contains the new variant.
//
// If this test ever fails, EITHER the UpsertVariant bump regressed,
// OR the transaction silently rolled back. Single-failure → root
// cause is in one of those two places.
func TestUpsertVariantDeltaManifestSurfacesNewVariant(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")

	// Read T0 (the timestamp UpsertTrack stamped on the parent row).
	var t0 int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&t0); err != nil {
		t.Fatalf("read T0: %v", err)
	}

	// Inject a clock that returns T0 + 1 hour for the variant write
	// — well past anything UpsertTrack could have set, so the delta
	// filter unambiguously sees the bumped row.
	t1 := t0 + (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, t1) }

	if err := s.UpsertVariant(VariantRow{
		SourcePath: "Music/A/1.flac", VariantID: "upscaled-v1-192000-24",
		SidecarPath: "/tmp/u.flac", Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 100,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: t1,
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	// Delta-sync from T0: the iOS-equivalent of "what changed since
	// my last finished scan".
	since := time.Unix(0, t0)
	delta, err := s.ListTracks(&since)
	if err != nil {
		t.Fatalf("ListTracks(since=T0): %v", err)
	}

	if len(delta) != 1 {
		t.Fatalf("delta returned %d rows, want 1 (the parent track whose variant just landed)", len(delta))
	}
	tr := delta[0]
	if tr.Path != "Music/A/1.flac" {
		t.Errorf("delta row path = %q, want %q", tr.Path, "Music/A/1.flac")
	}
	if len(tr.Variants) != 1 {
		t.Fatalf("delta row variants = %d, want 1", len(tr.Variants))
	}
	if tr.Variants[0].SampleRate != 192000 {
		t.Errorf("variant sample rate = %v, want 192000", tr.Variants[0].SampleRate)
	}
}

// TestDeleteVariantBumpsParentIndexedAt: mirror coverage for
// DeleteVariant. iOS needs to see variant-removals via delta-sync
// for the same reason it needs to see variant-additions: a stale
// `Track.bridgeVariants` after a server-side `--gc` would let the
// iOS picker offer a variant whose sidecar no longer exists, and
// the next `/v1/download?variant=` would 404.
//
// DeleteVariant has no production callers today (the GC sweep is
// filesystem-only — see runGC in cmd/bridge/upscale.go), so this
// is forward-looking coverage. Bump symmetry matches UpsertVariant.
func TestDeleteVariantBumpsParentIndexedAt(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")

	// Read the parent's UpsertTrack-stamped indexed_at to seed the
	// clock past it (the MAX guard would otherwise hold indexed_at
	// at the parent's value).
	var parentIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&parentIndexedAt); err != nil {
		t.Fatalf("read parent indexed_at: %v", err)
	}

	// First insert a variant to delete. Clock returns parent + 1h
	// so the UpsertVariant bump unambiguously advances past the
	// UpsertTrack-stamped value (MAX guard succeeds).
	preDeleteAt := parentIndexedAt + (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, preDeleteAt) }
	if err := s.UpsertVariant(VariantRow{
		SourcePath: "Music/A/1.flac", VariantID: "upscaled-v1-176400-24",
		SidecarPath: "/tmp/x.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 100,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertVariant (setup): %v", err)
	}

	var beforeDelete int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&beforeDelete); err != nil {
		t.Fatalf("read pre-delete indexed_at: %v", err)
	}
	if beforeDelete != preDeleteAt {
		t.Fatalf("pre-delete indexed_at = %d, want %d (UpsertVariant bump should have set it)", beforeDelete, preDeleteAt)
	}

	// Now flip the clock forward and delete. DeleteVariant must
	// bump indexed_at past the pre-delete value.
	expectedAfter := preDeleteAt + (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, expectedAfter) }
	if err := s.DeleteVariant("Music/A/1.flac", "upscaled-v1-176400-24"); err != nil {
		t.Fatalf("DeleteVariant: %v", err)
	}

	var afterDelete int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&afterDelete); err != nil {
		t.Fatalf("read post-delete indexed_at: %v", err)
	}
	if afterDelete != expectedAfter {
		t.Errorf("post-delete indexed_at = %d, want %d", afterDelete, expectedAfter)
	}
}

// TestDeleteVariantNoOpSkipsBump: when the requested
// (source_path, variant_id) doesn't exist, the parent row's
// `indexed_at` MUST NOT advance — there's no actual variant-set
// change to propagate to iOS via delta-sync, and bumping anyway
// would create false manifest churn (CodeRabbit + Gemini on PR #156).
func TestDeleteVariantNoOpSkipsBump(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")
	var beforeIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&beforeIndexedAt); err != nil {
		t.Fatalf("read parent indexed_at: %v", err)
	}

	// Inject a clock that would advance indexed_at if the bump
	// fired. The test asserts it does NOT.
	expectedNoBump := beforeIndexedAt + (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, expectedNoBump) }

	// Delete a (path, variantID) pair that doesn't exist. RowsAffected==0.
	if err := s.DeleteVariant("Music/A/1.flac", "nonexistent-variant-id"); err != nil {
		t.Fatalf("DeleteVariant (no-op): %v", err)
	}

	var afterIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&afterIndexedAt); err != nil {
		t.Fatalf("read parent indexed_at after no-op delete: %v", err)
	}
	if afterIndexedAt != beforeIndexedAt {
		t.Errorf("no-op DeleteVariant unexpectedly bumped indexed_at: before=%d after=%d (clock would have set %d)",
			beforeIndexedAt, afterIndexedAt, expectedNoBump)
	}
}

// TestUpsertVariantMonotonicGuard: an injected clock that returns a
// timestamp in the PAST must NOT regress the parent row's
// `indexed_at` — the SQL `MAX(indexed_at, ?)` form makes the bump
// monotonic, preserving the `WHERE indexed_at > since` delta-sync
// invariant under any clock-rewind condition (test injection, NTP
// step, manual wall-clock change). (Qodo on PR #156.)
func TestUpsertVariantMonotonicGuard(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")
	var initialIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&initialIndexedAt); err != nil {
		t.Fatalf("read parent indexed_at: %v", err)
	}

	// Inject a clock that returns a timestamp WAY in the past — if
	// the guard isn't applied, the UPDATE would regress indexed_at
	// to this value and break delta-sync.
	pastTimestamp := initialIndexedAt - (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, pastTimestamp) }

	if err := s.UpsertVariant(VariantRow{
		SourcePath: "Music/A/1.flac", VariantID: "upscaled-v1-176400-24",
		SidecarPath: "/tmp/x.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 100,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	var afterIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&afterIndexedAt); err != nil {
		t.Fatalf("read parent indexed_at after UpsertVariant: %v", err)
	}
	if afterIndexedAt < initialIndexedAt {
		t.Errorf("indexed_at regressed under past-clock injection: before=%d after=%d (clock returned %d)",
			initialIndexedAt, afterIndexedAt, pastTimestamp)
	}
	// The MAX(...) form should leave indexed_at unchanged when the
	// new value is in the past — equality is the expected outcome.
	if afterIndexedAt != initialIndexedAt {
		t.Errorf("expected indexed_at to stay at %d (MAX guard), got %d", initialIndexedAt, afterIndexedAt)
	}
}

// --- helpers ---

func openTempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s
}

// upsertParent creates a minimum-viable Track row at the given path
// so the FK on track_variants is satisfied. We don't care about tag
// content for these tests — only that the row exists.
func upsertParent(t *testing.T, s *Store, path string) {
	t.Helper()
	if err := s.UpsertTrack(&Track{
		Path:    path,
		Size:    100,
		ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack(%q): %v", path, err)
	}
}
