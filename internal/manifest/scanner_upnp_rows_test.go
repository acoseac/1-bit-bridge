package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// UPnP-routed rows live in `tracks` but never appear in a filesystem
// walk, so the scanner's missing-tracks pass must SPARE them entirely:
// no missing_count increments across scans, and the threshold DELETE
// must never reap them even when a counter accumulated before the
// exclusion shipped (pre-fix increments, or a rogue caller). Pre-fix
// the routed catalog (15k rows for a Chord 2Go upstream) was deleted
// after `threshold` filesystem scans once the ingest's
// skip-if-unchanged (PR #369) stopped resetting the counter via
// unconditional re-upserts — and even before #369 the same wipe fired
// whenever the upstream was offline for `threshold` consecutive scans.

func seedRoutedTrack(t *testing.T, store *Store, path string) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertTrack(ctx, &Track{
		Path:    path,
		Size:    999,
		ModTime: time.Unix(100, 0),
		Title:   "Routed",
		Artist:  "A",
		Album:   "Al",
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	if err := store.UpsertUPnPRouting(ctx, &UPnPRouting{
		SourcePath: path,
		ServerUDN:  "uuid:test",
		ResURL:     "http://h:8200/MediaItems/5.flac",
		LastSeenAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatalf("UpsertUPnPRouting: %v", err)
	}
}

func TestScanner_MissingPass_SparesUPnPRoutedRows(t *testing.T) {
	dir := t.TempDir()
	writeMinimalAudio(t, filepath.Join(dir, "keep.flac"))

	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const routedPath = "2go/Music/Artist/Album/01 - Track.flac"
	seedRoutedTrack(t, store, routedPath)

	s := NewScanner([]string{dir}, store, "")
	s.SetDeleteThreshold(3)

	// threshold + 1 scans: pre-fix the routed row would be reaped on
	// the third pass; post-fix its counter never moves.
	for i := 1; i <= 4; i++ {
		if _, err := s.Scan(context.Background()); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}

	if got := countTracksHelper(t, store); got != 2 {
		t.Fatalf("tracks = %d, want 2 (routed row must survive every filesystem scan)", got)
	}
	if got := missingCountHelper(t, store, routedPath); got != 0 {
		t.Fatalf("routed missing_count = %d, want 0 (filesystem scans must not touch it)", got)
	}
}

func TestIncrementMissingTracks_ThresholdDeleteSparesRoutedRows(t *testing.T) {
	// Defense-in-depth behind the scanner exclusion: even with a
	// pre-accumulated counter at/above the threshold (pre-fix state, or
	// a caller that passes routed paths anyway), the threshold DELETE
	// must spare routed rows while still reaping filesystem rows.
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	const routedPath = "2go/Music/Artist/Album/01 - Track.flac"
	seedRoutedTrack(t, store, routedPath)
	if err := store.UpsertTrack(ctx, &Track{
		Path:    "music/doomed.flac",
		Size:    1,
		ModTime: time.Unix(100, 0),
		Title:   "Doomed",
	}); err != nil {
		t.Fatalf("UpsertTrack doomed: %v", err)
	}

	// Three increment passes naming BOTH rows — the filesystem row
	// crosses the threshold and is reaped; the routed row survives.
	var deleted int64
	for i := 0; i < 3; i++ {
		d, err := store.IncrementMissingTracksAndDeleteAtThreshold(
			ctx, []string{routedPath, "music/doomed.flac"}, 3)
		if err != nil {
			t.Fatalf("increment pass %d: %v", i, err)
		}
		deleted += d
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the filesystem row)", deleted)
	}
	tr, err := store.GetTrack(ctx, routedPath)
	if err != nil || tr == nil {
		t.Fatalf("routed row must survive the threshold delete: %v / %v", tr, err)
	}
	if gone, err := store.GetTrack(ctx, "music/doomed.flac"); err != nil || gone != nil {
		t.Fatalf("filesystem row must still be reaped at threshold: %v / %v", gone, err)
	}
}

// ScanSubtree carries the same UPnP-routed exclusion as the full scan.
// Reachable in single-root mode: a watcher event for a file dropped
// directly at the library root gives relScope "." which short-circuits
// TrackPathsUnder to the WHOLE library, so every routed row lands in
// beforeTrackSet and gets counted "missing" on every subtree scan.
//
// The store-side NOT-IN guard stops the reap today, so the visible
// damage is a counter that only climbs — but a row that later leaves
// upnp_track_routing (upstream retired, or re-UDN'd after a firmware
// update) is then already far past threshold and gets reaped on its
// very next pass, with no grace period at all.
func TestScanSubtree_MissingPass_SparesUPnPRoutedRows(t *testing.T) {
	dir := t.TempDir()
	writeMinimalAudio(t, filepath.Join(dir, "keep.flac"))

	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const routedPath = "2go/Music/Artist/Album/01 - Track.flac"
	seedRoutedTrack(t, store, routedPath)

	s := NewScanner([]string{dir}, store, "")
	s.SetDeleteThreshold(3)

	// Subtree-scan the root itself — the relScope "." shape the
	// watcher produces for a root-level drop.
	for i := 1; i <= 4; i++ {
		if _, err := s.ScanSubtree(context.Background(), dir); err != nil {
			t.Fatalf("subtree scan %d: %v", i, err)
		}
	}

	if got := missingCountHelper(t, store, routedPath); got != 0 {
		t.Fatalf("routed missing_count = %d, want 0 (subtree scans must not touch it)", got)
	}
	if got := countTracksHelper(t, store); got != 2 {
		t.Fatalf("tracks = %d, want 2 (routed row must survive every subtree scan)", got)
	}
}
