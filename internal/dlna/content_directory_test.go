package dlna

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// Test_CDS_Browse_RootReturnsAllTracksOnly pins the post-PR-#309
// contract: the root container exposes ONLY `all_tracks`. The
// `Music` container was removed because its `music` Browse case
// returned 4 empty placeholder sub-containers with no real
// hierarchy implementation — strict third-party DLNA controllers
// (mconnect Lite 2026-05-28) refused to render the empty
// placeholders AND occasionally bailed to a "Browse failed" state
// that prevented navigation to `all_tracks` too. Music returns
// once the by-Artist / by-Album / by-Genre indexes are real.
func Test_CDS_Browse_RootReturnsAllTracksOnly(t *testing.T) {
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
		`id=&quot;all_tracks&quot;`,
		`&lt;dc:title&gt;All Tracks&lt;/dc:title&gt;`,
		`<NumberReturned>1</NumberReturned>`,
		`<TotalMatches>1</TotalMatches>`,
		// PR-pending: storageFolder subtype + searchable="0" on the
		// container — strict third-party DLNA controllers (mconnect
		// Lite confirmed 2026-05-28) drop child rendering when the
		// container is generic `object.container` OR when the
		// `searchable` attribute is missing. Per Gemini consult.
		`&lt;upnp:class&gt;object.container.storageFolder&lt;/upnp:class&gt;`,
		`searchable=&quot;0&quot;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
	// And specifically NOT the dropped `music` container.
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
	req := buildBrowseRequest(t, "all_tracks", "BrowseMetadata", 0, 0)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<NumberReturned>1</NumberReturned>`,
		`<TotalMatches>1</TotalMatches>`,
		`id=&quot;all_tracks&quot;`,
		`parentID=&quot;0&quot;`,
		`&lt;dc:title&gt;All Tracks&lt;/dc:title&gt;`,
		`&lt;upnp:class&gt;object.container.storageFolder&lt;/upnp:class&gt;`,
		`childCount=&quot;3&quot;`,
		`searchable=&quot;0&quot;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in BrowseMetadata response: %s", want, body)
		}
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
	req := buildBrowseRequest(t, "all_tracks", "BrowseDirectChildren", 0, 100)
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

// Test_CDS_Browse_AllTracks_ItemsCarryAllTracksParentID pins the
// PR #309 contract: items emitted from Browse(all_tracks) carry
// `parentID="all_tracks"` (escaped as `parentID=&quot;all_tracks&quot;`
// inside the SOAP envelope), NOT the legacy hardcoded `parentID="0"`.
// Strict third-party DLNA controllers (mconnect Lite, Linn Kazoo)
// reject items whose parentID doesn't match the ObjectID just
// Browse'd; pre-PR they refused to render the children.
func Test_CDS_Browse_AllTracks_ItemsCarryAllTracksParentID(t *testing.T) {
	lib := newTestLib(testTrack("t1", "First"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildBrowseRequest(t, "all_tracks", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `parentID=&quot;all_tracks&quot;`) {
		t.Errorf("expected items to carry parentID=\"all_tracks\" in response: %s", body)
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
	req := buildBrowseRequest(t, "all_tracks", "BrowseDirectChildren", 3, 4)
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
	req := buildBrowseRequest(t, "all_tracks", "BrowseDirectChildren", 0, 0)
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
	opts := track.toDIDLOpts("http://server", "TestUA", "all_tracks")
	if opts.TrackID != "abc" || opts.Title != "T" || opts.Year != 1965 ||
		opts.ServerURL != "http://server" || opts.UserAgent != "TestUA" ||
		opts.ParentID != "all_tracks" {
		t.Errorf("toDIDLOpts dropped fields: %+v", opts)
	}
}

// -----------------------------------------------------------------------------
// Bytes-end-to-end — actual file URL surfaces in escaped DIDL
// -----------------------------------------------------------------------------

func Test_CDS_Browse_FileURLContainsTrackID(t *testing.T) {
	lib := newTestLib(testTrack("xyz789", "MyTrack"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))
	req := buildBrowseRequest(t, "all_tracks", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	// The file URL is inside the DIDL-Lite which is escaped inside SOAP
	// response. Look for the escaped form.
	body := rec.Body.String()
	if !strings.Contains(body, "http://server:7790/dlna/file/xyz789") {
		t.Errorf("file URL with trackID not in response body. Body: %s", body)
	}
}
