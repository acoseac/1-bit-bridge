package dlna

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const cappedCatalogSize = 1050

// cappedCatalogHandler serves cappedCatalogSize tracks titled "Cap NNNN".
func cappedCatalogHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	tracks := make([]TrackInfo, cappedCatalogSize)
	for i := range tracks {
		tracks[i] = testTrack(fmt.Sprintf("trk%04d", i), fmt.Sprintf("Cap %04d", i))
	}
	return ContentDirectoryHandler(newTestLib(tracks...), staticServerURL("http://127.0.0.1:9"))
}

func browseCapped(t *testing.T, h http.HandlerFunc, start, count uint32) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, buildBrowseRequest(t, allTracksObjectID, "BrowseDirectChildren", start, count))
	if rec.Code != http.StatusOK {
		t.Fatalf("browse status %d", rec.Code)
	}
	return rec.Body.String()
}

// TestBrowseAndSearchCapAZeroRequestedCountAndAClientCanPageTheRest
// asks for every track the way a RequestedCount of 0 does. Browse and
// Search each return one page, and TotalMatches stays the whole set.
func TestBrowseAndSearchCapAZeroRequestedCountAndAClientCanPageTheRest(t *testing.T) {
	h := cappedCatalogHandler(t)
	n, tot, upd := soapCounts(t, browseCapped(t, h, 0, 0))
	if n != maxCDSPage || tot != cappedCatalogSize || upd != 1 {
		t.Fatalf("Browse RequestedCount=0 NumberReturned=%d TotalMatches=%d UpdateID=%d, want %d, %d, 1",
			n, tot, upd, maxCDSPage, cappedCatalogSize)
	}

	rec := httptest.NewRecorder()
	h(rec, buildSearchRequest(t, "0", `dc:title contains "Cap"`, 0, 0))
	if rec.Code != http.StatusOK {
		t.Fatalf("search status %d", rec.Code)
	}
	sn, st, su := soapCounts(t, rec.Body.String())
	if sn != maxCDSPage || st != cappedCatalogSize || su != 1 {
		t.Fatalf("Search RequestedCount=0 NumberReturned=%d TotalMatches=%d UpdateID=%d, want %d, %d, 1",
			sn, st, su, maxCDSPage, cappedCatalogSize)
	}
}

// TestARequestedCountUpToThePageIsReturnedWhole checks the counts a
// control point actually sends: 1, the app's 200, and the page itself.
func TestARequestedCountUpToThePageIsReturnedWhole(t *testing.T) {
	h := cappedCatalogHandler(t)
	for _, count := range []uint32{1, 200, 1000} {
		pn, pt, pu := soapCounts(t, browseCapped(t, h, 0, count))
		if pn != int(count) || pt != cappedCatalogSize || pu != 1 {
			t.Errorf("RequestedCount %d NumberReturned=%d TotalMatches=%d UpdateID=%d", count, pn, pt, pu)
		}
	}
}

// TestAClientPagingByNumberReturnedCollectsEveryCappedTrackOnce walks
// StartingIndex forward by NumberReturned and requires each id once.
func TestAClientPagingByNumberReturnedCollectsEveryCappedTrackOnce(t *testing.T) {
	h := cappedCatalogHandler(t)
	seen := map[string]int{}
	start := uint32(0)
	for pages := 0; pages < 8; pages++ {
		pn, pt := noteCappedPage(t, seen, browseCapped(t, h, start, 0), start)
		if pn == 0 || int(start)+pn >= pt {
			break
		}
		start += uint32(pn)
	}
	requireEachIDOnce(t, seen, cappedCatalogSize)
}

func noteCappedPage(t *testing.T, seen map[string]int, page string, start uint32) (numberReturned, totalMatches int) {
	t.Helper()
	numberReturned, totalMatches, _ = soapCounts(t, page)
	if totalMatches != cappedCatalogSize {
		t.Fatalf("page at %d TotalMatches=%d", start, totalMatches)
	}
	ids := didlItemIDs(page)
	if len(ids) != numberReturned {
		t.Fatalf("page at %d has %d item ids, NumberReturned=%d", start, len(ids), numberReturned)
	}
	for _, id := range ids {
		seen[id]++
	}
	return numberReturned, totalMatches
}

func requireEachIDOnce(t *testing.T, seen map[string]int, want int) {
	t.Helper()
	if len(seen) != want {
		t.Fatalf("collected %d tracks, want %d", len(seen), want)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("%s seen %d times", id, c)
		}
	}
}

func soapCounts(t *testing.T, body string) (numberReturned, totalMatches, updateID int) {
	t.Helper()
	var ok bool
	numberReturned, ok = soapInt(body, "NumberReturned")
	if !ok {
		t.Fatalf("no NumberReturned in %.200q", body)
	}
	totalMatches, ok = soapInt(body, "TotalMatches")
	if !ok {
		t.Fatalf("no TotalMatches")
	}
	updateID, ok = soapInt(body, "UpdateID")
	if !ok {
		t.Fatalf("no UpdateID")
	}
	return numberReturned, totalMatches, updateID
}

func soapInt(body, tag string) (int, bool) {
	open := "<" + tag + ">"
	i := strings.Index(body, open)
	if i < 0 {
		return 0, false
	}
	body = body[i+len(open):]
	j := strings.Index(body, "</"+tag+">")
	if j < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(body[:j])
	if err != nil {
		return 0, false
	}
	return n, true
}

// didlItemIDs reads item ids out of an escaped DIDL Result. The leading
// space keeps a parentID attribute out of the set.
func didlItemIDs(body string) []string {
	const mark = ` id=&quot;`
	var ids []string
	for {
		i := strings.Index(body, mark)
		if i < 0 {
			return ids
		}
		body = body[i+len(mark):]
		j := strings.Index(body, `&quot;`)
		if j < 0 {
			return ids
		}
		ids = append(ids, body[:j])
		body = body[j:]
	}
}

// TestBrowseAndSearchLogsTruncateClientFieldsAndDoNotRepeatThem sends
// client fields longer than the User-Agent cut. The request line keeps a
// truncated copy. The response and fault lines do not repeat it.
func TestBrowseAndSearchLogsTruncateClientFieldsAndDoNotRepeatThem(t *testing.T) {
	lib := newTestLib(testTrack("t1", "Test Track"))
	h := ContentDirectoryHandler(lib, staticServerURL("http://127.0.0.1:9"))
	logs := captureDLNALogs(t)

	longZ := strings.Repeat("Z", 300)
	longF := strings.Repeat("F", 300)
	longS := strings.Repeat("S", 300)
	longQ := strings.Repeat("Q", 300)

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/ctl", strings.NewReader(body))
		req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
		req.Header.Set("Content-Type", "text/xml; charset=\"utf-8\"")
		h(httptest.NewRecorder(), req)
	}
	post(browseSOAP(longZ, "BrowseDirectChildren", "*", "", 0, 1))
	post(browseSOAP(allTracksObjectID, "BrowseDirectChildren", longF, longS, 0, 1))

	searchBody := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <u:Search xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">
      <ContainerID>0</ContainerID>
      <SearchCriteria>dc:title contains "` + longQ + `"</SearchCriteria>
      <Filter>*</Filter>
      <StartingIndex>0</StartingIndex>
      <RequestedCount>1</RequestedCount>
      <SortCriteria></SortCriteria>
    </u:Search>
  </s:Body>
</s:Envelope>`
	sreq := httptest.NewRequest(http.MethodPost, "/ctl", strings.NewReader(searchBody))
	sreq.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Search"`)
	sreq.Header.Set("Content-Type", "text/xml; charset=\"utf-8\"")
	h(httptest.NewRecorder(), sreq)

	lines := strings.Split(logs.String(), "\n")
	reqZ := lineContaining(lines, `msg="Browse request"`, strings.Repeat("Z", 20))
	faultZ := lineContaining(lines, `msg="Browse fault"`, "")
	reqF := lineContaining(lines, `msg="Browse request"`, strings.Repeat("F", 20))
	resp := lineContaining(lines, `msg="Browse response"`, "")
	sreqLine := lineContaining(lines, `msg="Search request"`, strings.Repeat("Q", 20))
	sresp := lineContaining(lines, `msg="Search response"`, "")
	if reqZ == "" || faultZ == "" || reqF == "" || resp == "" || sreqLine == "" || sresp == "" {
		t.Fatalf("missing a log line:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(reqZ, strings.Repeat("Z", 101)) {
		t.Errorf("Browse request kept the ObjectID whole: %.180s", reqZ)
	}
	if !strings.Contains(reqZ, "objectID=") {
		t.Errorf("Browse request dropped objectID: %.180s", reqZ)
	}
	if strings.Contains(faultZ, "objectID=") || strings.Contains(faultZ, strings.Repeat("Z", 20)) {
		t.Errorf("Browse fault repeats the ObjectID: %.180s", faultZ)
	}
	if strings.Contains(reqF, strings.Repeat("F", 101)) || strings.Contains(reqF, strings.Repeat("S", 101)) {
		t.Errorf("Browse request kept Filter or SortCriteria whole: %.180s", reqF)
	}
	if !strings.Contains(reqF, "filter=") || !strings.Contains(reqF, "sortCriteria=") {
		t.Errorf("Browse request dropped a field: %.180s", reqF)
	}
	if strings.Contains(resp, "objectID=") {
		t.Errorf("Browse response repeats objectID: %.180s", resp)
	}
	if strings.Contains(sreqLine, strings.Repeat("Q", 101)) {
		t.Errorf("Search request kept SearchCriteria whole: %.180s", sreqLine)
	}
	if !strings.Contains(sreqLine, "searchCriteria=") {
		t.Errorf("Search request dropped searchCriteria: %.180s", sreqLine)
	}
	if strings.Contains(sresp, "searchCriteria=") || strings.Contains(sresp, "containerID=") {
		t.Errorf("Search response repeats a request field: %.180s", sresp)
	}
}

func browseSOAP(objectID, flag, filter, sort string, start, count uint32) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
  <s:Body>
    <u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">
      <ObjectID>` + objectID + `</ObjectID>
      <BrowseFlag>` + flag + `</BrowseFlag>
      <Filter>` + filter + `</Filter>
      <StartingIndex>` + uint32ToString(start) + `</StartingIndex>
      <RequestedCount>` + uint32ToString(count) + `</RequestedCount>
      <SortCriteria>` + sort + `</SortCriteria>
    </u:Browse>
  </s:Body>
</s:Envelope>`
}

func lineContaining(lines []string, parts ...string) string {
	for _, line := range lines {
		ok := true
		for _, p := range parts {
			if p != "" && !strings.Contains(line, p) {
				ok = false
				break
			}
		}
		if ok {
			return line
		}
	}
	return ""
}
