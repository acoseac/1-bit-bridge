package dlna

import (
	"strings"
	"testing"
)

func Test_ParseSOAPAction(t *testing.T) {
	cases := []struct {
		name            string
		header          string
		wantServiceType string
		wantActionName  string
	}{
		{
			name:            "quoted_browse",
			header:          `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`,
			wantServiceType: "urn:schemas-upnp-org:service:ContentDirectory:1",
			wantActionName:  "Browse",
		},
		{
			name:            "unquoted_browse",
			header:          `urn:schemas-upnp-org:service:ContentDirectory:1#Browse`,
			wantServiceType: "urn:schemas-upnp-org:service:ContentDirectory:1",
			wantActionName:  "Browse",
		},
		{
			name:            "whitespace_trimmed",
			header:          `  "urn:schemas-upnp-org:service:ConnectionManager:1#GetProtocolInfo"  `,
			wantServiceType: "urn:schemas-upnp-org:service:ConnectionManager:1",
			wantActionName:  "GetProtocolInfo",
		},
		{
			name:            "missing_hash_returns_empty",
			header:          `urn:schemas-upnp-org:service:ContentDirectory:1`,
			wantServiceType: "",
			wantActionName:  "",
		},
		{
			name:            "trailing_hash_returns_empty",
			header:          `urn:schemas-upnp-org:service:ContentDirectory:1#`,
			wantServiceType: "",
			wantActionName:  "",
		},
		{
			name:            "empty_header",
			header:          "",
			wantServiceType: "",
			wantActionName:  "",
		},
		{
			name:            "only_quotes",
			header:          `""`,
			wantServiceType: "",
			wantActionName:  "",
		},
		{
			name:            "action_with_hash_in_servicetype_path",
			header:          `"a#b#c"`,
			wantServiceType: "a#b",
			wantActionName:  "c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotST, gotAction := ParseSOAPAction(tc.header)
			if gotST != tc.wantServiceType {
				t.Errorf("serviceType = %q, want %q", gotST, tc.wantServiceType)
			}
			if gotAction != tc.wantActionName {
				t.Errorf("actionName = %q, want %q", gotAction, tc.wantActionName)
			}
		})
	}
}

func Test_SOAPResponseEnvelope_GoldenShape(t *testing.T) {
	got := SOAPResponseEnvelope(
		"urn:schemas-upnp-org:service:ContentDirectory:1",
		"Browse",
		`<Result>foo</Result><NumberReturned>1</NumberReturned><TotalMatches>1</TotalMatches><UpdateID>1</UpdateID>`,
	)
	want := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body>` +
		`<u:BrowseResponse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<Result>foo</Result><NumberReturned>1</NumberReturned><TotalMatches>1</TotalMatches><UpdateID>1</UpdateID>` +
		`</u:BrowseResponse>` +
		`</s:Body>` +
		`</s:Envelope>`
	if string(got) != want {
		t.Errorf("SOAP response envelope mismatch:\ngot:\n%s\n\nwant:\n%s", string(got), want)
	}
}

func Test_SOAPFaultEnvelope_GoldenShape(t *testing.T) {
	got := SOAPFaultEnvelope(UPnPErrNoSuchObject)
	want := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body>` +
		`<s:Fault>` +
		`<faultcode>s:Client</faultcode>` +
		`<faultstring>UPnPError</faultstring>` +
		`<detail>` +
		`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0">` +
		`<errorCode>701</errorCode>` +
		`<errorDescription>No Such Object</errorDescription>` +
		`</UPnPError>` +
		`</detail>` +
		`</s:Fault>` +
		`</s:Body>` +
		`</s:Envelope>`
	if string(got) != want {
		t.Errorf("SOAP fault envelope mismatch:\ngot:\n%s\n\nwant:\n%s", string(got), want)
	}
}

func Test_SOAPFaultEnvelope_AllStandardCodes(t *testing.T) {
	cases := []struct {
		code         int
		wantDescSubs string
	}{
		{UPnPErrInvalidAction, "Invalid Action"},
		{UPnPErrInvalidArgs, "Invalid Args"},
		{UPnPErrActionFailed, "Action Failed"},
		{UPnPErrNoSuchObject, "No Such Object"},
		{99999, "Unknown Error"}, // unrecognized code falls through to generic
	}
	for _, tc := range cases {
		got := string(SOAPFaultEnvelope(tc.code))
		if !strings.Contains(got, tc.wantDescSubs) {
			t.Errorf("code %d: missing description substring %q in fault: %s", tc.code, tc.wantDescSubs, got)
		}
		// Verify the numeric error code lands in the envelope literally
		codeAsString := ""
		switch tc.code {
		case 401:
			codeAsString = "<errorCode>401</errorCode>"
		case 402:
			codeAsString = "<errorCode>402</errorCode>"
		case 501:
			codeAsString = "<errorCode>501</errorCode>"
		case 701:
			codeAsString = "<errorCode>701</errorCode>"
		case 99999:
			codeAsString = "<errorCode>99999</errorCode>"
		}
		if codeAsString != "" && !strings.Contains(got, codeAsString) {
			t.Errorf("code %d: missing numeric code element %q in fault: %s", tc.code, codeAsString, got)
		}
	}
}

func Test_SOAPConstants(t *testing.T) {
	if SOAPContentType != `text/xml; charset="utf-8"` {
		t.Errorf("SOAPContentType drift: got %q", SOAPContentType)
	}
	if SOAPResponseHeader != "EXT" {
		t.Errorf("SOAPResponseHeader must be 'EXT' per UPnP UDA spec, got %q", SOAPResponseHeader)
	}
}
