package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestBackfillTrackNumbersFromPath(t *testing.T) {
	ip := func(n int) *int { return &n }
	in := []ReconcileTarget{
		{Path: "Ornette Coleman/Shape of Jazz/06. Congeniality.flac"},                 // nil → 6
		{Path: "Sam Lee/Old Wow/03. The Moon Shines Bright.flac", TrackNumber: ip(0)}, // 0 sentinel → 3
		{Path: "Aaron Parks/Find The Way/03. Unravel.flac", TrackNumber: ip(3)},       // present → skip
		{Path: "Various/Comp/Untitled Hidden.flac"},                                   // no number → skip
		{Path: "Various/Comp/1984 - Two Tribes.flac"},                                 // year prefix → skip
		{Path: `Metallica\Load\CD 13\01. Intro Jam.flac`},                             // windows sep, basename only
	}
	got := backfillTrackNumbersFromPath(in)

	want := map[string]int{
		"Ornette Coleman/Shape of Jazz/06. Congeniality.flac": 6,
		"Sam Lee/Old Wow/03. The Moon Shines Bright.flac":     3,
		`Metallica\Load\CD 13\01. Intro Jam.flac`:             1,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for _, c := range got {
		w, ok := want[c.Path]
		if !ok {
			t.Errorf("unexpected change for %q", c.Path)
			continue
		}
		if c.TrackNumber == nil || *c.TrackNumber != w {
			t.Errorf("%q: got %v, want %d", c.Path, c.TrackNumber, w)
		}
	}
	// deterministic path order
	for i := 1; i < len(got); i++ {
		if got[i-1].Path > got[i].Path {
			t.Errorf("not sorted by path: %q before %q", got[i-1].Path, got[i].Path)
		}
	}
}

func TestPathStem(t *testing.T) {
	cases := map[string]string{
		"Artist/Album/06. Title.flac": "06. Title",
		`Artist\Album\06. Title.flac`: "06. Title", // windows separator
		"06. Title.flac":              "06. Title", // bare basename
		"Album/Track.with.dots.mp3":   "Track.with.dots",
		"no-ext-basename":             "no-ext-basename", // no dot, returned as-is
		"Album/.hidden":               ".hidden",         // leading-dot preserved (dot > 0 guard)
	}
	for in, want := range cases {
		if got := pathStem(in); got != want {
			t.Errorf("pathStem(%q) = %q, want %q", in, got, want)
		}
	}
}

// tnFSPath is a filesystem track with a numbered filename but no track-number
// tag; tnRoutedPath is a UPnP-routed track (also numbered) that must be spared.
const (
	tnFSPath     = "Ornette Coleman/Shape of Jazz/06. Congeniality.flac"
	tnRoutedPath = "2go/Music/Pat Metheny/Bright Size Life/03. Phase Dance.flac"
)

// setupTrackNumberReconcile builds a store seeded with one filesystem track
// (tnFSPath) and one UPnP-routed track (tnRoutedPath), plus a Scanner, for the
// backfill end-to-end tests.
func setupTrackNumberReconcile(t *testing.T) (*Store, *Scanner, context.Context) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertTrack(ctx, &Track{
		Path: tnFSPath, Size: 10, ModTime: time.Unix(100, 0),
		Title: "Congeniality", Artist: "Ornette Coleman", Album: "Shape of Jazz",
	}); err != nil {
		t.Fatalf("UpsertTrack fs: %v", err)
	}
	seedRoutedTrack(t, store, tnRoutedPath)
	return store, NewScanner([]string{dir}, store, ""), ctx
}

// The filesystem row gets its number from the filename; the UPnP-routed row is
// SPARED (its number belongs to the upstream ingest); and a second pass over
// the now-clean library writes nothing.
func TestRunTrackNumberReconciliation_FillsFSSparesRouted(t *testing.T) {
	store, s, ctx := setupTrackNumberReconcile(t)

	n, err := s.runTrackNumberReconciliation(ctx)
	if err != nil {
		t.Fatalf("runTrackNumberReconciliation: %v", err)
	}
	if n != 1 {
		t.Fatalf("filled %d tracks, want 1 (fs only; routed spared)", n)
	}

	fs, _ := store.GetTrack(ctx, tnFSPath)
	if fs == nil || fs.TrackNumber == nil || *fs.TrackNumber != 6 {
		t.Errorf("fs track not filled to 6: %+v", fs)
	}
	routed, _ := store.GetTrack(ctx, tnRoutedPath)
	if routed == nil || routed.TrackNumber != nil {
		t.Errorf("routed track should keep a nil track number")
	}

	// Idempotent: a clean library produces zero writes on the next pass.
	if n2, err := s.runTrackNumberReconciliation(ctx); err != nil || n2 != 0 {
		t.Errorf("second pass: n=%d err=%v, want 0/nil", n2, err)
	}
}

// The backfill must leave enriched_at untouched — touching it would re-trigger
// the MB/CAA/Deezer enricher treadmill. Proven by marking the fs row enriched,
// then asserting it does NOT reappear in UnenrichedTracks after the pass.
func TestRunTrackNumberReconciliation_LeavesEnrichedAtUntouched(t *testing.T) {
	store, s, ctx := setupTrackNumberReconcile(t)

	fsBefore, err := store.GetTrack(ctx, tnFSPath)
	if err != nil || fsBefore == nil {
		t.Fatalf("GetTrack fs (pre): %v", err)
	}
	if err := store.MarkEnriched(ctx, fsBefore); err != nil {
		t.Fatalf("MarkEnriched: %v", err)
	}
	if _, err := s.runTrackNumberReconciliation(ctx); err != nil {
		t.Fatalf("runTrackNumberReconciliation: %v", err)
	}
	un, err := store.UnenrichedTracks(ctx, 100)
	if err != nil {
		t.Fatalf("UnenrichedTracks: %v", err)
	}
	for _, u := range un {
		if u.Path == tnFSPath {
			t.Errorf("backfill reset enriched_at on %q — re-triggers the enricher", tnFSPath)
		}
	}
}
