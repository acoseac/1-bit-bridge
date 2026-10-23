// Fuzz coverage for the player's SOAP layer: the LAN-facing, unauthenticated
// parser of the OpenHome surface, beside DLNA's (internal/dlna's
// FuzzParseSOAPAction). Every request answers one of the table's outcomes,
// and a fault is a fault the table names.
package openhome

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FuzzControlRequest drives the control handler of a service with any
// SOAPAction, Content-Type and body: it never panics; it answers only a
// status the table names; a 500 is a SOAP fault, MustUnderstand or a UPnP
// fault with a code this layer defines (401, 402 or 602) and its
// description; and every reply carries the empty EXT header.
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
	allowed := map[int]bool{400: true, 403: true, 405: true, 408: true, 413: true, 415: true, 500: true, 503: true}
	h := testHandler()
	f.Fuzz(func(t *testing.T, which uint8, soapAction, contentType string, body []byte) {
		s := services[int(which)%len(services)]
		r := request(http.MethodPost, "/openhome/"+s.path+"/control", bytes.NewReader(body))
		if soapAction != "" {
			r.Header["Soapaction"] = []string{soapAction}
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
		if res.StatusCode != http.StatusInternalServerError {
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
			if fault.Fault.Detail == nil || faultDescription[fault.Fault.Detail.Code] == "" ||
				fault.Fault.Detail.Description != faultDescription[fault.Fault.Detail.Code] {
				t.Fatalf("a UPnP fault the layer does not define: %s", raw)
			}
		default:
			t.Fatalf("a fault with the code %q", fault.Fault.Code)
		}
	})
}
