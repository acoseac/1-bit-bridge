package upnp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// stubResp is one canned dispatcher response.
type stubResp struct {
	status int
	body   string
	err    error
}

// stubDispatcher implements discovery.SOAPDispatcher with a queue of
// canned responses (advancing per call; the last response repeats once
// the queue is exhausted) and captures the requests + envelope bodies.
type stubDispatcher struct {
	queue  []stubResp
	idx    int
	reqs   []*http.Request
	bodies []string
}

func (s *stubDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	s.reqs = append(s.reqs, req)
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		s.bodies = append(s.bodies, string(b))
	} else {
		s.bodies = append(s.bodies, "")
	}
	var r stubResp
	switch {
	case s.idx < len(s.queue):
		r = s.queue[s.idx]
	case len(s.queue) > 0:
		r = s.queue[len(s.queue)-1]
	}
	s.idx++
	if r.err != nil {
		return nil, r.err
	}
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Header:     make(http.Header),
	}, nil
}

const testControlURL = "http://192.168.0.62:8200/ctl/ContentDir"

func TestBrowse_SendsActionAndArgs(t *testing.T) {
	stub := &stubDispatcher{queue: []stubResp{{status: 200, body: string(wrapBrowse(`<DIDL-Lite></DIDL-Lite>`, 0, 0))}}}
	c := NewContentDirectoryClient(stub)
	if _, err := c.Browse(context.Background(), testControlURL, "0", "BrowseDirectChildren", "*", 0, 200, ""); err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(stub.reqs) != 1 {
		t.Fatalf("reqs = %d; want 1", len(stub.reqs))
	}
	if got := stub.reqs[0].Header.Get("SOAPAction"); got != `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"` {
		t.Fatalf("SOAPAction = %q", got)
	}
	if ct := stub.reqs[0].Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	env := stub.bodies[0]
	for _, want := range []string{
		`<u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">`,
		`<ObjectID>0</ObjectID>`,
		`<BrowseFlag>BrowseDirectChildren</BrowseFlag>`,
		`<Filter>*</Filter>`,
		`<StartingIndex>0</StartingIndex>`,
		`<RequestedCount>200</RequestedCount>`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("envelope missing %q\n--- envelope ---\n%s", want, env)
		}
	}
}

func TestSearch_SendsActionAndEscapedCriteria(t *testing.T) {
	stub := &stubDispatcher{queue: []stubResp{{status: 200, body: string(wrapBrowse(`<DIDL-Lite></DIDL-Lite>`, 0, 0))}}}
	c := NewContentDirectoryClient(stub)
	crit := `dc:title = "Lilac" and upnp:artist = "X"`
	if _, err := c.Search(context.Background(), testControlURL, "0", crit, "*", 0, 10, ""); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := stub.reqs[0].Header.Get("SOAPAction"); got != `"urn:schemas-upnp-org:service:ContentDirectory:1#Search"` {
		t.Fatalf("SOAPAction = %q", got)
	}
	env := stub.bodies[0]
	if !strings.Contains(env, `<ContainerID>0</ContainerID>`) {
		t.Errorf("envelope missing ContainerID\n%s", env)
	}
	// MiniDLNA's rigid SearchCriteria parser rejects NUMERIC char refs
	// (&#34; -> 708 fault) but accepts named entities — A/B-confirmed
	// against the live 2Go. The builder MUST use named-entity escaping.
	if !strings.Contains(env, `dc:title = &quot;Lilac&quot;`) {
		t.Errorf("SearchCriteria quotes not named-entity escaped\n%s", env)
	}
	if strings.Contains(env, "&#34;") {
		t.Errorf("SearchCriteria uses numeric char refs (MiniDLNA rejects with 708)\n%s", env)
	}
}

func TestEscapeXMLArg_UsesNamedEntities(t *testing.T) {
	// Named entities only — MiniDLNA rejects numeric char refs in
	// SearchCriteria with a 708 fault (verified against the 2Go).
	got := escapeXMLArg(`a&b<c>d"e'f`)
	want := `a&amp;b&lt;c&gt;d&quot;e&apos;f`
	if got != want {
		t.Fatalf("escapeXMLArg = %q; want %q", got, want)
	}
}

func TestGetSystemUpdateID_ParsesID(t *testing.T) {
	resp := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:GetSystemUpdateIDResponse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><Id>0</Id></u:GetSystemUpdateIDResponse>` +
		`</s:Body></s:Envelope>`
	stub := &stubDispatcher{queue: []stubResp{{status: 200, body: resp}}}
	c := NewContentDirectoryClient(stub)
	id, err := c.GetSystemUpdateID(context.Background(), testControlURL)
	if err != nil {
		t.Fatalf("GetSystemUpdateID: %v", err)
	}
	// The live 2Go returns "0" — verbatim, untrusted by the ingest layer.
	if id != "0" {
		t.Fatalf("id = %q; want %q", id, "0")
	}
	if got := stub.reqs[0].Header.Get("SOAPAction"); got != `"urn:schemas-upnp-org:service:ContentDirectory:1#GetSystemUpdateID"` {
		t.Fatalf("SOAPAction = %q", got)
	}
}

func TestBrowseAll_Paginates(t *testing.T) {
	page1 := wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/">`+
			`<item id="a" parentID="0"><dc:title>A</dc:title></item>`+
			`<item id="b" parentID="0"><dc:title>B</dc:title></item>`+
			`</DIDL-Lite>`, 2, 3)
	page2 := wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/">`+
			`<item id="c" parentID="0"><dc:title>C</dc:title></item>`+
			`</DIDL-Lite>`, 1, 3)
	stub := &stubDispatcher{queue: []stubResp{
		{status: 200, body: string(page1)},
		{status: 200, body: string(page2)},
	}}
	c := NewContentDirectoryClient(stub)
	containers, items, err := c.BrowseAll(context.Background(), testControlURL, "0")
	if err != nil {
		t.Fatalf("BrowseAll: %v", err)
	}
	if len(containers) != 0 || len(items) != 3 {
		t.Fatalf("got %d containers / %d items; want 0 / 3", len(containers), len(items))
	}
	if len(stub.reqs) != 2 {
		t.Fatalf("Browse calls = %d; want 2 (pagination)", len(stub.reqs))
	}
	if items[0].ID != "a" || items[2].ID != "c" {
		t.Fatalf("items out of order: %q..%q", items[0].ID, items[2].ID)
	}
	// Second page's StartingIndex must advance by the first page's NumberReturned.
	if !strings.Contains(stub.bodies[1], `<StartingIndex>2</StartingIndex>`) {
		t.Errorf("page-2 StartingIndex not advanced:\n%s", stub.bodies[1])
	}
}

func TestBrowse_SOAPFault500_SurfacesErrSOAPFault(t *testing.T) {
	fault := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<s:Fault><faultcode>s:Client</faultcode><detail><UPnPError><errorCode>701</errorCode>` +
		`<errorDescription>No such object</errorDescription></UPnPError></detail></s:Fault>` +
		`</s:Body></s:Envelope>`
	stub := &stubDispatcher{queue: []stubResp{{status: 500, body: fault}}}
	c := NewContentDirectoryClient(stub)
	_, err := c.Browse(context.Background(), testControlURL, "999", "BrowseDirectChildren", "*", 0, 200, "")
	if !errors.Is(err, ErrSOAPFault) {
		t.Fatalf("err = %v; want ErrSOAPFault", err)
	}
}

func TestBrowse_Non200NonFault_IsHardError(t *testing.T) {
	stub := &stubDispatcher{queue: []stubResp{{status: 404, body: "not found"}}}
	c := NewContentDirectoryClient(stub)
	_, err := c.Browse(context.Background(), testControlURL, "0", "BrowseDirectChildren", "*", 0, 200, "")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if errors.Is(err, ErrSOAPFault) {
		t.Fatalf("404 should NOT be ErrSOAPFault; got %v", err)
	}
}

func TestBrowse_EmptyControlURL_Errors(t *testing.T) {
	c := NewContentDirectoryClient(&stubDispatcher{})
	if _, err := c.Browse(context.Background(), "", "0", "BrowseDirectChildren", "*", 0, 200, ""); err == nil {
		t.Fatal("expected error on empty controlURL")
	}
}

func TestInvoke_BindsContextOnRequest(t *testing.T) {
	// http.NewRequestWithContext must bind ctx to req.Context() so any
	// downstream middleware honors cancellation/deadlines. The stub
	// records the actual req we built.
	stub := &stubDispatcher{queue: []stubResp{{status: 200, body: string(wrapBrowse(`<DIDL-Lite></DIDL-Lite>`, 0, 0))}}}
	c := NewContentDirectoryClient(stub)
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	if _, err := c.Browse(ctx, testControlURL, "0", "BrowseDirectChildren", "*", 0, 1, ""); err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if got := stub.reqs[0].Context().Value(ctxKey{}); got != "marker" {
		t.Fatalf("req.Context() lost the caller's ctx value (= %v); want %q", got, "marker")
	}
}

func TestBrowseAll_ReturnsErrBrowseLimitOnTruncation(t *testing.T) {
	// Stub returns a single item per page and reports TotalMatches well
	// past MaxBrowseAllItems — the loop must surface ErrBrowseLimit
	// rather than silently truncating (which a sync-style ingest would
	// misread as "everything past N was deleted on the server").
	prev := maxBrowseAllItemsForTesting
	maxBrowseAllItemsForTesting = 5
	defer func() { maxBrowseAllItemsForTesting = prev }()

	item := func(i int) string {
		return `<item id="x` + strconv.Itoa(i) + `" parentID="0"><dc:title>T` + strconv.Itoa(i) + `</dc:title></item>`
	}
	stub := &stubDispatcher{}
	for i := 0; i < 10; i++ {
		stub.queue = append(stub.queue, stubResp{status: 200,
			body: string(wrapBrowse(`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/">`+item(i)+`</DIDL-Lite>`, 1, 99999))})
	}
	c := NewContentDirectoryClient(stub)
	_, items, err := c.BrowseAll(context.Background(), testControlURL, "0")
	if !errors.Is(err, ErrBrowseLimit) {
		t.Fatalf("err = %v; want ErrBrowseLimit", err)
	}
	if len(items) < 5 {
		t.Fatalf("items = %d; want >= 5 (partial results returned alongside the sentinel)", len(items))
	}
}

func TestGetSystemUpdateID_MissingResponse_SurfacesError(t *testing.T) {
	// A server returning <Body/> with no recognizable response element
	// must surface ErrMissingResponseElement, matching ParseBrowseResponse.
	stub := &stubDispatcher{queue: []stubResp{{status: 200,
		body: `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body></s:Body></s:Envelope>`}}}
	c := NewContentDirectoryClient(stub)
	if _, err := c.GetSystemUpdateID(context.Background(), testControlURL); !errors.Is(err, ErrMissingResponseElement) {
		t.Fatalf("err = %v; want ErrMissingResponseElement", err)
	}
}
