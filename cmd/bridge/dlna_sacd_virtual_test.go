package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// sacdCDSFixture is a library behind the real manifest adapter and the real
// ContentDirectory handler: an ordinary album, an SACD image beside it whose
// two virtual track rows are the rows a scan writes, and two tracks routed
// from an upstream MediaServer, one of them at a path of the virtual shape.
type sacdCDSFixture struct {
	cds http.HandlerFunc
	// virtual holds the image's virtual track paths, and listed the paths
	// the ContentDirectory serves: the album's track and both routed ones.
	virtual, listed []string
	// imageFolder is the library-relative folder the image's virtual rows
	// would make: the image itself, as a directory.
	imageFolder string
}

func newSACDCDSFixture(t *testing.T) sacdCDSFixture {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "Music")
	if err := os.MkdirAll(filepath.Join(root, "Glasswing", "Harbour"), 0o755); err != nil {
		t.Fatal(err)
	}
	flac := []byte("fLaC-an-ordinary-track")
	if err := os.WriteFile(filepath.Join(root, "Glasswing", "Harbour", "01 Harbour Light.flac"), flac, 0o644); err != nil {
		t.Fatal(err)
	}
	const image = "Glasswing/Tidewater.iso"
	imageBytes := bytes.Repeat([]byte{0x5A}, 4096)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(image)), imageBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	intp := func(v int) *int { return &v }
	f64 := func(v float64) *float64 { return &v }
	yes := true
	now := time.Now()
	f := sacdCDSFixture{imageFolder: image}
	upsert := func(tr *manifest.Track) {
		t.Helper()
		if err := store.UpsertTrack(ctx, tr); err != nil {
			t.Fatalf("UpsertTrack %q: %v", tr.Path, err)
		}
	}
	upsert(&manifest.Track{Path: "Glasswing/Harbour/01 Harbour Light.flac", Size: int64(len(flac)), ModTime: now,
		Title: "Harbour Light", Artist: "Glasswing", Album: "Harbour", Codec: "FLAC", TrackNumber: intp(1)})
	f.listed = append(f.listed, "Glasswing/Harbour/01 Harbour Light.flac")
	// The image's rows as processSACDISO writes them: typed SACD DSD, with
	// the image's own size and mtime as their identity stat.
	for i, title := range []string{"Tidewater One", "Tidewater Two"} {
		p := manifest.SACDVirtualTrackPath(image, i+1)
		if !manifest.IsSACDVirtualPath(p) {
			t.Fatalf("minted %q, which the classifier does not recognise", p)
		}
		upsert(&manifest.Track{Path: p, Size: int64(len(imageBytes)), ModTime: now,
			Title: title, Artist: "Glasswing", Album: "Tidewater", Codec: "DFF", Compression: "DST",
			IsDSD: &yes, SampleRate: f64(2822400), BitsPerSample: intp(1), Channels: intp(2),
			TrackNumber: intp(i + 1), Duration: f64(60)})
		f.virtual = append(f.virtual, p)
	}
	// Two routed tracks: the proxy serves them, whatever their path's shape.
	for _, r := range []struct{ path, title, album string }{
		{"2go/Remote/01 - Remote Song.flac", "Remote Song", "Remote"},
		{"2go/Remote Disc.iso/st/01.dff", "Remote Disc", "Remote Disc"},
	} {
		upsert(&manifest.Track{Path: r.path, Size: 1000, ModTime: now, Title: r.title, Artist: "Glasswing", Album: r.album})
		if err := store.UpsertUPnPRouting(ctx, &manifest.UPnPRouting{
			SourcePath: r.path, ServerUDN: "uuid:2go-test", ObjectID: "64$1", ResURL: "http://192.0.2.10:8200/MediaItems/1.dat",
			ProtocolInfo: "http-get:*:audio/x-flac:*", LastSeenAt: now,
		}); err != nil {
			t.Fatalf("UpsertUPnPRouting %q: %v", r.path, err)
		}
		f.listed = append(f.listed, r.path)
	}
	if !manifest.IsSACDVirtualPath("2go/Remote Disc.iso/st/01.dff") {
		t.Fatal("the routed row meant to have the virtual shape does not")
	}

	adapter := newManifestLibraryAdapter(store, bridgefs.New([]string{root}), []string{root}, testLogger())
	f.cds = dlna.ContentDirectoryHandler(adapter, func(*http.Request) string { return "http://bridge.test:7790" })
	return f
}

// cdsAnswer is one ContentDirectory response, decoded.
type cdsAnswer struct {
	fault          int // the UPnP error code of a SOAP fault; 0 for an answer
	numberReturned int
	totalMatches   int
	containers     []cdsContainer
	items          []cdsItem
}

type cdsContainer struct {
	ID         string `xml:"id,attr"`
	ChildCount int    `xml:"childCount,attr"`
	Title      string `xml:"title"`
}

type cdsItem struct {
	ID    string `xml:"id,attr"`
	Title string `xml:"title"`
}

// cdsCall posts one ContentDirectory action with its arguments and decodes
// the answer: the counts and the DIDL-Lite inside Result, or a fault's code.
func cdsCall(t *testing.T, h http.HandlerFunc, action, args string) cdsAnswer {
	t.Helper()
	body := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:` + action + ` xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` + args + `</u:` + action + `>` +
		`</s:Body></s:Envelope>`
	req := httptest.NewRequest(http.MethodPost, "/dlna/cds/control", strings.NewReader(body))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#`+action+`"`)
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	rec := httptest.NewRecorder()
	h(rec, req)
	var env struct {
		Body struct {
			Fault struct {
				Code int `xml:"detail>UPnPError>errorCode"`
			} `xml:"Fault"`
			Answer struct {
				Result         string `xml:"Result"`
				NumberReturned int    `xml:"NumberReturned"`
				TotalMatches   int    `xml:"TotalMatches"`
			} `xml:",any"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: undecodable answer (%d): %v\n%s", action, args, rec.Code, err, rec.Body.String())
	}
	if env.Body.Fault.Code != 0 {
		return cdsAnswer{fault: env.Body.Fault.Code}
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: status %d with no fault code\n%s", action, args, rec.Code, rec.Body.String())
	}
	a := cdsAnswer{numberReturned: env.Body.Answer.NumberReturned, totalMatches: env.Body.Answer.TotalMatches}
	var didl struct {
		Containers []cdsContainer `xml:"container"`
		Items      []cdsItem      `xml:"item"`
	}
	if err := xml.Unmarshal([]byte(env.Body.Answer.Result), &didl); err != nil {
		t.Fatalf("%s %s: undecodable DIDL-Lite: %v\n%s", action, args, err, env.Body.Answer.Result)
	}
	a.containers, a.items = didl.Containers, didl.Items
	return a
}

func cdsBrowse(t *testing.T, h http.HandlerFunc, objectID, flag string) cdsAnswer {
	t.Helper()
	return cdsCall(t, h, "Browse", "<ObjectID>"+objectID+"</ObjectID><BrowseFlag>"+flag+"</BrowseFlag>"+
		"<Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount><SortCriteria></SortCriteria>")
}

func cdsSearch(t *testing.T, h http.HandlerFunc, term string) cdsAnswer {
	t.Helper()
	return cdsCall(t, h, "Search", `<ContainerID>0</ContainerID><SearchCriteria>dc:title contains "`+term+`"</SearchCriteria>`+
		"<Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount><SortCriteria></SortCriteria>")
}

func cdsItemIDs(items []cdsItem) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	slices.Sort(ids)
	return ids
}

func dlnaIDsOf(paths []string) []string {
	ids := make([]string, 0, len(paths))
	for _, p := range paths {
		ids = append(ids, dlna.TrackID("", p))
	}
	slices.Sort(ids)
	return ids
}

// TestTheContentDirectoryListsNoSACDVirtualTrack: a renderer browsing or
// searching the bridge sees no SACD virtual track (backlog B415). The bridge
// never demuxes, so nothing is at a virtual row's path and a renderer told to
// play one fails; docs/SACDISOFeasibility.md §6 in the iOS repo planned the
// exclusion at the adapter, matching the app's own cast veto. The walk from
// the root checks that every container's childCount, in its parent's answer
// and in its own BrowseMetadata, is the TotalMatches of its children, and
// Search's TotalMatches the hits it returns. A routed track stays listed
// whatever its path's shape: the proxy serves it.
func TestTheContentDirectoryListsNoSACDVirtualTrack(t *testing.T) {
	f := newSACDCDSFixture(t)
	want := dlnaIDsOf(f.listed)

	// Walk every container from the root, Folders and All Tracks alike.
	type pending struct {
		id         string
		childCount int // as the parent's answer lists it
	}
	queue := []pending{{id: "0", childCount: -1}}
	seen := map[string]bool{}
	var allTracks, folderItems []cdsItem
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if seen[c.id] {
			t.Fatalf("container %s is reached twice", c.id)
		}
		seen[c.id] = true
		kids := cdsBrowse(t, f.cds, c.id, "BrowseDirectChildren")
		if kids.fault != 0 {
			t.Fatalf("BrowseDirectChildren %s: fault %d", c.id, kids.fault)
		}
		if got := len(kids.containers) + len(kids.items); kids.numberReturned != got || kids.totalMatches != got {
			t.Errorf("container %s: NumberReturned %d, TotalMatches %d, %d children in the DIDL", c.id, kids.numberReturned, kids.totalMatches, got)
		}
		if c.childCount >= 0 {
			if c.childCount != kids.totalMatches {
				t.Errorf("container %s: childCount %d in its parent's answer, %d children", c.id, c.childCount, kids.totalMatches)
			}
			meta := cdsBrowse(t, f.cds, c.id, "BrowseMetadata")
			if len(meta.containers) != 1 || meta.containers[0].ChildCount != kids.totalMatches {
				t.Errorf("container %s: BrowseMetadata %+v, want one container with childCount %d", c.id, meta.containers, kids.totalMatches)
			}
		}
		for _, k := range kids.containers {
			queue = append(queue, pending{id: k.ID, childCount: k.ChildCount})
		}
		if c.id == "1" { // All Tracks
			allTracks = kids.items
		} else {
			folderItems = append(folderItems, kids.items...)
		}
	}
	if got := cdsItemIDs(allTracks); !slices.Equal(got, want) {
		t.Errorf("All Tracks lists %v, want %v (virtual tracks: %v)", got, want, dlnaIDsOf(f.virtual))
	}
	if got := cdsItemIDs(folderItems); !slices.Equal(got, want) {
		t.Errorf("the Folders tree lists %v, want %v (virtual tracks: %v)", got, want, dlnaIDsOf(f.virtual))
	}
	if seen[dlna.FolderObjectID(f.imageFolder)] {
		t.Errorf("the Folders tree has a folder for the image %s", f.imageFolder)
	}

	// Search agrees with Browse: every track's artist matches, so the hits
	// are exactly what All Tracks lists, and none names the image's album.
	hits := cdsSearch(t, f.cds, "Glasswing")
	if got := cdsItemIDs(hits.items); !slices.Equal(got, want) || hits.totalMatches != len(want) || hits.numberReturned != len(want) {
		t.Errorf("Search: %v, NumberReturned %d, TotalMatches %d; want %v and %d of each", got, hits.numberReturned, hits.totalMatches, want, len(want))
	}
	if album := cdsSearch(t, f.cds, "Tidewater"); album.totalMatches != 0 || len(album.items) != 0 {
		t.Errorf("Search for the image's album: TotalMatches %d, items %v; want none", album.totalMatches, album.items)
	}

	// A control point that kept a virtual track's id learns it is gone.
	if meta := cdsBrowse(t, f.cds, dlna.TrackID("", f.virtual[0]), "BrowseMetadata"); meta.fault != dlna.UPnPErrNoSuchObject {
		t.Errorf("BrowseMetadata of a virtual track's id: fault %d, items %v; want NoSuchObject (%d)", meta.fault, meta.items, dlna.UPnPErrNoSuchObject)
	}
}
