package dlna

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestBrowseAndSearchCapAZeroRequestedCountAndAClientCanPageTheRest
// asks for every track the way a RequestedCount of 0 does, then walks
// StartingIndex forward by NumberReturned. Each track is collected once.
func TestBrowseAndSearchCapAZeroRequestedCountAndAClientCanPageTheRest(t *testing.T) {
	const total = 1050
	tracks := make([]TrackInfo, total)
	for i := range tracks {
		tracks[i] = testTrack(fmt.Sprintf("trk%04d", i), fmt.Sprintf("Cap %04d", i))
	}
	lib := newTestLib(tracks...)
	h := ContentDirectoryHandler(lib, staticServerURL("http://127.0.0.1:9"))

	browse := func(start, count uint32) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h(rec, buildBrowseRequest(t, allTracksObjectID, "BrowseDirectChildren", start, count))
		if rec.Code != http.StatusOK {
			t.Fatalf("browse status %d", rec.Code)
		}
		return rec.Body.String()
	}
	body := browse(0, 0)
	n, tot, upd := soapCounts(t, body)
	if n != maxCDSPage || tot != total || upd != 1 {
		t.Fatalf("Browse RequestedCount=0 NumberReturned=%d TotalMatches=%d UpdateID=%d, want %d, %d, 1",
			n, tot, upd, maxCDSPage, total)
	}

	rec := httptest.NewRecorder()
	h(rec, buildSearchRequest(t, "0", `dc:title contains "Cap"`, 0, 0))
	if rec.Code != http.StatusOK {
		t.Fatalf("search status %d", rec.Code)
	}
	sn, st, su := soapCounts(t, rec.Body.String())
	if sn != maxCDSPage || st != total || su != 1 {
		t.Fatalf("Search RequestedCount=0 NumberReturned=%d TotalMatches=%d UpdateID=%d, want %d, %d, 1",
			sn, st, su, maxCDSPage, total)
	}

	for _, count := range []uint32{1, 200, 1000} {
		page := browse(0, count)
		pn, pt, pu := soapCounts(t, page)
		if pn != int(count) || pt != total || pu != 1 {
			t.Errorf("RequestedCount %d NumberReturned=%d TotalMatches=%d UpdateID=%d", count, pn, pt, pu)
		}
	}

	seen := map[string]int{}
	start := uint32(0)
	for pages := 0; pages < 8; pages++ {
		page := browse(start, 0)
		pn, pt, _ := soapCounts(t, page)
		if pt != total {
			t.Fatalf("page at %d TotalMatches=%d", start, pt)
		}
		ids := didlItemIDs(page)
		if len(ids) != pn {
			t.Fatalf("page at %d has %d item ids, NumberReturned=%d", start, len(ids), pn)
		}
		for _, id := range ids {
			seen[id]++
		}
		if pn == 0 {
			break
		}
		start += uint32(pn)
		if int(start) >= pt {
			break
		}
	}
	if len(seen) != total {
		t.Fatalf("collected %d tracks, want %d", len(seen), total)
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
