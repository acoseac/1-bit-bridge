// Fuzz coverage for the player's SOAP layer: the LAN-facing, unauthenticated
// parser of the OpenHome surface, beside DLNA's (internal/dlna's
// FuzzParseSOAPAction). Every request answers one of the table's outcomes,
// a fault is a fault the layer defines, and a read's response parses.
package openhome

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// FuzzControlRequest drives the control handler of a service with any
// SOAPAction, Content-Type and body: it never panics; it answers only a
// status the table names, or a response; a 500 is a SOAP fault,
// MustUnderstand or a UPnP fault with a code this layer defines (401, 402,
// 501, 602, 605, or a read's OpenHome 800) and its description; a 200 is
// the response of the action SOAPAction names, which parses as XML, carries
// that action's out-arguments, and whose values that are XML themselves
// (SourceXml, ReadList's TrackList) parse too; and every reply carries the
// empty EXT header (B470 PR 5a3 added the responses). The handler reads an
// engine holding entries of every kind of metadata, what XML cannot carry
// among it, and a track heard with its details, when the engine starts on
// the fuzz's directory; without one, every read is 501.
func FuzzControlRequest(f *testing.F) {
	pl := servicesByPath["playlist"]
	f.Add(uint8(1), `"`+pl.serviceType()+`#Play"`, "text/xml", []byte(envelope(call(pl, "Play", ""))))
	f.Add(uint8(1), pl.serviceType()+"#Insert", `text/xml; charset="utf-8"`,
		[]byte(envelope(call(pl, "Insert", "<AfterId>0</AfterId><Uri>u</Uri><Metadata>&lt;DIDL-Lite/&gt;</Metadata>"))))
	f.Add(uint8(1), `"`+pl.serviceType()+`#SeekId"`, "text/xml", []byte(envelope(call(pl, "SeekId", "<Value>-1</Value>"))))
	f.Add(uint8(0), `"urn:av-openhome-org:service:Product:1#SetStandby"`, "text/xml",
		[]byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Header><t xmlns="urn:x" s:mustUnderstand="1"/></s:Header><s:Body/></s:Envelope>`))
	f.Add(uint8(2), "", "", []byte(`<!DOCTYPE x [<!ENTITY a "b">]><x>&a;</x>`))
	f.Add(uint8(3), `"urn:av-openhome-org:service:Time:1#Time"`, "text/xml", []byte("\xff\xfe<"))
	h := testHandler()
	ids := []uint32{0}
	if p, err := tryTestPlayer(f); err != nil {
		f.Logf("the fuzz reads no engine: %v", err)
	} else {
		ids = fuzzQueue(f, p)
		h = p.handler("Room & <Study> \x01")
	}
	for i, c := range readCalls {
		s := servicesByPath[c[0]]
		var args []byte
		for j := 2; j+1 < len(c); j += 2 {
			v := c[j+1]
			switch c[j] {
			case "Id", "Token":
				v = u(ids[i%len(ids)])
			case "IdList":
				for _, id := range ids {
					v += u(id) + " "
				}
			}
			args = append(args, "<"+c[j]+">"+escape(v)+"</"+c[j]+">"...)
		}
		which := uint8(0)
		for k, svc := range services {
			if svc == s {
				which = uint8(k)
			}
		}
		f.Add(which, header(s, c[1]), "text/xml", []byte(envelope(call(s, c[1], string(args)))))
	}
	allowed := map[int]bool{200: true, 400: true, 403: true, 405: true, 408: true, 413: true, 415: true, 500: true, 503: true}
	f.Fuzz(func(t *testing.T, which uint8, sa, contentType string, body []byte) {
		s := services[int(which)%len(services)]
		r := request(http.MethodPost, "/openhome/"+s.path+"/control", bytes.NewReader(body))
		if sa != "" {
			r.Header["Soapaction"] = []string{sa}
		}
		if contentType != "" {
			r.Header["Content-Type"] = []string{contentType}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		res := rec.Result()
		if !allowed[res.StatusCode] {
			t.Fatalf("status %d, which the table does not name", res.StatusCode)
		}
		if v, ok := res.Header["EXT"]; !ok || len(v) != 1 || v[0] != "" {
			t.Fatalf("the EXT header is %q", v)
		}
		switch res.StatusCode {
		case http.StatusOK:
			_, name, ok := soapAction(r.Header)
			if !ok || s.action(name) == nil || reads[s.name][name] == nil {
				t.Fatalf("a response to %q, which is no read of %s", sa, s.name)
			}
			got := readResult(t, res, s, name)
			for _, k := range []string{"SourceXml", "ReadList"} {
				if name == k {
					for _, v := range got.values {
						wellFormed(t, name, v)
					}
				}
			}
			return
		case http.StatusInternalServerError:
		default:
			return
		}
		raw, _ := io.ReadAll(res.Body)
		var fault soapFaultDoc
		if err := xml.Unmarshal(raw, &fault); err != nil {
			t.Fatalf("a 500 that is no SOAP fault: %v\n%s", err, raw)
		}
		switch fault.Fault.Code {
		case "s:MustUnderstand":
			if fault.Fault.Detail != nil {
				t.Fatalf("a MustUnderstand fault with a detail: %s", raw)
			}
		case "s:Client":
			if fault.Fault.Detail == nil || !knownFault(fault.Fault.Detail.Code, fault.Fault.Detail.Description) {
				t.Fatalf("a UPnP fault the layer does not define: %s", raw)
			}
		default:
			t.Fatalf("a fault with the code %q", fault.Fault.Code)
		}
	})
}

// fuzzQueue fills p's queue with entries of every kind of metadata (none,
// DIDL-Lite, markup and references, what XML 1.0 cannot carry, the most an
// entry holds), plays it with the first track heard and its details
// reported, and returns the IDs.
func fuzzQueue(tb testing.TB, p *testPlayer) []uint32 {
	tb.Helper()
	ids := []uint32{
		p.add("http://192.168.1.5:7790/dlna/file/0123456789abcdef", []byte(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/"><item id="1"><dc:title>T</dc:title></item></DIDL-Lite>`)),
		p.add("b", nil),
		p.add("c&d<e>'f\"", []byte("&amp; <![CDATA[x]]> \"' \t\r\n é")),
		p.add("d", []byte("\x00\x01\xff\xfe￾￿")),
		p.add("e", bytes.Repeat([]byte("<&>"), engine.MaxMetadataBytes/3)),
	}
	must(tb, "play", p.eng.Play(bg))
	s := p.be.session(tb, 1)
	s.hear(s.first.ID, &details)
	p.heardNow(s.first.ID)
	s.position(s.first.ID, 44_100*7, 44_100)
	return ids
}

// wellFormed fails tb unless doc is one well-formed XML element, read
// strictly, with nothing but white space after it.
func wellFormed(tb testing.TB, what, doc string) {
	tb.Helper()
	d := xml.NewDecoder(bytes.NewReader([]byte(doc)))
	d.Strict = true
	depth, roots := 0, 0
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			tb.Fatalf("%s's value does not parse: %v\n%s", what, err, doc)
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(tk)) != 0 {
				tb.Fatalf("%s's value has text outside its root: %s", what, doc)
			}
		}
	}
	if roots != 1 {
		tb.Fatalf("%s's value has %d roots: %s", what, roots, doc)
	}
}
