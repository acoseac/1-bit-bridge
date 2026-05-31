package dlna

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// -----------------------------------------------------------------------------
// Test fixtures
// -----------------------------------------------------------------------------

func newTestLib(tracks ...TrackInfo) *StaticLibrary {
	return &StaticLibrary{Tracks: tracks}
}

// buildBrowseRequest constructs a SOAP Browse request body + http.Request
// with the canonical SOAPAction header.
func buildBrowseRequest(t *testing.T, objectID, browseFlag string, startIdx, count uint32) *http.Request {
	t.Helper()
	body := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">
      <ObjectID>` + objectID + `</ObjectID>
      <BrowseFlag>` + browseFlag + `</BrowseFlag>
      <Filter>*</Filter>
      <StartingIndex>` + uint32ToString(startIdx) + `</StartingIndex>
      <RequestedCount>` + uint32ToString(count) + `</RequestedCount>
      <SortCriteria></SortCriteria>
    </u:Browse>
  </s:Body>
</s:Envelope>`
	req := httptest.NewRequest(http.MethodPost, "/dlna/cds/control", strings.NewReader(body))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	req.Header.Set("Content-Type", "text/xml; charset=\"utf-8\"")
	req.Header.Set("User-Agent", "Music Player Daemon 0.21.26") // Phase-0 2go-ish UA
	return req
}

func uint32ToString(v uint32) string {
	// Avoid importing strconv solely for this helper (one-line).
	return jsonNumberString(int64(v))
}

func jsonNumberString(v int64) string {
	// strconv.FormatInt would do, but we avoid the import. Trivial
	// helper — only used in test bodies for index/count fields.
	if v == 0 {
		return "0"
	}
	// Simple base-10 conversion.
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// testTrack returns a synthetic TrackInfo for fixture use. Default DSF
// shape; callers override individual fields as needed.
func testTrack(id, title string) TrackInfo {
	return TrackInfo{
		TrackID:         id,
		AbsolutePath:    "/library/" + title + ".dsf",
		Title:           title,
		Artist:          "Test Artist",
		Album:           "Test Album",
		Codec:           "DSF",
		FileExtension:   ".dsf",
		Size:            1000,
		DurationSeconds: 60,
		SampleRateHz:    2822400,
		BitsPerSample:   1,
		Channels:        2,
		IsDSD:           true,
	}
}

// -----------------------------------------------------------------------------
// ContentDirectoryHandler.Browse
// -----------------------------------------------------------------------------

// Test_CDS_Browse_RootReturnsAllTracksAndFolders pins the post-PR-#316
// contract: the root container exposes TWO siblings — `all_tracks`
// (flat list, fast access) AND `folders` (disk-hierarchy drill-down).
//
// Pre-PR-#316 the root exposed only `all_tracks` (PR #309 having
// removed the empty `Music` placeholder). PR #316 adds Folders as a
// second root entry so users can browse the on-disk hierarchy
// (Artist → Album → Track) rather than scrolling a 24k-row flat list.
// Don't reintroduce the single-container root shape — would re-close
// the navigation path operators expect from every reference
// MediaServer (MiniDLNA, MinimServer, Asset UPnP).
func Test_CDS_Browse_RootReturnsAllTracksAndFolders(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Hello"), testTrack("t2", "World"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))
	req := buildBrowseRequest(t, "0", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		// Numeric ObjectIDs per the `allTracksObjectID` ("1") and
		// `foldersRootObjectID` ("2") constants (PR #315
		// mconnect-Cling int-parse compat). Literal substrings so a
		// future change to either constant surfaces here.
		`id=&quot;1&quot;`,
		`&lt;dc:title&gt;All Tracks&lt;/dc:title&gt;`,
		`id=&quot;2&quot;`,
		`&lt;dc:title&gt;Folders&lt;/dc:title&gt;`,
		`<NumberReturned>2</NumberReturned>`,
		`<TotalMatches>2</TotalMatches>`,
		// `storageFolder` subtype + `searchable="1"` on every
		// container + `<upnp:storageUsed>-1</upnp:storageUsed>`.
		// Both containers share the same shape (the 2Go reference
		// emits storageFolder for every top-level container too).
		`&lt;upnp:class&gt;object.container.storageFolder&lt;/upnp:class&gt;`,
		`searchable=&quot;1&quot;`,
		`&lt;upnp:storageUsed&gt;-1&lt;/upnp:storageUsed&gt;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
	// Strengthen container-shape assertions — exact count `2` so a
	// regression affecting one container (e.g. dropped `searchable`
	// from the Folders root) can't slip through with a single-presence
	// check. Per CodeRabbit on PR #317.
	for _, want := range []struct {
		substr string
		count  int
	}{
		{`&lt;upnp:class&gt;object.container.storageFolder&lt;/upnp:class&gt;`, 2},
		{`searchable=&quot;1&quot;`, 2},
		{`&lt;upnp:storageUsed&gt;-1&lt;/upnp:storageUsed&gt;`, 2},
	} {
		if got := strings.Count(body, want.substr); got != want.count {
			t.Errorf("expected %d occurrences of %q in root response, got %d; body: %s",
				want.count, want.substr, got, body)
		}
	}
	// And specifically NOT the dropped `music` container (PR #309).
	for _, notWant := range []string{
		`id=&quot;music&quot;`,
		`&lt;dc:title&gt;Music&lt;/dc:title&gt;`,
	} {
		if strings.Contains(body, notWant) {
			t.Errorf("dropped substring %q should NOT appear in root response: %s", notWant, body)
		}
	}
}

// Test_CDS_Browse_BrowseMetadata_RootReturnsRootContainer pins the
// PR-pending spec-compliance fix: BrowseMetadata on ObjectID "0" MUST
// return a single DIDL element describing the root container itself
// (NOT empty DIDL — strict controllers like mconnect Lite issue
// BrowseMetadata as part of their drill-down handshake and bail to an
// infinite spinner if the response is empty). Per Gemini consult
// 2026-05-28.
// Test_CDS_Browse_Root_RespectsPagination pins that the root container
// honours StartingIndex / RequestedCount (PR-pending fix — pre-fix the
// root always returned both children regardless of the pagination
// arguments, which strict control points doing chunked validation scans
// with RequestedCount=1 would reject).
func Test_CDS_Browse_Root_RespectsPagination(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Hello"), testTrack("t2", "World"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))

	// RequestedCount=1 from index 0 → only the first container (All
	// Tracks), but TotalMatches still reports the full 2.
	t.Run("first_page_of_one", func(t *testing.T) {
		req := buildBrowseRequest(t, "0", "BrowseDirectChildren", 0, 1)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{
			`<NumberReturned>1</NumberReturned>`,
			`<TotalMatches>2</TotalMatches>`,
			`&lt;dc:title&gt;All Tracks&lt;/dc:title&gt;`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q in body: %s", want, body)
			}
		}
		// Folders (the second container) must NOT appear in the first
		// page of one.
		if strings.Contains(body, `&lt;dc:title&gt;Folders&lt;/dc:title&gt;`) {
			t.Errorf("Folders container should not appear in a RequestedCount=1 first page: %s", body)
		}
	})

	// StartingIndex=1 → skip All Tracks, return only Folders.
	t.Run("second_element_via_offset", func(t *testing.T) {
		req := buildBrowseRequest(t, "0", "BrowseDirectChildren", 1, 100)
		rec := httptest.NewRecorder()
		h(rec, req)
		body := rec.Body.String()
		for _, want := range []string{
			`<NumberReturned>1</NumberReturned>`,
			`<TotalMatches>2</TotalMatches>`,
			`&lt;dc:title&gt;Folders&lt;/dc:title&gt;`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q in body: %s", want, body)
			}
		}
		if strings.Contains(body, `&lt;dc:title&gt;All Tracks&lt;/dc:title&gt;`) {
			t.Errorf("All Tracks should be skipped at StartingIndex=1: %s", body)
		}
	})
}

// Test_clampPage pins the shared pagination clamp the root / All Tracks /
// folder-children arms now share.
func Test_clampPage(t *testing.T) {
	cases := []struct {
		name           string
		total          int
		start, count   uint32
		wantLo, wantHi int
	}{
		{"zero_count_returns_all", 10, 0, 0, 0, 10},
		{"count_within_range", 10, 0, 3, 0, 3},
		{"offset_plus_count", 10, 2, 3, 2, 5},
		{"count_exceeds_remaining_clamps", 10, 8, 100, 8, 10},
		{"start_beyond_total_collapses", 10, 50, 5, 10, 10},
		{"empty_total", 0, 0, 0, 0, 0},
		{"max_uint32_count_no_overflow", 10, 0, 0xFFFFFFFF, 0, 10},
		{"single_element_first_page", 2, 0, 1, 0, 1},
		{"single_element_second_page", 2, 1, 1, 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi := clampPage(tc.total, tc.start, tc.count)
			if lo != tc.wantLo || hi != tc.wantHi {
				t.Errorf("clampPage(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tc.total, tc.start, tc.count, lo, hi, tc.wantLo, tc.wantHi)
			}
		})
	}
}

// Test_folderIndexCache_ReusesUntilGenerationBumps pins the B6 cache
// contract: the folder index is reused across calls at a stable
// generation and rebuilt exactly when the generation advances (a
// manifest rescan). Verified via *FolderIndex pointer identity —
// BuildFolderIndex allocates a fresh instance per build.
func Test_folderIndexCache_ReusesUntilGenerationBumps(t *testing.T) {
	lib := &StaticLibrary{Tracks: []TrackInfo{
		{TrackID: "a", AbsolutePath: "/lib/A/x.dsf", RelativePath: "A/x.dsf"},
	}}
	fc := newFolderIndexCache(lib)

	i1 := fc.get()
	i2 := fc.get()
	if i1 != i2 {
		t.Fatalf("same generation must reuse the cached *FolderIndex")
	}

	lib.Gen++ // simulate a manifest rescan
	i3 := fc.get()
	if i3 == i1 {
		t.Fatalf("generation bump must rebuild the *FolderIndex")
	}

	i4 := fc.get()
	if i4 != i3 {
		t.Fatalf("post-bump generation must reuse the rebuilt index")
	}
}

// genTestLib is a LibrarySource with an atomic generation, for the
// concurrent cache test (StaticLibrary.Gen is a plain field and would
// itself race under concurrent bump + read).
type genTestLib struct {
	tracks []TrackInfo
	gen    atomic.Uint64
}

func (g *genTestLib) ListTrackInfos() []TrackInfo { return g.tracks }
func (g *genTestLib) TrackCount() int             { return len(g.tracks) }
func (g *genTestLib) GetTrackInfo(string) (TrackInfo, bool) {
	return TrackInfo{}, false
}
func (g *genTestLib) SearchTrackInfos(_ context.Context, q string) []TrackInfo {
	return filterTrackInfosBySubstring(g.tracks, q)
}
func (g *genTestLib) Generation() uint64 { return g.gen.Load() }

// Test_folderIndexCache_ConcurrentGetRaceFree hammers get() from many
// goroutines while another bumps the generation. Run under -race, it
// verifies the CAS publish path is race-free and the monotonic-store
// loop terminates (never spins or regresses).
func Test_folderIndexCache_ConcurrentGetRaceFree(t *testing.T) {
	lib := &genTestLib{tracks: []TrackInfo{
		{TrackID: "a", AbsolutePath: "/lib/A/x.dsf", RelativePath: "A/x.dsf"},
	}}
	fc := newFolderIndexCache(lib)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if fc.get() == nil {
					t.Errorf("get() returned nil index")
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			lib.gen.Add(1)
		}
	}()
	wg.Wait()

	if fc.get() == nil {
		t.Fatal("final get() returned nil index")
	}
}

// Test_CDS_Browse_BrowseMetadata_Track pins the B1 fix: BrowseMetadata
// on an individual track ObjectID returns a DIDL <item> (not a 701
// NoSuchObject). Strict control points (BubbleUPnP / Kazoo) query this
// before playback to read duration / resource constraints.
func Test_CDS_Browse_BrowseMetadata_Track(t *testing.T) {
	t.Run("top_level_track_parent_is_folders_root", func(t *testing.T) {
		lib := newTestLib(testTrack("trk1", "Song"))
		h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))
		req := buildBrowseRequest(t, "trk1", "BrowseMetadata", 0, 0)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{
			`<NumberReturned>1</NumberReturned>`,
			`id=&quot;trk1&quot;`,
			`parentID=&quot;2&quot;`, // top-level → foldersRootObjectID
			`&lt;dc:title&gt;Song&lt;/dc:title&gt;`,
			`&lt;item `, // an item, not a container
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q in body: %s", want, body)
			}
		}
		if strings.Contains(body, `&lt;container `) {
			t.Errorf("BrowseMetadata on a track must not emit a container: %s", body)
		}
	})

	t.Run("nested_track_parent_is_folder_object_id", func(t *testing.T) {
		nested := TrackInfo{
			TrackID:       "trkN",
			AbsolutePath:  "/library/Artist/Album/Deep.dsf",
			RelativePath:  "Artist/Album/Deep.dsf",
			Title:         "Deep",
			FileExtension: ".dsf", Codec: "DSF", Size: 10, Channels: 2,
		}
		lib := newTestLib(nested)
		h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))
		req := buildBrowseRequest(t, "trkN", "BrowseMetadata", 0, 0)
		rec := httptest.NewRecorder()
		h(rec, req)
		body := rec.Body.String()
		wantParent := FolderObjectID("Artist/Album")
		if !strings.Contains(body, `parentID=&quot;`+wantParent+`&quot;`) {
			t.Errorf("nested track parentID should be FolderObjectID(Artist/Album)=%s; body: %s", wantParent, body)
		}
		if !strings.Contains(body, `id=&quot;trkN&quot;`) {
			t.Errorf("missing track id in body: %s", body)
		}
	})

	t.Run("unknown_id_still_404", func(t *testing.T) {
		lib := newTestLib(testTrack("trk1", "Song"))
		h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
		req := buildBrowseRequest(t, "does-not-exist", "BrowseMetadata", 0, 0)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("unknown ObjectID should still fault (NoSuchObject); got status %d, body %s",
				rec.Code, rec.Body.String())
		}
	})
}

func Test_CDS_Browse_BrowseMetadata_RootReturnsRootContainer(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Hello"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildBrowseRequest(t, "0", "BrowseMetadata", 0, 0)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<NumberReturned>1</NumberReturned>`,
		`<TotalMatches>1</TotalMatches>`,
		`id=&quot;0&quot;`,
		`parentID=&quot;-1&quot;`,
		`&lt;dc:title&gt;1-bit Bridge&lt;/dc:title&gt;`,
		`&lt;upnp:class&gt;object.container&lt;/upnp:class&gt;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in BrowseMetadata response: %s", want, body)
		}
	}
}

// Test_CDS_Browse_BrowseMetadata_AllTracksReturnsAllTracksContainer
// pins that BrowseMetadata on `all_tracks` returns the storage-folder
// container itself with its current child count. mconnect Lite reads
// `<dc:title>` for the UI header title during drill-down — empty DIDL
// here caused the post-PR-#309 infinite spinner symptom.
func Test_CDS_Browse_BrowseMetadata_AllTracksReturnsAllTracksContainer(t *testing.T) {
	lib := newTestLib(testTrack("t1", "First"), testTrack("t2", "Second"), testTrack("t3", "Third"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildBrowseRequest(t, "1", "BrowseMetadata", 0, 0)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<NumberReturned>1</NumberReturned>`,
		`<TotalMatches>1</TotalMatches>`,
		`id=&quot;1&quot;`,
		`parentID=&quot;0&quot;`,
		`&lt;dc:title&gt;All Tracks&lt;/dc:title&gt;`,
		`&lt;upnp:class&gt;object.container.storageFolder&lt;/upnp:class&gt;`,
		`childCount=&quot;3&quot;`,
		`searchable=&quot;1&quot;`,
		`&lt;upnp:storageUsed&gt;-1&lt;/upnp:storageUsed&gt;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in BrowseMetadata response: %s", want, body)
		}
	}
}

// countingLib counts how often each LibrarySource read method is
// invoked, so a test can prove the BrowseMetadata ChildCount path goes
// through the cheap scalar TrackCount() and never the O(N) ListTrackInfos
// deep-copy (the pre-fix hot-path waste on 50k-track libraries).
type countingLib struct {
	tracks    []TrackInfo
	listCalls atomic.Int64
	tcCalls   atomic.Int64
}

func (c *countingLib) ListTrackInfos() []TrackInfo {
	c.listCalls.Add(1)
	out := make([]TrackInfo, len(c.tracks))
	copy(out, c.tracks)
	return out
}
func (c *countingLib) TrackCount() int { c.tcCalls.Add(1); return len(c.tracks) }
func (c *countingLib) SearchTrackInfos(_ context.Context, q string) []TrackInfo {
	return filterTrackInfosBySubstring(c.tracks, q)
}
func (c *countingLib) GetTrackInfo(id string) (TrackInfo, bool) {
	for _, t := range c.tracks {
		if t.TrackID == id {
			return t, true
		}
	}
	return TrackInfo{}, false
}
func (c *countingLib) Generation() uint64 { return 0 }

// Test_CDS_BrowseMetadata_AllTracks_UsesScalarTrackCount pins F2: the
// All Tracks BrowseMetadata ChildCount MUST be read via the scalar
// TrackCount() and MUST NOT invoke ListTrackInfos() (which deep-copies
// every TrackInfo). Asserting only the ChildCount value would let a
// regression that merely MOVES the copy back slip through — so we assert
// the call counts directly.
func Test_CDS_BrowseMetadata_AllTracks_UsesScalarTrackCount(t *testing.T) {
	lib := &countingLib{tracks: []TrackInfo{
		testTrack("t1", "First"), testTrack("t2", "Second"), testTrack("t3", "Third"),
	}}
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildBrowseRequest(t, "1", "BrowseMetadata", 0, 0)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `childCount=&quot;3&quot;`) {
		t.Errorf("expected childCount=3: %s", rec.Body.String())
	}
	if got := lib.tcCalls.Load(); got < 1 {
		t.Errorf("TrackCount() should be called for ChildCount; got %d", got)
	}
	if got := lib.listCalls.Load(); got != 0 {
		t.Errorf("ListTrackInfos() must NOT run on the BrowseMetadata ChildCount path; got %d (the O(N) deep-copy is back)", got)
	}
}

// Test_CDS_Browse_LegacyAllTracksStringIDReturnsNoSuchObject pins
// the PR-pending backward-compat contract: controllers that cached
// the previous string ObjectID `"all_tracks"` (pre-PR-#315) MUST
// receive a clean `NoSuchObject` SOAP fault when requesting the
// legacy ID. Per UPnP convention this is the canonical signal for
// "this object has been replaced / moved" — controllers re-browse
// from root + pick up the new numeric `allTracksObjectID` ("1")
// transparently. Without this pin, a future refactor that adds a
// fall-through alias from "all_tracks" → "1" would silently break
// the migration contract.
func Test_CDS_Browse_LegacyAllTracksStringIDReturnsNoSuchObject(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Hello"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildBrowseRequest(t, "all_tracks", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (SOAP fault) for legacy ObjectID; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<errorCode>701</errorCode>`) {
		t.Errorf("expected UPnP 701 NoSuchObject for legacy `all_tracks` ID, got body: %s", body)
	}
}

// Test_CDS_Browse_MusicReturnsNoSuchObject pins that the dropped
// `music` container is no longer browseable. Cached requests from
// controllers that saw the stub IDs pre-PR-#309 get a clean
// `NoSuchObject` SOAP fault — the canonical UPnP signal for "this
// container is no longer available."
func Test_CDS_Browse_MusicReturnsNoSuchObject(t *testing.T) {
	lib := newTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildBrowseRequest(t, "music", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	// SOAP faults come back with HTTP 500 + a Fault envelope per spec.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (SOAP fault); body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Verify the typed UPnP error code (701 NoSuchObject) surfaces.
	for _, want := range []string{
		`<faultcode>`,
		`<errorCode>701</errorCode>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in SOAP fault body: %s", want, body)
		}
	}
}

func Test_CDS_Browse_AllTracksReturnsFlatList(t *testing.T) {
	lib := newTestLib(testTrack("t1", "First"), testTrack("t2", "Second"), testTrack("t3", "Third"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildBrowseRequest(t, "1", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Note: DIDL-Lite is XML-escaped inside the SOAP envelope, so
	// attributes like `id="t1"` appear as `id=&quot;t1&quot;` in the
	// body. File URLs (paths) aren't affected by XML escape and are
	// the cleanest unambiguous substring to look for.
	for _, want := range []string{
		`/dlna/file/t1`,
		`/dlna/file/t2`,
		`/dlna/file/t3`,
		`<NumberReturned>3</NumberReturned>`,
		`<TotalMatches>3</TotalMatches>`,
		// Track titles appear inside the escaped DIDL — look for the
		// escaped form.
		`&lt;dc:title&gt;First&lt;/dc:title&gt;`,
		`&lt;dc:title&gt;Second&lt;/dc:title&gt;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
}

// Test_CDS_Browse_AllTracks_ItemsCarryNumericParentID pins the
// PR #309 contract (updated for the PR-pending numeric-ObjectID
// fix): items emitted from Browse("1", BrowseDirectChildren) carry
// `parentID="1"` (the numeric `allTracksObjectID`), NOT the legacy
// hardcoded `parentID="0"`. Strict third-party DLNA controllers
// (mconnect Lite, Linn Kazoo) reject items whose parentID doesn't
// match the ObjectID just Browse'd; the numeric form also addresses
// mconnect's Cling-style int-parse rejection that broke the string
// form `"all_tracks"`.
func Test_CDS_Browse_AllTracks_ItemsCarryNumericParentID(t *testing.T) {
	lib := newTestLib(testTrack("t1", "First"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildBrowseRequest(t, "1", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `parentID=&quot;1&quot;`) {
		t.Errorf("expected items to carry parentID=\"1\" in response: %s", body)
	}
	// And specifically NOT the legacy `parentID="0"` anywhere in
	// the response. BrowseDirectChildren returns only the children
	// (NOT the container's own metadata), so there should be zero
	// `parentID=&quot;0&quot;` matches — pre-PR, items hardcoded
	// parentID="0" produced 1 match per item.
	got := strings.Count(body, `parentID=&quot;0&quot;`)
	if got != 0 {
		t.Errorf("expected 0 occurrences of legacy parentID=\"0\", got %d in body: %s",
			got, body)
	}
}

func Test_CDS_Browse_AllTracksPaginationWindow(t *testing.T) {
	var tracks []TrackInfo
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		tracks = append(tracks, testTrack(id, "T"+id))
	}
	lib := newTestLib(tracks...)
	h := ContentDirectoryHandler(lib, staticServerURL("http://x"))

	// Request items 3..7 (StartingIndex=3, RequestedCount=4)
	req := buildBrowseRequest(t, "1", "BrowseDirectChildren", 3, 4)
	rec := httptest.NewRecorder()
	h(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `<NumberReturned>4</NumberReturned>`) {
		t.Errorf("expected NumberReturned=4 for paginated window, got body: %s", body)
	}
	if !strings.Contains(body, `<TotalMatches>10</TotalMatches>`) {
		t.Errorf("TotalMatches must always report the full library count, got body: %s", body)
	}
	// Look at file URLs (unaffected by DIDL-Lite XML escape) for
	// presence/absence assertions on the paginated window.
	for _, want := range []string{`/dlna/file/d`, `/dlna/file/e`, `/dlna/file/f`, `/dlna/file/g`} {
		if !strings.Contains(body, want) {
			t.Errorf("paginated window missing %q in body: %s", want, body)
		}
	}
	for _, notWant := range []string{`/dlna/file/a`, `/dlna/file/c`, `/dlna/file/h`, `/dlna/file/j`} {
		if strings.Contains(body, notWant) {
			t.Errorf("paginated window unexpectedly contains %q in body: %s", notWant, body)
		}
	}
}

func Test_CDS_Browse_RequestedCountZeroMeansAll(t *testing.T) {
	tracks := []TrackInfo{testTrack("a", "A"), testTrack("b", "B")}
	lib := newTestLib(tracks...)
	h := ContentDirectoryHandler(lib, staticServerURL("http://x"))
	// Per UPnP convention, RequestedCount=0 means "return as many as possible"
	req := buildBrowseRequest(t, "1", "BrowseDirectChildren", 0, 0)
	rec := httptest.NewRecorder()
	h(rec, req)
	if !strings.Contains(rec.Body.String(), `<NumberReturned>2</NumberReturned>`) {
		t.Errorf("RequestedCount=0 should return all tracks, body: %s", rec.Body.String())
	}
}

func Test_CDS_Browse_UnknownObjectIDReturnsNoSuchObjectFault(t *testing.T) {
	lib := newTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://x"))
	req := buildBrowseRequest(t, "music/nonexistent/path", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for SOAPFault", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<errorCode>701</errorCode>") {
		t.Errorf("expected errorCode 701 (NoSuchObject), got: %s", body)
	}
}

func Test_CDS_NonPostReturns405(t *testing.T) {
	lib := newTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://x"))
	req := httptest.NewRequest(http.MethodGet, "/dlna/cds/control", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET should return 405, got %d", rec.Code)
	}
}

func Test_CDS_UnknownSOAPActionReturnsInvalidActionFault(t *testing.T) {
	lib := newTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://x"))
	req := httptest.NewRequest(http.MethodPost, "/dlna/cds/control", strings.NewReader(""))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#FakeAction"`)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("unknown action should return 500 SOAPFault, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<errorCode>401</errorCode>") {
		t.Errorf("expected errorCode 401 (InvalidAction), got: %s", rec.Body.String())
	}
}

func Test_CDS_MalformedXMLBodyReturnsInvalidArgsFault(t *testing.T) {
	lib := newTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://x"))
	req := httptest.NewRequest(http.MethodPost, "/dlna/cds/control", strings.NewReader("this is not XML"))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("malformed XML should return 500 SOAPFault, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<errorCode>402</errorCode>") {
		t.Errorf("expected errorCode 402 (InvalidArgs), got: %s", rec.Body.String())
	}
}

func Test_CDS_ResponseCarriesEXTHeader(t *testing.T) {
	lib := newTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://x"))
	req := buildBrowseRequest(t, "0", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	// UPnP UDA spec: every SOAP response MUST include an empty EXT
	// header. Use Values() to bypass net/http's canonical-key
	// normalization ("EXT" → "Ext"); .Values handles the lookup
	// canonically and returns a non-nil slice if the key was set
	// (even to an empty string).
	if vals := rec.Header().Values(SOAPResponseHeader); len(vals) == 0 {
		t.Errorf("response missing %s header (UDA spec requires)", SOAPResponseHeader)
	}
	if rec.Header().Get("Content-Type") != SOAPContentType {
		t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), SOAPContentType)
	}
}

// -----------------------------------------------------------------------------
// StaticLibrary
// -----------------------------------------------------------------------------

func Test_StaticLibrary_ListReturnsDefensiveCopy(t *testing.T) {
	lib := newTestLib(testTrack("a", "A"))
	got := lib.ListTrackInfos()
	got[0].Title = "Mutated"
	again := lib.ListTrackInfos()
	if again[0].Title == "Mutated" {
		t.Errorf("ListTrackInfos must return defensive copy; got internal mutation")
	}
}

func Test_StaticLibrary_GetTrackInfoLookup(t *testing.T) {
	lib := newTestLib(testTrack("a", "A"), testTrack("b", "B"))
	if track, ok := lib.GetTrackInfo("b"); !ok || track.Title != "B" {
		t.Errorf("GetTrackInfo(b) returned (%+v, %v), want (Title:B, true)", track, ok)
	}
	if _, ok := lib.GetTrackInfo("nonexistent"); ok {
		t.Error("GetTrackInfo(nonexistent) should return ok=false")
	}
}

// -----------------------------------------------------------------------------
// TrackInfo.toDIDLOpts
// -----------------------------------------------------------------------------

func Test_TrackInfo_toDIDLOpts_PreservesFields(t *testing.T) {
	track := TrackInfo{
		TrackID: "abc", Title: "T", Artist: "A", Album: "Alb",
		AlbumArtist: "AA", Composer: "Comp", Genre: "G", Year: 1965, TrackNumber: 3,
		Size: 1000, DurationSeconds: 60.5, SampleRateHz: 44100, BitsPerSample: 16,
		Channels: 2, IsDSD: false, Codec: "FLAC", FileExtension: ".flac",
		ArtworkURL: "http://art/x",
	}
	opts := track.toDIDLOpts("http://server", "TestUA", "1")
	if opts.TrackID != "abc" || opts.Title != "T" || opts.Year != 1965 ||
		opts.ServerURL != "http://server" || opts.UserAgent != "TestUA" ||
		opts.ParentID != "1" {
		t.Errorf("toDIDLOpts dropped fields: %+v", opts)
	}
}

// -----------------------------------------------------------------------------
// Bytes-end-to-end — actual file URL surfaces in escaped DIDL
// -----------------------------------------------------------------------------

func Test_CDS_Browse_FileURLContainsTrackID(t *testing.T) {
	lib := newTestLib(testTrack("xyz789", "MyTrack"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))
	req := buildBrowseRequest(t, "1", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	// The file URL is inside the DIDL-Lite which is escaped inside SOAP
	// response. Look for the escaped form.
	body := rec.Body.String()
	if !strings.Contains(body, "http://server:7790/dlna/file/xyz789") {
		t.Errorf("file URL with trackID not in response body. Body: %s", body)
	}
}

// -----------------------------------------------------------------------------
// PR #316 — spec-mandatory CDS:1 introspection actions
//
// Empirically validated 2026-05-28 against mconnect Player via the
// minimal Go-based test server at /tmp/upnp-test: when the SCPD declared
// only `Browse`, mconnect rendered the root container (Browse(0) →
// "All Tracks [121]") but tap-to-drill silently aborted. Adding the 3
// spec-mandatory introspection actions made mconnect drill into the
// child container successfully on the very next attempt.
//
// These tests pin the wire shape for each handler. The constants
// `<SearchCaps></SearchCaps>` / `<SortCaps></SortCaps>` / `<Id>1</Id>`
// are deliberately matched by literal substring so a future refactor
// that changes the response shape surfaces here.
// -----------------------------------------------------------------------------

// buildCDSActionRequest constructs a SOAP request envelope + http.Request
// for a CDS action that takes no input arguments (GetSearchCapabilities /
// GetSortCapabilities / GetSystemUpdateID).
func buildCDSActionRequest(t *testing.T, actionName string) *http.Request {
	t.Helper()
	body := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <u:` + actionName + ` xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"/>
  </s:Body>
</s:Envelope>`
	req := httptest.NewRequest(http.MethodPost, "/dlna/cds/control", strings.NewReader(body))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#`+actionName+`"`)
	req.Header.Set("Content-Type", "text/xml; charset=\"utf-8\"")
	return req
}

// Test_CDS_GetSearchCapabilities_AdvertisesSearchableFields pins that
// GetSearchCapabilities now advertises the searchable metadata fields
// (title / artist / album) rather than an empty SearchCaps. This is the
// spec signal that flips the Search action on in control points; an
// empty value would declare Search unsupported, but we DO implement it
// (see Test_CDS_Search_*).
func Test_CDS_GetSearchCapabilities_AdvertisesSearchableFields(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Hello"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildCDSActionRequest(t, "GetSearchCapabilities")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<u:GetSearchCapabilitiesResponse`,
		`<SearchCaps>dc:title,upnp:artist,upnp:album</SearchCaps>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
}

func Test_CDS_GetSortCapabilities_ReturnsEmptySortCaps(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Hello"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildCDSActionRequest(t, "GetSortCapabilities")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<u:GetSortCapabilitiesResponse`,
		`<SortCaps></SortCaps>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
}

func Test_CDS_GetSystemUpdateID_ReturnsStableID(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Hello"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildCDSActionRequest(t, "GetSystemUpdateID")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<u:GetSystemUpdateIDResponse`,
		`<Id>1</Id>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
}

// buildSearchRequest constructs a SOAP Search request with the canonical
// SOAPAction header.
func buildSearchRequest(t *testing.T, containerID, criteria string, startIdx, count uint32) *http.Request {
	t.Helper()
	body := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <u:Search xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">
      <ContainerID>` + containerID + `</ContainerID>
      <SearchCriteria>` + criteria + `</SearchCriteria>
      <Filter>*</Filter>
      <StartingIndex>` + uint32ToString(startIdx) + `</StartingIndex>
      <RequestedCount>` + uint32ToString(count) + `</RequestedCount>
      <SortCriteria></SortCriteria>
    </u:Search>
  </s:Body>
</s:Envelope>`
	req := httptest.NewRequest(http.MethodPost, "/dlna/cds/control", strings.NewReader(body))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Search"`)
	req.Header.Set("Content-Type", "text/xml; charset=\"utf-8\"")
	return req
}

// Test_CDS_Search_ReturnsMatchingItems pins the happy path: a Search with
// a `dc:title contains "..."` criteria returns the matching tracks as
// DIDL items with correct NumberReturned / TotalMatches.
func Test_CDS_Search_ReturnsMatchingItems(t *testing.T) {
	lib := newTestLib(
		testTrack("t1", "Blue Train"),
		testTrack("t2", "Red Car"),
		testTrack("t3", "Blue Moon"),
	)
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildSearchRequest(t, "0", `dc:title contains "blue"`, 0, 0)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<u:SearchResponse`,
		`<NumberReturned>2</NumberReturned>`,
		`<TotalMatches>2</TotalMatches>`,
		`&lt;dc:title&gt;Blue Train&lt;/dc:title&gt;`,
		`&lt;dc:title&gt;Blue Moon&lt;/dc:title&gt;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in Search response: %s", want, body)
		}
	}
	if strings.Contains(body, "Red Car") {
		t.Errorf("non-matching track leaked into Search response: %s", body)
	}
}

// Test_CDS_Search_RespectsPagination pins StartingIndex / RequestedCount.
func Test_CDS_Search_RespectsPagination(t *testing.T) {
	lib := newTestLib(
		testTrack("t1", "Blue Train"),
		testTrack("t2", "Blue Moon"),
		testTrack("t3", "Blue Note"),
	)
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildSearchRequest(t, "0", `dc:title contains "blue"`, 0, 1)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<NumberReturned>1</NumberReturned>`) {
		t.Errorf("RequestedCount=1 should return 1 item: %s", body)
	}
	if !strings.Contains(body, `<TotalMatches>3</TotalMatches>`) {
		t.Errorf("TotalMatches should report full match count 3: %s", body)
	}
}

// Test_CDS_Search_DegradesNotFaults pins that an empty / bare-"*" /
// class-only criteria yields zero matches with a 200 OK, never a SOAP
// fault.
func Test_CDS_Search_DegradesNotFaults(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Blue Train"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	for _, criteria := range []string{
		`*`,
		``,
		`upnp:class derivedfrom "object.item.audioItem"`,
	} {
		req := buildSearchRequest(t, "0", criteria, 0, 0)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("criteria %q: status = %d, want 200 (degrade, not fault)", criteria, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `<NumberReturned>0</NumberReturned>`) {
			t.Errorf("criteria %q: expected 0 matches; body: %s", criteria, rec.Body.String())
		}
	}
}

// Test_searchCriteriaTerms pins the UPnP-SearchCriteria → free-text
// extraction, including the class-URN skip and multi-clause join.
func Test_searchCriteriaTerms(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`dc:title contains "blue train"`, "blue train"},
		{`upnp:artist contains "davis"`, "davis"},
		{`(upnp:class derivedfrom "object.item.audioItem") and (dc:title contains "miles")`, "miles"},
		{`upnp:artist contains "davis" or dc:album contains "kind"`, "davis kind"},
		{`*`, ""},
		{``, ""},
		{`dc:title contains ""`, ""},
		{`upnp:class derivedfrom "object.container"`, ""},
	}
	for _, tc := range cases {
		if got := searchCriteriaTerms(tc.in); got != tc.want {
			t.Errorf("searchCriteriaTerms(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Test_extractQuotedStrings pins the quoted-literal scanner including the
// backslash-escape and unterminated-final-quote handling.
func Test_extractQuotedStrings(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`a "one" b "two"`, []string{"one", "two"}},
		{`"esc \" inside"`, []string{`esc " inside`}},
		{`"unterminated`, nil},
		{`no quotes`, nil},
	}
	for _, tc := range cases {
		got := extractQuotedStrings(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("extractQuotedStrings(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("extractQuotedStrings(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// Test_CDS_SCPD_AdvertisesSpecMandatoryActions pins the SCPD wire shape
// so a future drop of any of the 3 introspection actions surfaces here
// — that drop would re-open the mconnect-silent-drill-abort regression
// PR #316 closes.
func Test_CDS_SCPD_AdvertisesSpecMandatoryActions(t *testing.T) {
	scpd := ContentDirectorySCPDXML
	for _, want := range []string{
		`<name>GetSearchCapabilities</name>`,
		`<name>GetSortCapabilities</name>`,
		`<name>GetSystemUpdateID</name>`,
		`<name>Browse</name>`,
		`<name>Search</name>`,
		`<name>SearchCapabilities</name>`,
		`<name>SortCapabilities</name>`,
		`<name>SystemUpdateID</name>`,
	} {
		if !strings.Contains(scpd, want) {
			t.Errorf("ContentDirectorySCPDXML missing %q — would re-open mconnect drill regression", want)
		}
	}
}

// -----------------------------------------------------------------------------
// PR #316 — folder hierarchy in CDS Browse
//
// Integration tests for the Browse handler's new dispatch arms:
//   - Browse("2", BrowseDirectChildren) — the Folders root container
//   - Browse(hashedFolderID, BrowseDirectChildren) — a specific folder
//   - Browse(hashedFolderID, BrowseMetadata)       — folder metadata
//
// These tests use the live `ContentDirectoryHandler` + `StaticLibrary`
// shape so the folder index → Browse → DIDL emission path is exercised
// end-to-end. Unit tests for the FolderIndex shape itself live in
// folder_index_test.go.
// -----------------------------------------------------------------------------

// folderTestLib returns a StaticLibrary with a small hierarchy:
//
//	/library/Artist A/Album X/track 01.flac
//	/library/Artist A/Album X/track 02.flac
//	/library/Artist B/track 01.flac
//
// Two top-level folders, one nested two levels deep, one flat.
func folderTestLib() *StaticLibrary {
	return newTestLib(
		folderTrack("t1", "Artist A/Album X/track 01.flac"),
		folderTrack("t2", "Artist A/Album X/track 02.flac"),
		folderTrack("t3", "Artist B/track 01.flac"),
	)
}

func Test_CDS_Browse_FoldersRoot_ReturnsTopLevelFolders(t *testing.T) {
	lib := folderTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))
	req := buildBrowseRequest(t, foldersRootObjectID, "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Two top-level folders surface as containers.
	for _, want := range []string{
		`&lt;dc:title&gt;Artist A&lt;/dc:title&gt;`,
		`&lt;dc:title&gt;Artist B&lt;/dc:title&gt;`,
		`<NumberReturned>2</NumberReturned>`,
		`<TotalMatches>2</TotalMatches>`,
		// Every container is a storageFolder with searchable=1 +
		// storageUsed=-1 per the 2Go reference shape (PR #314).
		`&lt;upnp:class&gt;object.container.storageFolder&lt;/upnp:class&gt;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
}

func Test_CDS_Browse_SubFolder_ReturnsChildren(t *testing.T) {
	lib := folderTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))

	// Resolve the Artist A folder ID via the same hash the handler
	// produces.
	artistAID := FolderObjectID("Artist A")

	req := buildBrowseRequest(t, artistAID, "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Artist A contains a single Album X sub-folder.
	for _, want := range []string{
		`&lt;dc:title&gt;Album X&lt;/dc:title&gt;`,
		`<NumberReturned>1</NumberReturned>`,
		`<TotalMatches>1</TotalMatches>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
}

func Test_CDS_Browse_DeepestFolder_ReturnsTrackItems(t *testing.T) {
	lib := folderTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))

	// Album X is the deepest folder for Artist A — it contains the
	// two tracks directly.
	albumXID := FolderObjectID("Artist A/Album X")

	req := buildBrowseRequest(t, albumXID, "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Two tracks surface as items.
	for _, want := range []string{
		`<NumberReturned>2</NumberReturned>`,
		`<TotalMatches>2</TotalMatches>`,
		// Items reference their parent folder ID.
		`parentID=&quot;` + albumXID + `&quot;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
}

func Test_CDS_Browse_FolderMetadata_ReturnsContainerSelf(t *testing.T) {
	lib := folderTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))

	artistAID := FolderObjectID("Artist A")
	req := buildBrowseRequest(t, artistAID, "BrowseMetadata", 0, 0)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// BrowseMetadata for a folder returns a single DIDL container
	// element describing the folder itself.
	for _, want := range []string{
		`id=&quot;` + artistAID + `&quot;`,
		`&lt;dc:title&gt;Artist A&lt;/dc:title&gt;`,
		`<NumberReturned>1</NumberReturned>`,
		`<TotalMatches>1</TotalMatches>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
}

func Test_CDS_Browse_UnknownFolderID_ReturnsNoSuchObject(t *testing.T) {
	lib := folderTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))

	// ObjectID guaranteed not to match any folder hash — pure literal
	// numeric outside the hash space.
	req := buildBrowseRequest(t, "999999", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (SOAPFault); body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<errorCode>701</errorCode>`) {
		t.Errorf("body missing NoSuchObject (701) error code: %s", body)
	}
}
