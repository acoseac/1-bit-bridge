package dlna

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// SourceProtocolInfo
// -----------------------------------------------------------------------------

func Test_SourceProtocolInfo_AllExpectedMIMEs(t *testing.T) {
	// The source list MUST advertise the superset of MIMEs across all
	// vendors so any renderer's capability-matching logic sees its
	// preferred form. Pin every MIME we care about.
	wantMIMEs := []string{
		"audio/x-dsf",  // Chord / Integra / Onkyo / generic DSD
		"audio/x-dsd",  // some Sony / vendor-neutral DSD alias
		"audio/dsd",    // Sony-preferred DSD
		"audio/x-dff",  // DFF
		"audio/x-flac", // FLAC
		"audio/wav",    // WAV
		"audio/aiff",   // AIFF
		"audio/mp4",    // ALAC + AAC container
		"audio/mpeg",   // MP3
		"audio/ogg",    // OGG
	}
	for _, mime := range wantMIMEs {
		if !strings.Contains(SourceProtocolInfo, mime) {
			t.Errorf("SourceProtocolInfo missing %q. Full value: %q", mime, SourceProtocolInfo)
		}
	}
}

func Test_SourceProtocolInfo_EveryEntryCarriesDLNAFlags(t *testing.T) {
	// Bit-exact contract: every entry MUST carry DLNA.ORG_CI=0
	// (non-transcoded) and the canonical DLNAFlags. A future
	// refactor that emits CI=1 silently for any entry is a structural
	// mission violation.
	entries := strings.Split(SourceProtocolInfo, ",")
	for i, entry := range entries {
		if !strings.Contains(entry, "DLNA.ORG_OP=01") {
			t.Errorf("entry %d missing DLNA.ORG_OP=01: %q", i, entry)
		}
		if !strings.Contains(entry, "DLNA.ORG_CI=0") {
			t.Errorf("BIT-EXACT VIOLATION: entry %d missing DLNA.ORG_CI=0: %q", i, entry)
		}
		if !strings.Contains(entry, "DLNA.ORG_FLAGS="+DLNAFlags) {
			t.Errorf("entry %d missing canonical DLNAFlags: %q", i, entry)
		}
		if !strings.HasPrefix(entry, "http-get:*:") {
			t.Errorf("entry %d missing http-get:*: prefix: %q", i, entry)
		}
	}
}

func Test_SinkProtocolInfo_IsEmpty(t *testing.T) {
	// MediaServer (we serve audio) — Sink list is empty by definition.
	// Pinned so a future refactor doesn't accidentally add sink
	// entries that would mis-identify the bridge as a MediaRenderer
	// to control points.
	if SinkProtocolInfo != "" {
		t.Errorf("SinkProtocolInfo must be empty for a MediaServer, got %q", SinkProtocolInfo)
	}
}

// -----------------------------------------------------------------------------
// ConnectionManagerHandler
// -----------------------------------------------------------------------------

func buildCMRequest(t *testing.T, action string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/dlna/cm/control", strings.NewReader(""))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ConnectionManager:1#`+action+`"`)
	req.Header.Set("Content-Type", "text/xml; charset=\"utf-8\"")
	return req
}

func Test_CM_GetProtocolInfo_ReturnsSourceAndSink(t *testing.T) {
	h := ConnectionManagerHandler()
	rec := httptest.NewRecorder()
	h(rec, buildCMRequest(t, "GetProtocolInfo"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<Source>") {
		t.Errorf("missing <Source> element in response: %s", body)
	}
	if !strings.Contains(body, "<Sink>") {
		t.Errorf("missing <Sink> element in response: %s", body)
	}
	// The Source field carries the protocolInfo CSV, escaped inside the SOAP envelope
	// (colons and commas don't need XML escape but quotes inside flag values do — none
	// here). Just verify a known MIME substring appears.
	if !strings.Contains(body, "audio/x-dsf") {
		t.Errorf("Source missing canonical DSF MIME: %s", body)
	}
}

func Test_CM_GetCurrentConnectionIDs_ReturnsZero(t *testing.T) {
	h := ConnectionManagerHandler()
	rec := httptest.NewRecorder()
	h(rec, buildCMRequest(t, "GetCurrentConnectionIDs"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<ConnectionIDs>0</ConnectionIDs>`) {
		t.Errorf("expected <ConnectionIDs>0</ConnectionIDs>, got: %s", rec.Body.String())
	}
}

func Test_CM_UnknownActionReturnsInvalidActionFault(t *testing.T) {
	h := ConnectionManagerHandler()
	rec := httptest.NewRecorder()
	h(rec, buildCMRequest(t, "PrepareForConnection"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("unknown action should return 500 SOAPFault, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<errorCode>401</errorCode>") {
		t.Errorf("expected errorCode 401 (InvalidAction), got: %s", rec.Body.String())
	}
}

func Test_CM_NonPostReturns405(t *testing.T) {
	h := ConnectionManagerHandler()
	req := httptest.NewRequest(http.MethodGet, "/dlna/cm/control", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET should return 405, got %d", rec.Code)
	}
}
