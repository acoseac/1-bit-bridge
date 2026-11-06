package manifest

// The client format gate on the served set (format_view.go, backlog B489):
// the SQL predicate held to formatgate's rule over every shape of path, the
// page cursor's grammar, an all-WavPack library's honest empty feed for a
// client that declares nothing, and the playlist feed's album-folder rule in
// each view.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
)

// streamedManifest is WriteManifest's answer, decoded.
func streamedManifest(t *testing.T, store *Store, roots []string, since time.Time, view formatgate.Set) *Manifest {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteManifest(context.Background(), &buf, store, roots, since, view); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.Bytes())
	}
	return &m
}

// pathsOfManifest is m's track paths, sorted.
func pathsOfManifest(m *Manifest) []string {
	out := make([]string, 0, len(m.Tracks))
	for _, tr := range m.Tracks {
		out = append(out, tr.Path)
	}
	slices.Sort(out)
	return out
}

// formatViewPaths are paths of every shape the extension rule has to agree
// on: each gated extension in several cases, a gated name in a folder, a
// gated extension inside a name or a folder, a dot-file, extensions one byte
// off, white space after the extension, non-ASCII and invalid UTF-8 before
// it.
var formatViewPaths = []string{
	"a.flac", "b.wv", "c.WV", "d.Wv", "e.ape", "f.APE", "g.Ape",
	"h.wvc", "dir.wv/i.flac", "j.wv.flac", "k.flac.wv", "l", "x/.wv", "x/.ape",
	"m.tape", "n.xwv", "o.wv ", "p.ap", "q.wvape", "ü/ö.wv", "\xff.wv", "\xc3.ape",
	"r/s.v", "t.w", "wv", "ape", "u.apex",
}

// allViews are the four views there are.
var allViews = []formatgate.Set{
	{}, formatgate.SetOf(formatgate.WavPack), formatgate.SetOf(formatgate.APE), formatgate.All(),
}

// TestTheFormatViewSQLIsFormatgatesRule holds the SQL predicate, both its
// spellings, to formatgate.Of over every shape of path, in every view: the
// served readers' (formatViewSQL) through the count and the list, the
// search's (formatViewSQLT) through a query of its own, and the tombstones'
// through DeletedSince.
func TestTheFormatViewSQLIsFormatgatesRule(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	at := time.Unix(1_700_000_000, 0).UTC()
	var rows []*Track
	for _, p := range formatViewPaths {
		rows = append(rows, &Track{Path: p, Size: 1, ModTime: at})
	}
	if err := s.UpsertTrackBatch(ctx, rows); err != nil {
		t.Fatal(err)
	}
	for _, view := range allViews {
		var want []string
		for _, p := range formatViewPaths {
			if view.Takes(p) {
				want = append(want, p)
			}
		}
		slices.Sort(want)

		n, err := s.CountServedTracks(ctx, view)
		if err != nil || n != len(want) {
			t.Errorf("view %q: CountServedTracks = %d (%v), want %d", view.Key(), n, err, len(want))
		}
		listed, err := s.ListServedTracks(ctx, nil, view)
		if err != nil {
			t.Fatal(err)
		}
		// A listed row's path is its JSON copy, whose bytes of invalid
		// UTF-8 are coerced (marshalForStorage); the predicate judged the
		// row's key.
		var got, wantListed []string
		for _, tr := range listed {
			got = append(got, tr.Path)
		}
		for _, p := range want {
			wantListed = append(wantListed, strings.ToValidUTF8(p, "�"))
		}
		slices.Sort(got)
		slices.Sort(wantListed)
		if !slices.Equal(got, wantListed) {
			t.Errorf("view %q: listed %q, want %q", view.Key(), got, wantListed)
		}

		rowsT, err := s.db.QueryContext(ctx, `SELECT t.path FROM tracks t WHERE `+formatViewSQLT, formatViewArgs(view)...)
		if err != nil {
			t.Fatal(err)
		}
		var gotT []string
		for rowsT.Next() {
			var p string
			if err := rowsT.Scan(&p); err != nil {
				t.Fatal(err)
			}
			gotT = append(gotT, p)
		}
		rowsT.Close()
		slices.Sort(gotT)
		if !slices.Equal(gotT, want) {
			t.Errorf("view %q: the t.path predicate kept %q, want %q", view.Key(), gotT, want)
		}
	}

	// The bind-free spelling, the zero view's, is formatgate.Gated's negation.
	var wantUngated []string
	for _, p := range formatViewPaths {
		if !formatgate.Gated(p) {
			wantUngated = append(wantUngated, p)
		}
	}
	slices.Sort(wantUngated)
	rowsU, err := s.db.QueryContext(ctx, `SELECT t.path FROM tracks t WHERE `+ungatedPathSQLT)
	if err != nil {
		t.Fatal(err)
	}
	var gotU []string
	for rowsU.Next() {
		var p string
		if err := rowsU.Scan(&p); err != nil {
			t.Fatal(err)
		}
		gotU = append(gotU, p)
	}
	rowsU.Close()
	slices.Sort(gotU)
	if !slices.Equal(gotU, wantUngated) {
		t.Errorf("the bind-free predicate kept %q, want %q", gotU, wantUngated)
	}
	// And its count of the rest, the operator surfaces' number.
	if n, err := s.CountGatedFormatTracks(ctx); err != nil || n != len(formatViewPaths)-len(wantUngated) {
		t.Errorf("CountGatedFormatTracks = %d (%v), want %d", n, err, len(formatViewPaths)-len(wantUngated))
	}

	// The tombstones, the same way: every path deleted in one call, beside a
	// library large enough that the call is a deletion list, never the
	// journal's mass delete (a quarter of the library), which journals none.
	var filler []*Track
	for i := 0; i < 400; i++ {
		filler = append(filler, &Track{Path: "filler/" + strconv.Itoa(i) + ".flac", Size: 1, ModTime: at})
	}
	if err := s.UpsertTrackBatch(ctx, filler); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Millisecond)
	if err := s.DeleteTracksBatch(ctx, formatViewPaths); err != nil {
		t.Fatal(err)
	}
	for _, view := range allViews {
		var want []string
		for _, p := range formatViewPaths {
			if view.Takes(p) {
				want = append(want, p)
			}
		}
		slices.Sort(want)
		got, overflow, err := s.DeletedSince(ctx, since, view)
		if err != nil || overflow {
			t.Fatalf("view %q: DeletedSince: %v (overflow %v)", view.Key(), err, overflow)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("view %q: tombstones %q, want %q", view.Key(), got, want)
		}
	}
}

// TestAPageCursorContinuesOnlyItsOwnView pins the cursor's grammar: the zero
// view's is the path as it always was, a declared view's names it, each reads
// back in its own view, and a cursor read in another view is
// ErrCursorOfAnotherView, never a path.
func TestAPageCursorContinuesOnlyItsOwnView(t *testing.T) {
	paths := []string{"Artist/Album/01 One.flac", "a\x01b/c d.wv", "f1.looks-declared", "ünï/cødé.ape"}
	for _, view := range allViews {
		for _, p := range paths {
			c := pageCursor(view, p)
			got, err := pagePath(view, c)
			if err != nil || got != p {
				t.Errorf("view %q: pagePath(pageCursor(%q)) = %q, %v", view.Key(), p, got, err)
			}
			if view.IsZero() && c != p {
				t.Errorf("the zero view's cursor for %q is %q, want the path as it always was", p, c)
			}
			for _, other := range allViews {
				if other.Key() == view.Key() {
					continue
				}
				if got, err := pagePath(other, c); !errors.Is(err, ErrCursorOfAnotherView) {
					t.Errorf("view %q's cursor %q read in view %q = %q, %v; want ErrCursorOfAnotherView",
						view.Key(), c, other.Key(), got, err)
				}
			}
		}
	}
	if got, err := pagePath(formatgate.All(), ""); got != "" || err != nil {
		t.Errorf("the first page = %q, %v; want \"\" in every view", got, err)
	}
}

// TestAnAllWavPackLibraryIsAnHonestEmptyFeedToAClientThatDeclaresNothing: a
// library holding only WavPack files serves a client that declares nothing
// no row and counts of 0, full, paged and delta, with its folders as they
// are; a client that declares WavPack is served every row.
func TestAnAllWavPackLibraryIsAnHonestEmptyFeedToAClientThatDeclaresNothing(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	writeWavPack(t, filepath.Join(root, "Artist", "Album", "01 One.wv"), stdWavPack(), []apeItem{apeText("Title", "One")})
	writeWavPack(t, filepath.Join(root, "Artist", "Album", "02 Two.wv"), stdWavPack(), []apeItem{apeText("Title", "Two")})
	scanOnce(t, sc, "the library")
	ctx := context.Background()
	roots := []string{root}

	none, err := BuildManifest(ctx, store, roots, time.Time{}, formatgate.Set{})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Tracks) != 0 || none.EnrichmentProgress == nil || none.EnrichmentProgress.TracksTotal != 0 {
		t.Errorf("full, declaring nothing: %d tracks, progress %+v; want none and a total of 0", len(none.Tracks), none.EnrichmentProgress)
	}
	if len(none.Folders) == 0 {
		t.Error("full, declaring nothing: no folders; want the folders the scan saw")
	}
	page, err := BuildManifestPage(ctx, store, roots, "", 10, formatgate.Set{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tracks) != 0 || page.Total == nil || *page.Total != 0 || page.NextCursor != nil {
		t.Errorf("paged, declaring nothing: %d tracks, total %v, next %v; want an empty last page of total 0", len(page.Tracks), page.Total, page.NextCursor)
	}
	delta, err := BuildManifest(ctx, store, roots, time.Unix(1, 0), formatgate.Set{})
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Tracks) != 0 || len(delta.Deleted) != 0 {
		t.Errorf("delta, declaring nothing: %d tracks, %d deleted; want none", len(delta.Tracks), len(delta.Deleted))
	}
	wv, err := BuildManifest(ctx, store, roots, time.Time{}, formatgate.SetOf(formatgate.WavPack))
	if err != nil {
		t.Fatal(err)
	}
	if len(wv.Tracks) != 2 || wv.EnrichmentProgress == nil || wv.EnrichmentProgress.TracksTotal != 2 {
		t.Errorf("full, declaring WavPack: %d tracks, progress %+v; want 2", len(wv.Tracks), wv.EnrichmentProgress)
	}
	if n, err := store.CountServedTracks(ctx, formatgate.Set{}); err != nil || n != 0 {
		t.Errorf("the health count declaring nothing = %d (%v), want 0", n, err)
	}
}

// TestTheStreamedManifestIsTheBuiltOneInEveryView keeps the streaming and the
// buffered legs to one answer in each view, full and delta.
func TestTheStreamedManifestIsTheBuiltOneInEveryView(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	writeWavPack(t, filepath.Join(root, "A", "01.wv"), stdWavPack(), []apeItem{apeText("Title", "One")})
	writeAPE(t, filepath.Join(root, "A", "02.ape"), stdAPE(), []apeItem{apeText("Title", "Two")})
	writeMinimalFLAC(t, filepath.Join(root, "A", "03.flac"), 44100, 16, map[string]string{"TITLE": "Three"})
	scanOnce(t, sc, "the library")
	ctx := context.Background()
	for _, view := range allViews {
		for _, since := range []time.Time{{}, time.Unix(1, 0)} {
			built, err := BuildManifest(ctx, store, []string{root}, since, view)
			if err != nil {
				t.Fatal(err)
			}
			streamed := streamedManifest(t, store, []string{root}, since, view)
			if got, want := pathsOfManifest(streamed), pathsOfManifest(built); !slices.Equal(got, want) {
				t.Errorf("view %q since %v: streamed %q, built %q", view.Key(), since, got, want)
			}
			wantN := 0
			for _, p := range []string{"A/01.wv", "A/02.ape", "A/03.flac"} {
				if view.Takes(p) {
					wantN++
				}
			}
			if len(built.Tracks) != wantN {
				t.Errorf("view %q since %v: %d tracks, want %d", view.Key(), since, len(built.Tracks), wantN)
			}
		}
	}
}

// TestThePlaylistFeedJudgesTheAlbumRuleInEachView: an album-folder playlist is
// judged on the folder's tracks the view takes. A playlist naming the one
// FLAC in a folder that also holds a WavPack file is the folder's album
// playlist to a client that declares nothing (as it was before the bridge
// indexed WavPack: the .wv had no row), and an ordinary playlist to one that
// declares WavPack; a playlist naming both is the album playlist there, and
// to the other an ordinary one whose WavPack line is unresolved.
func TestThePlaylistFeedJudgesTheAlbumRuleInEachView(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	writeWavPack(t, filepath.Join(root, "Artist", "Album", "02 Two.wv"), stdWavPack(), []apeItem{apeText("Title", "Two")})
	writeMinimalFLAC(t, filepath.Join(root, "Artist", "Album", "01 One.flac"), 44100, 16, map[string]string{"TITLE": "One"})
	writeLibraryFile(t, root, "Artist/Album/flac only.m3u", []byte("01 One.flac\n"))
	writeLibraryFile(t, root, "Artist/Album/both.m3u", []byte("01 One.flac\n02 Two.wv\n"))
	scanOnce(t, sc, "the library")
	ctx := context.Background()

	listed := func(view formatgate.Set) map[string]PlaylistFile {
		page, err := store.PlaylistFilesPage(ctx, "", "", view)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]PlaylistFile{}
		for _, f := range page.Files {
			out[filepath.Base(f.Path)] = f
		}
		return out
	}
	none := listed(formatgate.Set{})
	if _, ok := none["flac only.m3u"]; ok {
		t.Error("declaring nothing, the FLAC-only playlist is listed; want it the folder's album playlist, as before WavPack was indexed")
	}
	if both, ok := none["both.m3u"]; !ok || !slices.Equal(both.Members, []string{"Artist/Album/01 One.flac"}) ||
		!slices.Equal(both.Unresolved, []string{"02 Two.wv"}) {
		t.Errorf("declaring nothing, both.m3u = %+v (listed %v); want listed, its WavPack line unresolved", both, ok)
	}
	wv := listed(formatgate.SetOf(formatgate.WavPack))
	if _, ok := wv["flac only.m3u"]; !ok {
		t.Error("declaring WavPack, the FLAC-only playlist is not listed; want an ordinary playlist (the folder holds a second track)")
	}
	if _, ok := wv["both.m3u"]; ok {
		t.Error("declaring WavPack, both.m3u is listed; want it the folder's album playlist")
	}

	// A cursor of one view is stale in the other: the version names the view.
	first, err := store.playlistFilesPage(ctx, "", "", 1, playlistFeedBatchBytes, formatgate.Set{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PlaylistFilesPage(ctx, first.Version, first.Next, formatgate.SetOf(formatgate.WavPack)); !errors.Is(err, ErrPlaylistFilesChanged) {
		t.Errorf("a zero-view cursor read declaring WavPack: %v, want ErrPlaylistFilesChanged", err)
	}
}

// TestWavPackAndMonkeysAudioRowsTakePartInEnrichment: the client format gate
// is the serving end's alone (backlog B489). The enricher's work query and
// the Atlas harvest's release set read every row, a WavPack or Monkey's
// Audio file's among them, so these files are enriched and their releases
// harvested like any other; only a view that does not declare their format
// leaves them out.
func TestWavPackAndMonkeysAudioRowsTakePartInEnrichment(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	at := time.Unix(1_700_000_000, 0).UTC()
	const (
		wvRelease  = "11111111-1111-1111-1111-111111111111"
		apeRelease = "22222222-2222-2222-2222-222222222222"
	)
	rows := []*Track{
		{Path: "A/01.wv", Size: 1, ModTime: at, Codec: CodecWavPack, ArtworkMBID: wvRelease},
		{Path: "B/01.ape", Size: 1, ModTime: at, Codec: CodecAPE, ArtworkMBID: apeRelease},
	}
	if err := s.UpsertTrackBatch(ctx, rows); err != nil {
		t.Fatal(err)
	}
	unenriched, err := s.UnenrichedTracks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tr := range unenriched {
		got = append(got, tr.Path)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"A/01.wv", "B/01.ape"}) {
		t.Errorf("the enricher's work query holds %q, want both files", got)
	}
	releases, err := s.DistinctReleaseMBIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(releases)
	if !slices.Equal(releases, []string{wvRelease, apeRelease}) {
		t.Errorf("the harvest's releases are %q, want both", releases)
	}
	if n, err := s.CountServedTracks(ctx, formatgate.Set{}); err != nil || n != 0 {
		t.Errorf("an app that declares nothing is served %d (%v), want 0", n, err)
	}
}
