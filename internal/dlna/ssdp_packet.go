package dlna

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SSDPMulticastAddr is the canonical SSDP IPv4 multicast endpoint per
// UPnP Device Architecture 1.0 / 1.1. Every UPnP device on a LAN
// listens here for M-SEARCH requests and sends NOTIFY announcements
// here at startup, on a periodic interval, and at shutdown.
const SSDPMulticastAddr = "239.255.255.250:1900"

// SSDPMaxAge is the cache lifetime (seconds) the bridge advertises in
// SSDP CACHE-CONTROL headers. Per the spec, control points consider
// the advertised entry stale after `max-age` seconds; the advertiser
// MUST re-announce well before that interval to avoid being purged.
// 1800s (30 min) matches the conventional UPnP MediaServer default.
const SSDPMaxAge = 1800

// SSDPServerToken returns the canonical SSDP `SERVER` header value the
// bridge uses in NOTIFY and M-SEARCH-RESPONSE packets. Format per
// UDA spec: `<os>/<osver> UPnP/1.0 <productname>/<productver>`. The
// "DLNADOC/1.50" suffix is the DLNA Media-Server profile token —
// required by some renderers to recognize us as a DLNA-conforming
// MediaServer (vs. a bare UPnP MediaServer).
//
// `productVersion` is the bridge's `internal/version.ServerVersion`
// (e.g. "v0.1.4"). Callers supply it so the SSDP packets reflect the
// actual running build for telemetry / diagnostic clarity on the
// renderer side ("Server: 1-bit-bridge/v0.1.4...").
func SSDPServerToken(productVersion string) string {
	if productVersion == "" {
		productVersion = "dev"
	}
	// Wire shape kept stable: clients sometimes substring-match on
	// "1-bit-bridge" for telemetry / vendor identification.
	return fmt.Sprintf("Darwin/UPnP/1.0 1-bit-bridge/%s DLNADOC/1.50", productVersion)
}

// NotifyTarget pairs an NT (Notification Type) value with its
// corresponding USN (Unique Service Name) value. UPnP devices
// advertise themselves across multiple NT axes — the MediaServer
// advertises 5 in lockstep on every alive / byebye cycle, so the
// renderer's discovery table contains the device under every name
// it might search for.
type NotifyTarget struct {
	NT  string
	USN string
}

// NotifyTargetsFor returns the canonical 5-tuple of (NT, USN) values
// the bridge advertises for a MediaServer with the given UDN. The
// UDN MUST be the full string including the `uuid:` prefix (e.g.
// `"uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c"`).
//
// Order is deterministic for golden-test stability AND matches the
// spec-recommended order (rootdevice first → upnp:rootdevice is the
// most universally-searched ST, so emitting it first gets us into
// renderer tables fastest).
func NotifyTargetsFor(udn string) []NotifyTarget {
	return []NotifyTarget{
		{NT: "upnp:rootdevice", USN: udn + "::upnp:rootdevice"},
		{NT: udn, USN: udn},
		{NT: "urn:schemas-upnp-org:device:MediaServer:1", USN: udn + "::urn:schemas-upnp-org:device:MediaServer:1"},
		{NT: "urn:schemas-upnp-org:service:ContentDirectory:1", USN: udn + "::urn:schemas-upnp-org:service:ContentDirectory:1"},
		{NT: "urn:schemas-upnp-org:service:ConnectionManager:1", USN: udn + "::urn:schemas-upnp-org:service:ConnectionManager:1"},
	}
}

// BuildNotifyAlive returns the bytes of a single `ssdp:alive` NOTIFY
// packet. Multiplied across all 5 NotifyTargets per advertise cycle
// to populate every entry in the renderer's discovery table.
//
// `location` is the absolute URL of the bridge's DLNA device
// description XML (e.g. "http://192.168.0.14:7790/dlna/description.xml").
// `server` is the SSDP SERVER token (see `SSDPServerToken`).
//
// **Line endings are CRLF** per HTTP / SSDP spec — `\r\n` between
// every header AND a trailing `\r\n\r\n` to terminate the packet.
// Some renderers parse SSDP strictly and silently drop packets with
// bare-LF line endings.
func BuildNotifyAlive(location, server string, target NotifyTarget) []byte {
	return []byte(
		"NOTIFY * HTTP/1.1\r\n" +
			"HOST: " + SSDPMulticastAddr + "\r\n" +
			"CACHE-CONTROL: max-age=" + itoa(SSDPMaxAge) + "\r\n" +
			"LOCATION: " + location + "\r\n" +
			"SERVER: " + server + "\r\n" +
			"NT: " + target.NT + "\r\n" +
			"NTS: ssdp:alive\r\n" +
			"USN: " + target.USN + "\r\n" +
			"\r\n",
	)
}

// BuildNotifyByeBye returns the bytes of a single `ssdp:byebye`
// NOTIFY packet. Multiplied across all 5 NotifyTargets on graceful
// shutdown so renderers purge our entry from their discovery tables
// immediately rather than waiting for the SSDPMaxAge timeout.
//
// Per spec, byebye packets MAY omit LOCATION / SERVER / CACHE-CONTROL
// (they're not load-bearing for the purge operation), but several
// real-world renderers expect them anyway. We include LOCATION + SERVER
// for compatibility; CACHE-CONTROL is omitted to follow the spec
// minimum required-fields set on byebye.
func BuildNotifyByeBye(location, server string, target NotifyTarget) []byte {
	return []byte(
		"NOTIFY * HTTP/1.1\r\n" +
			"HOST: " + SSDPMulticastAddr + "\r\n" +
			"LOCATION: " + location + "\r\n" +
			"SERVER: " + server + "\r\n" +
			"NT: " + target.NT + "\r\n" +
			"NTS: ssdp:byebye\r\n" +
			"USN: " + target.USN + "\r\n" +
			"\r\n",
	)
}

// BuildMSearchResponse returns the bytes of an SSDP M-SEARCH response
// packet — sent UNICAST back to the source IP/port of an incoming
// M-SEARCH multicast request whose ST matches one of our advertised
// NotifyTargets. The DATE header uses Go's `http.TimeFormat` — the
// HTTP-compliant IMF-fixdate shape `"Mon, 02 Jan 2006 15:04:05 GMT"`
// per RFC 7231 §7.1.1.1. Pre-fix this used `time.RFC1123` which
// renders the TZ name (so a UTC-formatted time produces `"UTC"`
// instead of the spec-required literal `"GMT"`); some strict
// renderers reject the `UTC` form. Per CodeRabbit Major on PR #303.
//
// `st` is the response's ST header value — MUST match the ST from
// the incoming M-SEARCH that triggered this response. `usn` is the
// matching NotifyTarget's USN.
func BuildMSearchResponse(location, server, st, usn string, date time.Time) []byte {
	return []byte(
		"HTTP/1.1 200 OK\r\n" +
			"CACHE-CONTROL: max-age=" + itoa(SSDPMaxAge) + "\r\n" +
			"DATE: " + date.UTC().Format(http.TimeFormat) + "\r\n" +
			"EXT:\r\n" +
			"LOCATION: " + location + "\r\n" +
			"SERVER: " + server + "\r\n" +
			"ST: " + st + "\r\n" +
			"USN: " + usn + "\r\n" +
			"\r\n",
	)
}

// MSearchTargets selects which NotifyTargets (if any) should respond
// to an incoming M-SEARCH with the given ST. Per UPnP spec:
//
//   - `ssdp:all` — every NotifyTarget responds (renderer wants the full
//     enumeration of services the device offers)
//   - `upnp:rootdevice` — only the rootdevice target responds
//   - `uuid:<UDN>` — only the UDN target responds
//   - `urn:schemas-upnp-org:device:...` — match the specific device URN
//   - `urn:schemas-upnp-org:service:...` — match the specific service URN
//   - anything else — no response (renderer wasn't looking for us)
//
// Returns the SUBSET of `allTargets` that should respond. Empty
// slice = no response. Pure function — easy to golden-test against
// the spec's truth table without touching sockets.
func MSearchTargets(st string, allTargets []NotifyTarget) []NotifyTarget {
	switch st {
	case "ssdp:all":
		// Return a defensive copy so the caller can't mutate the
		// canonical slice if they sort or filter the result.
		out := make([]NotifyTarget, len(allTargets))
		copy(out, allTargets)
		return out
	case "":
		// Malformed M-SEARCH without ST — no response. Spec
		// technically requires ST; some buggy clients omit it.
		return nil
	}
	// Direct match against any target's NT
	for _, t := range allTargets {
		if t.NT == st {
			return []NotifyTarget{t}
		}
	}
	return nil
}

// ParseMSearchST extracts the ST header value from a raw SSDP
// M-SEARCH packet (already received over UDP). Returns "" if the
// packet is not an M-SEARCH OR if no ST header is present.
//
// Per HTTP / SSDP convention, header names are case-INSENSITIVE.
// Some renderers send `ST:`, others `St:`, others `st:` — we match
// all three (and any variant) by uppercasing the header-name portion
// before comparison.
//
// **Doesn't validate HOST / MAN** — those are required by spec but in
// practice we just need ST to know what to respond with. We accept any
// packet that's RECOGNIZABLY an M-SEARCH (starts with "M-SEARCH ") with
// an ST header. The MX header IS parsed separately via `ParseMSearchMX`
// for the spec-mandated randomized response delay.
func ParseMSearchST(packet []byte) string {
	s := string(packet)
	if !strings.HasPrefix(s, "M-SEARCH ") {
		return ""
	}
	for _, line := range strings.Split(s, "\n") { // split on \n; TrimSpace below drops any trailing \r (tolerates bare-LF SSDP)
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToUpper(strings.TrimSpace(line[:colon]))
		if name == "ST" {
			return strings.TrimSpace(line[colon+1:])
		}
	}
	return ""
}

// mxResponseCeilingSeconds clamps the parsed MX value. The UPnP UDA
// caps MX at 120 s, but a responder that honours a 2-minute MX is a
// liability (a hostile or buggy control point could park responders for
// minutes); UPnP-AV practice clamps far lower. 5 s is comfortably above
// any real control point's expectation while bounding the worst case.
const mxResponseCeilingSeconds = 5

// ParseMSearchMX extracts the MX header (maximum response delay, in
// whole seconds) from a raw M-SEARCH packet. Per the UPnP UDA, a device
// MUST wait a random interval in [0, MX] before unicasting its response,
// spreading reply bursts across the LAN so a search matching many
// targets can't overwhelm small switches / low-power renderers.
//
// Returns 0 when MX is absent, non-numeric, or zero — the caller
// defaults that to a small fixed jitter. The value is clamped to
// `mxResponseCeilingSeconds`. Only leading digits are read (stops at
// the first non-digit), matching lenient real-world header parsing.
func ParseMSearchMX(packet []byte) int {
	s := string(packet)
	if !strings.HasPrefix(s, "M-SEARCH ") {
		return 0
	}
	for _, line := range strings.Split(s, "\n") { // split on \n; TrimSpace below drops any trailing \r (tolerates bare-LF SSDP)
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToUpper(strings.TrimSpace(line[:colon]))
		if name != "MX" {
			continue
		}
		val := strings.TrimSpace(line[colon+1:])
		n := 0
		for _, c := range val {
			if c < '0' || c > '9' {
				break // leading-digits only
			}
			n = n*10 + int(c-'0')
			if n >= mxResponseCeilingSeconds {
				return mxResponseCeilingSeconds
			}
		}
		return n
	}
	return 0
}

// itoa returns an integer formatted in base 10 without bringing in
// the `strconv` import (we use fmt for other formatting in this
// package, and dragging in strconv just for this one call would
// inflate the import block). One-line; trivially inlined by the
// Go compiler.
func itoa(i int) string { return fmt.Sprintf("%d", i) }
