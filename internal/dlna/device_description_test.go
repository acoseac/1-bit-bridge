package dlna

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func Test_DeviceDescriptionXML_ContainsCanonicalElements(t *testing.T) {
	xml := string(DeviceDescriptionXML(DeviceDescriptionOpts{
		UDN:              "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c",
		FriendlyName:     "1-bit Bridge (test)",
		Manufacturer:     "acoseac",
		ManufacturerURL:  "https://github.com/acoseac/1-bit-bridge",
		ModelDescription: "1-bit DLNA MediaServer",
		ModelName:        "1-bit Bridge",
		ModelNumber:      "v0.1.4",
	}))
	wantSubs := []string{
		// XML preamble
		`<?xml version="1.0" encoding="UTF-8"?>`,
		// UPnP device namespace
		`xmlns="urn:schemas-upnp-org:device-1-0"`,
		// DLNA MediaServer profile token (load-bearing for strict
		// renderers that filter on DLNA conformance)
		`<dlna:X_DLNADOC xmlns:dlna="urn:schemas-dlna-org:device-1-0">DMS-1.50</dlna:X_DLNADOC>`,
		// Device type
		`<deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>`,
		// Caller-supplied fields
		`<friendlyName>1-bit Bridge (test)</friendlyName>`,
		`<manufacturer>acoseac</manufacturer>`,
		`<UDN>uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c</UDN>`,
		// Service list — both ContentDirectory + ConnectionManager
		`<serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>`,
		`<SCPDURL>/dlna/cds.xml</SCPDURL>`,
		`<controlURL>/dlna/cds/control</controlURL>`,
		`<serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>`,
		`<SCPDURL>/dlna/cm.xml</SCPDURL>`,
		`<controlURL>/dlna/cm/control</controlURL>`,
	}
	for _, sub := range wantSubs {
		if !strings.Contains(xml, sub) {
			t.Errorf("device description missing substring %q. Full XML:\n%s", sub, xml)
		}
	}
}

func Test_DeviceDescriptionXML_XMLEscapesSpecialChars(t *testing.T) {
	// Caller passes potentially-unsafe text via FriendlyName et al.;
	// the generator must escape it.
	xml := string(DeviceDescriptionXML(DeviceDescriptionOpts{
		UDN:              "uuid:test",
		FriendlyName:     `Bridge "Foo" & <Bar>`,
		Manufacturer:     "vendor & co",
		ModelDescription: "model'with'apostrophes",
	}))
	// Each escaped substring should appear; the raw unescaped form
	// should not (it would be invalid XML).
	mustContain := []string{
		`Bridge &quot;Foo&quot; &amp; &lt;Bar&gt;`,
		`vendor &amp; co`,
		`model&apos;with&apos;apostrophes`,
	}
	for _, want := range mustContain {
		if !strings.Contains(xml, want) {
			t.Errorf("expected escaped substring %q, got: %s", want, xml)
		}
	}
}

func Test_DeviceDescriptionHandler_ServesCorrectContentType(t *testing.T) {
	h := DeviceDescriptionHandler(DeviceDescriptionOpts{
		UDN: "uuid:test", FriendlyName: "Test",
	})
	req := httptest.NewRequest(http.MethodGet, "/dlna/description.xml", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/xml") {
		t.Errorf("Content-Type = %q, want text/xml prefix", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `<UDN>uuid:test</UDN>`) {
		t.Errorf("response body missing UDN: %s", rec.Body.String())
	}
}

func Test_SCPDHandler_ServesContentDirectorySCPD(t *testing.T) {
	h := SCPDHandler(ContentDirectorySCPDXML)
	req := httptest.NewRequest(http.MethodGet, "/dlna/cds.xml", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Browse action MUST be declared in the SCPD (it's the only action
	// we implement).
	if !strings.Contains(body, "<name>Browse</name>") {
		t.Errorf("CDS SCPD missing Browse action declaration: %s", body)
	}
}

func Test_SCPDHandler_ServesConnectionManagerSCPD(t *testing.T) {
	h := SCPDHandler(ConnectionManagerSCPDXML)
	req := httptest.NewRequest(http.MethodGet, "/dlna/cm.xml", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"<name>GetProtocolInfo</name>", "<name>GetCurrentConnectionIDs</name>"} {
		if !strings.Contains(body, want) {
			t.Errorf("CM SCPD missing action declaration %q in body: %s", want, body)
		}
	}
}

func Test_SCPDXMLConstants_ValidXMLPreamble(t *testing.T) {
	// Renderers strictly require the XML 1.0 preamble; some older
	// stacks silently drop responses without it.
	for name, scpd := range map[string]string{
		"CDS": ContentDirectorySCPDXML,
		"CM":  ConnectionManagerSCPDXML,
	} {
		if !strings.HasPrefix(scpd, `<?xml version="1.0" encoding="UTF-8"?>`) {
			t.Errorf("%s SCPD missing XML preamble", name)
		}
	}
}
