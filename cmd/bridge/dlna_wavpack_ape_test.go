package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/formatgate"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestTheContentDirectoryOffersNoWavPackOrMonkeysAudio: a renderer browsing or
// searching the bridge is offered no WavPack and no Monkey's Audio file
// (backlog B489). Few renderers play either, a DLNA profile names neither, and
// a renderer cannot say what it plays, so the adapter reads the served set in
// the view that declares no gated format, the one list every view of the
// ContentDirectory derives from (All Tracks, the folder tree, Search,
// BrowseMetadata, the file route's lookup). By the path, whatever the row's
// origin: an upstream's .wv is left out as a local one is.
func TestTheContentDirectoryOffersNoWavPackOrMonkeysAudio(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "Music")
	files := map[string][]byte{
		"Glasswing/Harbour/01 Harbour Light.flac": []byte("fLaC-an-ordinary-track"),
		"Glasswing/Packed/01 Packed Light.wv":     []byte("wvpk-a-wavpack-track"),
		"Glasswing/Packed/01 Packed Light.wvc":    []byte("wvpk-its-correction"),
		"Glasswing/Monkey/01 Monkey Light.APE":    []byte("MAC -a-monkeys-audio-track"),
	}
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	upsert := func(path, title, album, codec string, size int) {
		t.Helper()
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: path, Size: int64(size), ModTime: now,
			Title: title, Artist: "Glasswing", Album: album, Codec: codec}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", path, err)
		}
	}
	const listed = "Glasswing/Harbour/01 Harbour Light.flac"
	upsert(listed, "Harbour Light", "Harbour", "FLAC", len(files[listed]))
	gated := []string{"Glasswing/Packed/01 Packed Light.wv", "Glasswing/Monkey/01 Monkey Light.APE", "2go/Remote/01 Remote Light.wv"}
	upsert(gated[0], "Packed Light", "Packed", manifest.CodecWavPack, len(files[gated[0]]))
	upsert(gated[1], "Monkey Light", "Monkey", manifest.CodecAPE, len(files[gated[1]]))
	upsert(gated[2], "Remote Light", "Remote", "", 1000)
	if err := store.UpsertUPnPRouting(ctx, &manifest.UPnPRouting{
		SourcePath: gated[2], ServerUDN: "uuid:2go-test", ObjectID: "64$1", ResURL: "http://192.0.2.10:8200/MediaItems/1.dat",
		ProtocolInfo: "http-get:*:audio/x-wavpack:*", LastSeenAt: now,
	}); err != nil {
		t.Fatalf("UpsertUPnPRouting: %v", err)
	}

	adapter := newManifestLibraryAdapter(store, bridgefs.New([]string{root}), []string{root}, formatgate.Set{}, testLogger())
	cds := dlna.ContentDirectoryHandler(adapter, func(*http.Request) string { return "http://bridge.test:7790" })
	want := dlnaIDsOf([]string{listed})

	// Walk every container from the root, All Tracks and the Folders tree.
	queue := []string{"0"}
	seen := map[string]bool{}
	var allTracks, folderItems []cdsItem
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			t.Fatalf("container %s is reached twice", id)
		}
		seen[id] = true
		kids := cdsBrowse(t, cds, id, "BrowseDirectChildren")
		if kids.fault != 0 {
			t.Fatalf("BrowseDirectChildren %s: fault %d", id, kids.fault)
		}
		for _, k := range kids.containers {
			queue = append(queue, k.ID)
		}
		if id == "1" { // All Tracks
			allTracks = kids.items
		} else {
			folderItems = append(folderItems, kids.items...)
		}
	}
	if got := cdsItemIDs(allTracks); !slices.Equal(got, want) {
		t.Errorf("All Tracks lists %v, want %v (gated: %v)", got, want, dlnaIDsOf(gated))
	}
	if got := cdsItemIDs(folderItems); !slices.Equal(got, want) {
		t.Errorf("the Folders tree lists %v, want %v (gated: %v)", got, want, dlnaIDsOf(gated))
	}
	for _, folder := range []string{"Glasswing/Packed", "Glasswing/Monkey", "2go/Remote"} {
		if seen[dlna.FolderObjectID(folder)] {
			t.Errorf("the Folders tree has a folder for %s, which holds only a gated file", folder)
		}
	}
	if n := adapter.TrackCount(); n != 1 {
		t.Errorf("TrackCount = %d, want 1", n)
	}

	// Search agrees with Browse: every track's artist matches, and only the
	// FLAC is a hit.
	hits := cdsSearch(t, cds, "Glasswing")
	if got := cdsItemIDs(hits.items); !slices.Equal(got, want) || hits.totalMatches != len(want) {
		t.Errorf("Search: %v, TotalMatches %d; want %v", got, hits.totalMatches, want)
	}
	for _, term := range []string{"Packed", "Monkey", "Remote"} {
		if a := cdsSearch(t, cds, term); a.totalMatches != 0 || len(a.items) != 0 {
			t.Errorf("Search %q: TotalMatches %d, items %v; want none", term, a.totalMatches, a.items)
		}
	}

	// A control point that holds a gated track's id learns there is no such
	// object, and the file route's lookup finds none to serve.
	for _, p := range gated {
		id := dlna.TrackID("", p)
		if meta := cdsBrowse(t, cds, id, "BrowseMetadata"); meta.fault != dlna.UPnPErrNoSuchObject {
			t.Errorf("BrowseMetadata of %s's id: fault %d, items %v; want NoSuchObject (%d)", p, meta.fault, meta.items, dlna.UPnPErrNoSuchObject)
		}
		if _, ok := adapter.GetTrackInfo(id); ok {
			t.Errorf("the file route's lookup finds %s", p)
		}
	}
}
