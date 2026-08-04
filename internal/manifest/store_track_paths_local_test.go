package manifest

import (
	"context"
	"testing"
	"time"
)

// seedHybridLibrary lays out the shape the fix is about: a handful of
// filesystem tracks alongside rows routed from a UPnP upstream. It
// mirrors the local fixture (89 filesystem tracks + 15,283 routed from
// a Chord 2Go) at a size a test can assert on.
func seedHybridLibrary(t *testing.T, s *Store) (local, routed []string) {
	t.Helper()
	ctx := context.Background()

	local = []string{
		"Music/Diana Krall/Live in Paris/01 I Love Being Here With You.flac",
		"Music/Miles Davis/Kind of Blue/01 So What.flac",
	}
	routed = []string{
		"Chord 2Go/Music/4 Non Blondes/Bigger/01 What's Up.flac",
		"Chord 2Go/Music/ABBA/Gold/01 Dancing Queen.flac",
		"Chord 2Go/Music/Zappa/Apostrophe/01 Don't Eat.flac",
	}

	for _, p := range append(append([]string{}, local...), routed...) {
		if err := s.UpsertTrack(ctx, &Track{
			Path: p, Size: 1, ModTime: time.Unix(0, 0).UTC(),
		}); err != nil {
			t.Fatalf("seed track %q: %v", p, err)
		}
	}
	for i, p := range routed {
		if err := s.UpsertUPnPRouting(ctx, &UPnPRouting{
			SourcePath: p,
			ServerUDN:  "uuid:4d696e69-444c-164e-9d41-00b78f5ae46a",
			ObjectID:   "64$0$0$" + string(rune('0'+i)),
			ResURL:     "http://192.168.0.62:8200/MediaItems/" + string(rune('0'+i)) + ".flac",
			LastSeenAt: time.Unix(1_700_000_000, 0).UTC(),
		}); err != nil {
			t.Fatalf("seed routing %q: %v", p, err)
		}
	}
	return local, routed
}

// TrackPathsLocal is what any caller that will touch the filesystem
// wants: routed rows describe media on another device, so resolving one
// is a guaranteed miss.
//
// The analysis sweep was calling TrackPaths and reporting every routed
// row as `missing`, which on the hybrid fixture read `total 15372,
// missing 13553` beside a coverage tile that correctly said
// `totalLocal 89`.
func TestTrackPathsLocalExcludesUPnPRoutedRows(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	local, routed := seedHybridLibrary(t, s)

	got, err := s.TrackPathsLocal(context.Background())
	if err != nil {
		t.Fatalf("TrackPathsLocal: %v", err)
	}
	if len(got) != len(local) {
		t.Fatalf("TrackPathsLocal returned %d paths %v, want the %d filesystem "+
			"tracks only — a routed row would be resolved against a local disk "+
			"it does not live on", len(got), got, len(local))
	}
	inRouted := make(map[string]struct{}, len(routed))
	for _, p := range routed {
		inRouted[p] = struct{}{}
	}
	for _, p := range got {
		if _, bad := inRouted[p]; bad {
			t.Errorf("TrackPathsLocal returned routed path %q", p)
		}
	}
}

// The scanner's before-snapshot depends on TrackPaths staying
// all-inclusive. Its deletion pass spares routed rows by looking them
// up in the routed set — and it can only spare a row it was told to
// consider, so dropping them from the snapshot would not be "spared",
// it would be invisible.
//
// This is the reason the fix added a separate method rather than a bool
// on TrackPaths: the wrong value at THIS call site is silent data loss
// (PR #370's class), and a bool invites exactly that.
func TestTrackPathsStillIncludesUPnPRoutedRows(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	local, routed := seedHybridLibrary(t, s)

	got, err := s.TrackPaths(context.Background())
	if err != nil {
		t.Fatalf("TrackPaths: %v", err)
	}
	if want := len(local) + len(routed); len(got) != want {
		t.Fatalf("TrackPaths returned %d paths, want all %d — the scanner's "+
			"deletion pass cannot spare a routed row it was never given",
			len(got), want)
	}
	have := make(map[string]struct{}, len(got))
	for _, p := range got {
		have[p] = struct{}{}
	}
	for _, p := range routed {
		if _, ok := have[p]; !ok {
			t.Errorf("TrackPaths dropped routed path %q", p)
		}
	}
}

// Both enumerations must stay sorted — TrackPathsLocal took a new query
// and an `ORDER BY t.path` on the aliased column, which is exactly the
// kind of thing a rewrite drops. Callers diff these against a walk.
func TestTrackPathsBothSorted(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	seedHybridLibrary(t, s)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		fn   func(context.Context) ([]string, error)
	}{
		{"TrackPaths", s.TrackPaths},
		{"TrackPathsLocal", s.TrackPathsLocal},
	} {
		got, err := tc.fn(ctx)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for i := 1; i < len(got); i++ {
			if got[i-1] > got[i] {
				t.Errorf("%s not sorted: %q precedes %q", tc.name, got[i-1], got[i])
			}
		}
	}
}

// A library with no upstream configured must behave exactly as before —
// the anti-join is a no-op when `upnp_track_routing` is empty. Guards
// against the exclusion accidentally being written as an INNER-join
// shape that returns nothing on the majority (filesystem-only) install.
func TestTrackPathsLocalIsIdentityWithoutRouting(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	for _, p := range []string{"a.flac", "b/c.flac", "d/e/f.flac"} {
		if err := s.UpsertTrack(ctx, &Track{
			Path: p, Size: 1, ModTime: time.Unix(0, 0).UTC(),
		}); err != nil {
			t.Fatalf("seed %q: %v", p, err)
		}
	}

	all, err := s.TrackPaths(ctx)
	if err != nil {
		t.Fatalf("TrackPaths: %v", err)
	}
	local, err := s.TrackPathsLocal(ctx)
	if err != nil {
		t.Fatalf("TrackPathsLocal: %v", err)
	}
	if len(all) != 3 || len(local) != 3 {
		t.Fatalf("no routing configured: TrackPaths=%d TrackPathsLocal=%d, want 3 and 3",
			len(all), len(local))
	}
	for i := range all {
		if all[i] != local[i] {
			t.Errorf("index %d: TrackPaths=%q TrackPathsLocal=%q — must be "+
				"identical with an empty routing table", i, all[i], local[i])
		}
	}
}
