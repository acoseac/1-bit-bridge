// Fuzz coverage for the upstream-MediaServer response parsers.
//
// The UPnP ingest walks a third-party server (the Chord 2Go, MinimServer, …)
// and decodes whatever it answers. DIDL-Lite is the awkward case: it arrives
// XML-ESCAPED INSIDE an XML element, so `ParseBrowseResponse` unescapes and
// then re-parses — two decode passes over remote input, with the inner one
// operating on a string the outer pass produced.
//
// That double decode is the reason to fuzz rather than table-test: the
// interesting inputs are the ones where the two layers disagree about where
// the document ends, and those are not values anyone writes by hand.
package upnp

import "testing"

func FuzzParseBrowseResponse(f *testing.F) {
	f.Add([]byte(`<s:Envelope><s:Body><u:BrowseResponse>` +
		`<Result>&lt;DIDL-Lite&gt;&lt;/DIDL-Lite&gt;</Result>` +
		`<NumberReturned>0</NumberReturned><TotalMatches>0</TotalMatches>` +
		`</u:BrowseResponse></s:Body></s:Envelope>`))
	// A fault body — must surface ErrSOAPFault, not a decode panic.
	f.Add([]byte(`<s:Envelope><s:Body><s:Fault><faultstring>err</faultstring></s:Fault></s:Body></s:Envelope>`))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParseBrowseResponse(b) })
}

func FuzzParseSystemUpdateID(f *testing.F) {
	f.Add([]byte(`<s:Envelope><s:Body><u:GetSystemUpdateIDResponse><Id>1</Id>` +
		`</u:GetSystemUpdateIDResponse></s:Body></s:Envelope>`))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = parseSystemUpdateID(b) })
}
