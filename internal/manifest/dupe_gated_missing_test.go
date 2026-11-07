package manifest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/formatgate"
)

// servedPathsOf is the paths view is served, sorted.
func servedPathsOf(t *testing.T, store *Store, view formatgate.Set) []string {
	t.Helper()
	tracks, err := store.ListServedTracks(context.Background(), nil, view)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tr := range tracks {
		out = append(out, tr.Path)
	}
	slices.Sort(out)
	return out
}

// TestAGoneFLACBesideAWavPackAndAnAPECopyIsServedToNoView is round 4 of
// backlog B489's case through the scanner: a FLAC, a WavPack and a Monkey's
// Audio copy of one track. The FLAC wins while its file is there; once a full
// scan finds it gone, inside the deletion pass's grace, its row is suppressed
// whole, beside the two gated copies each served to the views that declare
// its format. Until round 4 the gone FLAC was a unit of its own, served to
// every view, a dead row beside the copy a WavPack or APE client can play
// until the deletion pass reaped it three full scans later.
func TestAGoneFLACBesideAWavPackAndAnAPECopyIsServedToNoView(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	sc.SetDeleteThreshold(config.DefaultDeleteAfterMissingScans)
	sc.SetDupePolicy(func() dupes.Policy { return dupes.Policy{Mode: dupes.FilterHighestQuality} })

	if err := os.MkdirAll(filepath.Join(root, "A"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t, filepath.Join(root, "A", "x.flac"), 44100, 16, map[string]string{
		"TITLE": "Song", "ARTIST": "Artist", "ALBUM": "Album", "TRACKNUMBER": "1"})
	tags := []apeItem{apeText("Title", "Song"), apeText("Artist", "Artist"), apeText("Album", "Album"), apeText("Track", "1")}
	// Five seconds each, as the FLAC fixture is, so the three are one group.
	writeWavPack(t, filepath.Join(root, "B", "x.wv"), wvSpec{rate: 96000, bytes: 3, channels: 2, total: 96000 * 5}, tags)
	writeAPE(t, filepath.Join(root, "C", "x.ape"), apeSpec{version: 3990, compression: fxAPENormal, channels: 2,
		rate: 44100, bits: 16, blocksPerFrame: 73728, finalFrameBlocks: 44100*5 - 2*73728, totalFrames: 3}, tags)
	scanOnce(t, sc, "the library")

	views := []struct {
		name string
		view formatgate.Set
	}{
		{"nothing", formatgate.Set{}},
		{"WavPack", formatgate.SetOf(formatgate.WavPack)},
		{"APE", formatgate.SetOf(formatgate.APE)},
		{"both", formatgate.All()},
	}
	for _, v := range views {
		if got := servedPathsOf(t, store, v.view); !slices.Equal(got, []string{"A/x.flac"}) {
			t.Fatalf("view %s, the FLAC there: served %v, want the FLAC alone (the three copies are one group)", v.name, got)
		}
	}

	removeLibraryFile(t, root, "A/x.flac")
	scanOnce(t, sc, "the scan after the FLAC went")
	if got := trackColumn(t, store, "A/x.flac", "missing_count"); got != 1 {
		t.Fatalf("A/x.flac: missing_count = %d, want 1: the row is not inside the grace", got)
	}
	want := map[string][]string{
		"nothing": nil,
		"WavPack": {"B/x.wv"},
		"APE":     {"C/x.ape"},
		"both":    {"B/x.wv", "C/x.ape"},
	}
	for _, v := range views {
		if got := servedPathsOf(t, store, v.view); !slices.Equal(got, want[v.name]) {
			t.Errorf("view %s, the FLAC gone: served %v, want %v", v.name, got, want[v.name])
		}
	}
}
