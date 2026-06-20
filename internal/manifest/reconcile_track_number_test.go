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

// TestRunTrackNumberReconciliation_FillsFSSparesRoutedKeepsEnriched is the
// end-to-end migration test: a filesystem row with a numbered filename but no
// track-number tag gets its number filled; a UPnP-routed row (whose track
// number belongs to the upstream ingest) is SPARED; enriched_at is left
// untouched so the backfill never re-triggers the enricher; and a second pass
// over the now-clean library writes nothing.
func TestRunTrackNumberReconciliation_FillsFSSparesRoutedKeepsEnriched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Filesystem track: numbered filename, no track-number tag.
	const fsPath = "Ornette Coleman/Shape of Jazz/06. Congeniality.flac"
	if err := store.UpsertTrack(ctx, &Track{
		Path: fsPath, Size: 10, ModTime: time.Unix(100, 0),
		Title: "Congeniality", Artist: "Ornette Coleman", Album: "Shape of Jazz",
	}); err != nil {
		t.Fatalf("UpsertTrack fs: %v", err)
	}
	// Mark it enriched so we can prove the backfill leaves enriched_at alone
	// (touching it would re-trigger the MB/CAA/Deezer treadmill).
	fsBefore, err := store.GetTrack(ctx, fsPath)
	if err != nil || fsBefore == nil {
		t.Fatalf("GetTrack fs (pre): %v / nil=%v", err, fsBefore == nil)
	}
	if err := store.MarkEnriched(ctx, fsBefore); err != nil {
		t.Fatalf("MarkEnriched: %v", err)
	}

	// UPnP-routed track: also numbered + untagged — must be SPARED.
	const routedPath = "2go/Music/Pat Metheny/Bright Size Life/03. Phase Dance.flac"
	seedRoutedTrack(t, store, routedPath)

	s := NewScanner([]string{dir}, store, "")
	n, err := s.runTrackNumberReconciliation(ctx)
	if err != nil {
		t.Fatalf("runTrackNumberReconciliation: %v", err)
	}
	if n != 1 {
		t.Fatalf("filled %d tracks, want 1 (fs only; routed spared)", n)
	}

	// fs row got its number from the filename.
	fs, _ := store.GetTrack(ctx, fsPath)
	if fs == nil {
		t.Fatal("fs track vanished")
	}
	if fs.TrackNumber == nil {
		t.Error("fs track: track number not filled (nil), want 6")
	} else if *fs.TrackNumber != 6 {
		t.Errorf("fs track: got %d, want 6", *fs.TrackNumber)
	}

	// routed row spared (number is the upstream ingest's domain).
	routed, _ := store.GetTrack(ctx, routedPath)
	if routed == nil {
		t.Fatal("routed track vanished")
	}
	if routed.TrackNumber != nil {
		t.Errorf("routed track should keep nil track number, got %d", *routed.TrackNumber)
	}

	// enriched_at untouched — the fs row must NOT reappear as unenriched.
	un, err := store.UnenrichedTracks(ctx, 100)
	if err != nil {
		t.Fatalf("UnenrichedTracks: %v", err)
	}
	for _, u := range un {
		if u.Path == fsPath {
			t.Errorf("backfill reset enriched_at on %q — re-triggers the enricher", fsPath)
		}
	}

	// Idempotent: a clean library produces zero writes on the next pass.
	if n2, err := s.runTrackNumberReconciliation(ctx); err != nil || n2 != 0 {
		t.Errorf("second pass: n=%d err=%v, want 0/nil", n2, err)
	}
}
