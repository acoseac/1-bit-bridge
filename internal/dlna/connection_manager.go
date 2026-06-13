package dlna

import (
	"fmt"
	"net/http"
	"strings"
)

// ConnectionManagerServiceType is the canonical UPnP service type for
// ConnectionManager:1. Surfaced as a constant so the handler + device
// description + SCPD all reference the same string.
const ConnectionManagerServiceType = "urn:schemas-upnp-org:service:ConnectionManager:1"

// SourceProtocolInfo is the CSV-formatted list of `protocolInfo`
// entries the bridge advertises to control points via the
// `GetProtocolInfo` SOAP action. Each entry follows the
// `http-get:*:<mime>:<DLNA.ORG flags>` shape — same wire form as
// individual track `<res protocolInfo>` attributes in DIDL-Lite.
//
// **Important:** the per-track MIME advertised in DIDL-Lite is
// resolved per-renderer via `PreferredMIMEFor` (Sony gets audio/dsd
// for DSF, Chord/MPD gets audio/x-dsf, etc.). The static
// `SourceProtocolInfo` here advertises the SUPERSET — all MIME forms
// for all formats we serve — so any renderer's capability-matching
// logic sees its preferred form somewhere in the list. Renderers
// match against their sink-list intersected with our source-list to
// determine playable formats; we want our source-list to be
// permissive so we don't accidentally exclude a renderer that has a
// niche MIME requirement.
//
// Entry order isn't load-bearing but is kept stable for golden-test
// purposes. DSF / DFF entries appear first (audio formats we
// specifically prioritize); PCM formats after.
//
// All entries carry `DLNA.ORG_CI=0` — the bit-exact contract is
// advertised at every layer so a strict renderer's capability
// matcher always sees "non-transcoded".
var SourceProtocolInfo = buildSourceProtocolInfo()

// buildSourceProtocolInfo returns the comma-separated source
// protocolInfo string. Computed once at package init via this helper
// (rather than as a string literal) so the DLNAFlags constant flows
// through — a future bump to DLNAFlags doesn't require touching this
// file separately.
func buildSourceProtocolInfo() string {
	entries := []string{
		// DSD formats — advertise both audio/x-dsf and audio/dsd /
		// audio/x-dsd so Chord-family + Sony + Integra/Onkyo
		// renderers all see their preferred form somewhere.
		mkEntry("audio/x-dsf"),
		mkEntry("audio/x-dsd"),
		mkEntry("audio/dsd"),
		mkEntry("audio/x-dff"),
		// PCM lossless
		mkEntry("audio/x-flac"),
		mkEntry("audio/wav"),
		mkEntry("audio/aiff"),
		mkEntry("audio/mp4"), // ALAC + AAC container
		// PCM lossy
		mkEntry("audio/mpeg"), // MP3
		mkEntry("audio/ogg"),
	}
	return strings.Join(entries, ",")
}

// mkEntry builds one `protocolInfo` CSV entry with the standard
// DLNA.ORG flag set. Reuses DLNAFlags from protocol_info.go so a
// flag-string drift only requires one source-of-truth update.
func mkEntry(mime string) string {
	return "http-get:*:" + mime + ":DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=" + DLNAFlags
}

// SinkProtocolInfo is the bridge's sink list — empty because the bridge
// is a MediaServer (sends audio), not a MediaRenderer (receives audio).
// The empty string is a spec-valid value for `GetProtocolInfo`'s
// `Sink` output argument when the device is server-only.
const SinkProtocolInfo = ""

// ConnectionManagerHandler returns an http.HandlerFunc that dispatches
// SOAP requests against the ConnectionManager:1 service at
// /dlna/cm/control. Supported actions:
//
//   - GetProtocolInfo — returns SourceProtocolInfo + SinkProtocolInfo
//   - GetCurrentConnectionIDs — returns the CSV list of active
//     connection IDs (we don't track these; always returns "0" per
//     UPnP spec convention for "single transient connection")
//
// Any other action returns SOAPFault with UPnPErrInvalidAction.
//
// The handler doesn't need a LibrarySource — both supported actions
// return static information. No body parsing required (the actions
// take zero input arguments).
func ConnectionManagerHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		_, actionName := ParseSOAPAction(r.Header.Get("SOAPAction"))
		switch actionName {
		case "GetProtocolInfo":
			body := SOAPResponseEnvelope(
				ConnectionManagerServiceType,
				"GetProtocolInfo",
				fmt.Sprintf(`<Source>%s</Source><Sink>%s</Sink>`,
					escapeXMLText(SourceProtocolInfo),
					escapeXMLText(SinkProtocolInfo)),
			)
			writeSOAPSuccess(w, body)

		case "GetCurrentConnectionIDs":
			// We don't track per-connection state; spec says "0" is
			// the canonical CSV for "single transient connection".
			body := SOAPResponseEnvelope(
				ConnectionManagerServiceType,
				"GetCurrentConnectionIDs",
				`<ConnectionIDs>0</ConnectionIDs>`,
			)
			writeSOAPSuccess(w, body)

		default:
			writeSOAPFault(w, UPnPErrInvalidAction)
		}
	}
}

// writeSOAPSuccess writes a SOAP success response with the standard
// headers (Content-Type, EXT). Sibling helper to writeSOAPFault.
func writeSOAPSuccess(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", SOAPContentType)
	w.Header().Set(SOAPResponseHeader, "")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
