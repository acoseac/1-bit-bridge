package manifest

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func seedUPnPTrack(t *testing.T, s *Store, p string) {
	t.Helper()
	tr := &Track{Path: p, Size: 1, ModTime: time.Unix(0, 0).UTC()}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("seed track %q: %v", p, err)
	}
}

func TestUpsertUPnPRouting_GetRoundTrip(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	const p = "Chord 2Go/Music/4 Non Blondes/Album/01 - What's Up?.flac"
	seedUPnPTrack(t, s, p)

	now := time.Unix(1_700_000_000, 0).UTC()
	r := &UPnPRouting{
		SourcePath:     p,
		ServerUDN:      "uuid:4d696e69-444c-164e-9d41-00b78f5ae46a",
		ObjectID:       "64$0$0$0$0",
		ParentObjectID: "64$0$0$0",
		ResURL:         "http://192.168.0.62:8200/MediaItems/25.flac",
		ProtocolInfo:   "http-get:*:audio/x-flac:*",
		LastSeenAt:     now,
	}
	if err := s.UpsertUPnPRouting(ctx, r); err != nil {
		t.Fatalf("UpsertUPnPRouting: %v", err)
	}
	got, err := s.GetUPnPRouting(ctx, p)
	if err != nil {
		t.Fatalf("GetUPnPRouting: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.SourcePath != r.SourcePath || got.ServerUDN != r.ServerUDN ||
		got.ObjectID != r.ObjectID || got.ParentObjectID != r.ParentObjectID ||
		got.ResURL != r.ResURL || got.ProtocolInfo != r.ProtocolInfo {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, r)
	}
	if !got.LastSeenAt.Equal(now) {
		t.Errorf("LastSeenAt = %v; want %v", got.LastSeenAt, now)
	}
}

func TestUpsertUPnPRouting_UpdateOnConflict(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	const p = "Chord 2Go/Music/X/01 - T.flac"
	seedUPnPTrack(t, s, p)

	r := &UPnPRouting{
		SourcePath: p, ServerUDN: "uuid:a", ObjectID: "1",
		ResURL:     "http://h1/MediaItems/1.flac",
		LastSeenAt: time.Unix(1000, 0).UTC(),
	}
	if err := s.UpsertUPnPRouting(ctx, r); err != nil {
		t.Fatal(err)
	}
	// Re-upsert with shifted ObjectID / res URL / time — the row must replace.
	r2 := *r
	r2.ObjectID = "99"
	r2.ResURL = "http://h1/MediaItems/99.flac"
	r2.LastSeenAt = time.Unix(2000, 0).UTC()
	if err := s.UpsertUPnPRouting(ctx, &r2); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUPnPRouting(ctx, p)
	if got.ObjectID != "99" || got.ResURL != "http://h1/MediaItems/99.flac" || !got.LastSeenAt.Equal(time.Unix(2000, 0).UTC()) {
		t.Fatalf("update-on-conflict failed: %+v", got)
	}
}

func TestGetUPnPRouting_AbsentReturnsNilNil(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	got, err := s.GetUPnPRouting(context.Background(), "no/such/track.flac")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil routing, got %+v", got)
	}
}

func TestUpsertUPnPRoutingBatch_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	paths := []string{"Chord/A/1.flac", "Chord/A/2.flac", "Chord/A/3.flac"}
	for _, p := range paths {
		seedUPnPTrack(t, s, p)
	}
	now := time.Unix(3000, 0).UTC()
	var rs []*UPnPRouting
	for i, p := range paths {
		rs = append(rs, &UPnPRouting{
			SourcePath: p, ServerUDN: "uuid:b",
			ObjectID: "obj-" + p, ResURL: "http://h/x.flac",
			LastSeenAt: now.Add(time.Duration(i) * time.Second),
		})
	}
	if err := s.UpsertUPnPRoutingBatch(ctx, rs); err != nil {
		t.Fatalf("batch: %v", err)
	}
	n, err := s.CountUPnPRoutingForServer(ctx, "uuid:b")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("count = %d; want 3", n)
	}
}

func TestUpsertUPnPRoutingBatch_EmptyIsNoop(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	if err := s.UpsertUPnPRoutingBatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestListUPnPSourcePathsOlderThan_FiltersByServerAndCutoff(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	// Two servers; some tracks are "fresh", some "stale".
	mk := func(p, udn string, t0 time.Time) {
		seedUPnPTrack(t, s, p)
		_ = s.UpsertUPnPRouting(ctx, &UPnPRouting{
			SourcePath: p, ServerUDN: udn, ObjectID: "x",
			ResURL: "http://h/x.flac", LastSeenAt: t0,
		})
	}
	old := time.Unix(1000, 0).UTC()
	fresh := time.Unix(9999, 0).UTC()
	mk("A/old1.flac", "uuid:1", old)
	mk("A/old2.flac", "uuid:1", old)
	mk("A/fresh.flac", "uuid:1", fresh)
	mk("B/old.flac", "uuid:2", old) // wrong server, must not appear

	cutoff := time.Unix(5000, 0).UTC()
	got, err := s.ListUPnPSourcePathsOlderThan(ctx, "uuid:1", cutoff)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0] != "A/old1.flac" || got[1] != "A/old2.flac" {
		t.Fatalf("got %v; want [A/old1.flac A/old2.flac]", got)
	}
}

func TestUPnPRouting_FKCascadeDeletesOnTrackDelete(t *testing.T) {
	// The FK ON DELETE CASCADE means reaping a stale track via
	// DeleteTrack should drop the routing row alongside.
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	const p = "Chord/X.flac"
	seedUPnPTrack(t, s, p)
	_ = s.UpsertUPnPRouting(ctx, &UPnPRouting{
		SourcePath: p, ServerUDN: "uuid:1", ObjectID: "x",
		ResURL: "http://h/x.flac", LastSeenAt: time.Unix(1, 0).UTC(),
	})
	if err := s.DeleteTrack(ctx, p); err != nil {
		t.Fatalf("DeleteTrack: %v", err)
	}
	got, err := s.GetUPnPRouting(ctx, p)
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("FK CASCADE failed: routing row still present after track delete: %+v", got)
	}
}

func TestDeleteTracksBatch_RemovesAllAndCascadesRouting(t *testing.T) {
	// Reaping the per-server reconcile sweep must drop every doomed
	// track AND its routing row in one transaction + lock acquisition.
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	paths := []string{"A/1.flac", "A/2.flac", "A/3.flac"}
	for _, p := range paths {
		seedUPnPTrack(t, s, p)
		_ = s.UpsertUPnPRouting(ctx, &UPnPRouting{
			SourcePath: p, ServerUDN: "uuid:1", ObjectID: "x",
			ResURL: "http://h/x.flac", LastSeenAt: time.Unix(1, 0).UTC(),
		})
	}
	// Also keep a sibling track that must SURVIVE the batch.
	seedUPnPTrack(t, s, "B/keep.flac")
	_ = s.UpsertUPnPRouting(ctx, &UPnPRouting{
		SourcePath: "B/keep.flac", ServerUDN: "uuid:2", ObjectID: "y",
		ResURL: "http://h/y.flac", LastSeenAt: time.Unix(1, 0).UTC(),
	})

	if err := s.DeleteTracksBatch(ctx, paths); err != nil {
		t.Fatalf("DeleteTracksBatch: %v", err)
	}
	for _, p := range paths {
		if got, _ := s.GetTrack(ctx, p); got != nil {
			t.Errorf("track %q not removed", p)
		}
		if r, _ := s.GetUPnPRouting(ctx, p); r != nil {
			t.Errorf("routing for %q not cascaded", p)
		}
	}
	if got, _ := s.GetTrack(ctx, "B/keep.flac"); got == nil {
		t.Errorf("sibling track was incorrectly reaped")
	}
}

func TestDeleteTracksBatch_EmptyIsNoop(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	if err := s.DeleteTracksBatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTracksBatch_LargeChunkBoundary(t *testing.T) {
	// 250 rows > the 200-chunk so we exercise the multi-chunk loop.
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	var paths []string
	for i := 0; i < 250; i++ {
		p := "X/" + strconv.Itoa(i) + ".flac"
		seedUPnPTrack(t, s, p)
		paths = append(paths, p)
	}
	if err := s.DeleteTracksBatch(ctx, paths); err != nil {
		t.Fatalf("DeleteTracksBatch: %v", err)
	}
	for _, p := range paths {
		if got, _ := s.GetTrack(ctx, p); got != nil {
			t.Errorf("track %q not removed", p)
			break
		}
	}
}

func TestUpsertUPnPRouting_RequiresIdentityFields(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	cases := []*UPnPRouting{
		nil,
		{},
		{SourcePath: "p"}, // missing UDN
		{ServerUDN: "uuid:a", ObjectID: "x", ResURL: "http://h/x"}, // missing SourcePath
	}
	for i, r := range cases {
		if err := s.UpsertUPnPRouting(ctx, r); err == nil {
			t.Errorf("case %d: expected error for %+v", i, r)
		}
	}
}

// The following Test* functions collectively guard the bulk-read
// regression that motivates `Store.AllUPnPRoutingPaths` — the Gemini
// HIGH on PR #356. Pre-fix the dlna library-adapter rebuild issued an
// N+1 `GetUPnPRouting` per filesystem-miss track and reliably tripped
// the 10 s context deadline at 15k+ routed tracks. Fix: one
// AllUPnPRoutingPaths SELECT + a `map[string]struct{}` lookup downstream.
//
// Split across multiple Test functions (not one big Test*+t.Run tree)
// so each contract maps to a focused failure message and each function
// stays below the SonarCloud cognitive-complexity threshold (S3776).
//
// Contract surface:
//   - empty store returns empty slice
//   - returns every routed path, omits non-routed (rebuild keeps the
//     adapter's resolver-miss branch correct for non-routed tracks)
//   - ordering is `source_path` ASC (deterministic test output)
//   - cancelled context surfaces an error rather than silently
//     returning a partial slice (so the dlna adapter logs + skips
//     routed tracks this rebuild instead of silently dropping them)
//   - bulk-scale completes well within the dlna rebuild's 10 s
//     deadline (pins the "bulk read, not N+1" guarantee)

func seedRoutedFixture(t *testing.T, s *Store, ctx context.Context, paths ...string) {
	t.Helper()
	for _, p := range paths {
		seedUPnPTrack(t, s, p)
		r := &UPnPRouting{
			SourcePath: p,
			ServerUDN:  "uuid:test",
			ObjectID:   "x",
			ResURL:     "http://h/" + p,
			LastSeenAt: time.Unix(1, 0).UTC(),
		}
		if err := s.UpsertUPnPRouting(ctx, r); err != nil {
			t.Fatalf("seed routing %q: %v", p, err)
		}
	}
}

func TestAllUPnPRoutingPaths_EmptyStoreReturnsEmptySlice(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	got, err := s.AllUPnPRoutingPaths(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %d paths: %v", len(got), got)
	}
}

func TestAllUPnPRoutingPaths_ReturnsRoutedPathsOmitsLocal(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	routed := []string{
		"2go/Music/AC-DC/01.flac",
		"2go/Music/AC-DC/02.flac",
		"2go/Music/4 Non Blondes/01.flac",
	}
	seedRoutedFixture(t, s, ctx, routed...)
	// A local track WITHOUT a routing row.
	seedUPnPTrack(t, s, "Music/Local/01.flac")

	got, err := s.AllUPnPRoutingPaths(ctx)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != len(routed) {
		t.Fatalf("got %d paths; want %d. got=%v", len(got), len(routed), got)
	}
	gotSet := make(map[string]struct{}, len(got))
	for _, p := range got {
		gotSet[p] = struct{}{}
	}
	for _, want := range routed {
		if _, ok := gotSet[want]; !ok {
			t.Errorf("missing routed path %q in result %v", want, got)
		}
	}
	if _, leaked := gotSet["Music/Local/01.flac"]; leaked {
		t.Error("non-routed local track leaked into result")
	}
}

func TestAllUPnPRoutingPaths_OrderingIsAscending(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	// Seed deliberately out-of-order to confirm the ORDER BY in the query.
	seedRoutedFixture(t, s, ctx,
		"z/track.flac",
		"a/track.flac",
		"m/track.flac",
	)

	got, err := s.AllUPnPRoutingPaths(ctx)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("not sorted at index %d: %q > %q", i, got[i-1], got[i])
		}
	}
}

func TestAllUPnPRoutingPaths_CancelledContextSurfacesError(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.AllUPnPRoutingPaths(cctx)
	if err == nil {
		t.Error("expected error from cancelled context, got nil — dlna rebuild relies on this to log + skip rather than silently include partial results")
	}
}

func TestAllUPnPRoutingPaths_BulkScaleCompletesQuickly(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	const N = 5000
	for i := 0; i < N; i++ {
		p := "bulk/" + strconv.Itoa(i) + ".flac"
		seedUPnPTrack(t, s, p)
		r := &UPnPRouting{
			SourcePath: p,
			ServerUDN:  "uuid:bulk",
			ObjectID:   strconv.Itoa(i),
			ResURL:     "http://h/" + strconv.Itoa(i),
			LastSeenAt: time.Unix(1, 0).UTC(),
		}
		if err := s.UpsertUPnPRouting(ctx, r); err != nil {
			t.Fatalf("seed bulk %d: %v", i, err)
		}
	}
	queryCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := s.AllUPnPRoutingPaths(queryCtx)
	if err != nil {
		t.Fatalf("AllUPnPRoutingPaths: %v", err)
	}
	if len(got) != N {
		t.Errorf("got %d paths; want %d", len(got), N)
	}
}
