package upnpingest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// --- walkFieldsEqual (pure skip decision) ---

func mkTrack(mutators ...func(*manifest.Track)) *manifest.Track {
	i := 7
	d := 215.0
	sr := 44100.0
	t := &manifest.Track{
		Path:        "2go/Music/A/Al/01 - T.flac",
		Size:        999,
		ModTime:     time.Unix(100, 0),
		Title:       "T",
		Artist:      "A",
		AlbumArtist: "A",
		Album:       "Al",
		Codec:       "FLAC",
		TrackNumber: &i,
		Year:        nil,
		Duration:    &d,
		SampleRate:  &sr,
	}
	for _, m := range mutators {
		m(t)
	}
	return t
}

func TestWalkFieldsEqual_IdenticalRows(t *testing.T) {
	if !walkFieldsEqual(mkTrack(), mkTrack()) {
		t.Fatal("identical walk fields must compare equal")
	}
}

func TestWalkFieldsEqual_NilInputs(t *testing.T) {
	if walkFieldsEqual(nil, mkTrack()) || walkFieldsEqual(mkTrack(), nil) || walkFieldsEqual(nil, nil) {
		t.Fatal("nil inputs must never compare equal (skip requires a baseline row)")
	}
}

func TestWalkFieldsEqual_ModTimeIgnored(t *testing.T) {
	// buildTrackAndRouting stamps ModTime with walkStart — it differs on
	// every walk by construction, so it must NOT participate or the
	// skip would never fire.
	a := mkTrack()
	b := mkTrack(func(x *manifest.Track) { x.ModTime = time.Unix(999, 0) })
	if !walkFieldsEqual(a, b) {
		t.Fatal("ModTime must be excluded from the walk-field comparison")
	}
}

func TestWalkFieldsEqual_EnricherFieldsIgnored(t *testing.T) {
	// The stored row accumulates enricher additions (MBIDs / artwork) a
	// fresh walk row never carries — including them would mark every
	// enriched row as changed forever (enrich → walk → wipe loop).
	enriched := mkTrack(func(x *manifest.Track) {
		x.MusicBrainzTrackID = "mb-track"
		x.MusicBrainzAlbumID = "mb-album"
		x.ArtworkMBID = "artwork-mbid"
		x.ArtistMBID = "artist-mbid"
	})
	if !walkFieldsEqual(enriched, mkTrack()) {
		t.Fatal("enricher-owned fields must be excluded from the walk-field comparison")
	}
}

func TestWalkFieldsEqual_ChangedFieldsDetected(t *testing.T) {
	cases := map[string]func(*manifest.Track){
		"size":            func(x *manifest.Track) { x.Size = 1000 },
		"title":           func(x *manifest.Track) { x.Title = "T2" },
		"artist":          func(x *manifest.Track) { x.Artist = "B" },
		"album":           func(x *manifest.Track) { x.Album = "Al2" },
		"codec":           func(x *manifest.Track) { x.Codec = "WAV" },
		"trackNumber":     func(x *manifest.Track) { n := 8; x.TrackNumber = &n },
		"trackNumber→nil": func(x *manifest.Track) { x.TrackNumber = nil },
		"duration":        func(x *manifest.Track) { d := 1.0; x.Duration = &d },
		"sampleRate":      func(x *manifest.Track) { x.SampleRate = nil },
		"year←val":        func(x *manifest.Track) { y := 2020; x.Year = &y },
		"isDSD":           func(x *manifest.Track) { b := true; x.IsDSD = &b },
		"albumArtist":     func(x *manifest.Track) { x.AlbumArtist = "B" },
		"bitsPerSample":   func(x *manifest.Track) { b := 24; x.BitsPerSample = &b },
	}
	for name, mutate := range cases {
		if walkFieldsEqual(mkTrack(), mkTrack(mutate)) {
			t.Errorf("%s: changed field must compare unequal", name)
		}
	}
}

// --- end-to-end: second walk of an unchanged upstream writes nothing ---

// addWalkRoutes queues one full walk's SOAP exchange (GetSystemUpdateID
// + the 4-level Browse descent from TestIngester_Run_WalksAndUpserts)
// with the track's <res> size + title parameterized so re-walk and
// changed-walk fixtures stay in one place.
func addWalkRoutes(stub *stubSOAP, title string, size int) {
	stub.addRoute("GetSystemUpdateID", wrapSystemUpdateID("0"))
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<container id="64$0" parentID="64"><dc:title>Music</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`+
			`</DIDL-Lite>`, 1, 1))
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<container id="64$0$0" parentID="64$0"><dc:title>Artist</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`+
			`</DIDL-Lite>`, 1, 1))
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<container id="64$0$0$0" parentID="64$0$0"><dc:title>Album</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`+
			`</DIDL-Lite>`, 1, 1))
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<item id="64$0$0$0$0" parentID="64$0$0$0">`+
			`<dc:title>`+title+`</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class>`+
			`<upnp:artist>Artist</upnp:artist><upnp:album>Album</upnp:album>`+
			`<upnp:originalTrackNumber>1</upnp:originalTrackNumber>`+
			fmt.Sprintf(`<res protocolInfo="http-get:*:audio/x-flac:*" size="%d">http://h:8200/MediaItems/5.flac</res>`, size)+
			`</item></DIDL-Lite>`, 1, 1))
}

func newSkipTestIngester(t *testing.T, stub *stubSOAP, store *manifest.Store) *Ingester {
	t.Helper()
	client := upnp.NewContentDirectoryClient(stub)
	cfg := config.UPnPUpstreamConfig{
		Enabled: true,
		Servers: []config.UPnPUpstreamServerConfig{
			{Name: "2Go", UDN: "uuid:test", PathPrefix: "Chord 2Go"},
		},
	}
	ing, err := NewIngester(cfg, client, &stubResolver{controlURL: "http://h:8200/ctl/CD"}, store, nil)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}
	return ing
}

func TestIngester_Run_SecondWalkSkipsUnchangedTracks(t *testing.T) {
	const wantPath = "Chord 2Go/Music/Artist/Album/01 - Track.flac"
	stub := newStubSOAP()
	addWalkRoutes(stub, "Track", 999)
	addWalkRoutes(stub, "Track", 999) // identical second walk
	store := openIngestTestStore(t)
	ing := newSkipTestIngester(t, stub, store)
	ctx := context.Background()

	if _, err := ing.Run(ctx, Options{}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	// Enrich the track so the second walk can prove it preserves
	// enriched_at (the pre-fix unconditional upsert reset it to 0,
	// re-queueing the whole upstream for enrichment every walk).
	tr, err := store.GetTrack(ctx, wantPath)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack after run 1: %v / %v", tr, err)
	}
	tr.ArtworkMBID = "test-mbid"
	if err := store.MarkEnriched(ctx, tr); err != nil {
		t.Fatalf("MarkEnriched: %v", err)
	}

	// Delta-sync cursor between the walks: anything the second walk
	// bumps would surface in ListTracks(since: t1).
	time.Sleep(2 * time.Millisecond)
	t1 := time.Now()
	time.Sleep(2 * time.Millisecond)

	res, err := ing.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	pr := res.PerServer[0]
	if pr.Err != nil {
		t.Fatalf("run 2 per-server err: %v", pr.Err)
	}
	if pr.Walked != 1 || pr.Unchanged != 1 || pr.Reaped != 0 {
		t.Fatalf("run 2: walked=%d unchanged=%d reaped=%d; want 1/1/0",
			pr.Walked, pr.Unchanged, pr.Reaped)
	}

	// indexed_at preserved → iOS delta-sync re-receives NOTHING.
	delta, err := store.ListTracks(ctx, &t1)
	if err != nil {
		t.Fatalf("ListTracks(since): %v", err)
	}
	if len(delta) != 0 {
		t.Fatalf("unchanged re-walk must not bump indexed_at; delta carried %d rows", len(delta))
	}

	// enriched_at preserved → the enricher has nothing to re-process.
	unenriched, err := store.UnenrichedTracks(ctx, 10)
	if err != nil {
		t.Fatalf("UnenrichedTracks: %v", err)
	}
	if len(unenriched) != 0 {
		t.Fatalf("unchanged re-walk must not reset enriched_at; %d rows re-queued", len(unenriched))
	}

	// The enricher's additions survive in the stored row.
	tr2, err := store.GetTrack(ctx, wantPath)
	if err != nil || tr2 == nil {
		t.Fatalf("GetTrack after run 2: %v / %v", tr2, err)
	}
	if tr2.ArtworkMBID != "test-mbid" {
		t.Fatalf("enricher additions must survive an unchanged re-walk; ArtworkMBID = %q", tr2.ArtworkMBID)
	}

	// The routing row WAS refreshed — last_seen_at advanced past t1, so
	// the reconcile sweep correctly kept the track (Reaped == 0 above).
	rt, err := store.GetUPnPRouting(ctx, wantPath)
	if err != nil || rt == nil {
		t.Fatalf("GetUPnPRouting: %v / %v", rt, err)
	}
	if !rt.LastSeenAt.After(t1) {
		t.Fatalf("routing last_seen_at must refresh on every walk (got %v, cursor %v)", rt.LastSeenAt, t1)
	}
}

func TestIngester_Run_ChangedTrackStillUpserts(t *testing.T) {
	const wantPath = "Chord 2Go/Music/Artist/Album/01 - Track.flac"
	stub := newStubSOAP()
	addWalkRoutes(stub, "Track", 999)
	addWalkRoutes(stub, "Track", 1234) // same path, changed size → real change
	store := openIngestTestStore(t)
	ing := newSkipTestIngester(t, stub, store)
	ctx := context.Background()

	if _, err := ing.Run(ctx, Options{}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	t1 := time.Now()
	time.Sleep(2 * time.Millisecond)

	res, err := ing.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	pr := res.PerServer[0]
	if pr.Err != nil {
		t.Fatalf("run 2 per-server err: %v", pr.Err)
	}
	if pr.Walked != 1 || pr.Unchanged != 0 {
		t.Fatalf("run 2: walked=%d unchanged=%d; want 1/0 (size changed)", pr.Walked, pr.Unchanged)
	}

	// The change must surface to iOS delta-sync…
	delta, err := store.ListTracks(ctx, &t1)
	if err != nil {
		t.Fatalf("ListTracks(since): %v", err)
	}
	if len(delta) != 1 {
		t.Fatalf("changed track must bump indexed_at; delta carried %d rows", len(delta))
	}
	// …and persist.
	tr, err := store.GetTrack(ctx, wantPath)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack: %v / %v", tr, err)
	}
	if tr.Size != 1234 {
		t.Fatalf("changed size must persist; got %d", tr.Size)
	}
}
