package admin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The two copies of one track that the duplicate tests below trash one of,
// and a track that is no copy of anything. The larger copy is the one the
// default filter (highest-quality) serves.
const (
	dupeLoser  = "CopyA/Album/01 Song.flac"
	dupeWinner = "CopyB/Album/01 Song.flac"
	dupeSolo   = "Other/Album/02 Other.flac"
)

// newDupeTrashServer is newSpellingServer with the duplicates policy wired as
// serve wires it (the live config's filter, highest-quality by default), and
// the library above indexed and stamped by a full scan: the loser suppressed,
// the winner and the solo track served.
//
// The rows are written with the format facts an extraction of a real file
// would give (the duplicate tiers need a codec, a rate, a depth and a
// duration), under the size and mtime of the files on disk, so the scan's
// skip gate keeps them as they are.
func newDupeTrashServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv, root := newSpellingServer(t)
	srv.deps.Scanner.SetDupePolicy(func() dupes.Policy {
		mode, err := srv.deps.CfgHolder.Load().Duplicates.EffectiveFilter()
		if err != nil {
			t.Errorf("duplicates filter: %v", err)
		}
		return dupes.Policy{Mode: dupes.FilterMode(mode)}
	})
	ctx := context.Background()
	seed := func(rel string, size int, title string, track int) {
		abs := seedLibraryFile(t, root, rel, strings.Repeat("x", size))
		info, err := os.Stat(abs)
		if err != nil {
			t.Fatal(err)
		}
		rate, dur, bits, disc, year := 44100.0, 200.0, 16, 1, 2020
		if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{
			Path: rel, Size: info.Size(), ModTime: info.ModTime(),
			Title: title, Artist: "Artist", AlbumArtist: "Artist", Album: "Album",
			TrackNumber: &track, DiscNumber: &disc, Year: &year,
			Duration: &dur, SampleRate: &rate, BitsPerSample: &bits, Codec: "FLAC",
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed(dupeLoser, 900, "Song", 1)
	seed(dupeWinner, 1000, "Song", 1)
	seed(dupeSolo, 500, "Other", 2)
	if _, err := srv.deps.Scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	wantServed(t, srv, "after the scan that stamped the copies", dupeWinner, dupeSolo)
	return srv, root
}

// wantServed fails unless the served set (what /v1/manifest streams) is
// exactly want.
func wantServed(t *testing.T, srv *Server, when string, want ...string) {
	t.Helper()
	got := servedPaths(t, srv, nil)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("served %s: %+q, want %+q", when, got, want)
	}
}

// servedPaths lists the served rows, sorted: all of them, or, given a
// cursor, the delta a client holding it is sent.
func servedPaths(t *testing.T, srv *Server, since *time.Time) []string {
	t.Helper()
	tracks, err := srv.deps.Manifest.ListServedTracks(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, tr := range tracks {
		paths = append(paths, tr.Path)
	}
	slices.Sort(paths)
	return paths
}

// TestTrashingTheServedCopyOfADuplicateServesTheOther: deleting the copy of a
// duplicate the bridge serves, through the console's real delete, serves the
// copy it had suppressed, and a phone holding a cursor from before the delete
// is sent both changes. The delete retires the winner's row itself, outside a
// scan, and on main at 6e9f5fc3 the subtree scan it then ran wrote and
// reaped nothing, so its tail skipped the duplicate restamp (backlog B218):
// the suppressed copy stayed hidden from every device, which had been sent
// the winner's tombstone, until the next full scan (6 h by default).
func TestTrashingTheServedCopyOfADuplicateServesTheOther(t *testing.T) {
	srv, root := newDupeTrashServer(t)
	cursor := time.Now()

	trashPaths(t, srv, dupeWinner)

	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dupeWinner))); !os.IsNotExist(err) {
		t.Fatalf("the trashed copy is still in the library: %v", err)
	}
	wantRows(t, srv, "after the delete", dupeLoser, dupeSolo)
	wantServed(t, srv, "after the delete", dupeLoser, dupeSolo)

	// What a delta-syncing phone holding a cursor from before the delete is
	// sent: the winner's tombstone, and the copy that now stands for it.
	if got := servedPaths(t, srv, &cursor); !slices.Equal(got, []string{dupeLoser}) {
		t.Errorf("delta since the delete: %+q, want %+q", got, []string{dupeLoser})
	}
	deleted, _, err := srv.deps.Manifest.DeletedSince(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{dupeWinner}) {
		t.Errorf("tombstones since the delete: %+q, want %+q", deleted, []string{dupeWinner})
	}
}

// TestTrashingATrackThatIsNoCopyRunsNoStampingPass is the positive control: a
// delete of a track that is no copy of anything behaves as it always did. Its
// row goes and its tombstone reaches a delta client, the copies stay as they
// were stamped, and no stamping pass runs, since the row deleted carried no
// stamp (the summary's stamp time is where a pass would show).
func TestTrashingATrackThatIsNoCopyRunsNoStampingPass(t *testing.T) {
	srv, _ := newDupeTrashServer(t)
	stampedAt := func() time.Time {
		t.Helper()
		sum, err := srv.deps.Manifest.LoadDupeSummary(context.Background())
		if err != nil || sum == nil {
			t.Fatalf("dupe summary: %v, %v", sum, err)
		}
		return sum.StampedAt
	}
	before := stampedAt()
	cursor := time.Now()

	trashPaths(t, srv, dupeSolo)

	wantRows(t, srv, "after the delete", dupeLoser, dupeWinner)
	wantServed(t, srv, "after the delete", dupeWinner)
	if got := servedPaths(t, srv, &cursor); len(got) != 0 {
		t.Errorf("delta since the delete: %+q, want none", got)
	}
	deleted, _, err := srv.deps.Manifest.DeletedSince(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{dupeSolo}) {
		t.Errorf("tombstones since the delete: %+q, want %+q", deleted, []string{dupeSolo})
	}
	if after := stampedAt(); !after.Equal(before) {
		t.Errorf("a stamping pass ran for the delete of a track that is no copy (stamped at %v, then %v)", before, after)
	}
}
