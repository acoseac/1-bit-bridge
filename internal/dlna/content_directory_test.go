package dlna

import (
	"io"
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

func Test_CDS_Browse_RootReturnsMusicAndAllTracks(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Hello"), testTrack("t2", "World"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://server:7790"))
	req := buildBrowseRequest(t, "0", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// DIDL-Lite is XML-escaped inside SOAP; check for the escaped forms
	// of attribute values and tag content.
	for _, want := range []string{
		`id=&quot;music&quot;`,
		`&lt;dc:title&gt;Music&lt;/dc:title&gt;`,
		`id=&quot;all_tracks&quot;`,
		`&lt;dc:title&gt;All Tracks&lt;/dc:title&gt;`,
		`<NumberReturned>2</NumberReturned>`,
		`<TotalMatches>2</TotalMatches>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
		}
	}
}

func Test_CDS_Browse_MusicReturnsCategoryContainers(t *testing.T) {
	lib := newTestLib()
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))
	req := buildBrowseRequest(t, "music", "BrowseDirectChildren", 0, 100)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id=&quot;music/artists&quot;`,
		`&lt;dc:title&gt;Artists&lt;/dc:title&gt;`,
		`id=&quot;music/albums&quot;`,
		`&lt;dc:title&gt;Albums&lt;/dc:title&gt;`,
		`id=&quot;music/genres&quot;`,
		`&lt;dc:title&gt;Genres&lt;/dc:title&gt;`,
		`id=&quot;music/composers&quot;`,
		`&lt;dc:title&gt;Composers&lt;/dc:title&gt;`,
		`<NumberReturned>4</NumberReturned>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing substring %q in response body: %s", want, body)
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
	opts := track.toDIDLOpts("http://server", "TestUA")
	if opts.TrackID != "abc" || opts.Title != "T" || opts.Year != 1965 ||
		opts.ServerURL != "http://server" || opts.UserAgent != "TestUA" {
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

// -----------------------------------------------------------------------------
// Helper: drain ioutil-style (avoids importing io.Discard alias)
// -----------------------------------------------------------------------------

func drainBody(body io.Reader) {
	_, _ = io.Copy(io.Discard, body)
}
