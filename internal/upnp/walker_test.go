package upnp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// setupWalk wires the existing stubDispatcher with one canned Browse
// response per upcoming Browse call, in expected order. Tests build a
// linear tree (root -> Music -> Artist -> Album -> tracks) and pre-load
// the responses for that traversal.
func setupWalk(t *testing.T, pages [][2]string) *ContentDirectoryClient {
	t.Helper()
	stub := &stubDispatcher{}
	for _, p := range pages {
		stub.queue = append(stub.queue, stubResp{status: 200, body: p[1]})
	}
	return NewContentDirectoryClient(stub)
}

// browsePage builds a Browse SOAP response containing the given DIDL
// inner content + the count fields.
func browsePage(didl string, numberReturned, totalMatches int) string {
	return string(wrapBrowse(didl, numberReturned, totalMatches))
}

// container builds a <container> snippet.
func ct(id, parentID, title string) string {
	return `<container id="` + id + `" parentID="` + parentID + `"><dc:title>` + title + `</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`
}

// item builds an <item> snippet with the given fields.
type itemSpec struct {
	id, parentID, title, artist, album, ext, proto string
	trackNo                                        int
	size                                           int64
}

func it(s itemSpec) string {
	ext := s.ext
	if ext == "" {
		ext = "flac"
	}
	proto := s.proto
	if proto == "" {
		proto = "http-get:*:audio/x-flac:*"
	}
	url := "http://h:8200/MediaItems/x." + ext
	tn := ""
	if s.trackNo > 0 {
		tn = `<upnp:originalTrackNumber>` + itoa(s.trackNo) + `</upnp:originalTrackNumber>`
	}
	szAttr := ""
	if s.size > 0 {
		szAttr = ` size="` + itoa64(s.size) + `"`
	}
	return `<item id="` + s.id + `" parentID="` + s.parentID + `">` +
		`<dc:title>` + s.title + `</dc:title>` +
		`<upnp:class>object.item.audioItem.musicTrack</upnp:class>` +
		`<upnp:artist>` + s.artist + `</upnp:artist>` +
		`<upnp:album>` + s.album + `</upnp:album>` +
		tn +
		`<res protocolInfo="` + proto + `"` + szAttr + `>` + url + `</res>` +
		`</item>`
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var s []byte
	for i > 0 {
		s = append([]byte{byte('0' + i%10)}, s...)
		i /= 10
	}
	if neg {
		s = append([]byte{'-'}, s...)
	}
	return string(s)
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	var s []byte
	for i > 0 {
		s = append([]byte{byte('0' + i%10)}, s...)
		i /= 10
	}
	return string(s)
}

const xmlnsHeader = `<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`

func TestBrowseFoldersWalk_BuildsStablePathFromDirChain(t *testing.T) {
	// Tree: root (id=64) -> Music (id=64$0) -> 4 Non Blondes (id=64$0$0) -> Album (id=64$0$0$0) -> track
	// Each "Browse" returns the children of that container.
	root := browsePage(xmlnsHeader+ct("64$0", "64", "Music")+`</DIDL-Lite>`, 1, 1)
	music := browsePage(xmlnsHeader+ct("64$0$0", "64$0", "4 Non Blondes")+`</DIDL-Lite>`, 1, 1)
	artist := browsePage(xmlnsHeader+ct("64$0$0$0", "64$0$0", "Bigger Better Faster More")+`</DIDL-Lite>`, 1, 1)
	album := browsePage(xmlnsHeader+
		it(itemSpec{id: "64$0$0$0$0", parentID: "64$0$0$0", title: "What's Up?", artist: "4 Non Blondes", album: "Bigger Better Faster More", trackNo: 1, size: 34_486_815})+
		`</DIDL-Lite>`, 1, 1)
	c := setupWalk(t, [][2]string{{"root", root}, {"music", music}, {"artist", artist}, {"album", album}})

	var yielded []Walked
	stats, err := BrowseFoldersWalk(context.Background(), c, testControlURL,
		WalkOptions{RootObjectID: "64", PathPrefix: "Chord 2Go"},
		func(w Walked) error { yielded = append(yielded, w); return nil })
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(yielded) != 1 {
		t.Fatalf("yielded %d; want 1", len(yielded))
	}
	got := yielded[0]
	wantPath := "Chord 2Go/Music/4 Non Blondes/Bigger Better Faster More/01 - What's Up?.flac"
	if got.Path != wantPath {
		t.Fatalf("Path = %q; want %q", got.Path, wantPath)
	}
	if got.ObjectID != "64$0$0$0$0" {
		t.Errorf("ObjectID = %q", got.ObjectID)
	}
	if got.AlbumPath != "Chord 2Go/Music/4 Non Blondes/Bigger Better Faster More" {
		t.Errorf("AlbumPath = %q", got.AlbumPath)
	}
	if got.Size != 34_486_815 {
		t.Errorf("Size = %d", got.Size)
	}
	if stats.Items != 1 || stats.Containers != 4 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestBrowseFoldersWalk_SkipsTopLevelContainers(t *testing.T) {
	// Root has Music + System Volume Information. The latter must be skipped
	// at the TOP LEVEL (we don't want to walk it).
	root := browsePage(xmlnsHeader+
		ct("64$0", "64", "Music")+
		ct("64$1", "64", "System Volume Information")+
		`</DIDL-Lite>`, 2, 2)
	music := browsePage(xmlnsHeader+
		it(itemSpec{id: "x1", parentID: "64$0", title: "T", artist: "A", trackNo: 1})+
		`</DIDL-Lite>`, 1, 1)
	c := setupWalk(t, [][2]string{{"root", root}, {"music", music}})

	var yielded []Walked
	_, err := BrowseFoldersWalk(context.Background(), c, testControlURL,
		WalkOptions{RootObjectID: "64", PathPrefix: "ChordPoly",
			SkipContainerTitles: []string{"System Volume Information"}},
		func(w Walked) error { yielded = append(yielded, w); return nil })
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(yielded) != 1 {
		t.Fatalf("yielded %d; want 1 (System Volume Information must be skipped)", len(yielded))
	}
	if !strings.HasPrefix(yielded[0].Path, "ChordPoly/Music/") {
		t.Errorf("path %q not under Music", yielded[0].Path)
	}
}

func TestBrowseFoldersWalk_FiltersNonAudioItems(t *testing.T) {
	root := browsePage(xmlnsHeader+ct("64$0", "64", "Mixed")+`</DIDL-Lite>`, 1, 1)
	mixed := browsePage(xmlnsHeader+
		// audio: real track
		it(itemSpec{id: "a1", parentID: "64$0", title: "Audio", trackNo: 1})+
		// non-audio: an image
		`<item id="img1" parentID="64$0"><dc:title>Cover</dc:title><upnp:class>object.item.imageItem</upnp:class><res protocolInfo="http-get:*:image/jpeg:*">http://h/img.jpg</res></item>`+
		`</DIDL-Lite>`, 2, 2)
	c := setupWalk(t, [][2]string{{"root", root}, {"mixed", mixed}})

	var yielded []Walked
	_, err := BrowseFoldersWalk(context.Background(), c, testControlURL,
		WalkOptions{RootObjectID: "64"},
		func(w Walked) error { yielded = append(yielded, w); return nil })
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(yielded) != 1 || yielded[0].Title != "Audio" {
		t.Fatalf("yielded = %+v; want exactly the audio track", yielded)
	}
}

func TestBrowseFoldersWalk_HonorsCtxCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := setupWalk(t, [][2]string{{"root", browsePage(xmlnsHeader+`</DIDL-Lite>`, 0, 0)}})
	if _, err := BrowseFoldersWalk(ctx, c, testControlURL,
		WalkOptions{RootObjectID: "64"}, func(Walked) error { return nil }); err == nil {
		t.Fatal("expected ctx.Cancel to propagate")
	}
}

func TestBrowseFoldersWalk_MaxItemsTruncatesWithErrWalkTruncated(t *testing.T) {
	// Root has one container with many items. MaxItems=2 → after yielding
	// 2, the walker returns ErrWalkTruncated and reports Truncated=true.
	root := browsePage(xmlnsHeader+ct("64$0", "64", "F")+`</DIDL-Lite>`, 1, 1)
	folder := browsePage(xmlnsHeader+
		it(itemSpec{id: "1", parentID: "64$0", title: "A", trackNo: 1})+
		it(itemSpec{id: "2", parentID: "64$0", title: "B", trackNo: 2})+
		it(itemSpec{id: "3", parentID: "64$0", title: "C", trackNo: 3})+
		`</DIDL-Lite>`, 3, 3)
	c := setupWalk(t, [][2]string{{"root", root}, {"folder", folder}})

	var n int
	stats, err := BrowseFoldersWalk(context.Background(), c, testControlURL,
		WalkOptions{RootObjectID: "64", MaxItems: 2},
		func(Walked) error { n++; return nil })
	if !errors.Is(err, ErrWalkTruncated) {
		t.Fatalf("err = %v; want ErrWalkTruncated", err)
	}
	if !stats.Truncated {
		t.Errorf("stats.Truncated = false; want true")
	}
	if n < 2 {
		t.Errorf("n = %d; want >= 2", n)
	}
}

func TestSanitizePathComponent_StripsSlashAndControls(t *testing.T) {
	if got := sanitizePathComponent("AC/DC"); got != "AC-DC" {
		t.Errorf("slash: %q", got)
	}
	if got := sanitizePathComponent("abc\x00def\x01ghi"); got != "abcdefghi" {
		t.Errorf("control chars: %q", got)
	}
	if got := sanitizePathComponent("  trimmed  "); got != "trimmed" {
		t.Errorf("trim: %q", got)
	}
	// path.Clean-significant segments are neutralized (traversal guard).
	if got := sanitizePathComponent("."); got != "_" {
		t.Errorf(`dot: got %q, want "_"`, got)
	}
	if got := sanitizePathComponent(".."); got != "_" {
		t.Errorf(`dotdot: got %q, want "_"`, got)
	}
	// Backslashes are stripped too (Windows path separator) — a "..\.."
	// title becomes "..-..", not a traversal segment.
	if got := sanitizePathComponent("..\\.."); got != "..-.." {
		t.Errorf(`backslash traversal: got %q, want "..-.."`, got)
	}
	// A legit title containing dots is preserved (not all-dots).
	if got := sanitizePathComponent("Vol.1"); got != "Vol.1" {
		t.Errorf(`dotted title: got %q, want "Vol.1"`, got)
	}
}

func TestBrowseFoldersWalk_NilYieldReturnsError(t *testing.T) {
	c := setupWalk(t, [][2]string{{"root", browsePage(xmlnsHeader+`</DIDL-Lite>`, 0, 0)}})
	if _, err := BrowseFoldersWalk(context.Background(), c, testControlURL,
		WalkOptions{RootObjectID: "64"}, nil); err == nil {
		t.Fatal("expected error for nil yield function")
	}
}

func TestBrowseFoldersWalk_EmptySkipEntryDoesNotSkipUntitledContainer(t *testing.T) {
	// A stray empty/whitespace SkipContainerTitles entry (hand-edited
	// bridge.yaml; ingest passes the list to the walker raw) must NOT skip
	// a container whose title trims to "".
	root := browsePage(xmlnsHeader+ct("64$0", "64", "")+`</DIDL-Lite>`, 1, 1)
	folder := browsePage(xmlnsHeader+
		it(itemSpec{id: "x1", parentID: "64$0", title: "T", artist: "A", trackNo: 1})+
		`</DIDL-Lite>`, 1, 1)
	c := setupWalk(t, [][2]string{{"root", root}, {"folder", folder}})

	var yielded []Walked
	_, err := BrowseFoldersWalk(context.Background(), c, testControlURL,
		WalkOptions{RootObjectID: "64", SkipContainerTitles: []string{"", "  "}},
		func(w Walked) error { yielded = append(yielded, w); return nil })
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(yielded) != 1 {
		t.Fatalf("yielded %d; want 1 (empty skip entry must not skip the untitled container)", len(yielded))
	}
}

func TestSynthesizeFilename_FallbacksAndExtensions(t *testing.T) {
	// Title + trackNo + ext from URL.
	got := synthesizeFilename(Object{
		Title: "What's Up", TrackNumber: 1,
		Res: "http://h/MediaItems/1.flac",
	})
	if got != "01 - What's Up.flac" {
		t.Errorf("got %q", got)
	}
	// No trackNo → no leading NN -.
	got = synthesizeFilename(Object{Title: "Solo", Res: "http://h/x.dsf"})
	if got != "Solo.dsf" {
		t.Errorf("got %q", got)
	}
	// Ext from protocolInfo when URL has none.
	got = synthesizeFilename(Object{
		Title: "T", TrackNumber: 2,
		Res:          "http://h/get?id=5",
		ProtocolInfo: "http-get:*:audio/x-dsf:*",
	})
	if got != "02 - T.dsf" {
		t.Errorf("got %q", got)
	}
}

func TestExtFromURL_StripsQueryAndFragment(t *testing.T) {
	if e := extFromURL("http://h/get?id=1&fmt=flac"); e != "" {
		t.Errorf("got %q; want empty (no extension on path)", e)
	}
	if e := extFromURL("http://h/x.flac?id=1"); e != "flac" {
		t.Errorf("got %q", e)
	}
	if e := extFromURL("http://h/x.flac#frag"); e != "flac" {
		t.Errorf("got %q", e)
	}
}

func TestLooksLikeAudioItem_AcceptsByClassMimeOrExtension(t *testing.T) {
	cases := []struct {
		name string
		o    Object
		want bool
	}{
		{"by class", Object{Class: "object.item.audioItem.musicTrack", Res: "http://h/x"}, true},
		{"by proto MIME", Object{Class: "", Res: "http://h/get?id=1", ProtocolInfo: "http-get:*:audio/x-dsf:*"}, true},
		{"by extension", Object{Class: "", Res: "http://h/x.flac"}, true},
		{"image rejected", Object{Class: "object.item.imageItem", Res: "http://h/x.jpg", ProtocolInfo: "http-get:*:image/jpeg:*"}, false},
		{"no res rejected", Object{Class: "object.item.audioItem"}, false},
	}
	for _, c := range cases {
		if got := looksLikeAudioItem(c.o); got != c.want {
			t.Errorf("%s: got %v; want %v", c.name, got, c.want)
		}
	}
}
